package agentd

import (
	"encoding/json"
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
//	DELETE /api/agents/{conv}    → wipe the conversation + orphan-clean
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
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	if len(parts) > 1 && parts[1] != "" {
		http.Error(w, "unknown subpath /api/agents/{conv}/"+parts[1], http.StatusNotFound)
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
//	DELETE /api/groups/{name}/members/{conv}    → remove from group
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
	case "members":
		// /api/groups/{name}/members/{conv} — DELETE only.
		if len(parts) < 3 || parts[2] == "" {
			http.Error(w, "expected /api/groups/{name}/members/{conv}", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
			return
		}
		dashboardRemoveMember(w, g, parts[2])
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
