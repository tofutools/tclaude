package agentd

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
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

// dashboardSudoGranter is the audit label for proactive sudo grants
// minted via POST /api/sudo. Distinct from dashboardGranter so a
// forensic query can tell "human typed permissions grant in the
// dashboard" apart from "human proactively elevated alice for 5m"
// — different operations with different blast radius.
const dashboardSudoGranter = "<human-dashboard>:proactive"

// registerDashboardEditRoutes wires the mutation endpoints onto the
// loopback mux. Called from registerDashboardRoutes.
func registerDashboardEditRoutes(mux *http.ServeMux) {
	registerDashboardGroupRoutes(mux)
	mux.HandleFunc("/api/agents/", handleDashboardAgentsAPI)
	mux.HandleFunc("/api/worktrees", handleDashboardWorktreesAPI)
	mux.HandleFunc("/api/jump/", handleDashboardJumpAPI)
	mux.HandleFunc("/api/term/", handleDashboardTermAPI)
	mux.HandleFunc("/api/sudo", handleDashboardSudoAPI)
	mux.HandleFunc("/api/sudo/", handleDashboardSudoAPI)
	mux.HandleFunc("/api/permissions", handleDashboardPermissionsAPI)
	mux.HandleFunc("/api/cleanup/", handleDashboardCleanup)
	mux.HandleFunc("/api/emergency-shutdown", handleEmergencyShutdown)
	registerDashboardCronRoutes(mux)
}

// handleDashboardGroupsCreate is the cookie-auth twin of POST /v1/groups.
// Delegates to handleGroups after stamping a synthetic human peer — the
// cookie+Origin pin is the human-consent layer; requirePermission then
// short-circuits the slug check via the !HasClaudeAncestor branch.
// Registered as `POST /api/groups`, so the mux rejects other methods.
func handleDashboardGroupsCreate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	handleGroups(w, asDashboardHumanPeer(r))
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
//	POST   /api/agents/{conv}/reincarnate  → spawn successor + soft-exit original
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
		case "reincarnate":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			dashboardReincarnateAgent(w, r, convSelector)
			return
		case "rename":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			dashboardRenameAgent(w, r, convSelector)
			return
		case "worktree":
			if r.Method != http.MethodGet {
				http.Error(w, "GET only", http.StatusMethodNotAllowed)
				return
			}
			dashboardAgentWorktree(w, convSelector)
			return
		case "promote", "retire", "reinstate":
			dashboardEnrollmentVerb(w, r, convSelector, parts[1])
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
	// long as it's UUID-shaped — needed for cleaning up orphans whose
	// conv-index row is already gone. The raw path is gated on shape
	// so we don't blindly run DELETE WHERE conv_id = '<arbitrary>'.
	var convID string
	if res, _, err := agent.ResolveSelector(convSelector); err == nil {
		convID = res.ConvID
	} else if looksLikeConvID(convSelector) {
		convID = convSelector
	} else {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}

	// Optional ?delete_worktree=1 (the delete-agent modal's checkbox)
	// also removes the git worktree this agent worked in. Resolve it
	// BEFORE the purge — DeleteConvByID wipes the session rows the
	// resolution reads from. The repo's main worktree and any worktree
	// a surviving agent still uses are left alone (worktree_cleanup.go).
	delWorktree := r.URL.Query().Get("delete_worktree") == "1" ||
		r.URL.Query().Get("delete_worktree") == "true"
	var wt agentWorktreeView
	if delWorktree {
		wt = inspectAgentWorktree(convID)
		wt.Shared = wt.Path != "" &&
			otherAgentWorktreeRoots(map[string]bool{convID: true})[wt.Path]
	}

	// Dashboard deletes always force-kill any alive tmux session for
	// this conv — the "delete forever" button is unambiguous human
	// intent. Without this, the conv resurrects in handlePeers via
	// the still-alive sessions row.
	stopOneConv(convID, true /* force */)

	// Single source of truth for the comprehensive cleanup: filesystem
	// + DB union purge across every conv-id-referencing table +
	// session-env + sync tombstone.
	if _, err := conv.DeleteConvByID(convID); err != nil {
		http.Error(w, "delete conv: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Without ?delete_worktree the original 204 contract holds (orphan
	// cleanup, drag-move, the bare delete button). With it, return 200
	// + JSON so the modal can surface what happened to the worktree.
	if !delWorktree {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id":  convID,
		"worktree": applyWorktreeCleanup(wt, true),
	})
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

// registerDashboardGroupRoutes wires the cookie-authed /api/groups
// endpoints onto the loopback mux as Go 1.22 method+pattern routes:
//
//	POST   /api/groups                          → create a group
//	DELETE /api/groups/{name}                   → delete group
//	PATCH  /api/groups/{name}                   → update settings (body: {default_cwd})
//	POST   /api/groups/{name}/rename            → rename (body: {new_name})
//	POST   /api/groups/{name}/spawn             → spawn a new tclaude session and auto-join this group
//	POST   /api/groups/{name}/members           → add member (body: {conv, role?, descr?})
//	DELETE /api/groups/{name}/members/{conv}    → remove from group
//	PATCH  /api/groups/{name}/members/{conv}    → update role/descr
//	POST   /api/groups/{name}/owners            → grant owner (body: {conv})
//	DELETE /api/groups/{name}/owners/{conv}     → revoke owner
//	POST   /api/groups/{name}/links             → add link (body: {to, mode?, bidir?})
//	PATCH  /api/groups/{name}/links/{id}        → update link mode
//	DELETE /api/groups/{name}/links/{id}        → remove link
//
// The {name} / {conv} / {id} wildcards are matched and percent-decoded
// by the mux itself (read via r.PathValue), which replaces the old
// hand-rolled TrimPrefix + SplitN dispatch. That manual parse split on
// r.URL.Path — already percent-decoded — so a group name containing a
// slash (e.g. sent as "team%2Fsub" by the browser) was re-split into
// bogus path segments and the route was lost. A {name} wildcard matches
// one segment of the *escaped* path, so the embedded slash survives.
func registerDashboardGroupRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/groups", handleDashboardGroupsCreate)

	mux.HandleFunc("DELETE /api/groups/{name}", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		dashboardDeleteGroup(w, g.Name)
	}))
	mux.HandleFunc("PATCH /api/groups/{name}", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupUpdate(w, asDashboardHumanPeer(r), g)
	}))
	mux.HandleFunc("POST /api/groups/{name}/rename", groupRoute(dashboardRenameGroup))
	mux.HandleFunc("POST /api/groups/{name}/spawn", groupRoute(dashboardSpawnInGroup))
	mux.HandleFunc("POST /api/groups/{name}/members", groupRoute(dashboardAddMember))
	mux.HandleFunc("DELETE /api/groups/{name}/members/{conv}", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		dashboardRemoveMember(w, g, r.PathValue("conv"))
	}))
	mux.HandleFunc("PATCH /api/groups/{name}/members/{conv}", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		dashboardUpdateMember(w, r, g, r.PathValue("conv"))
	}))
	mux.HandleFunc("POST /api/groups/{name}/owners", groupRoute(dashboardAddOwner))
	mux.HandleFunc("DELETE /api/groups/{name}/owners/{conv}", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		dashboardRemoveOwner(w, g, r.PathValue("conv"))
	}))
	mux.HandleFunc("POST /api/groups/{name}/links", groupRoute(dashboardAddLink))
	mux.HandleFunc("PATCH /api/groups/{name}/links/{id}", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		dashboardUpdateLink(w, r, g, r.PathValue("id"))
	}))
	mux.HandleFunc("DELETE /api/groups/{name}/links/{id}", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		dashboardRemoveLink(w, g, r.PathValue("id"))
	}))
}

// groupRoute adapts a group-scoped dashboard handler into an
// http.HandlerFunc: it runs the dashboard cookie/Origin auth, resolves
// the {name} path wildcard to a group row, replies 404 when no such
// group exists, and otherwise hands the resolved group to fn. Hoisting
// auth + lookup here keeps each route registration a single line.
func groupRoute(fn func(http.ResponseWriter, *http.Request, *db.AgentGroup)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkDashboardAuth(w, r) {
			return
		}
		name := r.PathValue("name")
		g, err := db.GetAgentGroupByName(name)
		if err != nil {
			http.Error(w, "group lookup: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if g == nil {
			http.Error(w, "no such group "+name, http.StatusNotFound)
			return
		}
		fn(w, r, g)
	}
}

// dashboardAddLink mirrors handleGroupLinksAdd on the daemon socket
// side, but trusts the cookie-auth caller (the dashboard is human-only)
// and writes through the DB helpers directly. Body: {to, mode?, bidir?}.
func dashboardAddLink(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	var body struct {
		To    string `json:"to"`
		Mode  string `json:"mode"`
		Bidir bool   `json:"bidir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.To = strings.TrimSpace(body.To)
	if body.To == "" {
		http.Error(w, "missing to (target group name)", http.StatusBadRequest)
		return
	}
	to, err := db.GetAgentGroupByName(body.To)
	if err != nil {
		http.Error(w, "lookup target: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if to == nil {
		http.Error(w, "no such target group "+body.To, http.StatusNotFound)
		return
	}
	mode := strings.TrimSpace(body.Mode)
	if mode == "" {
		mode = db.LinkModeMembersToMembers
	}
	if !db.ValidLinkMode(mode) {
		http.Error(w, "unknown link mode "+mode, http.StatusBadRequest)
		return
	}
	id, err := db.InsertAgentGroupLink(g.ID, to.ID, mode, dashboardGranter)
	if err != nil {
		if errors.Is(err, db.ErrLinkExists) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, "add link: "+err.Error(), http.StatusBadRequest)
		return
	}
	out := map[string]any{"id": id, "from": g.Name, "to": to.Name, "mode": mode}
	if body.Bidir {
		revID, err := db.InsertAgentGroupLink(to.ID, g.ID, mode, dashboardGranter)
		switch {
		case err == nil:
			out["reverse_id"] = revID
		case errors.Is(err, db.ErrLinkExists):
			out["reverse_id"] = "already-exists"
		default:
			out["reverse_error"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// dashboardUpdateLink: PATCH /api/groups/{name}/links/{id} body {mode}.
func dashboardUpdateLink(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "link id must be integer", http.StatusBadRequest)
		return
	}
	link, err := db.GetAgentGroupLinkByID(id)
	if err != nil {
		http.Error(w, "lookup link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if link == nil {
		http.Error(w, "no such link", http.StatusNotFound)
		return
	}
	if link.FromGroupID != g.ID && link.ToGroupID != g.ID {
		http.Error(w, "link does not touch this group", http.StatusNotFound)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	mode := strings.TrimSpace(body.Mode)
	if !db.ValidLinkMode(mode) {
		http.Error(w, "unknown link mode "+mode, http.StatusBadRequest)
		return
	}
	if mode == link.Mode {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "mode": mode, "changed": false})
		return
	}
	n, err := db.UpdateAgentGroupLinkMode(id, mode)
	if err != nil {
		if errors.Is(err, db.ErrLinkExists) {
			http.Error(w, "another link with the same from/to/mode already exists", http.StatusConflict)
			return
		}
		http.Error(w, "update link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "no such link", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "mode": mode, "changed": true})
}

// dashboardRemoveLink: DELETE /api/groups/{name}/links/{id}.
func dashboardRemoveLink(w http.ResponseWriter, g *db.AgentGroup, idStr string) {
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "link id must be integer", http.StatusBadRequest)
		return
	}
	link, err := db.GetAgentGroupLinkByID(id)
	if err != nil {
		http.Error(w, "lookup link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if link == nil {
		http.Error(w, "no such link", http.StatusNotFound)
		return
	}
	if link.FromGroupID != g.ID && link.ToGroupID != g.ID {
		http.Error(w, "link does not touch this group", http.StatusNotFound)
		return
	}
	n, err := db.DeleteAgentGroupLink(id)
	if err != nil {
		http.Error(w, "delete link: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if n == 0 {
		http.Error(w, "no such link", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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
		Role  *string `json:"role,omitempty"`
		Descr *string `json:"descr,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if body.Role == nil && body.Descr == nil {
		http.Error(w, "at least one of role/descr is required", http.StatusBadRequest)
		return
	}
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve target: "+err.Error(), http.StatusNotFound)
		return
	}
	n, err := db.UpdateAgentGroupMember(g.ID, res.ConvID, body.Role, body.Descr)
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
	followUp, noCopyConv, cwd, ok := decodeCloneBody(w, r)
	if !ok {
		return
	}
	runCloneOrchestration(w, res.ConvID, dashboardGranter, "", followUp, noCopyConv, cwd)
}

// dashboardReincarnateAgent is the cookie-auth twin of POST
// /v1/agent/{conv}/reincarnate. Spawns a fresh CC instance that inherits
// the target's identity (groups / perms / ownerships migrate onto the
// new conv-id), renames it `<prev>-r-<N>`, and soft-exits the original
// pane. Body: `{follow_up}` — REQUIRED (the new pane comes up with
// clean context and would otherwise sit idle). Cookie auth ≈ human, so
// requireCrossAgentPermission short-circuits via the !HasClaudeAncestor
// branch and the audit trail records the dashboard granter.
func dashboardReincarnateAgent(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	handleAgentReincarnate(w, asDashboardHumanPeer(r), res.ConvID)
}

// dashboardRenameAgent is the cookie-auth twin of POST
// /v1/agent/{conv}/rename. Body shape matches the daemon endpoint:
// `{title: "..."}` for an explicit rename, or `{auto: true}` to
// inject a system nudge that asks the agent to pick its own title
// via the agent-rename skill / CLI. Cookie auth ≈ human, so
// requireCrossAgentPermission short-circuits via the
// !HasClaudeAncestor branch.
func dashboardRenameAgent(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	handleAgentRename(w, asDashboardHumanPeer(r), res.ConvID)
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
// /v1/groups/{name}/members. Body: `{conv, role?, descr?}`.
// `conv` accepts a title / prefix / full conv-id selector and is
// resolved through agent.ResolveSelector — same rules as the CLI.
func dashboardAddMember(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	var body struct {
		Conv  string `json:"conv"`
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
		Role:    body.Role,
		Descr:   body.Descr,
	}); err != nil {
		http.Error(w, "add member: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"conv_id": res.ConvID})
}

// dashboardSpawnInGroup is the cookie-auth twin of POST
// /v1/groups/{name}/spawn. Forks a fresh `tclaude session new -d --global`
// detached, waits for its conv-id to materialise, then joins it to the
// group with the supplied role/descr (and renames it to the supplied
// name). Delegates to handleGroupSpawn with a synthetic human peer so
// the inner requirePermission passes.
func dashboardSpawnInGroup(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	handleGroupSpawn(w, asDashboardHumanPeer(r), g)
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

// handleDashboardSudoAPI is the cookie-auth twin of the daemon's
// /v1/sudo surface (peer-cred-auth on the Unix socket). Same DB
// writes, same human-only rules — the dashboard is human-only by
// definition, so these endpoints unconditionally treat the caller
// as human.
//
//	POST   /api/sudo                 → proactive grant (no popup —
//	                                   the cookie IS the human consent)
//	DELETE /api/sudo/{id}            → revoke one
//	DELETE /api/sudo?conv=<selector> → revoke all for one conv
//	DELETE /api/sudo?all=1           → revoke every active grant
//
// Read paths (list / per-conv view) are not surfaced separately —
// the snapshot already exposes per-agent active sudo state, so the
// dashboard renders off that single round-trip.
func handleDashboardSudoAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method == http.MethodPost {
		handleDashboardSudoGrant(w, r)
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "POST or DELETE only", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/sudo")
	rest = strings.TrimPrefix(rest, "/")
	if rest != "" {
		// Per-id revoke: /api/sudo/{id}.
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			http.Error(w, "id must be a positive integer", http.StatusBadRequest)
			return
		}
		n, err := db.RevokeSudoGrant(id)
		if err != nil {
			http.Error(w, "revoke: "+err.Error(), http.StatusInternalServerError)
			return
		}
		if n == 0 {
			http.Error(w, "no active grant with that id", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"revoked": n, "id": id})
		return
	}
	// Bulk revoke: /api/sudo?conv=… or /api/sudo?all=1.
	q := r.URL.Query()
	if q.Get("all") == "1" || q.Get("all") == "true" {
		n, err := db.RevokeAllActiveSudoGrants()
		if err != nil {
			http.Error(w, "revoke all: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"revoked": n, "scope": "all"})
		return
	}
	convSel := strings.TrimSpace(q.Get("conv"))
	if convSel == "" {
		http.Error(w, "DELETE /api/sudo requires ?conv=<selector> or ?all=1", http.StatusBadRequest)
		return
	}
	if u, err := url.QueryUnescape(convSel); err == nil {
		convSel = u
	}
	res, _, err := agent.ResolveSelector(convSel)
	if err != nil {
		http.Error(w, "resolve conv: "+err.Error(), http.StatusNotFound)
		return
	}
	n, err := db.RevokeSudoGrantsByConv(res.ConvID)
	if err != nil {
		http.Error(w, "revoke by conv: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": n, "conv_id": res.ConvID})
}

// handleDashboardSudoGrant is the cookie-auth front for proactive
// sudo grants. The dashboard cookie + Origin pinning is the human-
// consent layer here; an agent reaching this endpoint would have
// to forge the cookie, which is the same threat model that protects
// every other dashboard mutate.
//
// Body shape and validation logic are shared with the daemon's
// /v1/sudo POST path via insertSudoBundle / blockedSlugs /
// resolveSudoDuration. The granter label is
// "<human-dashboard>:proactive" so audit can distinguish dashboard
// grants from CLI grants and from agent-requested ones.
func handleDashboardSudoGrant(w http.ResponseWriter, r *http.Request) {
	if rest := strings.TrimPrefix(r.URL.Path, "/api/sudo"); rest != "" && rest != "/" {
		http.Error(w, "POST /api/sudo only (no path suffix)", http.StatusBadRequest)
		return
	}
	var body struct {
		Conv     string   `json:"conv"`
		Slugs    []string `json:"slugs"`
		Duration string   `json:"duration"`
		Reason   string   `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Conv = strings.TrimSpace(body.Conv)
	if body.Conv == "" {
		http.Error(w, "missing conv (selector for the agent to elevate)",
			http.StatusBadRequest)
		return
	}
	body.Slugs = dedupeNonEmpty(body.Slugs)
	if len(body.Slugs) == 0 {
		http.Error(w, "slugs[] is required (at least one slug to elevate)",
			http.StatusBadRequest)
		return
	}
	res, _, err := agent.ResolveSelector(body.Conv)
	if err != nil {
		http.Error(w, "resolve conv: "+err.Error(), http.StatusNotFound)
		return
	}
	title := ""
	if row := agent.FreshConvRowResolved(res.ConvID); row != nil {
		title = agent.DisplayTitle(row)
	}
	cfg, _ := config.Load()
	policy := resolveSudoConfig(cfg, res.ConvID, title)

	if blocked := blockedSlugs(body.Slugs, policy.Blocklist); len(blocked) > 0 {
		http.Error(w,
			"slug(s) blocklisted from sudo (would enable permanent escalation): "+
				strings.Join(blocked, ", "),
			http.StatusForbidden)
		return
	}
	dur, ok := resolveSudoDuration(w, body.Duration, policy)
	if !ok {
		return
	}
	out, status := insertSudoBundle(res.ConvID, title, body.Slugs, dur,
		strings.TrimSpace(body.Reason), dashboardSudoGranter)
	writeJSON(w, status, out)
}

// handleDashboardPermissionsAPI is the cookie-auth endpoint behind the
// dashboard's permanent-permission editor. It applies a batch of
// per-conv tri-state overrides in a single round-trip:
//
//	POST /api/permissions
//	  { "conv": "<selector>",
//	    "overrides": { "<slug>": "grant" | "deny" | "default" } }
//
// "grant" / "deny" write an agent_permissions row; "default" clears any
// existing row so the slug falls back to the config defaults. Slugs
// absent from the map are left untouched. These are PERMANENT
// overrides — distinct from the time-bounded `+ sudo` elevation.
//
// The dashboard cookie + Origin pin is the human-consent layer, same as
// every other /api mutation; the granter is recorded as
// "<human-dashboard>". The whole batch is validated before any write,
// so a malformed slug / effect can't leave a partial apply behind.
func handleDashboardPermissionsAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Conv      string            `json:"conv"`
		Overrides map[string]string `json:"overrides"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	body.Conv = strings.TrimSpace(body.Conv)
	if body.Conv == "" {
		http.Error(w, "missing conv (selector for the agent to edit)", http.StatusBadRequest)
		return
	}
	if len(body.Overrides) == 0 {
		http.Error(w, "overrides{} is required (at least one slug → grant|deny|default)", http.StatusBadRequest)
		return
	}
	// Validate the whole batch before touching the DB.
	for slug, effect := range body.Overrides {
		if !IsKnownPermSlug(slug) {
			http.Error(w, "unknown permission slug: "+slug, http.StatusBadRequest)
			return
		}
		switch effect {
		case db.PermEffectGrant, db.PermEffectDeny, "default":
		default:
			http.Error(w, "invalid effect "+strconv.Quote(effect)+" for slug "+slug+
				" (want grant, deny, or default)", http.StatusBadRequest)
			return
		}
	}
	res, _, err := agent.ResolveSelector(body.Conv)
	if err != nil {
		http.Error(w, "resolve conv: "+err.Error(), http.StatusNotFound)
		return
	}
	current, err := db.ListAgentPermissionOverridesForConv(res.ConvID)
	if err != nil {
		http.Error(w, "load current overrides: "+err.Error(), http.StatusInternalServerError)
		return
	}
	changed := 0
	for slug, effect := range body.Overrides {
		if effect == "default" {
			if _, ok := current[slug]; !ok {
				continue // already at the inherited default
			}
			if _, err := db.RevokeAgentPermission(res.ConvID, slug); err != nil {
				http.Error(w, "clear "+slug+": "+err.Error(), http.StatusInternalServerError)
				return
			}
			changed++
			continue
		}
		if current[slug] == effect {
			continue // already at the requested grant/deny
		}
		if err := db.SetAgentPermissionOverride(res.ConvID, slug, effect, dashboardGranter); err != nil {
			http.Error(w, "set "+slug+": "+err.Error(), http.StatusInternalServerError)
			return
		}
		changed++
	}
	overrides, _ := db.ListAgentPermissionOverridesForConv(res.ConvID)
	effective, _ := db.ListAgentPermissionsForConv(res.ConvID)
	title := ""
	if row := agent.FreshConvRowResolved(res.ConvID); row != nil {
		title = agent.DisplayTitle(row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id":   res.ConvID,
		"title":     title,
		"changed":   changed,
		"overrides": overrides,
		"effective": effective,
	})
}
