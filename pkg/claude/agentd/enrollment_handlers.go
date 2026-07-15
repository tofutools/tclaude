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
	CronDisabled int64    `json:"cron_disabled"`
	Retired      bool     `json:"retired"`
}

// retireAgentConv demotes convID from an agent to a plain
// conversation: it unjoins every group (owner rows included), revokes
// every permission and sudo grant in one transaction, then flips the
// enrollment bit. The conversation data — the .jsonl, the conv_index row,
// the worktree —
// is left completely untouched; this is the non-destructive half of
// cleanup. Safe on a conv that is already retired or was never an
// agent: the group/grant revokes are no-ops and Retired comes back
// false. Shared by the /v1 retire verb and the bulk cleanup endpoint.
//
// The second return value is the IDs of groups whose owner roster this
// retire touched — the bulk cleanup uses them for its ownerless-group
// warning; the /v1 handler ignores them.
func retireAgentConv(convID, by, reason string) (retireConvOutcome, []int64, error) {
	cronAuthorityMu.Lock()
	defer cronAuthorityMu.Unlock()
	var out retireConvOutcome
	retired, err := db.RetireAgentAuthorizationByConv(convID, by, reason)
	if err != nil {
		return out, nil, err
	}
	out.GroupsLeft = retired.GroupsLeft
	out.PermsRevoked = retired.PermsRevoked
	out.SudoRevoked = retired.SudoRevoked
	out.CronDisabled = retired.CronDisabled
	out.Retired = retired.Retired
	return out, retired.OwnerGroupIDs, nil
}

// isDanglingAgentEntry reports whether convID names a known agent
// enrollment (active or retired) that agent.ResolveSelector can no
// longer resolve — its conversation data is gone (no conv_index row,
// no group membership, no succession chain), yet the enrollment table
// still lists it as an agent. This is the "dangling agent" case: the
// dashboard roster surfaces the row (it walks db.ListActiveAgents),
// but retire — which resolves the selector first — used to dead-end on
// "no conversation matches" and the entry got stuck.
//
// Caller contract: only consult this AFTER ResolveSelector has already
// failed — a resolvable conv is never dangling. A "none" enrollment
// (nothing references convID as an agent) returns false, so callers
// never offer to "clean up" an arbitrary unknown conv-id.
func isDanglingAgentEntry(convID string) bool {
	state, err := db.AgentState(convID)
	if err != nil {
		return false
	}
	return state == db.AgentStateActive || state == db.AgentStateRetired
}

// writeDanglingAgentResponse signals a dangling agent entry to the
// caller: HTTP 409 with {dangling:true, conv_id, error}. The dashboard
// turns this into a "remove the dangling entry?" confirm that purges the
// orphan rows via the DELETE endpoint; the CLI prints guidance pointing
// at `tclaude agent delete`. Distinct from the generic 404 resolve
// error so callers can offer best-effort cleanup instead of dead-ending.
func writeDanglingAgentResponse(w http.ResponseWriter, convID string) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"dangling": true,
		"conv_id":  convID,
		"error": "no conversation data found for " + short8(convID) +
			" — dangling agent entry (remove it instead of retiring)",
	})
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
// Unlike shutdown, this defaults OFF for an absent param: a caller that
// omits it must never nuke a worktree by accident. Both retire surfaces
// that delete worktrees send the flag explicitly — the dashboard modal
// (after probing for a removable worktree) and the `tclaude agent retire`
// CLI (delete is its default; --no-delete-worktree sends =0). The OFF
// default is the failsafe for any other / future caller.
// ?delete_worktree=1 (or =true) opts in.
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
	// Gate on the LIVE generation, not just "active": retiring acts on the
	// actor, so accepting a superseded predecessor handle would demote the live
	// agent. Every normal caller already resolves to the current generation
	// (agent.ResolveSelector redirects a predecessor to the chain head); this is
	// the explicit guard for that invariant.
	live, err := db.IsLiveAgentConv(convID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	if !live {
		state, _ := db.AgentState(convID)
		writeError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("conv %s is not a live agent at this generation (state: %s) — nothing to retire", short8(convID), state))
		return
	}
	shutdown := retireShouldShutdown(r)
	deleteWorktree := retireShouldDeleteWorktree(r)

	// Resolve before demotion as well as before shutdown. Historical retired
	// session rows are not worktree claimants, so the safety view must be taken
	// while this target and every sibling still have their pre-retire state.
	var wt agentWorktreeView
	if deleteWorktree {
		wt = resolveRetireWorktree(convID)
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
		"state":       db.AgentStateActive,
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
		state, _ := db.AgentState(convID)
		writeError(w, http.StatusConflict, "conflict",
			fmt.Sprintf("conv %s is not retired (state: %s) — nothing to reinstate", short8(convID), state))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"conv_id": convID,
		"state":   db.AgentStateActive,
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
		// A retire whose target can't be resolved but looks like a raw
		// conv-id may be a DANGLING agent entry — an enrollment whose
		// conversation data is gone, so the resolver legitimately can't
		// find it. Rather than a dead-end 404 that leaves the entry
		// stuck on the roster, signal the dashboard so it can offer to
		// purge the orphan via DELETE (whose union cleanup is a no-op on
		// the missing conv but drops the leftover enrollment/group/perm
		// rows). promote/reinstate intentionally stay a 404 — there is
		// nothing to promote when the conversation is gone.
		if verb == "retire" && looksLikeConvID(selector) && isDanglingAgentEntry(selector) {
			writeDanglingAgentResponse(w, selector)
			return
		}
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
