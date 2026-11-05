package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/conv"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// handleAgentStop stops a single conv's tmux session. Sibling of
// the bulk groups.members.stop. Auth: agent.stop slug OR caller is owner of
// a group containing target. Routed via /v1/agent/{selector}/stop;
// `?force=1` kills the pane at once, with the signal ladder after it.
func handleAgentStop(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentStop, targetConv)
	if !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	action := db.AgentExitActionStop
	if force {
		action = db.AgentExitActionForceStop
	}
	res := stopOneConvWithIntent(targetConv, force, action, auditRequestEventID(r))
	resp := map[string]any{
		"conv_id":      res.ConvID,
		"action":       res.Action,
		"tmux_session": res.TmuxSes,
	}
	if res.Detail != "" {
		resp["detail"] = res.Detail
	}
	if caller != "" && caller != targetConv {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	status := http.StatusOK
	if res.Action == "error" {
		status = http.StatusInternalServerError
		setAuditDetail(r, res.Detail)
		// The standard {"error": ...} envelope rides along with the
		// lifecycle result fields so DaemonError.Msg carries the bounded
		// failure detail — without it the CLI can only print the bare
		// "agentd returned 500" status line.
		resp["error"] = "stop failed: " + res.Detail
		resp["code"] = "stop_failed"
	}
	writeJSON(w, status, resp)
}

// handleAgentDelete permanently removes an agent: every row in every
// agent / conv / session table that references the conv-id, plus the
// .jsonl file and the ~/.claude/session-env/<conv-id> token. Sibling
// of stop / resume but DESTRUCTIVE — there is no undo. Auth:
// agent.delete slug OR caller is owner of a group containing target.
// Default-grant policy explicitly excludes agent.delete (humans
// only, unless someone explicitly grants).
//
// Refuses when the target's tmux session is alive — the human must
// stop it first via `tclaude agent stop`. `?force=1` kills the tmux
// session inline before deleting (mirrors the stop endpoint's force
// switch). Refusing-by-default avoids racing the live agent's writes
// to its own .jsonl while we're tearing it down.
//
// Returns the per-table deletion counts so the human can see scope.
func handleAgentDelete(w http.ResponseWriter, r *http.Request, targetConv string) {
	if r.Method != http.MethodDelete {
		writeError(w, http.StatusMethodNotAllowed, "method", "DELETE only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentDelete, targetConv)
	if !ok {
		return
	}
	// Self-delete prevention. An agent shouldn't be able to wipe its
	// own conv mid-turn — the daemon's own request context is keyed
	// off the caller's conv-id, and the cleanup goroutine would race
	// the response write. Humans (caller == "") can always proceed.
	//
	// Match on the stable actor (JOH-323): DeleteAgentAllGenerations below
	// sweeps EVERY generation of the actor, so deleting any generation of
	// oneself wipes the live request conv too and hits the same race. The
	// selector already resolves a predecessor forward to the head, so today
	// targetConv == caller for a self-delete; sameActor only ever widens
	// this guard to the same actor's generations — a genuinely different
	// agent still differs and a peer/owner delete is unaffected.
	if caller != "" && sameActor(caller, targetConv) {
		writeError(w, http.StatusBadRequest, "invalid_arg",
			"cannot delete self via this endpoint; use `tclaude conv rm` from a human shell or have a peer/owner do it")
		return
	}
	force := r.URL.Query().Get("force") == "1"
	// ?force=1 goes on to purge immediately, so it must WAIT for the pane
	// process to actually be gone — the teardown below unlinks the .jsonl and
	// every row the agent might still be writing to. The non-force path
	// deliberately does NOT wait: its contract is to refuse (409) and let the
	// caller retry, so blocking it for the escalation window would buy nothing.
	var stopRes memberOpResult
	if force {
		var stopErr error
		stopRes, stopErr = stopBeforePurge(targetConv, auditRequestEventID(r))
		if stopErr != nil {
			// The ladder is exhausted and the agent is still up. Purging now
			// would leave a live orphan writing into rows and a .jsonl that no
			// longer exist.
			writeError(w, http.StatusConflict, "alive", stopErr.Error())
			return
		}
		// Deliberately NOT gated on stopRes.Action here. The force path is
		// soft-first now, and a soft exit whose injection failed reports
		// Action="error" and then falls through to the ladder — which routinely
		// goes on to kill the pane successfully. stopBeforePurge returning nil
		// IS the authority that the process is gone; failing the delete on the
		// injection's Action would 500 on an agent we just killed and leave its
		// rows behind.
	} else {
		stopRes = stopOneConv(targetConv, force)
		if stopRes.Action == "error" {
			writeError(w, http.StatusInternalServerError, "stop", stopRes.Detail)
			return
		}
	}
	// If the conv is alive but force wasn't passed, stopOneConv
	// returned `soft_stopped` (sent /exit) — the tmux pane may still
	// be in the process of dying. Refuse without ?force=1 to avoid
	// racing the live agent's writes during teardown.
	if !force && stopRes.Action == "soft_stopped" {
		writeError(w, http.StatusConflict, "alive",
			"target had a live tmux session; sent /exit. Re-run with ?force=1 to delete now, or wait for the pane to exit and retry.")
		return
	}

	if db.AgentConvAway(targetConv) {
		if _, err := db.RetireAgentAuthorizationByConv(targetConv, "human", "deleted while away"); err != nil {
			writeError(w, 500, "retire", err.Error())
			return
		}
		notifyAwayAgentTerminal(targetConv)
	}

	// Comprehensive cleanup: DB purge + filesystem + sync tombstone +
	// session-env. Single source of truth shared with the dashboard
	// `DELETE /api/agents/...` path and `tclaude conv rm`. Actor-aware
	// (JOH-26 PR3d): when targetConv is an agent's head generation, this
	// also sweeps every predecessor generation's rows + .jsonl, so a
	// multi-generation actor's delete leaves nothing orphaned. The selector
	// resolves a predecessor forward to the head before it reaches here, so
	// `targetConv` is the head in the agent-delete case.
	// Pages close, and saved ones go, while the agent ID still resolves
	// (before the purge).
	deletePagesForConv(targetConv)
	if _, err := removeAgentDirectoriesForConv(targetConv); err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"delete agent-owned directories: "+err.Error())
		return
	}
	if agentID, err := db.AgentIDForConv(targetConv); err != nil {
		writeError(w, 500, "managed_instructions", err.Error())
		return
	} else if err = db.RetireManagedInstructionLaunch(agentID); err != nil {
		writeError(w, 500, "managed_instructions", err.Error())
		return
	}
	counts, swept, err := conv.DeleteAgentAllGenerations(targetConv)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io",
			"delete failed: "+err.Error())
		return
	}

	resp := map[string]any{
		"conv_id":   targetConv,
		"action":    "deleted",
		"db_counts": counts,
	}
	// Surface the full generation set reaped when more than the named conv
	// went (a multi-generation actor) — otherwise it's just [targetConv].
	if len(swept) > 1 {
		resp["generations"] = swept
	}
	if caller != "" && caller != targetConv {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	if stopRes.Action != "skipped:already_offline" {
		resp["pre_stop"] = stopRes.Action
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleAgentResume resumes a single conv into a fresh detached
// tmux session. Sibling of the bulk groups.members.resume. Auth:
// agent.resume slug OR caller is owner of a group containing
// target. Routed via /v1/agent/{selector}/resume.
func handleAgentResume(w http.ResponseWriter, r *http.Request, targetConv string) {
	if refuseExternalStewardLaunch(w, targetConv) {
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method", "POST only")
		return
	}
	caller, ok := requireCrossAgentPermission(w, r, PermAgentResume, targetConv)
	if !ok {
		return
	}
	reconcileResumingSteward(targetConv)
	trustRoot := caller == "" || hasHumanApprovalContinuation(r, PermAgentResume, targetConv)
	sendKeys := r.URL.Query().Get("send_keys") == "1"
	if sendKeys && !trustRoot {
		if parseAskHumanHeader(r) <= 0 || !requestCodexDriveRollbackApproval(w, r, PermAgentResume, targetConv, targetConv) {
			if parseAskHumanHeader(r) <= 0 {
				writeError(w, http.StatusForbidden, "codex_drive_change_restricted",
					"moving a Codex agent from its selected app-server drive to send-keys requires a direct human resume or actual --ask-human approval")
			}
			return
		}
		trustRoot = true
	}
	// Recreating a missing path is a daemon-side mkdir with the human's
	// filesystem authority. Keep that opt-in human-only; an agent cannot prove
	// write access inside a directory that does not exist.
	recreate := r.URL.Query().Get("recreate") == "1"
	if recreate && !trustRoot {
		if parseAskHumanHeader(r) <= 0 || !requestResumeDirectoryRecreationApproval(w, r, PermAgentResume, targetConv, targetConv) {
			if parseAskHumanHeader(r) <= 0 {
				writeError(w, http.StatusForbidden, "recreate_dir_restricted",
					"agent-initiated resume may not recreate a missing launch directory without actual human approval; ask the human to run resume --recreate-dir or retry with --ask-human")
			}
			return
		}
	}
	// ?recreate=1 opts into recreating a deleted launch dir empty before the
	// relaunch (the CLI's `--recreate-dir`, the dashboard's confirm-and-retry).
	// Absent it, a vanished cwd comes back as `error:missing_cwd` so the caller
	// can decide.
	var res memberOpResult
	if sendKeys {
		res = resumeOneConvWithCodexRollbackLocked(targetConv, recreate)
	} else {
		res = resumeOneConvLocked(targetConv, recreate)
	}
	resp := map[string]any{
		"conv_id": res.ConvID,
		"action":  res.Action,
	}
	if res.Detail != "" {
		resp["detail"] = res.Detail
	}
	if len(res.Warnings) > 0 {
		resp["warnings"] = res.Warnings
	}
	if len(res.Changes) > 0 {
		resp["changes"] = res.Changes
	}
	if caller != "" && caller != targetConv {
		resp["caller_conv"] = caller
		stampCallerAgentID(resp, caller)
	}
	writeJSON(w, http.StatusOK, resp)
}

// requestResumeDirectoryRecreationApproval creates a real, audited access
// request before an agent-triggered resume creates a missing directory with
// the daemon user's filesystem authority.
func requestResumeDirectoryRecreationApproval(w http.ResponseWriter, r *http.Request, perm, authTarget, targetConv string) bool {
	return requestTargetActionApproval(w, r, perm, authTarget, targetConv, targetActionApproval{
		what:    "launch-directory recreation",
		doing:   "recreating the launch directory",
		preview: "Recreate the stopped target's missing launch directory for this resume.",
		label:   "Resume launch-directory recreation",
	})
}

func requestCodexDriveRollbackApproval(w http.ResponseWriter, r *http.Request, perm, authTarget, targetConv string) bool {
	return requestTargetActionApproval(w, r, perm, authTarget, targetConv, targetActionApproval{
		what:    "Codex drive rollback",
		doing:   "approving Codex drive rollback",
		preview: "Durably change the stopped target from the Codex app-server drive to tmux send-keys, tear down its recorded app-server runtime, and resume it. Future relaunches will remain on send-keys until explicitly changed again.",
		label:   "Codex drive rollback (app-server → send-keys)",
	})
}

// targetActionApproval describes an extra-risk action on a target agent that
// an agent caller may ask the human to approve.
type targetActionApproval struct {
	what    string // "<what> cannot be approved" without a popup URL
	doing   string // "... after <timeout> while <doing> for target <conv>"
	preview string
	label   string
}

// requestTargetActionApproval asks the human, through an audited popup, to
// approve action a on targetConv for an agent caller. It answers false
// without writing when the caller did not ask for a popup or is not an
// agent, and 403 when no popup can be shown or the human declines or the
// popup times out. Approval marks the continuation for perm on authTarget.
func requestTargetActionApproval(w http.ResponseWriter, r *http.Request, perm, authTarget, targetConv string, a targetActionApproval) bool {
	timeout := parseAskHumanHeader(r)
	if timeout <= 0 {
		return false
	}
	if popupBaseURL == "" {
		writeError(w, http.StatusForbidden, "permission",
			"no popup base URL configured; "+a.what+" cannot be approved")
		return false
	}
	p := peerFromContext(r.Context())
	if classify(p) != classAgent {
		return false
	}
	callerTitle := convDisplayTitle(p.ConvID)
	targetTitle := convDisplayTitle(targetConv)
	slot, ok := admitApproval(w, p.ConvID)
	if !ok {
		return false
	}
	defer approvals.release(slot)
	targetGroup, _, _ := extractApprovalTargets(r, "")
	req := newAgentApprovalRequest(r, perm, p.ConvID, callerTitle, timeout)
	req.bodyPreview, req.bodyLabel = a.preview, a.label
	req.targetGroup, req.targetConvID, req.targetConvTitle = targetGroup, targetConv, targetTitle
	if requestHumanApproval(req, popupBaseURL) {
		markHumanApprovalContinuation(r, perm, authTarget)
		return true
	}
	writeError(w, http.StatusForbidden, "permission",
		fmt.Sprintf("human declined or timed out after %s while %s for target %s",
			timeout, a.doing, short8(targetConv)))
	return false
}

// pickAliveSession returns the most-recent session row for convID
// whose tmux session is still alive. Same selector as queued mail delivery.
func pickAliveSession(convID string) *db.SessionRow {
	alive, _ := pickAliveSessionStrict(convID)
	return alive
}

// pickAliveSessionStrict is pickAliveSession for a caller that must not read a
// failed lookup as "stopped": the lookup error is returned.
func pickAliveSessionStrict(convID string) (*db.SessionRow, error) {
	candidates, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return nil, err
	}
	for _, c := range candidates {
		if c.TmuxSession != "" && session.IsTmuxSessionAlive(c.TmuxSession) {
			return c, nil
		}
	}
	return nil, nil
}

// armRemoteControlOnNewRow tags a freshly-relaunched session row's best-known
// remote-control state ON, out-of-band (db.SetSessionRemoteControl) — the same
// discipline executeSpawn uses after a --remote-control spawn (JOH-258): a
// targeted UPDATE the hook callback's SaveSession UPSERT never writes, so a
// status tick can't clobber it. label is the NEW row's tclaude session id; the
// --remote-control launch flag already armed Claude Code's Remote Access, so a
// write failure here is only a best-known-state drift the human can re-toggle —
// logged, never fatal, never a broken relaunch. See JOH-261.
func armRemoteControlOnNewRow(label string) {
	if err := db.SetSessionRemoteControl(label, true); err != nil {
		slog.Warn("relaunch: failed to arm remote-control on new session row",
			"label", label, "error", err)
	}
}

// armRemoteControlAfterResume waits for a resumed pane's FRESH session row to
// come online, then tags its best-known remote-control state ON. Resume mints a
// new session row (new label) for the SAME conv-id, so its remote_control
// defaults to 0 even when the source was armed; without this re-tag the
// dashboard indicator + the toggle's direction logic would read OFF after every
// resume, even though the --remote-control launch flag already re-armed CC's
// Remote Access.
//
// Unlike reincarnate / clone — whose handlers already poll for the new row
// synchronously, so they tag inline — resume is fire-and-forget with no known
// label, so this runs in the background (goBackground) and the bulk
// groups-resume loop is never serialised on each member's online-wait.
//
// pickAliveSession is unambiguous here: resumeOneConv only relaunches a conv
// that is OFFLINE (it gates on !isConvOnline), so the resumed pane is the only
// ALIVE row for the conv-id — the dead predecessor row is skipped. See JOH-261.
func armRemoteControlAfterResume(convID string) {
	deadline := time.Now().Add(reincarnateSpawnTimeout)
	for time.Now().Before(deadline) {
		if s := pickAliveSession(convID); s != nil {
			armRemoteControlOnNewRow(s.ID)
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	slog.Warn("resume: remote-control re-arm timed out; resumed pane never came online",
		"conv", convID)
}
