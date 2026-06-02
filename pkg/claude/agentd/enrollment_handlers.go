package agentd

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Enrollment lifecycle endpoints — the explicit promote / retire /
// reinstate verbs that move a conversation across the agent boundary.
//
//	POST /v1/agent/{selector}/promote    plain conversation → agent
//	POST /v1/agent/{selector}/retire     agent → plain conversation
//	POST /v1/agent/{selector}/reinstate  retired agent → active agent
//
// All three route through handleAgentByConv's dispatcher and are gated
// by requireCrossAgentPermission, so a human (CLI or dashboard) always
// passes, an agent needs the slug, and a group owner may act on its
// own members. The dashboard's per-row buttons reach the same handlers
// via dashboardEnrollmentVerb with a synthetic human peer.

// retireConvOutcome reports what one retire pass actually changed, so
// the CLI / dashboard can show the human a precise summary.
type retireConvOutcome struct {
	GroupsLeft   []string `json:"groups_left"`
	PermsRevoked int64    `json:"perms_revoked"`
	SudoRevoked  int64    `json:"sudo_revoked"`
	Retired      bool     `json:"retired"`
}

// retireAgentConv demotes convID from an agent to a plain
// conversation: it unjoins every group (owner rows included), revokes
// every permission and sudo grant, then flips the enrollment bit. The
// conversation data — the .jsonl, the conv_index row, the worktree —
// is left completely untouched; this is the non-destructive half of
// cleanup. Safe on a conv that is already retired or was never an
// agent: the group/grant revokes are no-ops and Retired comes back
// false. Shared by the /v1 retire verb and the bulk cleanup endpoint.
//
// The second return value is the IDs of groups whose owner roster this
// retire touched — the bulk cleanup uses them for its ownerless-group
// warning; the /v1 handler ignores them.
func retireAgentConv(convID, by, reason string) (retireConvOutcome, []int64, error) {
	var out retireConvOutcome
	// Unjoin every group, owner rows included — a retired conv keeps
	// no group ties. unjoinConvFromAllGroups lives in dashboard_cleanup.go.
	removed, _, ownerGroups, err := unjoinConvFromAllGroups(convID, true)
	if err != nil {
		return out, nil, fmt.Errorf("unjoin groups: %w", err)
	}
	out.GroupsLeft = removed
	if n, rerr := db.RevokeAllAgentPermissionsForConv(convID); rerr == nil {
		out.PermsRevoked = n
	}
	if n, rerr := db.RevokeSudoGrantsByConv(convID); rerr == nil {
		out.SudoRevoked = n
	}
	did, err := db.RetireAgent(convID, by, reason)
	if err != nil {
		return out, ownerGroups, fmt.Errorf("retire: %w", err)
	}
	out.Retired = did
	return out, ownerGroups, nil
}

// enrollmentActor names who performed an enrollment action for the
// audit columns: "human" for a human caller (callerConv empty), the
// caller's conv-id for an agent.
func enrollmentActor(callerConv string) string {
	if callerConv == "" {
		return "human"
	}
	return callerConv
}

// retireShouldShutdown reports whether a retire request should also
// soft-stop the agent's running tmux session. Shutdown is the default
// across every retire surface — a human retiring an agent almost
// always wants the idle process gone, not left occupying a pane. The
// caller opts OUT with ?shutdown=0 (or =false); an absent or
// unparseable param keeps the default ON, so a forgetful caller fails
// safe to the documented behaviour.
func retireShouldShutdown(r *http.Request) bool {
	v := strings.TrimSpace(r.URL.Query().Get("shutdown"))
	if v == "" {
		return true
	}
	on, err := strconv.ParseBool(v)
	return err != nil || on
}

// retireShouldDeleteWorktree reports whether a retire request should
// also remove the agent's git worktree and delete its local branch.
// Unlike shutdown, this defaults OFF: only the dashboard's retire modal
// — which has already probed for a removable worktree — sends the flag.
// A bare CLI `retire`, or any caller that omits it, must never nuke a
// worktree by accident. ?delete_worktree=1 (or =true) opts in.
func retireShouldDeleteWorktree(r *http.Request) bool {
	v := strings.TrimSpace(r.URL.Query().Get("delete_worktree"))
	on, err := strconv.ParseBool(v)
	return err == nil && on
}

// handleAgentRetire serves POST /v1/agent/{selector}/retire.
//
// Unless ?shutdown=0 is passed, a successful retire also soft-exits
// the agent's running tmux session (stopOneConv with force=false —
// injects /exit, never a kill). Retire semantics are unchanged: the
// conversation stays on disk and is reinstatable; shutdown only ends
// the live process. A retired agent whose session is already dead is
// a no-op (stopOneConv reports skipped:already_offline).
func handleAgentRetire(w http.ResponseWriter, r *http.Request, convID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentRetire, convID)
	if !ok {
		return
	}
	state, err := db.EnrollmentState(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if state != db.EnrollmentActive {
		writeError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("conv %s is not an active agent (enrollment: %s) — nothing to retire", short8(convID), state))
		return
	}
	reason := strings.TrimSpace(r.URL.Query().Get("reason"))
	outcome, _, err := retireAgentConv(convID, enrollmentActor(caller), reason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	resp := map[string]any{
		"conv_id": convID,
		"outcome": outcome,
	}

	shutdown := retireShouldShutdown(r)
	deleteWorktree := retireShouldDeleteWorktree(r)

	// Resolve the worktree BEFORE issuing the shutdown: the deferred
	// removal waits on the pane exiting, and the shared-worktree check
	// reads sibling sessions that the soft-stop will start tearing down.
	var wt agentWorktreeView
	if deleteWorktree {
		wt = resolveRetireWorktree(convID)
	}

	// Shutdown after the demotion: the agent is already a plain
	// conversation by the time it processes /exit. Soft only — a
	// retired agent's pane should close gracefully, not be killed.
	if shutdown {
		resp["shutdown"] = stopOneConv(convID, false /* soft exit */)
	}

	// Worktree+branch cleanup runs only after the agent's process exits
	// (its cwd is the worktree). scheduleRetireWorktreeCleanup removes
	// inline when the agent is already offline, defers to a background
	// waiter when a /exit is in flight, and keeps the worktree when the
	// session is left running.
	if deleteWorktree {
		resp["worktree"] = scheduleRetireWorktreeCleanup(convID, wt, shutdown)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAgentPromote serves POST /v1/agent/{selector}/promote — turns a
// plain conversation into an agent. Also reinstates a retired conv
// (PromoteAgent always lands the conv active), so the dashboard's
// "promote" button works regardless of prior state.
func handleAgentPromote(w http.ResponseWriter, r *http.Request, convID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if _, ok := requireCrossAgentPermission(w, r, PermAgentPromote, convID); !ok {
		return
	}
	prior, err := db.PromoteAgent(convID, "promote")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id":     convID,
		"prior_state": prior,
		"state":       db.EnrollmentActive,
	})
}

// handleAgentReinstate serves POST /v1/agent/{selector}/reinstate —
// returns a retired agent to active status. Distinct from promote in
// that it only acts on a conv that is actually retired; a 409 makes a
// mistargeted reinstate obvious rather than silently succeeding.
func handleAgentReinstate(w http.ResponseWriter, r *http.Request, convID string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	if _, ok := requireCrossAgentPermission(w, r, PermAgentPromote, convID); !ok {
		return
	}
	did, err := db.ReinstateAgent(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if !did {
		state, _ := db.EnrollmentState(convID)
		writeError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("conv %s is not retired (enrollment: %s) — nothing to reinstate", short8(convID), state))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id": convID,
		"state":   db.EnrollmentActive,
	})
}

// dashboardEnrollmentVerb backs the Agents-tab per-row promote / retire
// / reinstate buttons (POST /api/agents/{conv}/{verb}). It resolves the
// selector and delegates to the matching /v1 handler with a synthetic
// human peer — the dashboard cookie + Origin pin is the consent layer,
// so no permission slug is required.
func dashboardEnrollmentVerb(w http.ResponseWriter, r *http.Request, selector, verb string) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	res, _, err := agent.ResolveSelector(selector)
	if err != nil {
		http.Error(w, "resolve agent: "+err.Error(), http.StatusNotFound)
		return
	}
	hr := asDashboardHumanPeer(r)
	switch verb {
	case "promote":
		handleAgentPromote(w, hr, res.ConvID)
	case "retire":
		handleAgentRetire(w, hr, res.ConvID)
	case "reinstate":
		handleAgentReinstate(w, hr, res.ConvID)
	default:
		http.Error(w, "unknown enrollment verb "+verb, http.StatusNotFound)
	}
}
