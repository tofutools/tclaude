package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Trigger dashboard REST contract (slice 1):
//
//	GET/POST       /api/triggers
//	GET/PATCH/DELETE /api/triggers/{id}
//	POST           /api/triggers/{id}/enable|disable
//	GET            /api/triggers/{id}/firings?limit=N
func registerDashboardTriggerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/triggers", handleDashboardTriggers)
	mux.HandleFunc("/api/triggers/", handleDashboardTrigger)
}

type dashboardTriggerMutation struct {
	Name            string             `json:"name"`
	RowVersion      int64              `json:"row_version,omitempty"`
	Enabled         *bool              `json:"enabled,omitempty"`
	Scope           string             `json:"scope"`
	Group           string             `json:"group,omitempty"`
	Source          string             `json:"source"`
	AuthorIsAgent   *bool              `json:"author_is_agent,omitempty"`
	DraftFilter     string             `json:"draft_filter"`
	DebounceSeconds int64              `json:"debounce_seconds,omitempty"`
	CooldownSeconds int64              `json:"cooldown_seconds,omitempty"`
	Actions         []db.TriggerAction `json:"actions"`
}

type dashboardTriggerView struct {
	*db.TriggerRule
	Group   string             `json:"group,omitempty"`
	Firings []db.TriggerFiring `json:"firings,omitempty"`
}

func handleDashboardTriggers(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		rules, err := db.ListTriggerRules()
		if err != nil {
			writeError(w, 500, "io", err.Error())
			return
		}
		views := make([]dashboardTriggerView, 0, len(rules))
		for _, rule := range rules {
			views = append(views, triggerView(rule))
		}
		writeJSON(w, 200, map[string]any{"triggers": views})
	case http.MethodPost:
		rule, _, ok := decodeDashboardTrigger(w, r, nil)
		if !ok {
			return
		}
		triggerAuthorityMu.Lock()
		id, err := db.InsertTriggerRule(rule)
		triggerAuthorityMu.Unlock()
		if err != nil {
			writeTriggerError(w, err)
			return
		}
		writeDashboardTrigger(w, id)
	default:
		writeError(w, 405, "method", "GET or POST")
	}
}

func handleDashboardTrigger(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/triggers/"), "/")
	parts := strings.Split(rest, "/")
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || id <= 0 {
		writeError(w, 400, "invalid_arg", "id must be positive")
		return
	}
	rule, err := db.GetTriggerRule(id)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	if rule == nil {
		writeError(w, 404, "not_found", "trigger not found")
		return
	}
	if len(parts) == 2 && parts[1] == "firings" {
		if r.Method != http.MethodGet {
			writeError(w, 405, "method", "GET")
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		rows, err := db.ListTriggerFirings(id, limit)
		if err != nil {
			writeError(w, 500, "io", err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"firings": rows})
		return
	}
	if len(parts) == 2 && (parts[1] == "enable" || parts[1] == "disable") {
		if r.Method != http.MethodPost {
			writeError(w, 405, "method", "POST")
			return
		}
		rv, _ := strconv.ParseInt(r.URL.Query().Get("row_version"), 10, 64)
		if rv <= 0 {
			writeError(w, 400, "invalid_arg", "row_version is required")
			return
		}
		triggerAuthorityMu.Lock()
		err := db.SetTriggerRuleEnabled(id, rv, parts[1] == "enable")
		triggerAuthorityMu.Unlock()
		if err != nil {
			writeTriggerError(w, err)
			return
		}
		writeDashboardTrigger(w, id)
		return
	}
	if len(parts) != 1 {
		writeError(w, 404, "not_found", "unknown trigger route")
		return
	}
	switch r.Method {
	case http.MethodGet:
		firings, _ := db.ListTriggerFirings(id, 20)
		view := triggerView(rule)
		view.Firings = firings
		writeJSON(w, 200, view)
	case http.MethodPatch:
		replacement, rv, ok := decodeDashboardTrigger(w, r, rule)
		if !ok {
			return
		}
		triggerAuthorityMu.Lock()
		err := db.UpdateTriggerRule(id, rv, replacement)
		triggerAuthorityMu.Unlock()
		if err != nil {
			writeTriggerError(w, err)
			return
		}
		writeDashboardTrigger(w, id)
	case http.MethodDelete:
		rv, _ := strconv.ParseInt(r.URL.Query().Get("row_version"), 10, 64)
		if rv <= 0 {
			writeError(w, 400, "invalid_arg", "row_version is required")
			return
		}
		triggerAuthorityMu.Lock()
		err := db.DeleteTriggerRule(id, rv)
		triggerAuthorityMu.Unlock()
		if err != nil {
			writeTriggerError(w, err)
			return
		}
		w.WriteHeader(204)
	default:
		writeError(w, 405, "method", "GET, PATCH, or DELETE")
	}
}

func decodeDashboardTrigger(w http.ResponseWriter, r *http.Request, existing *db.TriggerRule) (*db.TriggerRule, int64, bool) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return nil, 0, false
	}
	var body dashboardTriggerMutation
	if err := json.Unmarshal(raw, &body); err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return nil, 0, false
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(raw, &present); err != nil {
		writeError(w, 400, "invalid_arg", err.Error())
		return nil, 0, false
	}
	rule := &db.TriggerRule{Enabled: true, OperatorAuthored: true, Source: db.TriggerSourcePROpened, DraftFilter: db.TriggerDraftInclude}
	if existing != nil {
		copy := *existing
		copy.Actions = append([]db.TriggerAction(nil), existing.Actions...)
		rule = &copy
	}
	if _, ok := present["name"]; ok {
		rule.Name = body.Name
	}
	if _, ok := present["enabled"]; ok && body.Enabled != nil {
		rule.Enabled = *body.Enabled
	}
	if _, ok := present["scope"]; ok {
		rule.ScopeKind = body.Scope
	}
	if _, ok := present["source"]; ok {
		rule.Source = body.Source
	}
	if value, ok := present["author_is_agent"]; ok {
		if string(value) == "null" {
			rule.AuthorIsAgent = nil
		} else {
			rule.AuthorIsAgent = body.AuthorIsAgent
		}
	}
	if _, ok := present["draft_filter"]; ok {
		rule.DraftFilter = body.DraftFilter
	}
	if _, ok := present["debounce_seconds"]; ok {
		rule.DebounceSeconds = body.DebounceSeconds
	}
	if _, ok := present["cooldown_seconds"]; ok {
		rule.CooldownSeconds = body.CooldownSeconds
	}
	if _, ok := present["actions"]; ok {
		rule.Actions = body.Actions
	}
	if rule.ScopeKind == db.TriggerScopeGroup {
		if _, groupPresent := present["group"]; groupPresent || rule.GroupID == 0 {
			g, err := resolveDashboardTriggerGroup(body.Group)
			if err != nil {
				writeError(w, 400, "invalid_group", err.Error())
				return nil, 0, false
			}
			rule.GroupID = g.ID
		}
	} else {
		rule.GroupID = 0
	}
	if err := rule.Validate(); err != nil {
		writeTriggerError(w, err)
		return nil, 0, false
	}
	return rule, body.RowVersion, true
}

func resolveDashboardTriggerGroup(raw string) (*db.AgentGroup, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("group is required for group scope")
	}
	if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
		g, err := db.GetAgentGroupByID(id)
		if err != nil {
			return nil, err
		}
		if g == nil {
			return nil, errors.New("group not found")
		}
		return g, nil
	}
	g, err := db.GetAgentGroupByName(raw)
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, errors.New("group not found")
	}
	return g, nil
}

func triggerView(rule *db.TriggerRule) dashboardTriggerView {
	view := dashboardTriggerView{TriggerRule: rule}
	if rule.GroupID > 0 {
		if g, _ := db.GetAgentGroupByID(rule.GroupID); g != nil {
			view.Group = g.Name
		}
	}
	return view
}
func writeDashboardTrigger(w http.ResponseWriter, id int64) {
	rule, err := db.GetTriggerRule(id)
	if err != nil {
		writeError(w, 500, "io", err.Error())
		return
	}
	writeJSON(w, 200, triggerView(rule))
}
func writeTriggerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, db.ErrTriggerInvalid):
		writeError(w, 400, "invalid_trigger", err.Error())
	case errors.Is(err, db.ErrTriggerNameTaken):
		writeError(w, 409, "name_taken", err.Error())
	case errors.Is(err, db.ErrTriggerVersionConflict):
		writeError(w, 409, "version_conflict", err.Error())
	default:
		writeError(w, 500, "io", err.Error())
	}
}
