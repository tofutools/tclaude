package agentd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	mux.HandleFunc("/api/agent-generations/", handleDashboardAgentGenerationDelete)
	mux.HandleFunc("/api/worktrees", handleDashboardWorktreesAPI)
	mux.HandleFunc("/api/worktrees/cleanup", handleDashboardWorktreeCleanup)
	mux.HandleFunc("/api/jump/", handleDashboardJumpAPI)
	mux.HandleFunc("/api/hide/", handleDashboardHideAPI)
	mux.HandleFunc("/api/term/", handleDashboardTermAPI)
	mux.HandleFunc("/api/open-window/", handleDashboardOpenWindowAPI)
	mux.HandleFunc("/api/scribe", handleDashboardScribeAPI)
	mux.HandleFunc("/api/term-ws/", handleDashboardTermWS)
	mux.HandleFunc("/api/group-term-ws/", handleDashboardGroupTermWS)
	mux.HandleFunc("/api/open-window-ws/", handleDashboardOpenWindowWS)
	mux.HandleFunc("/api/spawn-focus-ws/", handleDashboardSpawnFocusWS)
	mux.HandleFunc("/api/pick-directory", handleDashboardPickDirAPI)
	mux.HandleFunc("/api/browse-directories", handleDashboardBrowseDirsAPI)
	mux.HandleFunc("/api/pending/focus/", handleDashboardPendingFocusAPI)
	mux.HandleFunc("/api/pending/delete/", handleDashboardPendingDeleteAPI)
	mux.HandleFunc("/api/sudo", handleDashboardSudoAPI)
	mux.HandleFunc("/api/sudo/", handleDashboardSudoAPI)
	mux.HandleFunc("/api/permissions", handleDashboardPermissionsAPI)
	mux.HandleFunc("/api/config", handleDashboardConfigAPI)
	mux.HandleFunc("/api/cost-factor", handleDashboardCostFactorAPI)
	mux.HandleFunc("/api/notifications", handleDashboardNotificationsAPI)
	mux.HandleFunc("/api/claude-settings/default-model", handleDashboardClaudeDefaultModel)
	mux.HandleFunc("/api/slop/volumes", handleDashboardSlopVolumesAPI)
	mux.HandleFunc("/api/slop/channel", handleDashboardSlopChannelAPI)
	mux.HandleFunc("/api/slop/nowplaying", handleDashboardSlopNowPlayingAPI)
	mux.HandleFunc("/api/dashboard/prefs", handleDashboardPrefsAPI)
	mux.HandleFunc("/api/cleanup/", handleDashboardCleanup)
	mux.HandleFunc("/api/shutdown", handleShutdown)
	mux.HandleFunc("/api/power-on", handlePowerOn)
	mux.HandleFunc("/api/agent-windows", handleAgentWindows)
	mux.HandleFunc("/api/export-jobs/", handleDashboardExportJobsAPI)
	mux.HandleFunc("/api/human-messages/read", handleDashboardHumanMessagesRead)
	mux.HandleFunc("/api/human-messages/clear", handleDashboardHumanMessagesClear)
	mux.HandleFunc("/api/human-messages/delete", handleDashboardHumanMessagesDelete)
	mux.HandleFunc("/api/human-messages/reply", handleDashboardHumanMessagesReply)
	mux.HandleFunc("/api/human-messages/", handleDashboardHumanMessageAttachment)
	registerDashboardCronRoutes(mux)
	registerDashboardMessageRoutes(mux)
	registerDashboardOperatorMessageRoutes(mux)
	registerDashboardMailboxRoutes(mux)
	registerDashboardAccessRequestRoutes(mux)
	registerDashboardLists(mux)
	registerDashboardTemplateRoutes(mux)
	registerDashboardSpawnProfileRoutes(mux)
	registerDashboardSandboxProfileRoutes(mux)
	registerDashboardRoleRoutes(mux)
	registerDashboardSpawnAttachmentRoutes(mux)
	registerDashboardPluginRoutes(mux)
	registerRemoteAccessAdminRoutes(mux)
}

// handleDashboardGroupsCreate is the cookie-auth twin of POST /v1/groups.
// Delegates to handleGroups after stamping a synthetic human peer — the
// cookie+Origin pin is the human-consent layer; requirePermission then
// sees a classHuman caller (asDashboardHumanPeer sets DashboardHuman).
// Registered as `POST /api/groups`, so the mux rejects other methods.
func handleDashboardGroupsCreate(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	handleGroups(w, asDashboardHumanPeer(r))
}

// handleDashboardGroupImport is the cookie-auth twin of POST
// /v1/groups/import — the dashboard's "⤒ import" button. It delegates to
// the SHARED, permission-checked handleGroupImport after stamping a
// synthetic human peer with asDashboardHumanPeer, exactly as the export
// route does (commit 6a1ade5): the cookie + Origin pin is the
// human-consent layer, and routing through the shared handler keeps the
// groups.import slug structurally enforced on every path. handleGroupImport
// reads the multipart upload (an "archive" file part + into / as fields)
// the browser posts. Registered as `POST /api/groups/import`.
func handleDashboardGroupImport(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	handleGroupImport(w, asDashboardHumanPeer(r))
}

// handleDashboardGroupImportInspect is the cookie-auth twin of POST
// /v1/groups/import/inspect — the dashboard import preview. It delegates
// to the shared, permission-checked handleGroupImportInspect after
// stamping a synthetic human peer; the dashboard calls it the moment a
// .zip is picked to render the manifest summary + collision report.
// Writes nothing. Registered as `POST /api/groups/import/inspect`.
func handleDashboardGroupImportInspect(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	handleGroupImportInspect(w, asDashboardHumanPeer(r))
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

// hideAgentResp is the wire shape returned by POST /api/hide/{conv}.
// Detached is the number of tmux clients dismissed — 0 when the agent
// had no window open, the idempotent no-op the dashboard toasts as
// "already hidden".
type hideAgentResp struct {
	ConvID   string `json:"conv_id"`
	Detached int    `json:"detached"`
}

// handleDashboardHideAPI dispatches:
//
//	POST /api/hide/{conv}    → detach the agent's tmux-attached terminal
//
// The per-agent twin of POST /api/jump/{conv}: where jump RAISES the
// agent's terminal window, hide DISMISSES it. It runs the exact
// per-agent op the bulk "windows" button performs for direction
// "unfocus" — detachAgentWindows (see window_focus.go) — scoped to one
// agent: `tmux detach-client` for every client attached to the
// session.
//
// Window-only: the agent PROCESS is never touched. It keeps running,
// and the window can be brought back at any time with focus.
//
// Idempotent by construction: an agent whose session already has no
// client attached detaches zero clients and reports detached:0 — a
// clean no-op, never an error. 404 only when the conv has no live
// session at all (the same boundary handleDashboardJumpAPI draws).
func handleDashboardHideAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/api/hide/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) == 0 || parts[0] == "" {
		http.Error(w, "expected /api/hide/{conv}", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if len(parts) > 1 && parts[1] != "" {
		http.Error(w, "unknown subpath /api/hide/{conv}/"+parts[1], http.StatusNotFound)
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
	n, err := detachAgentWindows(sess)
	if err != nil {
		http.Error(w, "detach windows: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, hideAgentResp{ConvID: res.ConvID, Detached: n})
}

// handleDashboardAgentsAPI dispatches:
//
//	DELETE /api/agents/{conv}              → wipe the conversation + orphan-clean
//	POST   /api/agents/{conv}/stop         → soft exit / force kill
//	POST   /api/agents/{conv}/resume       → wake (resume tmux pane)
//	POST   /api/agents/{conv}/clone        → fork a sibling (cookie-auth twin)
//	POST   /api/agents/{conv}/reincarnate  → spawn successor + soft-exit original
//	POST   /api/agents/{conv}/task         → set/clear task-reference link
//
// Behaviour:
//   - Runs conv.DeleteAgentAllGenerations (unlinks .jsonl, drops conv_index
//     row, strips sessions-index.json, writes sync tombstone) — and when the
//     conv is an agent's head generation, sweeps its predecessor generations
//     too (JOH-26 PR3d), so a multi-generation actor leaves nothing orphaned.
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
			dashboardResumeAgent(w, r, convSelector)
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
		case "notify":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			dashboardSetAgentNotify(w, r, convSelector)
			return
		case "tags":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			dashboardSetAgentTags(w, r, convSelector)
			return
		case "task":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			dashboardSetAgentTask(w, r, convSelector)
			return
		case "remote-control":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			dashboardRemoteControlAgent(w, r, convSelector)
			return
		case "export":
			if r.Method != http.MethodPost {
				http.Error(w, "POST only", http.StatusMethodNotAllowed)
				return
			}
			dashboardCreateExport(w, r, convSelector)
			return
		case "exports":
			switch r.Method {
			case http.MethodGet:
				dashboardListExports(w, convSelector)
			case http.MethodDelete:
				dashboardClearExports(w, convSelector)
			default:
				http.Error(w, "GET or DELETE only", http.StatusMethodNotAllowed)
			}
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
	//
	// NOTE: this path resolves a predecessor selector FORWARD to the actor's
	// live head — deleting a specific past generation goes through the
	// dedicated DELETE /api/agent-generations/{conv} endpoint instead, which
	// never resolves forward and refuses the live head.
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
	query := r.URL.Query()
	delWorktree := query.Get("delete_worktree") == "1" ||
		query.Get("delete_worktree") == "true"
	_, hasExpectedWorktree := query["expected_worktree"]
	if hasExpectedWorktree && !delWorktree {
		http.Error(w, "expected_worktree requires delete_worktree=1", http.StatusBadRequest)
		return
	}
	var expectedWorktreePath string
	if hasExpectedWorktree {
		expectedWorktreePath = query.Get("expected_worktree")
		if expectedWorktreePath == "" {
			http.Error(w, "expected_worktree must not be empty", http.StatusBadRequest)
			return
		}
	}
	var wt agentWorktreeView
	if delWorktree {
		// DeleteAgentAllGenerations sweeps EVERY generation of this actor, not
		// just convID. Exclude them all from the shared-check — otherwise a
		// predecessor generation that shares this worktree would flag it
		// "shared" (and leave it behind) even though it is about to be swept
		// too. Resolve the generation set the same way the sweep does.
		excluding := map[string]bool{convID: true}
		if agentID, err := db.AgentIDForConv(convID); err == nil && agentID != "" {
			if gens, gErr := db.ConvsForAgent(agentID); gErr == nil {
				for _, g := range gens {
					if g != "" {
						excluding[g] = true
					}
				}
			}
		}
		wt = captureAgentWorktreeClaims().resolve(convID, excluding)
		// A Preact opt-in freezes the exact removable path returned by its
		// earlier probe. Re-check that precondition at DELETE time, before
		// stopping the pane or purging any state. Older callers that omit the
		// precondition retain the established delete_worktree contract.
		if hasExpectedWorktree &&
			(wt.Path != expectedWorktreePath || !wt.Removable()) {
			http.Error(w,
				"worktree changed since confirmation; refresh and retry",
				http.StatusConflict)
			return
		}
	}

	// Dashboard deletes always force-kill any alive tmux session for
	// this conv — the "delete forever" button is unambiguous human
	// intent. Without this, the conv resurrects in handlePeers via
	// the still-alive sessions row.
	stopOneConv(convID, true /* force */)

	// Single source of truth for the comprehensive cleanup: filesystem
	// + DB union purge across every conv-id-referencing table +
	// session-env + sync tombstone. Actor-aware (JOH-26 PR3d): deleting an
	// agent's head generation also sweeps its predecessor generations'
	// rows + .jsonl, so a multi-generation actor leaves nothing orphaned.
	if _, err := removeAgentDirectoriesForConv(convID); err != nil {
		http.Error(w, "delete agent-owned directories: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if _, _, err := conv.DeleteAgentAllGenerations(convID); err != nil {
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

// handleDashboardAgentGenerationDelete deletes ONE replaced (predecessor)
// conversation generation by its EXACT conv-id — the per-row delete behind the
// dashboard's "Replaced generations" virtual group.
//
//	DELETE /api/agent-generations/{convID}
//
// Why a dedicated endpoint rather than /api/agents/{selector}: that resource
// resolves a selector FORWARD to the actor's live head (agent.ResolveSelector →
// ResolveLatestConv), so deleting a predecessor through it would delete the
// LIVE actor. This endpoint deliberately does NOT resolve forward and REFUSES
// the live head (409) — so "never delete the live generation here" is a tested
// backend invariant, not a UI convention that a future refactor could break.
//
// Behaviour:
//   - 400 if the conv-id isn't UUID-shaped (no blind DELETE on arbitrary input).
//   - 404 if the conv-id isn't a linked generation of any actor (use the
//     ordinary agent delete for plain conversations / orphans).
//   - 409 if the conv-id IS the actor's current/live generation — the caller
//     must use the agent delete (which tears the whole actor down).
//   - otherwise: force-kill any (unexpected) live pane, then conv.DeleteConvByID
//     removes exactly that generation's DB rows + .jsonl. Because it is a
//     predecessor, db.DeleteAgentByConvID unlinks just this conv and leaves the
//     live actor, its identity, and its other generations intact (JOH-26).
func handleDashboardAgentGenerationDelete(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	if r.Method != http.MethodDelete {
		http.Error(w, "DELETE only", http.StatusMethodNotAllowed)
		return
	}
	convID := strings.TrimPrefix(r.URL.Path, "/api/agent-generations/")
	if u, err := url.PathUnescape(convID); err == nil {
		convID = u
	}
	if !looksLikeConvID(convID) {
		http.Error(w, "expected /api/agent-generations/{conv-id}", http.StatusBadRequest)
		return
	}

	actor, err := db.GetAgentByConv(convID)
	if err != nil {
		http.Error(w, "resolve generation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if actor == nil {
		http.Error(w, "not a linked generation; use the agent delete for plain conversations",
			http.StatusNotFound)
		return
	}
	if actor.CurrentConvID == convID {
		http.Error(w, "that is the actor's live generation; delete the agent instead",
			http.StatusConflict)
		return
	}

	// A predecessor is offline by construction (a /clear shares the head's
	// process; a reincarnate soft-exits the original), but force-kill any
	// lingering pane before teardown — same discipline as the agent delete.
	stopOneConv(convID, true /* force */)

	// Exact, single-generation teardown: rows + .jsonl for THIS conv only.
	// db.DeleteAgentByConvID takes the predecessor-unlink branch (this is not
	// the actor's current_conv_id), so the live actor survives untouched.
	counts, err := conv.DeleteConvByID(convID)
	if err != nil {
		http.Error(w, "delete generation: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id":   convID,
		"action":    "deleted",
		"db_counts": counts,
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
//	POST   /api/groups/import                   → import a group from an uploaded .zip
//	POST   /api/groups/import/inspect           → dry-run analyse an uploaded .zip (preview)
//	DELETE /api/groups/{name}                   → delete group
//	PATCH  /api/groups/{name}                   → update settings (body: {default_cwd})
//	POST   /api/groups/{name}/clone             → clone the group (body: {new_name?, no_clone_members?, copy_owners?})
//	POST   /api/groups/{name}/rename            → rename (body: {new_name})
//	GET    /api/groups/{name}/worktrees         → discover the group's repo worktrees for cleanup
//	GET    /api/groups/{name}/export            → download the group as a .zip archive
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
	mux.HandleFunc("/api/spawn-harness-policy", handleDashboardGlobalSpawnHarnessPolicy)

	// Import is NOT group-scoped (it creates a group), so it carries no
	// {name} wildcard. The literal `import` segment is more specific than
	// the {name} wildcard, so these coexist with the /{name}/... routes
	// below without ambiguity — the mux always picks the literal match.
	mux.HandleFunc("POST /api/groups/import", handleDashboardGroupImport)
	mux.HandleFunc("POST /api/groups/import/inspect", handleDashboardGroupImportInspect)

	mux.HandleFunc("GET /api/groups/{name}/export", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		// asDashboardHumanPeer so the shared, permission-checked
		// handleGroupExport sees the cookie-authed dashboard caller as a
		// human — same wiring as PATCH /api/groups/{name}.
		handleGroupExport(w, asDashboardHumanPeer(r), g)
	}))

	mux.HandleFunc("DELETE /api/groups/{name}", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		dashboardDeleteGroup(w, g.Name)
	}))
	mux.HandleFunc("PATCH /api/groups/{name}", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupUpdate(w, asDashboardHumanPeer(r), g)
	}))
	mux.HandleFunc("POST /api/groups/{name}/clone", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		// asDashboardHumanPeer so the shared, permission-checked
		// handleGroupClone sees the cookie-authed dashboard caller as a
		// human — same wiring as the export/rename routes. Body:
		// {new_name?, no_clone_members?, copy_owners?}.
		handleGroupClone(w, asDashboardHumanPeer(r), g)
	}))
	mux.HandleFunc("POST /api/groups/{name}/rename", groupRoute(dashboardRenameGroup))
	// Nest a group under another / clear its parent (JOH-392). asDashboardHumanPeer
	// so the shared, permission-checked handleGroupParent sees the cookie-authed
	// dashboard caller as the human (groups.nest-gated on the /v1 path). Body: {parent}.
	mux.HandleFunc("PUT /api/groups/{name}/parent", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupParent(w, asDashboardHumanPeer(r), g)
	}))
	mux.HandleFunc("POST /api/groups/{name}/retire", groupRoute(dashboardGroupRetire))
	mux.HandleFunc("GET /api/groups/{name}/worktrees", groupRoute(dashboardGroupWorktrees))
	mux.HandleFunc("POST /api/groups/{name}/spawn", groupRoute(dashboardSpawnInGroup))
	mux.HandleFunc("/api/groups/{name}/spawn-harness-policy", groupRoute(handleDashboardGroupSpawnHarnessPolicy))
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
	// Advisory process runtime (JOH-242): read the current phase, advance to
	// the next / a named phase. asDashboardHumanPeer so the shared,
	// permission-checked handler sees the cookie-authed dashboard caller as the
	// human (advance is process.advance-gated on the /v1 path).
	mux.HandleFunc("GET /api/groups/{name}/process", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupProcessGet(w, asDashboardHumanPeer(r), g)
	}))
	mux.HandleFunc("POST /api/groups/{name}/process/advance", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupProcessAdvance(w, asDashboardHumanPeer(r), g)
	}))
	// Re-brief a deployed force (JOH-247): re-deliver the source template's work
	// pattern to the live roster. asDashboardHumanPeer so the shared,
	// permission-checked handler sees the cookie-authed dashboard caller as the
	// human (re-brief is templates.instantiate-gated on the /v1 path).
	mux.HandleFunc("POST /api/groups/{name}/rebrief", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupRebrief(w, asDashboardHumanPeer(r), g)
	}))
	// Stand down a task force (JOH-345): retire the roster + sweep the
	// deploy-seeded rhythms and pending waves, keeping the group row.
	// asDashboardHumanPeer so the shared, permission-checked handler sees the
	// cookie-authed dashboard caller as the human (stand-down is
	// groups.retire-gated on the /v1 path).
	mux.HandleFunc("POST /api/groups/{name}/stand-down", groupRoute(func(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
		handleGroupStandDown(w, asDashboardHumanPeer(r), g)
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

// dashboardGroupRetire is the cookie-auth twin of
// /v1/groups/{name}/retire (handleGroupRetire) — the human-driven bulk
// retire behind the command palette's "Retire idle/offline agents in
// <group>". It trusts the cookie-authed caller (the dashboard is
// human-only, so caller=""), hence no groups.retire slug check, and
// shares the same parallel core.
//
// Two cohort-selection modes, chosen by the request body:
//   - EXPLICIT (the preview path): an optional JSON body {convs:[…],
//     shutdown?, reason?}. When convs is non-empty the retire is scoped to
//     exactly those conv-ids — the list the human ticked in the retire
//     preview modal — and the BE retires precisely that set, never a
//     re-derived cohort. This is what keeps "the list the human saw" and
//     "the list the BE acts on" identical even if an agent flips status
//     between preview and submit.
//   - STATUS FILTER (the legacy path): no body (or an empty convs list)
//     falls back to the same query knobs as the /v1 endpoint —
//     ?shutdown= (default on), ?status= (idle/offline/…; absent = every
//     member) and ?reason=.
//
// shutdown/reason/delete_worktree may be supplied in the body (explicit
// path) or the query (filter path); a body field, when present, wins.
// delete_worktree opts the whole batch into removing each retired
// member's git worktree+branch (the preview modal's checkbox); it
// defaults OFF, and the per-member safety rules of the single-agent
// retire still apply (main repo / shared worktrees kept, removal deferred
// until each pane exits).
func dashboardGroupRetire(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	var body struct {
		// Convs is a POINTER so an absent key (nil → legacy status-filter
		// path) is distinguishable from an explicit empty list ({"convs":[]}
		// → explicit path with nothing selected, which is a 400, NOT a
		// silent fallback that would retire the whole group).
		Convs    *[]string `json:"convs"`
		Shutdown *bool     `json:"shutdown"`
		Reason   *string   `json:"reason"`
		// DeleteWorktree opts the whole batch into worktree+branch cleanup
		// (the preview modal's checkbox). A POINTER so an absent key keeps
		// the query fallback / the OFF failsafe; present wins.
		DeleteWorktree *bool `json:"delete_worktree"`
	}
	// The body is optional — the legacy status-filter callers send none.
	// A present-but-malformed body is a 400, not a silent fallthrough, so
	// a client bug surfaces instead of retiring the wrong cohort. The 64
	// KiB cap bounds an abusive payload; a realistic convs list is tiny.
	if r.ContentLength != 0 {
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
		if err != nil {
			http.Error(w, "retire: read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				http.Error(w, "retire: decode body: "+err.Error(), http.StatusBadRequest)
				return
			}
		}
	}

	// shutdown/reason/delete_worktree resolve from the body when present,
	// else the query (delete_worktree defaults OFF — the failsafe, so a
	// caller that omits it never nukes a worktree by accident).
	shutdown := retireShouldShutdown(r)
	if body.Shutdown != nil {
		shutdown = *body.Shutdown
	}
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	if body.Reason != nil {
		reason = strings.TrimSpace(*body.Reason)
	}
	deleteWorktree := retireShouldDeleteWorktree(r)
	if body.DeleteWorktree != nil {
		deleteWorktree = *body.DeleteWorktree
	}

	// An explicit `convs` key (even empty) selects the explicit path and
	// wins over the status filter; its absence keeps the legacy filter
	// path. Dedupe into a set so a duplicated conv-id costs one retire, not
	// two. A present-but-empty (or all-blank) list is a 400, never a
	// fallthrough — that guards against {"convs":[]} silently meaning
	// "retire everyone".
	var selected map[string]struct{}
	var filter retireStatusFilter
	if body.Convs != nil {
		selected = make(map[string]struct{}, len(*body.Convs))
		for _, c := range *body.Convs {
			if c = strings.TrimSpace(c); c != "" {
				selected[c] = struct{}{}
			}
		}
		if len(selected) == 0 {
			http.Error(w, "retire: convs was provided but held no valid conv-ids", http.StatusBadRequest)
			return
		}
	} else {
		var ferr error
		filter, ferr = parseRetireStatusFilter(r.URL.Query().Get("status"))
		if ferr != nil {
			http.Error(w, "retire: "+ferr.Error(), http.StatusBadRequest)
			return
		}
	}

	out, err := bulkRetireGroupMembers(g, "", reason, shutdown, deleteWorktree, filter, selected)
	if err != nil {
		http.Error(w, "retire: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// If this retire left the group with no live members, disable its
	// template-seeded rhythms so they stop firing to nobody (JOH-345).
	out.RhythmsDisabled = disableGroupRhythmsIfEmptied(g)
	writeJSON(w, http.StatusOK, out)
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
	body, ok := decodeCloneBody(w, r)
	if !ok {
		return
	}
	runCloneOrchestration(w, r, res.ConvID, dashboardGranter, "", body)
}

// reincarnateMode* are the two modes the dashboard reincarnate button
// offers, selected by the POST body's `mode` field.
//
//   - "self" is the DEFAULT. The daemon does NOT reincarnate the
//     agent; it delivers an inbox message asking the agent to
//     reincarnate ITSELF. The agent writes its own handoff at a clean
//     point, so the successor inherits a context-aware summary the
//     agent chose — something a daemon-forced reincarnation cannot
//     produce, since it knows nothing of the agent's working state.
//   - "force" is the unchanged direct path: the daemon spawns the
//     successor and soft-exits the original immediately. For an agent
//     that is stuck / unresponsive and cannot self-reincarnate.
const (
	reincarnateModeSelf  = "self"
	reincarnateModeForce = "force"
)

// dashboardReincarnateAgent handles POST /api/agents/{conv}/reincarnate.
// It dispatches on the body's `mode` field (default "self"):
//
//   - "self": delegate to dashboardAskSelfReincarnate — deliver an
//     inbox message asking the agent to reincarnate itself, with an
//     optional `focus_hint` folded in as guidance. The target's tmux
//     session is left running; nothing is force-killed.
//   - "force": the cookie-auth twin of POST /v1/agent/{conv}/reincarnate.
//     Spawns a fresh CC instance that inherits the target's identity
//     (groups / perms / ownerships migrate onto the new conv-id),
//     renames it `<prev>-r-<N>`, and soft-exits the original pane.
//     Body: `{follow_up}` — REQUIRED.
//
// Cookie auth ≈ human (checkDashboardAuth is the consent layer), so the
// force path's requireCrossAgentPermission sees a classHuman caller
// (asDashboardHumanPeer sets DashboardHuman) and the audit trail records
// the dashboard granter.
func dashboardReincarnateAgent(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	// Peek at `mode` without consuming the body for the force path:
	// handleAgentReincarnate re-decodes r.Body itself, so we buffer the
	// raw bytes and hand them back verbatim. force-mode stays the
	// unchanged direct path — its decoder simply ignores the extra
	// `mode` / `focus_hint` fields. The 64 KiB MaxBytesReader bounds the
	// raw JSON request so an abusive payload can't be slurped whole; the
	// real per-field caps (follow_up / focus_hint length + charset) live
	// downstream in decodeReincarnateBody / isValidInitialMessage.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", "read body: "+err.Error())
		return
	}
	var body struct {
		Mode      string `json:"mode"`
		FocusHint string `json:"focus_hint"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	mode := strings.TrimSpace(body.Mode)
	if mode == "" {
		mode = reincarnateModeSelf
	}
	switch mode {
	case reincarnateModeSelf:
		dashboardAskSelfReincarnate(w, res.ConvID, body.FocusHint)
	case reincarnateModeForce:
		r.Body = io.NopCloser(bytes.NewReader(raw))
		r.ContentLength = int64(len(raw))
		handleAgentReincarnate(w, asDashboardHumanPeer(r), res.ConvID)
	default:
		writeError(w, http.StatusBadRequest, "invalid_arg",
			fmt.Sprintf("unknown reincarnate mode %q (want %q or %q)",
				mode, reincarnateModeSelf, reincarnateModeForce))
	}
}

// dashboardAskSelfReincarnate is the "self" reincarnate mode — the
// dashboard default. Rather than the daemon reincarnating the agent, it
// delivers an inbox message asking the agent to reincarnate itself:
// write a handoff for its successor capturing the context that matters,
// then run `tclaude agent reincarnate` with that handoff as the
// follow-up. The agent acts on it at its next clean point — and because
// it does the work itself, the successor inherits a context-aware
// summary the agent chose, not a daemon-forced reincarnation blind to
// the agent's working state.
//
// focusHint is OPTIONAL free text — a hint from the human about what to
// concentrate on while gathering context for the handoff. When
// non-empty it is folded into the message as guidance (NOT a command).
// When blank, the agent just writes a general handoff.
//
// The request rides the universal inbox (db.InsertAgentMessage +
// nudgeIfAlive) — the same transport reincarnate's own handoff uses. A
// live target is nudged immediately; an offline / busy one picks the
// message up from its inbox when it next comes online (the daemon
// flushes undelivered messages on the agent's next request). The
// target's tmux session is left running — nothing is force-killed.
//
// Unlike the force path, this does NOT go through requireCrossAgentPermission:
// self-mode only delivers an inbox message, which is an ungated
// capability (the cookie-auth human could equally send it via
// /api/message). Self-reincarnation itself is intrinsic to every active agent;
// cross-agent reincarnation remains gated on agent.reincarnate or ownership.
func dashboardAskSelfReincarnate(w http.ResponseWriter, target, focusHint string) {
	subject, instruction := buildSelfReincarnateInstruction(focusHint)
	// The instruction rides the inbox like any agent_messages row, so it
	// must clear the same charset/length rule. A blank focus hint always
	// passes; this only ever fires on a hint carrying control characters
	// or one long enough to push the composed body past the cap.
	if !isValidInitialMessage(instruction) {
		writeError(w, http.StatusBadRequest, "invalid_focus_hint",
			fmt.Sprintf("the composed reincarnate instruction is invalid — the focus hint most "+
				"likely contains control characters, or it is long enough to push the message past "+
				"%d bytes. Shorten or clean up the focus hint.", agent.MaxInitialMessageBytes))
		return
	}
	// FromConv is empty: this is a daemon-originated system instruction,
	// not a peer-to-peer send — the same shape reincarnate's own handoff
	// uses when triggered from the dashboard. group_id 0 is a direct
	// message, the universal-inbox transport.
	id, err := db.InsertAgentMessage(&db.AgentMessage{
		GroupID:      0,
		FromConv:     "",
		ToConv:       target,
		Subject:      subject,
		Body:         instruction,
		ToRecipients: []string{target},
	})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"queue self-reincarnate request: "+err.Error())
		return
	}
	outcome := nudgeIfAlive(id, target)
	delivered := outcome.delivered()
	note := fmt.Sprintf("asked %s to reincarnate itself; instruction delivered to its inbox as "+
		"message #%d — it will write its own handoff and reincarnate at a clean point",
		short8(target), id)
	if !delivered {
		// Two distinct not-delivered cases: offline (picks it up when it
		// next comes online) vs held because the target is mid-question with
		// a human (it IS online — picks it up when it resumes). Tailor both
		// the reason and the closing clause so the held line doesn't read as
		// "offline".
		reason := "target offline or busy"
		tail := "comes online"
		if outcome.held() {
			reason = "target is waiting on human input; held in its mailbox"
			tail = "resumes"
		}
		note = fmt.Sprintf("asked %s to reincarnate itself; instruction queued in its inbox as "+
			"message #%d (%s) — it will pick the request up when it next %s",
			short8(target), id, reason, tail)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":       reincarnateModeSelf,
		"conv_id":    target,
		"message_id": id,
		"delivered":  delivered,
		"held":       outcome.held(),
		"note":       note,
	})
}

// buildSelfReincarnateInstruction composes the subject + body of the
// "please reincarnate yourself" inbox message. focusHint, when
// non-empty, is appended as guidance on what to emphasise — phrased so
// the agent treats it as a hint, not a command and not the whole task.
//
// No backticks: the body is read by the agent and may be echoed into
// shells / forwarded messages downstream, where backticks would be
// eaten by shell command substitution. Plain text keeps it paste-safe.
func buildSelfReincarnateInstruction(focusHint string) (subject, body string) {
	subject = "Please reincarnate yourself (dashboard request)"
	var b strings.Builder
	b.WriteString("The human has asked you, from the dashboard, to reincarnate yourself.\n\n")
	b.WriteString("Reincarnation replaces you with a fresh Claude Code instance that inherits your ")
	b.WriteString("identity (group memberships, permissions, ownerships) but starts with a clean ")
	b.WriteString("context window. Doing it yourself — rather than having the daemon force it — lets ")
	b.WriteString("you collect your own relevant context first, so your successor starts from a ")
	b.WriteString("handoff you wrote.\n\n")
	b.WriteString("At a clean point (finish your current turn or sub-task first — there is no need to ")
	b.WriteString("interrupt yourself mid-thought), please:\n")
	b.WriteString("  1. Persist any work-in-progress to disk.\n")
	b.WriteString("  2. Write a handoff for your successor: a concise but self-contained summary of ")
	b.WriteString("what you were working on, where the relevant files are, what is done, what is ")
	b.WriteString("next, and any open questions.\n")
	b.WriteString("  3. Run: tclaude agent reincarnate \"<your handoff text>\"\n")
	b.WriteString("     The handoff is the REQUIRED follow-up — it becomes your successor's first ")
	b.WriteString("turn, so make it stand on its own.\n\n")
	b.WriteString("See the agent-lifecycle skill for the full details of reincarnate.")
	if hint := strings.TrimSpace(focusHint); hint != "" {
		b.WriteString("\n\nFocus hint from the human — guidance on what to emphasise while gathering ")
		b.WriteString("context for your handoff. Treat it as a hint, not a command and not the whole ")
		b.WriteString("task:\n")
		b.WriteString(hint)
	}
	return subject, b.String()
}

// dashboardRenameAgent is the cookie-auth twin of POST
// /v1/agent/{conv}/rename. Body shape matches the daemon endpoint:
// `{title: "..."}` for an explicit rename, or `{auto: true}` to
// inject a system nudge that asks the agent to pick its own title
// via the agent-rename skill / CLI. Cookie auth ≈ human, so
// requireCrossAgentPermission sees a classHuman caller
// (asDashboardHumanPeer sets DashboardHuman).
func dashboardRenameAgent(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	handleAgentRename(w, asDashboardHumanPeer(r), res.ConvID)
}

// dashboardRemoteControlAgent is the cookie-auth twin of POST
// /v1/agent/{conv}/remote-control: it flips the agent's harness Remote
// Access (Claude Code's /remote-control toggle) on the operator's behalf so
// the fleet view can expose an agent to the phone before the human steps
// away (JOH-259). The dashboard cookie IS the human-consent layer, so it
// delegates to the shared /v1 handler under a synthetic human peer
// (asDashboardHumanPeer) — clearing the agent.remote-control gate exactly
// the way the rename / clone / reincarnate dashboard verbs clear theirs.
// Body: {intent} — "on" | "off" | "toggle" | "status"; the merged handler
// owns the toggle direction + the disable confirm-Enter, the UI only sends
// intent. Codex (no Remote Access) is rejected by the handler's
// CanRemoteControl gate; the UI also hides the control for it.
func dashboardRemoteControlAgent(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	handleAgentRemoteControl(w, asDashboardHumanPeer(r), res.ConvID)
}

// dashboardResumeAgent is the cookie-auth twin of POST
// /v1/agent/{conv}/resume. Idempotent — already-online conv-ids
// surface as `skipped:already_online`. No body.
//
// ?recreate=1 opts into recreating a deleted launch dir empty before the
// relaunch. Without it, a resume whose recorded cwd was deleted comes back
// as `error:missing_cwd` (Detail = the path); the wake button then pops a
// confirm and retries with ?recreate=1 so the agent can start.
func dashboardResumeAgent(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	recreate := r.URL.Query().Get("recreate") == "1"
	out := resumeOneConvRecreate(res.ConvID, recreate)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// dashboardSetAgentNotify sets (or clears) the per-agent OS-notification
// override: POST /api/agents/{conv}/notify, body {mode}, where mode is
// "on" (notify even when a containing group is muted), "off" (always
// silent) or "inherit" (drop the override — follow group/global).
func dashboardSetAgentNotify(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Validate here so a bad mode is a 400 while a failed DB write
	// below stays a 500 — collapsing both into 400 would mislabel
	// lock/I-O failures as client bugs.
	mode := strings.TrimSpace(body.Mode)
	switch mode {
	case "", db.NotifyPrefInherit:
		mode = db.NotifyPrefInherit
	case db.NotifyPrefOn, db.NotifyPrefOff:
	default:
		http.Error(w, "invalid notify mode "+mode+" (want on, off or inherit)", http.StatusBadRequest)
		return
	}
	if err := db.SetConvNotifyPref(res.ConvID, mode); err != nil {
		http.Error(w, "set notify pref: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conv": res.ConvID, "mode": mode})
}

// dashboardSetAgentTags replaces an agent's tag set from the dashboard's
// edit-member panel (POST /api/agents/{conv}/tags {"tags": [...]}). This
// is the cookie-authed human twin of the CLI's /v1/agent/{conv}/tags — no
// permission slug (the dashboard IS the operator). The tags are agent-
// level (keyed on the stable agent_id), so they follow the actor across
// groups; the write is a replace-set, validated + de-duped + count-capped
// by db.ReplaceAgentTags. A bad tag is a 400 while a DB failure is a 500.
func dashboardSetAgentTags(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	var body struct {
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	agentID, err := db.AgentIDForConv(res.ConvID)
	if err != nil {
		http.Error(w, "resolve agent id: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if agentID == "" {
		http.Error(w, "no agent enrolled for this conversation", http.StatusNotFound)
		return
	}
	if err := db.ReplaceAgentTags(agentID, body.Tags); err != nil {
		if isTagValidationError(err) {
			http.Error(w, "invalid tag: "+err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "set tags: "+err.Error(), http.StatusInternalServerError)
		return
	}
	tags, err := db.ListAgentTags(agentID)
	if err != nil {
		http.Error(w, "read tags: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"conv": res.ConvID, "tags": tags})
}

// dashboardSetAgentTask is the cookie-authenticated human twin of POST
// /v1/agent/{conv}/task. It backs the Task column's edit dialog and accepts
// the same body shape: {"url":"https://…"} sets, while {"clear":true}
// clears. Resolving the selector first lets dashboard rows use stable agent_id
// handles; delegating to handleAgentTask keeps validation, label derivation,
// persistence, and response shape shared with the CLI/API path. The dashboard
// cookie is the operator-consent layer, so the synthetic peer is classified as
// human and does not depend on the target agent's self.task permission.
func dashboardSetAgentTask(w http.ResponseWriter, r *http.Request, convSelector string) {
	res, _, err := agent.ResolveSelector(convSelector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	handleAgentTask(w, asDashboardHumanPeer(r), res.ConvID)
}

// notificationsStateJSON is the shared GET/POST response shape for the
// top-bar bell popover: the master switch, the per-type checklist (each
// canonical destination state → on/off, derived from the wildcard
// transition rules) and the human-messages intent. The Config tab edits
// the same underlying config.notifications block through the full-config
// save path; both views agree because both read these helpers.
func notificationsStateJSON(cfg *config.Config) map[string]any {
	var n *config.NotificationConfig
	if cfg != nil {
		n = cfg.Notifications
	}
	types := make(map[string]bool, len(config.NotifyTypes))
	for _, ty := range config.NotifyTypes {
		types[ty] = n.NotifyTypeEnabled(ty)
	}
	return map[string]any{
		"enabled":         n != nil && n.Enabled,
		"types":           types,
		"human_messages":  n.HumanMessagesIntent(),
		"access_requests": cfg.AccessRequestSystemNotification(),
	}
}

// handleDashboardNotificationsAPI backs the top-bar bell popover (and is
// the lightweight twin of the Config tab's full-config editor for the
// notifications block in ~/.tclaude/config.json).
//
//	GET  → {enabled, types{state:bool…}, human_messages, access_requests}
//	POST → any subset of {enabled?, types?{state:bool…}, human_messages?, access_requests?};
//	       only the provided fields change, response echoes the new state.
//
// The master `enabled` sits ABOVE the per-group/per-agent filters — off
// means nothing notifies, anywhere. The per-type `types` map toggles the
// wildcard transition rules ({from:"*", to:state}); from-specific or
// non-canonical "advanced" rules are preserved untouched (see
// config.SetNotifyType). `human_messages` is the notify-human OS-banner
// knob, default-on within an enabled block. `access_requests` is the
// --ask-human OS-banner knob under agent config, default-off.
func handleDashboardNotificationsAPI(w http.ResponseWriter, r *http.Request) {
	if !checkDashboardAuth(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		cfg, err := config.Load()
		if err != nil {
			http.Error(w, "load config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, notificationsStateJSON(cfg))
	case http.MethodPost:
		var body struct {
			Enabled        *bool           `json:"enabled"`
			Types          map[string]bool `json:"types"`
			HumanMessages  *bool           `json:"human_messages"`
			AccessRequests *bool           `json:"access_requests"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "expected body {enabled?, types?, human_messages?, access_requests?}", http.StatusBadRequest)
			return
		}
		if body.Enabled == nil && body.Types == nil && body.HumanMessages == nil && body.AccessRequests == nil {
			http.Error(w, "no recognised field; expected one of enabled, types, human_messages, access_requests", http.StatusBadRequest)
			return
		}
		// Validate every type key up front so a typo is a clean 400 rather
		// than a silently-ignored no-op write.
		for k := range body.Types {
			if !config.IsNotifyType(k) {
				http.Error(w, "unknown notification type: "+k, http.StatusBadRequest)
				return
			}
		}
		// Serialized read-modify-write so a concurrent config writer
		// (another dashboard action, the Config tab save) can't drop this
		// change — and vice versa.
		saved, err := config.Update(func(cfg *config.Config, loadErr error) error {
			if loadErr != nil {
				// Load fell back to defaults, so a save here would replace
				// the corrupt file with defaults-plus-this-toggle — silently
				// discarding whatever it held (custom transitions, cooldown,
				// command, rate-limit, slop volumes…). Refuse; the Config
				// tab owns that recovery. Mirrors handleDashboardSlopVolumesPost.
				return errNotifConfigMalformed
			}
			if cfg.Notifications == nil {
				cfg.Notifications = config.DefaultConfig().Notifications
			}
			n := cfg.Notifications
			if body.Enabled != nil {
				n.Enabled = *body.Enabled
			}
			for ty, on := range body.Types {
				n.SetNotifyType(ty, on)
			}
			if body.HumanMessages != nil {
				if *body.HumanMessages {
					// Absence is the default-on state — keep the config
					// minimal rather than persisting an explicit `true`.
					n.HumanMessages = nil
				} else {
					off := false
					n.HumanMessages = &off
				}
			}
			if body.AccessRequests != nil {
				if cfg.Agent == nil {
					if *body.AccessRequests {
						cfg.Agent = &config.AgentConfig{AccessRequestSystemNotification: true}
					}
				} else {
					cfg.Agent.AccessRequestSystemNotification = *body.AccessRequests
				}
			}
			return nil
		})
		if errors.Is(err, errNotifConfigMalformed) {
			http.Error(w, "config.json on disk is corrupt — fix or replace it via the Config tab before changing notifications", http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, "save config: "+err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, notificationsStateJSON(saved))
	default:
		http.Error(w, "GET or POST only", http.StatusMethodNotAllowed)
	}
}

// errNotifConfigMalformed signals that the bell popover's notification
// write refused because config.json failed to load — Update fell back to
// defaults and a blind save would discard the user's real config. The
// caller maps it to a 409 (the Config tab owns corrupt-file recovery).
// Mirrors errSlopConfigMalformed.
var errNotifConfigMalformed = errors.New("notifications config.json is malformed")

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
	policy := resolveSudoConfig(cfg, res.ConvID, res.AgentID, title)

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
