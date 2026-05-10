package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Mutating endpoints for the dashboard. These live on the loopback
// HTTP server (alongside /api/snapshot) and are gated by the same
// dashboard cookie + Origin/Referer pinning that protects reads.
//
// Why not just call /v1/groups/...? Those endpoints are mounted on
// the daemon's Unix socket and authenticate via SO_PEERCRED — the
// browser can't speak that. So the dashboard server gets parallel
// endpoints that internally call the same db.* helpers.
//
// Same threat model as the rest of the loopback HTTP surface: the
// per-process random session token guards against drive-by browser
// tabs, but a same-user process with /proc access can scrape the
// cookie. Documented and accepted in dashboard.go.
//
// All mutations record `<human-dashboard>` as the granter on
// audit-trail columns (agent_group_owners.granted_by) — the
// dashboard is human-only by definition; agents talk to /v1/.

const dashboardGranter = "<human-dashboard>"

// registerDashboardEditRoutes wires the mutation endpoints onto the
// loopback mux. Called from registerDashboardRoutes.
func registerDashboardEditRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/groups/", handleDashboardGroupsAPI)
	mux.HandleFunc("/api/agents/", handleDashboardAgentsAPI)
	mux.HandleFunc("/api/jump/", handleDashboardJumpAPI)
	registerDashboardCronRoutes(mux)
}

// handleDashboardJumpAPI dispatches:
//
//	POST /api/jump/{conv}    → focus the agent's tmux-attached terminal
//
// Resolves the conv to its alive tmux session row daemon-side and
// calls session.TryFocusAttachedSession (per-platform: AppleScript /
// wmctrl / PowerShell). Best-effort — the helper logs but doesn't
// return errors; we 204 on dispatch and 404 only when the conv has
// no live session at all.
func handleDashboardJumpAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/jump/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/jump/{conv}", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if len(parts) > 1 && parts[1] != "" {
		http.Error(w, "unknown subpath /api/jump/{conv}/"+parts[1], http.StatusNotFound)
		return
	}
	convSelector := parts[0]
	if u, err := url.PathUnescape(convSelector); err == nil {
		convSelector = u
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	sess := pickAliveSession(res.ConvID)
	if sess == nil {
		http.Error(w, "no live tmux session for "+short8(res.ConvID), http.StatusNotFound)
		return
	}
	// Pass the session ID explicitly so the WSL focus path can match
	// "tclaude:<id>" titles. Plain TryFocusAttachedSession reads the
	// id from $TCLAUDE_SESSION_ID, which the daemon doesn't set.
	session.TryFocusAttachedSessionWithID(sess.TmuxSession, sess.ID)
	w.WriteHeader(http.StatusNoContent)
}

// handleDashboardAgentsAPI dispatches:
//
//	DELETE /api/agents/{conv}              → wipe the conversation + orphan-clean
//	POST   /api/agents/{conv}/stop         → soft exit / force kill
//	POST   /api/agents/{conv}/resume       → wake (resume tmux pane)
//	POST   /api/agents/{conv}/clone        → fork a sibling (cookie-auth twin)
//
// Behaviour:
//   - If the conv still exists in conv_index, runs conv.DeleteConvByID
//     (unlinks .jsonl, drops conv_index row, strips sessions-index.json,
//     writes sync tombstone).
//   - Always: drops every agent_group_members / agent_group_owners /
//     agent_permissions row referencing this conv-id, so the dashboard
//     listing actually drops the row instead of leaving a "(unknown)"
//     ghost. Without this, a re-delete attempt would 404 in the
//     resolver (no conv-index row to match) and the entry would be
//     un-removable.
//
// Accepts either the resolver-friendly selector (if the agent still
// exists) or a raw UUID-shaped conv-id (for cleaning up orphans whose
// conv-index row is already gone).
func handleDashboardAgentsAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/agents/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/agents/{conv}", http.StatusNotFound)
		return
	}
	convSelector := parts[0]
	if u, err := url.PathUnescape(convSelector); err == nil {
		convSelector = u
	}
	// Sub-verbs: stop / resume — thin pass-throughs to the per-conv
	// helpers shared with the bulk groups.{stop,resume} paths.
	if len(parts) > 1 && parts[1] != "" {
		switch parts[1] {
		case "stop":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			dashboardStopAgent(w, r, convSelector)
			return
		case "resume":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			dashboardResumeAgent(w, convSelector)
			return
		case "clone":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			dashboardCloneAgent(w, r, convSelector)
			return
		default:
			http.Error(w, "unknown subpath /api/agents/{conv}/"+parts[1], http.StatusNotFound)
			return
		}
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}

	// Try to resolve normally; if it works we get a canonical
	// conv-id. If the resolver fails (no conv-index row, no
	// membership row pointing to this conv), accept the raw input as
	// long as it's UUID-shaped — needed for cleaning up orphan
	// owner/permission rows whose conv-index is already gone. The
	// raw path is gated on shape so we don't blindly run
	// DELETE WHERE conv_id = '<arbitrary>'.
	var convID string
	if res, _, err := agent.ResolveSelector(convSelector); err == nil {
		convID = res.ConvID
	} else if looksLikeConvID(convSelector) {
		convID = convSelector
	} else {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}

	// DeleteConvByID is a no-op on missing conv-index rows, so we
	// run it unconditionally — caller doesn't need to know whether
	// the underlying file is still there.
	if err := conv.DeleteConvByID(convID); err != nil {
		http.Error(w, "delete conv: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Always clean orphan rows. Errors are surfaced as 500 — these
	// are pure DB DELETE-by-conv-id operations and shouldn't fail
	// in practice.
	if _, err := db.RemoveAllAgentGroupMembershipsForConv(convID); err != nil {
		http.Error(w, "drop memberships: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := db.RemoveAllAgentGroupOwnershipsForConv(convID); err != nil {
		http.Error(w, "drop ownerships: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, err := db.RevokeAllAgentPermissionsForConv(convID); err != nil {
		http.Error(w, "drop permissions: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// looksLikeConvID is a cheap sanity check for raw conv-id input on
// the orphan-cleanup path. Only allows the canonical UUID shape
// (8-4-4-4-12 hex with dashes). Avoids blindly running DELETE WHERE
// conv_id = ? against unsanitised user input — defence-in-depth on
// top of the dashboard's auth/origin check.
func looksLikeConvID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

// handleDashboardGroupsAPI dispatches:
//
//	DELETE /api/groups/{name}                   → delete group
//	POST   /api/groups/{name}/rename            → rename (body: {new_name})
//	POST   /api/groups/{name}/members           → add member (body: {conv, alias?, role?, descr?})
//	DELETE /api/groups/{name}/members/{conv}    → remove from group
//	PATCH  /api/groups/{name}/members/{conv}    → update alias/role/descr
//	POST   /api/groups/{name}/owners            → grant owner (body: {conv})
//	DELETE /api/groups/{name}/owners/{conv}     → revoke owner
//
// Anything else returns 404.
func handleDashboardGroupsAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/groups/")
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/groups/{name}[/{members|owners}[/{conv}]]", http.StatusNotFound)
		return
	}
	groupName := parts[0]
	if u, err := url.PathUnescape(groupName); err == nil {
		groupName = u
	}
	g, err := db.GetAgentGroupByName(groupName)
	if err != nil {
		http.Error(w, "group lookup: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if g == nil {
		http.Error(w, "no such group "+groupName, http.StatusNotFound)
		return
	}
	if len(parts) == 1 {
		if r.Method != http.MethodDelete {
			http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
			return
		}
		dashboardDeleteGroup(w, groupName)
		return
	}
	switch parts[1] {
	case "rename":
		if len(parts) >= 3 && parts[2] != "" {
			http.Error(w, "POST /api/groups/{name}/rename takes new_name in the body, not the URL", http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		dashboardRenameGroup(w, r, g)
	case "members":
		// /api/groups/{name}/members          — POST adds a new member.
		// /api/groups/{name}/members/{conv}   — DELETE removes; PATCH
		//                                       updates alias/role/descr.
		if len(parts) < 3 || parts[2] == "" {
			if r.Method != http.MethodPost {
				http.Error(w, "POST /api/groups/{name}/members or DELETE/PATCH /api/groups/{name}/members/{conv}", http.StatusMethodNotAllowed)
				return
			}
			dashboardAddMember(w, r, g)
			return
		}
		switch r.Method {
		case http.MethodDelete:
			dashboardRemoveMember(w, g, parts[2])
		case http.MethodPatch:
			dashboardUpdateMember(w, r, g, parts[2])
		default:
			http.Error(w, "DELETE or PATCH", http.StatusMethodNotAllowed)
		}
	case "owners":
		switch r.Method {
		case http.MethodPost:
			// Body: {"conv": "<selector>"}
			if len(parts) >= 3 && parts[2] != "" {
				http.Error(w, "POST takes the conv in the request body, not the URL", http.StatusBadRequest)
				return
			}
			dashboardAddOwner(w, r, g)
		case http.MethodDelete:
			if len(parts) < 3 || parts[2] == "" {
				http.Error(w, "expected /api/groups/{name}/owners/{conv}", http.StatusNotFound)
				return
			}
			dashboardRemoveOwner(w, g, parts[2])
		default:
			http.Error(w, "POST or DELETE", http.StatusMethodNotAllowed)
		}
	default:
		http.Error(w, "unknown endpoint /api/groups/{name}/"+parts[1], http.StatusNotFound)
	}
}

func dashboardDeleteGroup(w http.ResponseWriter, name string) {
	if err := db.DeleteAgentGroup(name); err != nil {
		http.Error(w, "delete group: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// dashboardRenameGroup is the dashboard-cookie-auth twin of
// /v1/groups/{name}/rename — same db.RenameAgentGroup call, same
// validateGroupName check, same 400/404/409 surface. The dashboard
// caller is the human (cookie auth ≈ requirePermission's bypass), so
// no slug check is needed here.
func dashboardRenameGroup(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	var body struct {
		NewName string `json:"new_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "rename: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateGroupName(body.NewName); err != nil {
		http.Error(w, "rename: "+err.Error(), http.StatusBadRequest)
		return
	}
	renamed, err := db.RenameAgentGroup(g.Name, body.NewName, "")
	if errors.Is(err, db.ErrGroupNameTaken) {
		http.Error(w, "rename: a group named \""+body.NewName+"\" already exists", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "rename: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if renamed == nil {
		http.Error(w, "rename: group \""+g.Name+"\" no longer exists", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"group":    renamed.Name,
		"old_name": g.Name,
	})
}

// dashboardUpdateMember is the dashboard-cookie-auth twin of
// /v1/groups/{name}/members/{conv} PATCH. Only fields explicitly
// present (non-nil) in the request body are touched, matching the
// /v1 contract — pass `null` (or omit) to leave a field unchanged.
func dashboardUpdateMember(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, convSelector string) {
	if u, err := url.PathUnescape(convSelector); err == nil {
		convSelector = u
	}
	var body struct {
		Alias *string `json:"alias,omitempty"`
		Role  *string `json:"role,omitempty"`
		Descr *string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Alias == nil && body.Role == nil && body.Descr == nil {
		http.Error(w, "at least one of alias/role/descr is required", http.StatusBadRequest)
		return
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve target: "+err.Error(), http.StatusNotFound)
		return
	}
	n, err := db.UpdateAgentGroupMember(g.ID, res.ConvID, body.Alias, body.Role, body.Descr)
	if err != nil {
		http.Error(w, "update member: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "no such member in group", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"conv_id": res.ConvID})
}

// dashboardStopAgent is the cookie-auth twin of POST
// /v1/agent/{conv}/stop. Body is optional `{"force": true}`. Calls
// the same `stopOneConv` helper the bulk groups.stop path uses, so
// "soft exit" / "force kill" semantics match exactly.
func dashboardStopAgent(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	var body struct {
		Force bool `json:"force"`
	}
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&body) // optional; default false
	}
	out := stopOneConv(res.ConvID, body.Force)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// dashboardCloneAgent is the cookie-auth twin of POST
// /v1/agent/{conv}/clone. Forks a sibling that inherits the source's
// identity (groups / perms / ownership). Body matches the v1 endpoint:
// optional `follow_up` (typed-into-new-pane handoff) + `no_copy_conv`
// (skip the .jsonl copy, fresh CC instead). Cookie auth ≈ human, so
// no slug check; the audit trail records `<human-dashboard>` as the
// caller via the existing `runCloneOrchestration` granter compose
// path.
//
// The dashboard's Ctrl-drag handler uses this to clone a member into
// a target group: it fires this endpoint, parses the response for
// `new_conv`, then fires `POST /api/groups/{target}/members` to add
// the clone to the drop target group (idempotent). Keeping the
// target-group join client-side keeps the daemon endpoint identical
// to the v1 surface.
func dashboardCloneAgent(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	followUp, noCopyConv, ok := decodeCloneBody(w, r)
	if !ok {
		return
	}
	runCloneOrchestration(w, res.ConvID, dashboardGranter, followUp, noCopyConv)
}

// dashboardResumeAgent is the cookie-auth twin of POST
// /v1/agent/{conv}/resume. Idempotent — already-online conv-ids
// surface as `skipped:already_online`. No body.
func dashboardResumeAgent(w http.ResponseWriter, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	out := resumeOneConv(res.ConvID)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// dashboardAddMember is the cookie-auth twin of POST
// /v1/groups/{name}/members. Body: `{conv, alias?, role?, descr?}`.
// `conv` accepts an alias / prefix / full conv-id selector and is
// resolved through agent.ResolveSelector — same rules as the CLI.
func dashboardAddMember(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	var body struct {
		Conv  string `json:"conv"`
		Alias string `json:"alias,omitempty"`
		Role  string `json:"role,omitempty"`
		Descr string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Conv = strings.TrimSpace(body.Conv)
	if body.Conv == "" {
		http.Error(w, "missing conv", http.StatusBadRequest)
		return
	}
	res, _, err := agent.ResolveSelector(body.Conv)
	if err != nil {
		http.Error(w, "resolve target: "+err.Error(), http.StatusNotFound)
		return
	}
	if err := db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: g.ID,
		ConvID:  res.ConvID,
		Alias:   body.Alias,
		Role:    body.Role,
		Descr:   body.Descr,
	}); err != nil {
		http.Error(w, "add member: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID})
}

func dashboardRemoveMember(w http.ResponseWriter, g *db.AgentGroup, convSelector string) {
	if u, err := url.PathUnescape(convSelector); err == nil {
		convSelector = u
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve target: "+err.Error(), http.StatusNotFound)
		return
	}
	if err := db.RemoveAgentGroupMember(g.ID, res.ConvID); err != nil {
		http.Error(w, "remove member: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func dashboardAddOwner(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	var body struct {
		Conv string `json:"conv"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Conv = strings.TrimSpace(body.Conv)
	if body.Conv == "" {
		http.Error(w, "missing conv", http.StatusBadRequest)
		return
	}
	res, _, err := agent.ResolveSelector(body.Conv)
	if err != nil {
		http.Error(w, "resolve target: "+err.Error(), http.StatusNotFound)
		return
	}
	if err := db.AddAgentGroupOwner(g.ID, res.ConvID, dashboardGranter); err != nil {
		http.Error(w, "add owner: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID})
}

func dashboardRemoveOwner(w http.ResponseWriter, g *db.AgentGroup, convSelector string) {
	if u, err := url.PathUnescape(convSelector); err == nil {
		convSelector = u
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve target: "+err.Error(), http.StatusNotFound)
		return
	}
	n, err := db.RemoveAgentGroupOwner(g.ID, res.ConvID)
	if err != nil {
		http.Error(w, "remove owner: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "not an owner of this group", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
