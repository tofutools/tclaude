package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
)

// usage_rules_api.go — the HTTP surface for usage limits and usage profiles.
// The /v1 routes serve the CLI and agents; the dashboard reaches the same
// handlers under /api as the synthetic human peer. Reads need usage.read;
// every change needs usage-rules.manage, which is not default-granted and
// not implied by group ownership: an agent able to edit budgets could lift
// its own.

const PermUsageRulesManage = "usage-rules.manage"

func registerUsageRuleRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/usage/rules", handleUsageRulesOverview)
	mux.HandleFunc("GET /v1/usage/self", handleUsageSelf)
	mux.HandleFunc("POST /v1/usage/profiles", handleUsageProfileCreate)
	mux.HandleFunc("PUT /v1/usage/profiles/{ref}", handleUsageProfileUpdate)
	mux.HandleFunc("DELETE /v1/usage/profiles/{ref}", handleUsageProfileDelete)
	mux.HandleFunc("POST /v1/usage/limits", handleUsageLimitCreate)
	mux.HandleFunc("PUT /v1/usage/limits/{id}", handleUsageLimitUpdate)
	mux.HandleFunc("DELETE /v1/usage/limits/{id}", handleUsageLimitDelete)
	mux.HandleFunc("PUT /v1/usage/assignments", handleUsageAssignmentsSet)
	mux.HandleFunc("POST /v1/usage/agents/{selector}/adjust", handleUsageAgentAdjust)
}

func registerDashboardUsageRuleRoutes(mux *http.ServeMux) {
	wrap := dashboardSandboxProfilesRoute // dashboard auth + synthetic human peer
	mux.HandleFunc("GET /api/usage/rules", wrap(handleUsageRulesOverview))
	mux.HandleFunc("POST /api/usage/profiles", wrap(handleUsageProfileCreate))
	mux.HandleFunc("PUT /api/usage/profiles/{ref}", wrap(handleUsageProfileUpdate))
	mux.HandleFunc("DELETE /api/usage/profiles/{ref}", wrap(handleUsageProfileDelete))
	mux.HandleFunc("POST /api/usage/limits", wrap(handleUsageLimitCreate))
	mux.HandleFunc("PUT /api/usage/limits/{id}", wrap(handleUsageLimitUpdate))
	mux.HandleFunc("DELETE /api/usage/limits/{id}", wrap(handleUsageLimitDelete))
	mux.HandleFunc("PUT /api/usage/assignments", wrap(handleUsageAssignmentsSet))
	mux.HandleFunc("POST /api/usage/agents/{selector}/adjust", wrap(handleUsageAgentAdjust))
}

// reevaluateUsageRules re-runs the evaluator after a change, so new limits,
// budgets and adjustments take effect (and show) without waiting a tick.
var reevaluateUsageRules = func() {
	goBackground(func() { pollUsageRules(time.Now()) })
}

type usageAssignmentView struct {
	db.UsageProfileAssignment
	ScopeName   string `json:"scope_name,omitempty"`
	ProfileName string `json:"profile_name"`
}

type usageLimitView struct {
	*db.UsageLimit
	Group string `json:"group,omitempty"`
}

type usageStatusView struct {
	usageRuleStatus
	AgentTitle string `json:"agent_title,omitempty"`
}

type usageRulesOverview struct {
	Profiles    []*db.UsageProfile       `json:"profiles"`
	Limits      []usageLimitView         `json:"limits"`
	Assignments []usageAssignmentView    `json:"assignments"`
	Overrides   []*db.UsageAgentOverride `json:"overrides"`
	Statuses    []usageStatusView        `json:"statuses"`
	// LimitStatuses is the latest reading of every enabled limit.
	LimitStatuses []usageStatusView `json:"limit_statuses"`
	EvaluatedAt   string            `json:"evaluated_at,omitempty"`
	// Groups, SpawnProfiles and Agents name the assignment targets, so an
	// editor can offer them without reading the rest of the dashboard.
	Groups        []string            `json:"groups"`
	SpawnProfiles []string            `json:"spawn_profiles"`
	Agents        []usageAgentRefView `json:"agents"`
	// Windows names the provider quota windows a window_pct limit can watch.
	Windows []string `json:"windows"`
}

type usageAgentRefView struct {
	ID    string `json:"id"`
	Title string `json:"title,omitempty"`
}

func usageStatusViews(statuses []usageRuleStatus) []usageStatusView {
	out := make([]usageStatusView, 0, len(statuses))
	titles := map[string]string{}
	for _, s := range statuses {
		v := usageStatusView{usageRuleStatus: s}
		if s.AgentID != "" {
			title, ok := titles[s.AgentID]
			if !ok {
				if a, err := db.GetAgent(s.AgentID); err == nil && a != nil {
					title = agent.CachedTitle(a.CurrentConvID)
				}
				titles[s.AgentID] = title
			}
			v.AgentTitle = title
		}
		out = append(out, v)
	}
	return out
}

func handleUsageRulesOverview(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRead); !ok {
		return
	}
	out, err := collectUsageRulesOverview()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func collectUsageRulesOverview() (usageRulesOverview, error) {
	out := usageRulesOverview{Profiles: []*db.UsageProfile{}, Limits: []usageLimitView{}, Assignments: []usageAssignmentView{},
		Overrides: []*db.UsageAgentOverride{}, Groups: []string{}, SpawnProfiles: []string{}, Agents: []usageAgentRefView{}, Windows: []string{}}
	profiles, err := db.ListUsageProfiles()
	if err != nil {
		return out, err
	}
	names := map[int64]string{}
	for _, p := range profiles {
		names[p.ID] = p.Name
		out.Profiles = append(out.Profiles, p)
	}
	groups, err := db.ListAgentGroups()
	if err != nil {
		return out, err
	}
	groupNames := map[string]string{}
	for _, g := range groups {
		groupNames[db.UsageGroupScopeID(g.ID)] = g.Name
		out.Groups = append(out.Groups, g.Name)
	}
	sort.Strings(out.Groups)
	limits, err := db.ListUsageLimits()
	if err != nil {
		return out, err
	}
	for _, l := range limits {
		out.Limits = append(out.Limits, usageLimitView{UsageLimit: l, Group: groupNames[db.UsageGroupScopeID(l.GroupID)]})
	}
	assignments, err := db.ListUsageProfileAssignments()
	if err != nil {
		return out, err
	}
	spawnProfiles := map[string]string{}
	if sps, err := db.ListSpawnProfiles(); err == nil {
		for _, sp := range sps {
			spawnProfiles[strconv.FormatInt(sp.ID, 10)] = sp.Name
			out.SpawnProfiles = append(out.SpawnProfiles, sp.Name)
		}
	}
	sort.Strings(out.SpawnProfiles)
	if agents, err := db.ListActiveAgents(); err == nil {
		for _, a := range agents {
			out.Agents = append(out.Agents, usageAgentRefView{ID: a.AgentID, Title: agent.CachedTitle(a.CurrentConvID)})
		}
	}
	for _, a := range assignments {
		v := usageAssignmentView{UsageProfileAssignment: a, ProfileName: names[a.ProfileID]}
		switch a.Scope {
		case db.UsageScopeGroup:
			v.ScopeName = groupNames[a.ScopeID]
		case db.UsageScopeSpawnProfile:
			v.ScopeName = spawnProfiles[a.ScopeID]
		case db.UsageScopeAgent:
			if ag, err := db.GetAgent(a.ScopeID); err == nil && ag != nil {
				v.ScopeName = agent.CachedTitle(ag.CurrentConvID)
			}
		}
		out.Assignments = append(out.Assignments, v)
	}
	overrides, err := db.ListUsageAgentOverrides()
	if err != nil {
		return out, err
	}
	for _, o := range overrides {
		out.Overrides = append(out.Overrides, o)
	}
	sort.Slice(out.Overrides, func(i, j int) bool { return out.Overrides[i].AgentID < out.Overrides[j].AgentID })
	statuses, at := latestUsageRuleStatus()
	out.Statuses = usageStatusViews(statuses)
	out.LimitStatuses = usageStatusViews(latestUsageLimitStatus())
	if usage, err := collectAccountUsage(time.Now(), 0); err == nil {
		for _, w := range usage.Windows {
			if spec := w.Provider + ":" + w.WindowName; !slices.Contains(out.Windows, spec) {
				out.Windows = append(out.Windows, spec)
			}
		}
	}
	if !at.IsZero() {
		out.EvaluatedAt = at.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}

type usageSelfResponse struct {
	AgentID     string             `json:"agent_id"`
	Layer       string             `json:"layer,omitempty"`
	Profiles    []*db.UsageProfile `json:"profiles"`
	Statuses    []usageStatusView  `json:"statuses"`
	Limits      []usageStatusView  `json:"limits"`
	Exempt      bool               `json:"exempt,omitempty"`
	SnoozeUntil string             `json:"snooze_until,omitempty"`
	EvaluatedAt string             `json:"evaluated_at,omitempty"`
}

// handleUsageSelf is an agent's own budget view: its effective usage
// profiles, their latest readings, and the limits covering it.
func handleUsageSelf(w http.ResponseWriter, r *http.Request) {
	conv, ok := requirePermission(w, r, PermUsageRead)
	if !ok {
		return
	}
	agentID, err := db.AgentIDForConv(conv)
	if err != nil || agentID == "" {
		writeError(w, http.StatusBadRequest, "agent_required", "usage self requires an identified agent caller")
		return
	}
	out, err := collectUsageSelf(agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func collectUsageSelf(agentID string) (usageSelfResponse, error) {
	out := usageSelfResponse{AgentID: agentID, Profiles: []*db.UsageProfile{}, Statuses: []usageStatusView{}, Limits: []usageStatusView{}}
	profiles, err := db.ListUsageProfiles()
	if err != nil {
		return out, err
	}
	byID := map[int64]*db.UsageProfile{}
	for _, p := range profiles {
		byID[p.ID] = p
	}
	assignments, err := db.ListUsageProfileAssignments()
	if err != nil {
		return out, err
	}
	groups, err := db.ListGroupsForAgent(agentID)
	if err != nil {
		return out, err
	}
	groupIDs := make([]int64, 0, len(groups))
	for _, g := range groups {
		groupIDs = append(groupIDs, g.ID)
	}
	effective, layer := effectiveUsageProfiles(agentID, groupIDs, assignments, byID)
	out.Layer = layer
	if effective != nil {
		out.Profiles = effective
	}
	overrides, err := db.ListUsageAgentOverrides()
	if err != nil {
		return out, err
	}
	if o := overrides[agentID]; o != nil {
		out.Exempt = o.Exempt
		if !o.SnoozeUntil.IsZero() {
			out.SnoozeUntil = o.SnoozeUntil.UTC().Format(time.RFC3339)
		}
	}
	inGroup := map[int64]bool{}
	for _, id := range groupIDs {
		inGroup[id] = true
	}
	statuses, at := latestUsageRuleStatus()
	var mine, limits []usageRuleStatus
	for _, s := range statuses {
		switch {
		case s.AgentID == agentID:
			mine = append(mine, s)
		case s.Scope == string(usagerules.ScopeGlobal), s.Scope == string(usagerules.ScopeGroup) && inGroup[s.GroupID]:
			limits = append(limits, s)
		}
	}
	out.Statuses, out.Limits = usageStatusViews(mine), usageStatusViews(limits)
	if !at.IsZero() {
		out.EvaluatedAt = at.UTC().Format(time.RFC3339Nano)
	}
	return out, nil
}

var usageProfileNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 _.-]{0,63}$`)

type usageProfileBody struct {
	Name  string            `json:"name"`
	Descr string            `json:"descr"`
	Rules []usagerules.Rule `json:"rules"`
}

func decodeUsageBody(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_body", err.Error())
		return false
	}
	return true
}

func (b *usageProfileBody) validate() (*db.UsageProfile, error) {
	b.Name, b.Descr = strings.TrimSpace(b.Name), strings.TrimSpace(b.Descr)
	if !usageProfileNameRE.MatchString(b.Name) {
		return nil, errors.New("profile name must be 1-64 letters, digits, spaces, '_', '.' or '-'")
	}
	if _, err := strconv.ParseInt(b.Name, 10, 64); err == nil {
		// Profiles are addressed by id or name; an all-digit name would be ambiguous.
		return nil, errors.New("profile name must not be only digits")
	}
	if len(b.Descr) > 500 {
		return nil, errors.New("description is limited to 500 characters")
	}
	rules, err := usagerules.ValidateProfileRules(b.Rules)
	if err != nil {
		return nil, err
	}
	return &db.UsageProfile{Name: b.Name, Descr: b.Descr, Rules: rules}, nil
}

// resolveUsageProfileRef finds a profile by numeric id or name.
func resolveUsageProfileRef(ref string) (*db.UsageProfile, error) {
	ref = strings.TrimSpace(ref)
	if id, err := strconv.ParseInt(ref, 10, 64); err == nil {
		if p, err := db.GetUsageProfile(id); err != nil || p != nil {
			return p, err
		}
	}
	return db.GetUsageProfileByName(ref)
}

func writeUsageProfileErr(w http.ResponseWriter, err error) {
	if errors.Is(err, db.ErrUsageProfileNameTaken) {
		writeError(w, http.StatusConflict, "name_taken", err.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "io", err.Error())
}

func handleUsageProfileCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	var body usageProfileBody
	if !decodeUsageBody(w, r, &body) {
		return
	}
	p, err := body.validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_profile", err.Error())
		return
	}
	if err := db.CreateUsageProfile(p); err != nil {
		writeUsageProfileErr(w, err)
		return
	}
	reevaluateUsageRules()
	writeJSON(w, http.StatusOK, p)
}

func handleUsageProfileUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	existing, err := resolveUsageProfileRef(r.PathValue("ref"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such usage profile")
		return
	}
	var body usageProfileBody
	if !decodeUsageBody(w, r, &body) {
		return
	}
	p, err := body.validate()
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_profile", err.Error())
		return
	}
	p.ID, p.CreatedAt = existing.ID, existing.CreatedAt
	if _, err := db.UpdateUsageProfile(p); err != nil {
		writeUsageProfileErr(w, err)
		return
	}
	reevaluateUsageRules()
	writeJSON(w, http.StatusOK, p)
}

func handleUsageProfileDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	p, err := resolveUsageProfileRef(r.PathValue("ref"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such usage profile")
		return
	}
	if _, err := db.DeleteUsageProfile(p.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	reevaluateUsageRules()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": p.Name})
}

type usageLimitBody struct {
	Scope   string          `json:"scope"` // global | group (create only)
	Group   string          `json:"group"` // group name for a group limit (create only)
	Rule    usagerules.Rule `json:"rule"`
	Enabled *bool           `json:"enabled"`
}

func handleUsageLimitCreate(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	var body usageLimitBody
	if !decodeUsageBody(w, r, &body) {
		return
	}
	l := &db.UsageLimit{Scope: strings.TrimSpace(body.Scope), Rule: body.Rule, Enabled: body.Enabled == nil || *body.Enabled}
	scope := usagerules.ScopeGlobal
	switch l.Scope {
	case db.UsageScopeGlobal:
	case db.UsageScopeGroup:
		scope = usagerules.ScopeGroup
		g, err := db.GetAgentGroupByName(strings.TrimSpace(body.Group))
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if g == nil {
			writeError(w, http.StatusNotFound, "group_not_found", fmt.Sprintf("no group %q", body.Group))
			return
		}
		l.GroupID = g.ID
	default:
		writeError(w, http.StatusBadRequest, "invalid_scope", `scope must be "global" or "group"`)
		return
	}
	l.Rule.Normalize()
	if err := l.Rule.Validate(scope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_rule", err.Error())
		return
	}
	if err := db.CreateUsageLimit(l); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	reevaluateUsageRules()
	writeJSON(w, http.StatusOK, l)
}

func usageLimitFromPath(w http.ResponseWriter, r *http.Request) *db.UsageLimit {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_id", "limit id must be a number")
		return nil
	}
	l, err := db.GetUsageLimit(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return nil
	}
	if l == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such usage limit")
	}
	return l
}

func handleUsageLimitUpdate(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	l := usageLimitFromPath(w, r)
	if l == nil {
		return
	}
	var body usageLimitBody
	if !decodeUsageBody(w, r, &body) {
		return
	}
	if body.Scope != "" && body.Scope != l.Scope {
		writeError(w, http.StatusBadRequest, "invalid_scope", "a limit's scope is fixed; delete it and create another")
		return
	}
	scope := usagerules.ScopeGlobal
	if l.Scope == db.UsageScopeGroup {
		scope = usagerules.ScopeGroup
	}
	body.Rule.Normalize()
	if err := body.Rule.Validate(scope); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_rule", err.Error())
		return
	}
	l.Rule = body.Rule
	if body.Enabled != nil {
		l.Enabled = *body.Enabled
	}
	if _, err := db.UpdateUsageLimit(l); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	reevaluateUsageRules()
	writeJSON(w, http.StatusOK, l)
}

func handleUsageLimitDelete(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	l := usageLimitFromPath(w, r)
	if l == nil {
		return
	}
	if _, err := db.DeleteUsageLimit(l.ID); err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	reevaluateUsageRules()
	writeJSON(w, http.StatusOK, map[string]any{"deleted": l.ID})
}

type usageAssignmentsBody struct {
	Scope    string   `json:"scope"`    // global | group | agent | spawn_profile
	Target   string   `json:"target"`   // group name, agent selector or spawn profile name; "" for global
	Profiles []string `json:"profiles"` // usage profile names or ids; empty clears the layer
}

func handleUsageAssignmentsSet(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	var body usageAssignmentsBody
	if !decodeUsageBody(w, r, &body) {
		return
	}
	scopeID, status, err := resolveUsageAssignmentTarget(strings.TrimSpace(body.Scope), strings.TrimSpace(body.Target))
	if err != nil {
		writeError(w, status, "invalid_target", err.Error())
		return
	}
	ids := make([]int64, 0, len(body.Profiles))
	for _, ref := range body.Profiles {
		p, err := resolveUsageProfileRef(ref)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
		if p == nil {
			writeError(w, http.StatusNotFound, "profile_not_found", fmt.Sprintf("no usage profile %q", ref))
			return
		}
		ids = append(ids, p.ID)
	}
	if err := db.SetUsageProfileAssignments(body.Scope, scopeID, ids, ""); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_assignment", err.Error())
		return
	}
	reevaluateUsageRules()
	writeJSON(w, http.StatusOK, map[string]any{"scope": body.Scope, "scope_id": scopeID, "profile_ids": ids})
}

func resolveUsageAssignmentTarget(scope, target string) (string, int, error) {
	switch scope {
	case db.UsageScopeGlobal:
		return "", 0, nil
	case db.UsageScopeGroup:
		g, err := db.GetAgentGroupByName(target)
		if err != nil {
			return "", http.StatusInternalServerError, err
		}
		if g == nil {
			return "", http.StatusNotFound, fmt.Errorf("no group %q", target)
		}
		return db.UsageGroupScopeID(g.ID), 0, nil
	case db.UsageScopeAgent:
		id, _, err := resolveAgentSelectorForResilience(target)
		if err != nil {
			return "", http.StatusNotFound, err
		}
		return id, 0, nil
	case db.UsageScopeSpawnProfile:
		sp, err := db.ResolveSpawnProfile(target)
		if err != nil {
			return "", http.StatusInternalServerError, err
		}
		if sp == nil {
			return "", http.StatusNotFound, fmt.Errorf("no spawn profile %q", target)
		}
		return strconv.FormatInt(sp.ID, 10), 0, nil
	default:
		return "", http.StatusBadRequest, errors.New(`scope must be "global", "group", "agent" or "spawn_profile"`)
	}
}

type usageAdjustBody struct {
	Exempt        *bool    `json:"exempt"`
	SnoozeMinutes *int     `json:"snooze_minutes"` // 0 clears
	RuleKey       string   `json:"rule_key"`       // with extra_budget
	ExtraBudget   *float64 `json:"extra_budget"`   // total extra on rule_key; 0 clears
}

// handleUsageAgentAdjust applies the operator's per-agent adjustments:
// exempt the agent from its usage profiles, snooze them, or give one rule
// extra budget (which re-arms that rule's ladder at the new total).
func handleUsageAgentAdjust(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermUsageRulesManage); !ok {
		return
	}
	agentID, _, err := resolveAgentSelectorForResilience(r.PathValue("selector"))
	if err != nil {
		writeError(w, http.StatusNotFound, "agent_not_found", err.Error())
		return
	}
	var body usageAdjustBody
	if !decodeUsageBody(w, r, &body) {
		return
	}
	overrides, err := db.ListUsageAgentOverrides()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	cur := overrides[agentID]
	if cur == nil {
		cur = &db.UsageAgentOverride{AgentID: agentID}
	}
	if body.Exempt != nil || body.SnoozeMinutes != nil {
		exempt, snooze := cur.Exempt, cur.SnoozeUntil
		if body.Exempt != nil {
			exempt = *body.Exempt
		}
		if body.SnoozeMinutes != nil {
			switch m := *body.SnoozeMinutes; {
			case m < 0 || m > 7*24*60:
				writeError(w, http.StatusBadRequest, "invalid_snooze", "snooze_minutes must be between 0 and 10080 (a week)")
				return
			case m == 0:
				snooze = time.Time{}
			default:
				snooze = time.Now().Add(time.Duration(m) * time.Minute)
			}
		}
		if err := db.SetUsageAgentOverride(agentID, exempt, snooze); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
	}
	if body.ExtraBudget != nil {
		extra := *body.ExtraBudget
		if extra < 0 || extra > 1e6 || extra != extra {
			writeError(w, http.StatusBadRequest, "invalid_extension", "extra_budget must be between 0 and 1000000")
			return
		}
		key, err := canonicalUsageRuleKey(body.RuleKey)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_extension", err.Error())
			return
		}
		if err := db.SetUsageBudgetExtension(agentID, key, extra); err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return
		}
	} else if body.RuleKey != "" {
		writeError(w, http.StatusBadRequest, "invalid_extension", "rule_key needs extra_budget")
		return
	}
	reevaluateUsageRules()
	overrides, err = db.ListUsageAgentOverrides()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	out := overrides[agentID]
	if out == nil {
		out = &db.UsageAgentOverride{AgentID: agentID}
	}
	writeJSON(w, http.StatusOK, out)
}

// canonicalUsageRuleKey checks that "profile:<id-or-name>:<rule>" names an
// existing rule of an existing usage profile and returns the engine's key,
// so an extension can never be stored under a key nothing reads.
func canonicalUsageRuleKey(key string) (string, error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(key), "profile:")
	ref, ruleName, ok2 := strings.Cut(rest, ":")
	if !ok || !ok2 || ref == "" || ruleName == "" {
		return "", errors.New(`rule_key must look like "profile:<profile>:<rule>" (see the rule_key values in 'tclaude usage rules --json')`)
	}
	p, err := resolveUsageProfileRef(ref)
	if err != nil {
		return "", err
	}
	if p == nil {
		return "", fmt.Errorf("no usage profile %q", ref)
	}
	for _, r := range p.Rules {
		if strings.EqualFold(r.Name, ruleName) {
			return usageProfileRuleKey(p.ID, r.Name), nil
		}
	}
	return "", fmt.Errorf("usage profile %q has no rule %q", p.Name, ruleName)
}

// resolveSpawnUsageProfiles validates a spawn's explicit usage profiles
// before anything launches. Choosing them is human-only.
func resolveSpawnUsageProfiles(w http.ResponseWriter, r *http.Request, refs []string) ([]int64, bool) {
	if len(refs) == 0 {
		return nil, true
	}
	if classify(peerFromContext(r.Context())) != classHuman {
		writeError(w, http.StatusForbidden, "usage_profile_restricted", "only the human operator may choose a spawned agent's usage profiles")
		return nil, false
	}
	ids := make([]int64, 0, len(refs))
	for _, ref := range refs {
		p, err := resolveUsageProfileRef(ref)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "io", err.Error())
			return nil, false
		}
		if p == nil {
			writeError(w, http.StatusBadRequest, "usage_profile_not_found", fmt.Sprintf("no usage profile %q", ref))
			return nil, false
		}
		ids = append(ids, p.ID)
	}
	return ids, true
}

// applySpawnUsageProfiles records a freshly spawned agent's own usage
// profiles: the explicit choice, else those linked to the first of the given
// spawn profiles (the named one, the group default, the global default) that
// has any. The
// copy is an agent-layer assignment, so it takes precedence over the group
// and global defaults and later edits to the link do not move running agents.
//
// A spawn still PENDING when the handler returns (its conversation id came
// too late) has no agent to record against yet; it falls back to its group
// and global layers.
func applySpawnUsageProfiles(agentID, convID string, explicit []int64, spawnProfiles ...*db.SpawnProfile) {
	if agentID == "" && convID != "" {
		var err error
		if agentID, err = db.AgentIDForConv(convID); err != nil {
			slog.Warn("usage rules: resolve spawned agent", "conv", convID, "error", err)
			return
		}
	}
	if agentID == "" {
		if len(explicit) > 0 {
			slog.Warn("usage rules: spawn is still pending; its explicit usage profiles were not applied")
		}
		return
	}
	ids, source := explicit, "spawn"
	if len(ids) == 0 {
		assignments, err := db.ListUsageProfileAssignments()
		if err != nil {
			slog.Warn("usage rules: read spawn profile links", "error", err)
			return
		}
		for _, sp := range spawnProfiles {
			if sp == nil {
				continue
			}
			scopeID := strconv.FormatInt(sp.ID, 10)
			for _, a := range assignments {
				if a.Scope == db.UsageScopeSpawnProfile && a.ScopeID == scopeID {
					ids = append(ids, a.ProfileID)
				}
			}
			if len(ids) > 0 {
				source = "spawn profile " + sp.Name
				break
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	if err := db.SetUsageProfileAssignments(db.UsageScopeAgent, agentID, ids, source); err != nil {
		slog.Warn("usage rules: assign spawned agent's usage profiles", "agent", agentID, "error", err)
	}
}
