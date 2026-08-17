package agentd

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// auto_permit_handlers.go serves the per-agent auto-permit opt-in endpoints:
//
//	GET/POST /v1/whoami/auto-permit        → read/change the CALLER's own opt-ins (self.auto-permit)
//	GET/POST /v1/agent/{conv}/auto-permit  → read/change ANOTHER agent's (agent.auto-permit / owner)
//
// The write is one condition at a time ({"condition": "...", "enabled": bool}),
// not a replace-set: each call is a discrete act of consent or revocation, and
// the audit row for it should say which condition moved rather than showing a
// before/after list.
//
// Both gates are deliberately unlike the other self.* verbs. self.auto-permit is
// NOT default-granted and NOT owner-implied (requirePermission, never the *Ex
// owner-bypass form) because what it consents to is a prompt the harness
// reserves for a human keystroke. The cross-agent path keeps the ordinary
// manager pattern, consistent with every other `tclaude agent … --target` verb.
//
// This is a pure DB write — no tmux send-keys — so there is no injection sink
// here. The keystrokes the sweep eventually sends come from the compile-time
// condition registry, never from anything stored through this endpoint; the
// registry check below means an unknown condition can't even be stored.

// handleWhoamiAutoPermit reads (GET) or changes (POST) the calling agent's own
// auto-permit opt-ins. Permission-gated on self.auto-permit.
func handleWhoamiAutoPermit(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, POST or PUT only")
		return
	}
	convID, ok := requirePermission(w, r, PermSelfAutoPermit)
	if !ok {
		return
	}
	if convID == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"this endpoint operates on the calling agent's own auto-permit opt-ins; use /v1/agent/{conv}/auto-permit to act on another agent")
		return
	}
	if r.Method == http.MethodGet {
		writeAutoPermitResponse(w, convID, convID)
		return
	}
	runAutoPermitChange(w, r, convID, convID)
}

// handleAgentAutoPermit reads (GET) or changes (POST) ANOTHER agent's
// auto-permit opt-ins. Routed via handleAgentByConv. Auth: agent.auto-permit
// slug OR caller owns a group containing the target.
func handleAgentAutoPermit(w http.ResponseWriter, r *http.Request, targetConv string) {
	switch r.Method {
	case http.MethodGet, http.MethodPost, http.MethodPut:
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "GET, POST or PUT only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentAutoPermit, targetConv)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		writeAutoPermitResponse(w, targetConv, caller)
		return
	}
	runAutoPermitChange(w, r, targetConv, caller)
}

// runAutoPermitChange decodes the request, validates the condition against the
// compile-time registry, and stores or removes the opt-in.
func runAutoPermitChange(w http.ResponseWriter, r *http.Request, target, caller string) {
	var body struct {
		Condition string `json:"condition"`
		Enabled   bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	name, err := db.NormalizeAutoPermitCondition(body.Condition)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// An unknown condition is refused rather than stored. A stored name no
	// build recognizes is inert, but silently accepting one would let an
	// operator believe they had consented to something. Disabling is exempt:
	// a stale opt-in written by an older build must stay revocable by name.
	if body.Enabled && lookupAutoPermitCondition(name) == nil {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"unknown auto-permit condition "+name+"; known conditions: "+
				strings.Join(autoPermitConditionNames(), ", "))
		return
	}
	agentID, err := db.AgentIDForConv(target)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if agentID == "" {
		writeError(w, http.StatusNotFound, "not_found", "no agent enrolled for conv "+short8(target))
		return
	}
	if body.Enabled {
		if err := db.SetAgentAutoPermit(agentID, name, autoPermitGranterLabel(caller), time.Now()); err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
	} else if _, err := db.ClearAgentAutoPermit(agentID, name); err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeAutoPermitResponse(w, target, caller)
}

// autoPermitGranterLabel snapshots who consented, for the stored row. "human"
// for an operator call (no caller conv), else the caller's display title —
// denormalized at write time so the row stays readable after a rename.
func autoPermitGranterLabel(caller string) string {
	if strings.TrimSpace(caller) == "" {
		return db.AuditActorHuman
	}
	if title := agent.FreshTitle(caller); title != agent.UnknownTitle {
		return title
	}
	return short8(caller)
}

// writeAutoPermitResponse returns the target's current opt-ins alongside the
// full condition registry, so one round trip tells a caller both what is on and
// what could be turned on.
func writeAutoPermitResponse(w http.ResponseWriter, convID, caller string) {
	agentID, err := db.AgentIDForConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if agentID == "" {
		writeError(w, http.StatusNotFound, "not_found", "no agent enrolled for conv "+short8(convID))
		return
	}
	optIns, err := db.ListAgentAutoPermits(agentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	enabled := map[string]db.AutoPermitOptIn{}
	for _, o := range optIns {
		enabled[o.Condition] = o
	}
	conditions := make([]map[string]any, 0, len(autoPermitConditions))
	for _, c := range autoPermitConditions {
		entry := map[string]any{
			"name":    c.Name,
			"summary": c.Summary,
			"harness": c.Harness,
			"enabled": false,
		}
		if o, on := enabled[c.Name]; on {
			entry["enabled"] = true
			entry["granted_by"] = o.GrantedBy
			entry["granted_at"] = o.CreatedAt.UTC().Format(time.RFC3339)
		}
		conditions = append(conditions, entry)
	}
	// Opt-ins whose condition this build no longer registers are reported
	// separately rather than dropped: they are inert, but an operator looking
	// at the list should see that a stale consent is still on the record (and
	// can be turned off).
	unknown := []string{}
	for _, o := range optIns {
		if lookupAutoPermitCondition(o.Condition) == nil {
			unknown = append(unknown, o.Condition)
		}
	}
	resp := map[string]any{
		"conv_id":    convID,
		"conditions": conditions,
	}
	if len(unknown) > 0 {
		resp["unknown_conditions"] = unknown
	}
	if caller != "" && caller != convID {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAutoPermitLog serves GET /v1/auto-permit/log — the answered-prompt
// trail. It reads back the audit rows the sweep writes, so "what was approved
// on my behalf" is answerable from the CLI as well as from the dashboard's
// Audit tab.
//
// Read-only and self-describing, gated the same way the write is: seeing which
// prompts were auto-answered is only interesting to whoever may configure them.
func handleAutoPermitLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method", "GET only")
		return
	}
	if _, ok := requirePermission(w, r, PermSelfAutoPermit); !ok {
		return
	}
	limit := 20
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, 200)
		}
	}
	filter := db.AuditLogFilter{Verb: auditVerbAutoPermitAnswer, Limit: limit}
	if conv := strings.TrimSpace(r.URL.Query().Get("conv")); conv != "" {
		// Search is a substring across the identity columns, target_conv
		// included, which is the narrowest filter the audit reader offers.
		filter.Search = conv
	}
	entries, err := db.ListAuditLog(filter)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{
			"at":          e.At.UTC().Format(time.RFC3339),
			"conv_id":     e.TargetConv,
			"agent_id":    e.TargetAgent,
			"agent_label": e.TargetLabel,
			"detail":      e.Detail,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"answers": out})
}
