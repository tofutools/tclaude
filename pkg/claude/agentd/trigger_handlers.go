package agentd

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	triggerlogic "github.com/tofutools/tclaude/pkg/claude/triggers"
)

// The agent-facing trigger surface is deliberately read-only in slice 1.
// Dashboard mutations are operator-authenticated; agents can inspect only the
// global/group rules covered by triggers.read or groups.triggers.read.
func handleTriggers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	caller, human, ok := authedCaller(w, r)
	if !ok {
		return
	}
	if !human {
		state, err := db.AgentState(caller)
		if err != nil || state == db.AgentStateRetired {
			writeError(w, http.StatusForbidden, "auth", "caller is not an active agent")
			return
		}
	}
	rules, err := db.ListTriggerRules()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	views := make([]dashboardTriggerView, 0, len(rules))
	for _, rule := range rules {
		if human || triggerRuleReadable(r, caller, rule) {
			view := triggerView(rule)
			view.Firings, _ = db.ListTriggerFirings(rule.ID, 1)
			views = append(views, view)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"triggers": views})
}

func handleTriggerRead(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/triggers/"), "/")
	if rest == "explain" {
		handleTriggerExplain(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET")
		return
	}
	id, err := strconv.ParseInt(rest, 10, 64)
	if err != nil || id <= 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "id must be positive")
		return
	}
	rule, err := db.GetTriggerRule(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if rule == nil {
		writeError(w, http.StatusNotFound, "not_found", "trigger not found")
		return
	}
	if rule.ScopeKind == db.TriggerScopeGlobal {
		if _, ok := requirePermission(w, r, PermTriggersRead); !ok {
			return
		}
	} else {
		g, err := db.GetAgentGroupByID(rule.GroupID)
		if err != nil || g == nil {
			writeError(w, http.StatusForbidden, "auth", "trigger group is unavailable")
			return
		}
		if _, ok := requireGroupPermission(w, r, PermGroupsTriggersRead, g); !ok {
			return
		}
	}
	view := triggerView(rule)
	view.Firings, _ = db.ListTriggerFirings(rule.ID, 20)
	writeJSON(w, http.StatusOK, view)
}

type triggerExplainRequest struct {
	Source        string `json:"source"`
	PRURL         string `json:"pr_url"`
	PRNumber      int    `json:"pr_number"`
	PRBranch      string `json:"pr_branch"`
	PRAuthorAgent string `json:"author_agent"`
	Group         string `json:"group"`
	Draft         bool   `json:"draft"`
}

type triggerExplainResult struct {
	RuleID   int64  `json:"rule_id"`
	RuleName string `json:"rule_name"`
	Fire     bool   `json:"fire"`
	Outcome  string `json:"outcome"`
	Detail   string `json:"detail"`
}

func handleTriggerExplain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST")
		return
	}
	caller, human, ok := authedCaller(w, r)
	if !ok {
		return
	}
	var body triggerExplainRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	now := time.Now().UTC()
	source := strings.TrimSpace(body.Source)
	if source == "" {
		source = db.TriggerSourcePROpened
	}
	event := db.TriggerPREvent{Source: source, PRURL: strings.TrimSpace(body.PRURL), PRNumber: body.PRNumber, PRBranch: body.PRBranch, PRAuthorAgent: strings.TrimSpace(body.PRAuthorAgent), Draft: body.Draft, OccurredAt: now, UpdatedAt: now}
	if body.Group != "" {
		g, err := resolveTriggerGroup(body.Group)
		if err != nil || g == nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", "group not found")
			return
		}
		event.GroupIDs = []int64{g.ID}
	}
	rules, err := db.ListTriggerRules()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	results := make([]triggerExplainResult, 0, len(rules))
	for _, rule := range rules {
		if !human && !triggerRuleReadable(r, caller, rule) {
			continue
		}
		last, _ := db.LatestCompletedTriggerFiring(rule.ID)
		var lastAt time.Time
		if last != nil {
			lastAt = last.FinishedAt
		}
		loop, _ := db.RuleSpawnedAgent(rule.ID, event.PRAuthorAgent)
		decision := triggerlogic.Evaluate(rule, event, now, lastAt, loop)
		results = append(results, triggerExplainResult{RuleID: rule.ID, RuleName: rule.Name, Fire: decision.Fire, Outcome: decision.Outcome, Detail: decision.Detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func triggerRuleReadable(r *http.Request, caller string, rule *db.TriggerRule) bool {
	if rule.ScopeKind == db.TriggerScopeGlobal {
		allowed, _, _ := permissionAllowsAction(r, caller, PermTriggersRead, ActionContext{})
		return allowed
	}
	g, err := db.GetAgentGroupByID(rule.GroupID)
	if err != nil || g == nil || g.IsArchived() {
		return false
	}
	ctx := ActionContext{Group: g.Name, structuralGroup: g.Name}
	if allowed, _, _ := permissionAllowsAction(r, caller, PermGroupsTriggersRead, ctx); allowed {
		return true
	}
	// An explicit deny suppresses the otherwise owner-implied group grant.
	if resolvePermissionVerdictForRequest(r, caller, PermGroupsTriggersRead).Resolution == permDeny {
		return false
	}
	allowed, _ := structuralPermissionMatch(caller, PermGroupsTriggersRead, ctx)
	return allowed
}

func resolveTriggerGroup(raw string) (*db.AgentGroup, error) {
	if id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64); err == nil {
		return db.GetAgentGroupByID(id)
	}
	return db.GetAgentGroupByName(strings.TrimSpace(raw))
}
