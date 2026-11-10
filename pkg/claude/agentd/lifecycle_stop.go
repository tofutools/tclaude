package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

const daemonSoftExitReason = "soft_exit"

// handleGroupStop ends every member's running tmux session.
//
// Modes:
//   - soft (default): inject `/exit` via tmux send-keys, mirroring the
//     /rename pattern. Lets CC clean up its own state. The actual tmux
//     session usually goes away on CC's next iteration.
//   - force (?force=1): tmux kill-session -t <name>. Last resort —
//     drops any unsubmitted input the agent hadn't sent yet.
//
// Members that aren't currently online are reported as
// `skipped:already_offline` and skipped — stop is idempotent.
func handleGroupStop(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", err.Error())
		return
	}
	affected := make([]string, 0, len(members))
	selected := make(map[string]bool, len(members))
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "io", "snapshot live group members: "+err.Error())
		return
	}
	for _, member := range members {
		online, _ := convLiveStatus(member.ConvID, alive)
		if online {
			affected = append(affected, member.ConvID)
			selected[member.ConvID] = true
		}
	}
	if _, ok := requireGroupPermission(w, r, PermGroupsMembersStop, g,
		ActionContext{affectedConvs: affected}); !ok {
		return
	}
	force := r.URL.Query().Get("force") == "1"
	action := db.AgentExitActionStop
	if force {
		action = db.AgentExitActionForceStop
	}
	requestEventID := auditRequestEventID(r)
	// Every member is stopped CONCURRENTLY and each stop WAITS for its pane
	// process to actually die (the full ladder: the exit command plus its
	// double-tap re-injections, then kill-pane, SIGTERM, SIGKILL). Sequentially
	// that would be the sum of every member's exit latency — a group of ten
	// agents that each take their time closing would hold the request for a
	// minute-plus and report "soft_stopped" for panes still running. Bounded
	// fan-out makes the wall-clock the SLOWEST member instead of the sum, which
	// is what makes waiting affordable in the first place.
	out := groupOpResp{Group: g.Name, Action: "stop", Members: []memberOpResult{}}
	out.Members = mapAgentsConcurrently(members, batchAgentOpConcurrency,
		func(_ int, m *db.AgentGroupMember) (memberOpResult, bool) {
			if m.ConvID != "" && !selected[m.ConvID] {
				return memberOpResult{ConvID: m.ConvID, AgentID: peerAgentID(m.ConvID),
					Title: agent.FreshTitle(m.ConvID), Action: "skipped:already_offline"}, true
			}
			res, _ := stopOneConvAndWait(m.ConvID, force, action, requestEventID, 0)
			res.AgentID = peerAgentID(m.ConvID)
			res.Title = agent.FreshTitle(m.ConvID)
			return res, true
		})
	if out.Members == nil {
		out.Members = []memberOpResult{}
	}
	writeJSON(w, http.StatusOK, out)
}

// stopOneConv soft-stops (or force-kills with `force=true`) the live
// tmux session for convID. Returns the per-conv result. Shared between
// the bulk groups.members.stop loop and the single-conv agent.stop endpoint.
//
// Result shape mirrors the existing memberOpResult so the bulk
// summary table renders the same regardless of how the call was
// initiated. Idempotent: convs already offline come back as
// `skipped:already_offline`.
func stopOneConv(convID string, force bool) memberOpResult {
	action := db.AgentExitActionStop
	if force {
		action = db.AgentExitActionForceStop
	}
	return stopOneConvWithIntent(convID, force, action, "")
}

// stopOneConvWithIntent adds audit-only lifecycle attribution to a stop. The
// intent is armed immediately before the tmux mutation and cleared whenever
// that mutation fails. Persistence is deliberately best-effort: an audit I/O
// failure is logged but never changes the established stop result.
func stopOneConvWithIntent(convID string, force bool, lifecycleAction, relatedEventID string) memberOpResult {
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	return stopOneConvWithIntentUnderLaunchLock(convID, force, lifecycleAction, relatedEventID)
}

// stopWaitPolicy says whether a stop returns as soon as the exit command is
// delivered (the default, stopNoWait) or stays until the pane process is
// actually gone.
//
// A delivered /exit is not a stopped agent. Every caller that goes on to touch
// what the agent still holds — retire deleting its agent-owned directories or
// its worktree, a bulk stop reporting to the human what it achieved, a restart
// about to relaunch into the same identity — needs the second contract, or it
// races the pane it just asked to close. Waiting is also what makes a bulk
// stop's answer true: without it the response says "soft_stopped" for agents
// that are still very much running.
//
// deadline is how long the delivered exit (and its bounded double-tap
// re-injections) has to work before the kill ladder starts. It is taken
// literally, zero included: a caller that wants "no grace, escalate now"
// (the power buttons' grace_ms=0) gets exactly that.
type stopWaitPolicy struct {
	wait     bool
	deadline time.Duration
}

// stopNoWait is the fire-and-forget contract: deliver the exit, arm the
// out-of-band escalation (scheduleSoftExitEscalation), return. The pane still
// dies — the same ladder runs, just in the background — the caller simply does
// not learn when.
//
// It is correct ONLY where "stopped" is the entire request and nothing
// downstream depends on the process being gone. That is exactly three callers
// today, and the list should stay short:
//
//   - POST /v1/agent/{selector}/stop      — the CLI's `tclaude agent stop`
//   - POST /api/agents/{id}/stop          — without {"wait":true}; the shutdown
//     dialog opts INTO waiting so its spinner spans the real stop (OpenCode's
//     control-API exit dispatch returns in milliseconds), and its retry /
//     force-kill affordances give a stuck ladder somewhere to land
//   - the non-force agent delete, which deliberately refuses with 409 and asks
//     the caller to retry rather than waiting out the escalation window
//
// The CLI stop would otherwise hold a human's request for the length of the
// escalation window to report something the human can already see in the pane.
// Anything that goes on to touch what the agent holds — a retire deleting its
// directories or worktree, a delete unlinking its .jsonl, a restart relaunching
// into the same identity, a bulk op reporting what it achieved — must use
// stopWaitForExit / stopOneConvAndWait instead.
var stopNoWait = stopWaitPolicy{}

// stopWaitForExit blocks the stop until the pane process is gone or the kill
// ladder is exhausted. deadline 0 → softExitEscalationDeadline.
func stopWaitForExit(deadline time.Duration) stopWaitPolicy {
	return stopWaitPolicy{wait: true, deadline: deadline}
}

// stopOneConvWithIntentUnderLaunchLock is stopOneConvWithIntent after the
// caller has acquired the conversation launch lock. Compound lifecycle
// operations use it to keep stop → posture mutation → resume indivisible from
// other daemon wake/stop requests.
func stopOneConvWithIntentUnderLaunchLock(convID string, force bool, lifecycleAction, relatedEventID string) memberOpResult {
	res, _ := stopOneConvUnderLaunchLock(convID, force, lifecycleAction, relatedEventID, stopNoWait)
	return res
}

// stopOneConvAndWait soft-stops convID and does not return until its pane
// process has actually gone away — the full ladder run inline: the delivered
// exit command plus its bounded re-injections get `deadline` to work, then
// kill-pane, SIGTERM and SIGKILL each get their turn.
//
// This is the primitive for every caller that must not proceed while the agent
// is still running: the retire tiers (which then delete the agent's
// directories and worktree) and the bulk stop surfaces. The returned
// softExitOutcome says whether it closed on its own, needed the ladder, or is
// STILL alive — the last of which a caller must treat as "the agent never
// released anything".
//
// deadline 0 means "the standard window" (softExitEscalationDeadline); a
// caller that needs an explicit zero-grace stop drives
// stopOneConvUnderLaunchLock with stopWaitForExit(0) itself.
func stopOneConvAndWait(convID string, force bool, lifecycleAction, relatedEventID string, deadline time.Duration) (memberOpResult, softExitOutcome) {
	if deadline <= 0 {
		deadline = softExitEscalationDeadline
	}
	launchLock := resumeLaunchLock(convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	return stopOneConvUnderLaunchLock(convID, force, lifecycleAction, relatedEventID, stopWaitForExit(deadline))
}

// errAgentStillRunning reports that a stop ran the whole escalation ladder (or
// could not act on the pane at all) and the process is STILL alive.
var errAgentStillRunning = errors.New("agent is still running after the full stop escalation")

// stopBeforePurge is the stop every DESTRUCTIVE path must use before it unlinks
// what the agent holds — its agent-owned directories, its worktree, its .jsonl,
// its rows.
//
// Retire does not need this: its cleanups defer themselves until the pane is
// observed offline and keep whatever they cannot safely remove. A purge has no
// such luxury — it acts immediately and cannot be undone — so it needs the
// strong contract: soft exit first (with the double-tap re-injections), then
// kill-pane, SIGTERM, SIGKILL, and a definite answer about whether the process
// is gone.
//
// A softExitStuck or softExitUnattempted outcome is a REFUSAL, not a warning.
// Deleting a running agent's files leaves a live orphan writing into paths that
// no longer exist and rows that no longer exist — strictly worse than the
// delete not happening. Callers surface the error; the human retries or
// investigates. An already-offline conv returns immediately with no error,
// which is the overwhelmingly common case for a delete.
func stopBeforePurge(convID, relatedEventID string) (memberOpResult, error) {
	if db.AgentConvAway(convID) {
		if _, err := db.RetireAgentAuthorizationByConv(convID, "human", "deleted while away"); err != nil {
			return memberOpResult{}, err
		}
		notifyAwayAgentTerminal(convID)
	}
	res, outcome := stopOneConvAndWait(
		convID, false /* soft exit first */, db.AgentExitActionForceStop, relatedEventID, 0)
	switch outcome {
	case softExitStuck, softExitUnattempted:
		detail := res.Detail
		if detail == "" {
			detail = res.Action
		}
		return res, fmt.Errorf("%w: %s", errAgentStillRunning, detail)
	}
	return res, nil
}

// stopOneConvUnderLaunchLock is the one stop implementation both contracts go
// through; waitPolicy picks which. The softExitOutcome is only meaningful when
// waitPolicy.wait is set (it is softExitClosed otherwise — nothing was
// waited for, so nothing is known).
func stopOneConvUnderLaunchLock(convID string, force bool, lifecycleAction, relatedEventID string, waitPolicy stopWaitPolicy) (memberOpResult, softExitOutcome) {
	defer db.NotifyStatusChanged()
	recoveryReason := lifecycleAction
	if recoveryReason == "" {
		recoveryReason = db.AgentExitActionStop
	}
	if cancelled, err := db.CancelAgentRecoveryForConv(convID, recoveryReason, time.Now()); err != nil {
		slog.Warn("stop: cancel automatic recovery failed", "conv", short8(convID), "error", err)
	} else if cancelled {
		if recovery, _ := db.AgentRecoveryForConv(convID); recovery != nil {
			_ = db.RecordAgentRecoveryAudit(*recovery, db.AuditVerbAgentRecoveryCancelled, recoveryReason, time.Now())
		}
	}
	res := memberOpResult{ConvID: convID}
	sess := pickAliveSession(convID)
	if sess == nil {
		res.Action = "skipped:already_offline"
		return res, softExitClosed
	}
	res.TmuxSes = sess.TmuxSession
	target, targetErr := captureLifecycleTarget(sess)
	if targetErr != nil {
		logLifecycleStopFailure("capture", sess.TmuxSession, sess.ID, targetErr)
		res.Action = "error"
		res.Detail = "capture selected pane: " + targetErr.Error()
		return res, softExitUnattempted
	}
	if force {
		intentSet := setExitIntentTargetBestEffort(target, lifecycleAction, relatedEventID)
		if lifecycleAction != "" && intentSet == nil {
			res.Action, res.Detail = "error", "selected launch intent became stale"
			return res, softExitUnattempted
		}
		if err := killLifecycleTarget(target); err != nil {
			clearFailedExitIntent(intentSet)
			res.Action = "error"
			res.Detail = "kill-session: " + err.Error()
		} else {
			res.Action = "killed"
		}
		// A successful kill-pane is tmux letting go of the pane, not proof the
		// harness process died — it can be wedged in uninterruptible work or
		// held open by a child. The rest of the ladder follows while tmux still
		// lists the pane, so a force stop reaches as far as a soft one: a
		// waiting caller gets it inline, anyone else out-of-band.
		if !waitPolicy.wait {
			scheduleForceStopEscalation(target, lifecycleAction, relatedEventID)
		}
		return res, finishStopWait(target, waitPolicy, lifecycleAction, relatedEventID, "force-stop", daemonEscalatedKillReason, &res)
	}
	// Soft stop: inject the harness's exit command (CC's `/exit`). The
	// harness closes the conversation cleanly and the tmux session goes
	// away when it exits. The command is sourced from the harness's
	// Lifecycle so a non-CC pane is never typed `/exit` if that's not its
	// exit command.
	//
	// An API-connected Copilot agent stays on this path, unlike its rename,
	// compaction and message delivery (TCL-1058). The RPCs that look like the
	// answer are not one, measured against Copilot CLI 1.0.78: `session.
	// shutdown` and `sessions.close` both succeed and leave the copilot process
	// running with its session still foregrounded, and `runtime.shutdown`
	// refuses with "Runtime shutdown is not available for this server". They end
	// a session, not the CLI. Routing soft exit through them would report a
	// delivered exit for a pane that never dies — which every stop, retire and
	// dashboard surface then reads as an agent that finished its work. Ending
	// the CLI through the pane really does work, so the pane path stays —
	// though for Copilot the pane input is ctrl-c presses rather than a typed
	// /exit (see sendSoftExitToTarget).
	h := harnessForConv(convID)
	if h.SupportsSoftExit() {
		exitCmd := h.Life.SoftExitCommand()
		fallbackExitReason := ""
		piDrive := false
		if h.Name == harness.PiName {
			selected, err := piDriveSelected(convID)
			if err != nil {
				res.Action, res.Detail = "error", "pi drive posture unreadable: "+err.Error()
				return res, softExitUnattempted
			}
			piDrive = selected
		}
		if h.Name == harness.OpenCodeName && openCodeControlInputBlocked(sess.Status) {
			res.Action = "error"
			res.Detail = "OpenCode TUI is " + sess.Status + "; retry soft stop when idle or force kill"
			return res, softExitUnattempted
		}
		intentSet := setExitIntentTargetBestEffort(target, lifecycleAction, relatedEventID)
		if lifecycleAction != "" && intentSet == nil {
			res.Action, res.Detail = "error", "selected launch intent became stale"
			return res, softExitUnattempted
		}
		delivered := false
		if h.Name == harness.OpenCodeName {
			delivered = injectAPISoftExitTarget(target, "soft-exit", intentSet, openCodeSoftExit)
		} else if piDrive {
			delivered = injectAPISoftExitTarget(target, "soft-exit", intentSet, piDriveSoftExit)
		} else {
			delivered = injectSoftExitTarget(target, exitCmd, h.Life.SoftExitPrefixKeys(), "soft-exit", intentSet)
		}
		if delivered {
			fallbackExitReason = daemonSoftExitReason
			if h.Name == harness.CodexName {
				// Codex has no SessionEnd hook; record daemon-owned /quit
				// separately from an unclassified user pane close.
				if err := db.SetSessionExitReason(sess.ID, daemonSoftExitReason); err != nil {
					slog.Warn("failed to record daemon soft-exit reason",
						"session", sess.ID, "conv", convID, "error", err)
				}
			}
			res.Action = "soft_stopped"
		} else {
			clearFailedExitIntent(intentSet)
			res.Action = "error"
			switch {
			case h.Name == harness.OpenCodeName:
				res.Detail = "managed OpenCode TUI exit dispatch failed"
			case piDrive:
				// Channel-only: a stop never falls back to keystrokes.
				res.Detail = "pi drive exit dispatch failed (is the extension connected?)"
			case len(h.SignalExitKeys()) > 0:
				// Every keystroke-free harness (Copilot, Claude Code, Codex)
				// reports the signal path it actually took, not a typed
				// exitCmd it never sent.
				res.Detail = "managed " + h.Name + " signal exit dispatch failed"
			default:
				res.Detail = "send-keys " + exitCmd + " failed"
			}
		}
		// Whether the exit was delivered or the injection itself failed, the
		// pane must end: this is a stop. Nothing stronger used to follow a soft
		// exit that never took, which is how a retired agent's pane outlived
		// its own directory-cleanup grace (TCL-1001). The ladder converges well
		// inside that grace.
		if stopIntendsPaneClosure(lifecycleAction) {
			if waitPolicy.wait {
				return res, finishStopWait(target, waitPolicy, lifecycleAction, relatedEventID, "soft-exit", fallbackExitReason, &res)
			}
			scheduleSoftExitEscalation(target, lifecycleAction, relatedEventID, "soft-exit", fallbackExitReason)
		} else if waitPolicy.wait {
			// The caller asked to wait, but this action does not entitle us to
			// kill (stopIntendsPaneClosure is what gates that). Wait for the pane
			// anyway — silently returning "closed" without a single probe would
			// hand the caller a guarantee nothing checked — just never escalate.
			if waitForLifecycleTargetGone(target, waitPolicy.deadline) {
				// Verified gone: the retry watchdog has nothing left to do.
				// (A pane still alive keeps it armed — this stop may not
				// escalate, but the re-injections are still meaningful.)
				target.markSoftExitSettled()
				return res, softExitClosed
			}
			res.Detail = joinDetail(res.Detail, "pane still alive; no escalation for this stop")
			return res, softExitStuck
		}
		return res, softExitClosed
	}
	// No soft-exit command for this harness → hard kill so the pane never
	// lingers because we couldn't type a graceful exit.
	intentSet := setExitIntentTargetBestEffort(target, lifecycleAction, relatedEventID)
	if lifecycleAction != "" && intentSet == nil {
		res.Action, res.Detail = "error", "selected launch intent became stale"
		return res, softExitUnattempted
	}
	if err := killLifecycleTarget(target); err != nil {
		clearFailedExitIntent(intentSet)
		res.Action = "error"
		res.Detail = "kill-session (harness has no soft-exit): " + err.Error()
	} else {
		res.Action = "killed_no_soft_exit"
	}
	return res, finishStopWait(target, waitPolicy, lifecycleAction, relatedEventID, "no-soft-exit", daemonEscalatedKillReason, &res)
}

// finishStopWait runs the inline half of a waiting stop and annotates the
// result with what the wait cost. A non-waiting policy is a no-op, so every
// stop branch can end with the same call.
//
// The Detail notes matter to the operator: a bulk retire whose members all
// came back "escalated to kill" is a harness that stopped honouring its own
// exit command, and a "still alive" member is one whose directories and
// worktree were deliberately left in place.
func finishStopWait(target *lifecycleTarget, waitPolicy stopWaitPolicy, lifecycleAction, relatedEventID, reason, fallbackExitReason string, res *memberOpResult) softExitOutcome {
	defer db.NotifyStatusChanged()
	if !waitPolicy.wait {
		return softExitClosed
	}
	deadline := waitPolicy.deadline
	if reason != "soft-exit" {
		// The kill already happened; only the signal ladder is left, so there
		// is no delivered exit command to wait out first.
		deadline = softExitEscalationSignalGrace
	}
	outcome := awaitLifecycleTargetExit(target, deadline, lifecycleAction, relatedEventID, reason)
	// Whatever the outcome, the stop is resolved: the pane exit was verified,
	// or the ladder ran to its end. Release the background retry watchdog now
	// rather than letting it sleep out its delay to rediscover this.
	target.markSoftExitSettled()
	switch outcome {
	case softExitClosed:
		if err := reconcileStoppedLifecycleTarget(target, lifecycleAction, relatedEventID, fallbackExitReason); err != nil {
			res.Action = "error"
			res.Detail = joinDetail(res.Detail, "session stopped but recording exited state failed: "+err.Error())
		}
	case softExitEscalated:
		if err := reconcileStoppedLifecycleTarget(target, lifecycleAction, relatedEventID, daemonEscalatedKillReason); err != nil {
			res.Action = "error"
			res.Detail = joinDetail(res.Detail, "session stopped but recording exited state failed: "+err.Error())
		}
		if reason == "soft-exit" {
			res.Detail = joinDetail(res.Detail, "pane did not exit; escalated to kill")
		}
	case softExitStuck:
		res.Detail = joinDetail(res.Detail, "pane process still alive after kill escalation")
	}
	return outcome
}

type lifecycleTarget struct {
	sessionID           string
	convID              string
	tmuxSession         string
	generation          string
	paneID              string
	panePID             int
	paneGenerationBound bool

	// softExitSettled closes (via markSoftExitSettled) once a stop path has
	// verified this target's pane is gone or run the escalation ladder to its
	// end. The background soft-exit retry watchdog selects on it between
	// attempts so it stands down the moment the stop resolves, instead of
	// sleeping out its full delay only to rediscover a session that is long
	// gone. Never closed directly — always through markSoftExitSettled.
	softExitSettled     chan struct{}
	softExitSettledOnce sync.Once
}

// markSoftExitSettled tells this target's background soft-exit retry watchdog
// that the stop already resolved — the pane's exit was verified, or the
// escalation ladder ran to its end (after which re-typing an exit command is
// pointless either way). Idempotent, and safe when no watchdog was ever
// scheduled.
func (t *lifecycleTarget) markSoftExitSettled() {
	if t == nil || t.softExitSettled == nil {
		return
	}
	t.softExitSettledOnce.Do(func() { close(t.softExitSettled) })
}

type paneProbeState int

const (
	paneProbeLive paneProbeState = iota
	paneProbeDead
	paneProbeUnknown
)

func (s paneProbeState) String() string {
	switch s {
	case paneProbeLive:
		return "live"
	case paneProbeDead:
		return "dead"
	case paneProbeUnknown:
		return "unknown"
	}
	return fmt.Sprintf("paneProbeState(%d)", int(s))
}

type lifecyclePaneProbe struct {
	state      paneProbeState
	paneID     string
	panePID    int
	generation string
}

func captureLifecycleTarget(sess *db.SessionRow) (*lifecycleTarget, error) {
	identity, err := db.GetSessionExitLaunchIdentity(sess.ID)
	if err != nil {
		return nil, err
	}
	p, err := probeLifecyclePane(sess.TmuxSession)
	if err != nil {
		return nil, err
	}
	if p.state != paneProbeLive {
		return nil, fmt.Errorf("pane is not live")
	}
	if identity.Generation != "" && p.generation != "" && p.generation != identity.Generation {
		return nil, fmt.Errorf("pane generation mismatch")
	}
	return &lifecycleTarget{sessionID: sess.ID, convID: sess.ConvID, tmuxSession: sess.TmuxSession, generation: identity.Generation, paneID: p.paneID, panePID: p.panePID, paneGenerationBound: p.generation != "", softExitSettled: make(chan struct{})}, nil
}

// probeLifecyclePane reads the pane's identity and liveness in one tmux call.
//
// Bounded by tmuxCommandTimeout, like every other tmux subprocess the daemon
// runs on a latency-sensitive path. It matters more here than most: a stop that
// WAITS calls this in a loop from inside an HTTP handler while holding the
// conversation's launch lock, so a tmux client that connects and never returns
// would park the request forever and block every later wake/stop/retire for
// that conv. The loop's own deadline bounds the polling, not the subprocess
// inside it — only this does that.
func probeLifecyclePane(tmuxSession string) (lifecyclePaneProbe, error) {
	format := "#{session_name}|#{pane_id}|#{pane_pid}|#{pane_dead}|#{pane_dead_status}|#{pane_dead_signal}|#{@tclaude_exit_generation}"
	out, err := tmuxOutputWithTimeout(
		"display-message", "-p", "-t", clcommon.ExactTarget(tmuxSession)+":", format)
	if err != nil {
		if strings.Contains(err.Error(), "can't find session") {
			err = fmt.Errorf("tmux session %q %w (%v)", tmuxSession, errTmuxSessionNotFound, err)
		}
		return lifecyclePaneProbe{state: paneProbeUnknown}, err
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) == 7 && parts[0] == "" && parts[1] == "" {
		// tmux 3.7 no longer fails display-message for a target that does
		// not exist: it exits 0 and expands every field empty. Older tmux
		// errored with "can't find session" (handled above). Both report
		// errTmuxSessionNotFound, so a caller can tell "gone" from a
		// garbled probe whatever the tmux version.
		return lifecyclePaneProbe{state: paneProbeUnknown}, fmt.Errorf("tmux session %q %w", tmuxSession, errTmuxSessionNotFound)
	}
	if len(parts) != 7 || parts[0] != tmuxSession || !validLifecyclePaneID(parts[1]) {
		return lifecyclePaneProbe{state: paneProbeUnknown}, fmt.Errorf("malformed pane probe")
	}
	pid, pidErr := strconv.Atoi(parts[2])
	if pidErr != nil || pid <= 0 {
		return lifecyclePaneProbe{state: paneProbeUnknown}, fmt.Errorf("malformed pane pid")
	}
	if parts[3] == "1" {
		return lifecyclePaneProbe{state: paneProbeDead, paneID: parts[1], panePID: pid, generation: parts[6]}, nil
	}
	return lifecyclePaneProbe{state: paneProbeLive, paneID: parts[1], panePID: pid, generation: parts[6]}, nil
}

// errTmuxSessionNotFound is what probeLifecyclePane reports when tmux does
// not know the session at all.
var errTmuxSessionNotFound = errors.New("not found")

func validLifecyclePaneID(v string) bool { return strings.HasPrefix(v, "%") && len(v) > 1 }

func (t *lifecycleTarget) revalidate() (lifecyclePaneProbe, error) {
	p, err := probeLifecyclePane(t.tmuxSession)
	if err != nil {
		return p, err
	}
	if p.state != paneProbeLive || !lifecycleProbeMatchesTarget(p, t) {
		return p, fmt.Errorf("selected pane identity changed")
	}
	return p, nil
}

func killLifecycleTarget(t *lifecycleTarget) error {
	if beforeSoftExitTargetRevalidateForTest != nil {
		beforeSoftExitTargetRevalidateForTest()
	}
	if _, err := t.revalidate(); err != nil {
		logLifecycleStopFailure("revalidate", t.paneID, t.sessionID, err)
		return err
	}
	// Bounded: the escalation ladder runs this from inside a waiting stop's
	// request goroutine, under the launch lock.
	if err := runTmuxCommand("kill-pane", "-t", t.paneID); err != nil {
		logLifecycleStopFailure("kill", t.paneID, t.sessionID, err)
		return err
	}
	return nil
}

func logLifecycleStopFailure(action, target, session string, err error) {
	slog.Warn("stop: managed lifecycle action failed",
		"target", auditClip(target, 120),
		"session", auditClip(session, 120),
		"action", auditClip(action, 32),
		"error", auditClip(err.Error(), 240))
}

func setExitIntentBestEffort(sess *db.SessionRow, action, relatedEventID string) *db.SessionExitIntentRef {
	if sess == nil || action == "" {
		return nil
	}
	ref, err := db.SetSessionExitIntent(sess.ID, action, relatedEventID, time.Now())
	if err != nil {
		slog.Warn("exit audit: record lifecycle intent failed",
			"session", sess.ID, "action", action, "error", err)
		return nil
	}
	if cancelled, cancelErr := db.CancelAgentRecoveryForConv(sess.ConvID, action, time.Now()); cancelErr != nil {
		slog.Warn("lifecycle: cancel automatic recovery failed", "conv", short8(sess.ConvID), "action", action, "error", cancelErr)
	} else if cancelled {
		if recovery, _ := db.AgentRecoveryForConv(sess.ConvID); recovery != nil {
			_ = db.RecordAgentRecoveryAudit(*recovery, db.AuditVerbAgentRecoveryCancelled, action, time.Now())
		}
	}
	return &ref
}

func setExitIntentTargetBestEffort(target *lifecycleTarget, action, relatedEventID string) *db.SessionExitIntentRef {
	if target == nil || action == "" {
		return nil
	}
	ref, err := db.SetSessionExitIntentIfTarget(target.sessionID, target.tmuxSession, target.generation, action, relatedEventID, time.Now())
	if err != nil {
		slog.Warn("exit audit: selected lifecycle intent CAS failed", "session", target.sessionID, "error", err)
		return nil
	}
	if cancelled, cancelErr := db.CancelAgentRecoveryForConv(target.convID, action, time.Now()); cancelErr != nil {
		slog.Warn("lifecycle: cancel automatic recovery failed", "conv", short8(target.convID), "action", action, "error", cancelErr)
	} else if cancelled {
		if recovery, _ := db.AgentRecoveryForConv(target.convID); recovery != nil {
			_ = db.RecordAgentRecoveryAudit(*recovery, db.AuditVerbAgentRecoveryCancelled, action, time.Now())
		}
	}
	return &ref
}

// apiSoftExit is a soft exit sent over a harness's control channel instead of
// typed into the pane: the first send and every bounded retry use send, and
// a retry waits while blocked says the harness cannot take it yet.
type apiSoftExit struct {
	label   string
	send    func() error
	blocked func(status string) bool
}

// openCodeSoftExit asks the managed server to dispatch app.exit to the
// attached TUI's command registry. This is the semantic equivalent of /exit
// without prompt-state or keybinding risk.
func openCodeSoftExit(target *lifecycleTarget) apiSoftExit {
	return apiSoftExit{
		label: "OpenCode TUI API",
		send: func() error {
			return sendOpenCodeTUICommand(target.convID, target.sessionID, openCodeTUIExit)
		},
		blocked: openCodeControlInputBlocked,
	}
}

// piDriveSoftExit asks a pi on the extension drive to shut down (ctx.shutdown).
func piDriveSoftExit(target *lifecycleTarget) apiSoftExit {
	return apiSoftExit{
		label:   "pi drive",
		send:    func() error { return piDriveExit(target.convID) },
		blocked: func(string) bool { return false },
	}
}

// injectAPISoftExitTarget sends a soft exit over the harness's control
// channel. The selected session ID and pane identity remain bound across the
// send and the bounded retry; nothing here types into the pane.
func injectAPISoftExitTarget(
	target *lifecycleTarget,
	reason string,
	intentRef *db.SessionExitIntentRef,
	api func(*lifecycleTarget) apiSoftExit,
) bool {
	if target == nil {
		return false
	}
	if beforeSoftExitTargetRevalidateForTest != nil {
		beforeSoftExitTargetRevalidateForTest()
	}
	if _, err := target.revalidate(); err != nil {
		logLifecycleStopFailure("revalidate", target.paneID, target.sessionID, err)
		clearFailedExitIntentTarget(intentRef, target.tmuxSession)
		return false
	}
	exit := api(target)
	if err := exit.send(); err != nil {
		logLifecycleStopFailure(exit.label, target.paneID, target.sessionID, err)
		return false
	}
	if afterSoftExitTargetSendForTest != nil {
		afterSoftExitTargetSendForTest()
	}
	probe, _ := probeLifecyclePane(target.tmuxSession)
	if probe.state == paneProbeUnknown {
		if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
			return true
		}
		scheduleUnknownIntentCleanup(target, intentRef)
		return true
	}
	if probe.state == paneProbeDead || !lifecycleProbeMatchesTarget(probe, target) {
		return true
	}
	scheduleAPISoftExitRetryTarget(target, reason, intentRef, exit)
	return true
}

func scheduleAPISoftExitRetryTarget(
	target *lifecycleTarget,
	reason string,
	intentRef *db.SessionExitIntentRef,
	exit apiSoftExit,
) {
	goBackground(func() {
		for attempt := 2; attempt <= softExitMaxAttempts; attempt++ {
			select {
			case <-target.softExitSettled:
				// Mirrors scheduleSoftExitRetryTarget: the stop's own wait
				// already verified the outcome, so stand down silently
				// (attempt logging is a Copilot forensic, not an OpenCode one).
				return
			case <-time.After(softExitRetryDelay):
			}
			if beforeSoftExitTargetRetryProbeForTest != nil {
				beforeSoftExitTargetRetryProbeForTest(attempt)
			}
			probe, err := probeLifecyclePane(target.tmuxSession)
			if err != nil || probe.state == paneProbeUnknown {
				if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
					return
				}
				scheduleUnknownIntentCleanup(target, intentRef)
				return
			}
			if probe.state == paneProbeDead || !lifecycleProbeMatchesTarget(probe, target) {
				return
			}
			sess := aliveSessionForConv(target.convID)
			if sess == nil || sess.ID != target.sessionID {
				return
			}
			if exit.blocked(sess.Status) {
				slog.Info("soft-exit retry deferred while the harness is not idle",
					"via", exit.label, "conv_id", target.convID, "tmux_session", target.tmuxSession,
					"status", sess.Status, "attempt", attempt, "reason", reason)
				if attempt == softExitMaxAttempts {
					scheduleUnknownIntentCleanup(target, intentRef)
				}
				continue
			}
			if err := exit.send(); err != nil {
				slog.Warn("soft-exit retry dispatch failed",
					"via", exit.label, "error", err, "conv_id", target.convID,
					"tmux_session", target.tmuxSession, "attempt", attempt,
					"reason", reason)
				if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
					return
				}
				scheduleUnknownIntentCleanup(target, intentRef)
				return
			}
			if attempt == softExitMaxAttempts {
				scheduleUnknownIntentCleanup(target, intentRef)
			}
		}
	})
}

func clearFailedExitIntent(intentRef *db.SessionExitIntentRef) {
	if intentRef == nil {
		return
	}
	if _, err := db.ClearSessionExitIntentIfCurrent(*intentRef); err != nil {
		slog.Warn("exit audit: clear failed lifecycle intent failed",
			"session", intentRef.SessionID, "error", err)
	}
}

// logSoftExitBatchStart records that one soft-exit attempt (a "batch" — the
// full signal-key sequence, or the typed exit command) is about to be sent
// into the pane, and which attempt of the bounded ladder it is. This is cheap
// and runs for every harness: when several agents stop in parallel these lines
// are what lets an operator line up per-pane batch timings against how long
// each stop actually took, without copying pane contents into the logs.
func logSoftExitBatchStart(target *lifecycleTarget, reason string, attempt int, signalKeys []string) {
	markSessionShuttingDown(target.sessionID)
	mode := "typed"
	if len(signalKeys) > 0 {
		mode = "signal"
	}
	slog.Info("soft-exit: sending exit batch",
		"conv", short8(target.convID), "session", target.sessionID,
		"tmux_session", target.tmuxSession, "pane_id", target.paneID,
		"attempt", attempt, "max_attempts", softExitMaxAttempts,
		"mode", mode, "keys", len(signalKeys), "reason", reason)
}

// sendSoftExitToTarget delivers one soft-exit attempt to the pane. A harness
// with a keystroke-free signal exit (harness.Lifecycle.SignalExitKeys non-empty:
// Copilot, Claude Code, Codex) gets those keys sent as signals — a typed slash
// command is silently dropped both mid-turn and whenever a TUI's keypress
// reader wedges, while ctrl-c handling survives both states; every other
// harness gets its exit command typed and submitted.
//
// signalKeys is resolved ONCE per stop by the caller and threaded through, like
// exitCmd and prefixKeys already are: re-resolving per attempt would let a conv
// whose rows vanish mid-stop fall back to the default harness on a retry and
// type "/exit" into the very pane this branch exists to never type into.
func sendSoftExitToTarget(target *lifecycleTarget, signalKeys []string, exitCmd string, prefixKeys []string) error {
	if len(signalKeys) > 0 {
		return injectSignalExitSerializedBy(target.tmuxSession+":0.0", target.paneID, signalKeys)
	}
	return injectSoftExitTextSerializedBy(target.tmuxSession+":0.0", target.paneID, exitCmd, prefixKeys)
}

// softExitPaneScreenTail is capturePaneScreenTail gated to Copilot panes —
// the one harness whose soft-exit failures need screen-state forensics.
// Returns "" for every other harness so their escalation logs stay
// screen-free.
func softExitPaneScreenTail(target *lifecycleTarget) string {
	if harnessForConv(target.convID).Name != harness.CopilotName {
		return ""
	}
	return capturePaneScreenTail(target.paneID)
}

func injectSoftExitTarget(target *lifecycleTarget, exitCmd string, prefixKeys []string, reason string, intentRef *db.SessionExitIntentRef) bool {
	if target == nil {
		return false
	}
	if beforeSoftExitTargetRevalidateForTest != nil {
		beforeSoftExitTargetRevalidateForTest()
	}
	if _, err := target.revalidate(); err != nil {
		logLifecycleStopFailure("revalidate", target.paneID, target.sessionID, err)
		clearFailedExitIntentTarget(intentRef, target.tmuxSession)
		return false
	}
	// Resolved once for the whole stop, retries included — see
	// sendSoftExitToTarget for why per-attempt re-resolution is unsafe.
	h := harnessForConv(target.convID)
	signalKeys := h.SignalExitKeys()
	copilot := h.Name == harness.CopilotName
	logSoftExitBatchStart(target, reason, 1, signalKeys)
	if err := sendSoftExitToTarget(target, signalKeys, exitCmd, prefixKeys); err != nil {
		logLifecycleStopFailure("send", target.paneID, target.sessionID, err)
		return false
	}
	if afterSoftExitTargetSendForTest != nil {
		afterSoftExitTargetSendForTest()
	}
	probe, _ := probeLifecyclePane(target.tmuxSession)
	if probe.state == paneProbeUnknown {
		if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
			return true // confirmed session disappearance; reaper owns attribution
		}
		scheduleUnknownIntentCleanup(target, intentRef)
		return true // preserve intent; bounded cleanup is owned by retry watchdog
	}
	if probe.state == paneProbeDead {
		return true
	}
	if !lifecycleProbeMatchesTarget(probe, target) {
		// The command was already delivered; a post-send identity change means
		// the predecessor transitioned, so preserve intent for callback/reaper
		// attribution and never retry against a successor.
		return true
	}
	scheduleSoftExitRetryTarget(target, signalKeys, copilot, exitCmd, prefixKeys, reason, intentRef)
	return true
}

func lifecycleProbeMatchesTarget(probe lifecyclePaneProbe, target *lifecycleTarget) bool {
	generationMatches := probe.generation == ""
	if target.paneGenerationBound {
		generationMatches = target.generation != "" && probe.generation == target.generation
	}
	return probe.paneID == target.paneID &&
		(target.panePID <= 0 || probe.panePID == target.panePID) && generationMatches
}

// lifecycleSessionAlive reports whether tmux still lists tmuxSession, and
// whether the answer is known at all. Bounded for the same reason as
// probeLifecyclePane: the ladder's signal rungs poll it from a request
// goroutine, and an unbounded call there is an unbounded request.
//
// A list-sessions failure that says NO SERVER EXISTS is a known all-offline
// answer, not an unreadable probe. When the pane being stopped was the last
// live session, its death takes the whole tmux server down with it (tmux's
// default exit-empty on), and from that moment every pane probe and every
// list-sessions errors — treating that as "unknown" left
// waitForLifecycleTargetGone polling blind for the entire escalation deadline
// and turned the fastest possible exit into the slowest observed stop (a
// retire that stalled the full 10 s after the pane had died within 1 s).
// liveTmuxSessionsWithTimeout already reads a dead server as the normal
// all-offline state; this is the same call, made distinguishable from a
// transient failure so the unknown contract stays intact for real faults.
func lifecycleSessionAlive(tmuxSession string) (alive, known bool) {
	out, err := tmuxOutputWithTimeout("list-sessions", "-F", "#{session_name}")
	if err != nil {
		if tmuxServerNotRunning(err) {
			return false, true
		}
		return false, false
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name == tmuxSession {
			return true, true
		}
	}
	return false, true
}

// tmuxServerNotRunning reports whether a failed tmux invocation failed because
// no tmux server exists at all — no socket, or a socket nothing is serving.
// Message-matched because that is the only channel tmux reports it on, and the
// spelling differs across versions: "error connecting to <socket> (No such
// file or directory)" (observed on 3.x when the socket is gone) and "no server
// running on <socket>" (when the socket file remains). A timeout is explicitly
// NOT a dead server — a wedged server still owns live panes — and any other
// failure keeps the unknown contract.
func tmuxServerNotRunning(err error) bool {
	if err == nil || errors.Is(err, errTmuxCommandTimeout) {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "no server running") ||
		strings.Contains(msg, "error connecting to")
}

func scheduleUnknownIntentCleanup(target *lifecycleTarget, intentRef *db.SessionExitIntentRef) {
	goBackground(func() {
		time.Sleep(unknownIntentCleanupDelay)
		clearFailedExitIntentTarget(intentRef, target.tmuxSession)
	})
}

// scheduleUnknownIntentCleanupCurrent is the current-CAS twin of
// scheduleUnknownIntentCleanup for the pid-keyed retry path, whose intent
// ref carries no selected-target binding to key the target CAS on.
func scheduleUnknownIntentCleanupCurrent(intentRef *db.SessionExitIntentRef) {
	goBackground(func() {
		time.Sleep(unknownIntentCleanupDelay)
		clearFailedExitIntent(intentRef)
	})
}

var beforeSoftExitTargetRevalidateForTest func()
var beforeSoftExitTargetRetryProbeForTest func(int)
var afterSoftExitTargetSendForTest func()

func clearFailedExitIntentTarget(ref *db.SessionExitIntentRef, tmuxSession string) {
	if ref == nil {
		return
	}
	if _, err := db.ClearSessionExitIntentIfTarget(*ref, tmuxSession); err != nil {
		slog.Warn("exit audit: clear selected lifecycle intent failed", "session", ref.SessionID, "error", err)
	}
}

func scheduleSoftExitRetryTarget(target *lifecycleTarget, signalKeys []string, copilot bool, exitCmd string, prefixKeys []string, reason string, intentRef *db.SessionExitIntentRef) {
	goBackground(func() {
		// Attempt-timeline logging is Copilot-only: the ignored-soft-exit
		// forensics it exists for is a Copilot failure mode, and every other
		// harness would just log more for nothing. The signal-exit send path is
		// picked by the caller-resolved signalKeys (sendSoftExitToTarget).
		logAttempts := copilot
		for attempt := 2; attempt <= softExitMaxAttempts; attempt++ {
			select {
			case <-target.softExitSettled:
				// The stop's own wait already verified the outcome; no reason
				// to wake later just to rediscover it (or to keep an operator
				// guessing whether the watchdog is still pending).
				if logAttempts {
					slog.Info("soft-exit retry: watchdog cancelled; stop already settled",
						"session", target.sessionID, "conv", short8(target.convID),
						"attempt", attempt, "reason", reason)
				}
				return
			case <-time.After(softExitRetryDelay):
			}
			if beforeSoftExitTargetRetryProbeForTest != nil {
				beforeSoftExitTargetRetryProbeForTest(attempt)
			}
			probe, err := probeLifecyclePane(target.tmuxSession)
			if err != nil || probe.state == paneProbeUnknown {
				// The first /exit was already delivered (the stop reported
				// soft_stopped), so this is not a stop failure to log: the
				// watchdog mirrors the synchronous unknown branch. A confirmed
				// disappearance is the delivered exit landing — the reaper
				// owns attribution — and anything else keeps the intent
				// through the bounded observer window. An instant clear here
				// would erase a delivered exit's attribution on a transient
				// probe failure.
				if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
					if logAttempts {
						slog.Info("soft-exit retry: session gone before attempt",
							"session", target.sessionID, "conv", short8(target.convID),
							"attempt", attempt, "reason", reason)
					}
					return
				}
				scheduleUnknownIntentCleanup(target, intentRef)
				return
			}
			if probe.state == paneProbeDead {
				if logAttempts {
					slog.Info("soft-exit retry: pane closed before attempt",
						"session", target.sessionID, "conv", short8(target.convID),
						"attempt", attempt, "reason", reason)
				}
				return
			}
			if !lifecycleProbeMatchesTarget(probe, target) {
				// Delivered plus a post-send identity change means the
				// predecessor transitioned; preserve intent for
				// callback/reaper attribution and never retry against a
				// successor (mirrors injectSoftExitTarget).
				if logAttempts {
					slog.Info("soft-exit retry: pane identity changed before attempt",
						"session", target.sessionID, "conv", short8(target.convID),
						"attempt", attempt, "reason", reason)
				}
				return
			}
			logSoftExitBatchStart(target, reason, attempt, signalKeys)
			if err := sendSoftExitToTarget(target, signalKeys, exitCmd, prefixKeys); err != nil {
				logLifecycleStopFailure("send", target.paneID, target.sessionID, err)
				// The first /exit was already delivered; a failed RE-send must
				// not erase that delivery's attribution. Mirror the unknown
				// branch: a confirmed disappearance is the delivered exit
				// landing (the send often fails precisely because the pane
				// just died) — the reaper owns it — and anything else keeps
				// the intent through the bounded observer window instead of
				// clearing instantly.
				if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
					return
				}
				scheduleUnknownIntentCleanup(target, intentRef)
				return
			}
			if attempt == softExitMaxAttempts {
				// Delivery succeeded; retain attribution through the observer window.
				scheduleUnknownIntentCleanup(target, intentRef)
			}
		}
	})
}

// injectSoftExit injects a harness soft-exit command (Claude Code's
// /exit, Codex's /quit) into convID's live pane and arms a background
// retry. It returns whether the FIRST injection's send-keys succeeded —
// the soft_stopped/error contract callers (stopOneConv, reincarnate)
// already rely on.
//
// Why the retry: a single /exit can be silently lost when the pane's
// input buffer wasn't empty. send-keys appends the command to whatever
// junk was already sitting there (a half-typed line, a stray paste), so
// the trailing Enter submits "<junk>/exit" as one ordinary prompt instead
// of an exit — and the pane keeps running. That submit DOES clear the
// buffer, though, so a second /exit a few seconds later lands on a clean
// input box and takes. scheduleSoftExitRetry re-injects while the SAME
// pane process is still alive.
func injectSoftExit(convID, exitCmd, reason string, intentRef *db.SessionExitIntentRef) bool {
	sess := aliveSessionForConv(convID)
	if sess == nil {
		return false
	}
	if sess.Harness == harness.OpenCodeName {
		target, err := captureLifecycleTarget(sess)
		if err != nil {
			logLifecycleStopFailure("capture OpenCode soft exit", sess.TmuxSession, sess.ID, err)
			return false
		}
		// OpenCode's first send and every bounded retry must use the managed
		// TUI API. Falling through to scheduleSoftExitRetry would reintroduce
		// tmux keystrokes when a reincarnating attach pane lingers.
		return injectAPISoftExitTarget(target, reason, intentRef, openCodeSoftExit)
	}
	if sess.Harness == harness.PiName {
		piDrive, err := piDriveSelected(convID)
		if err != nil {
			slog.Warn("soft-exit: pi drive posture unreadable; refusing pane fallback",
				"conv_id", convID, "error", err)
			return false
		}
		if piDrive {
			target, err := captureLifecycleTarget(sess)
			if err != nil {
				logLifecycleStopFailure("capture pi drive soft exit", sess.TmuxSession, sess.ID, err)
				return false
			}
			return injectAPISoftExitTarget(target, reason, intentRef, piDriveSoftExit)
		}
	}
	// Capture before injection: a responsive pane can exit synchronously after
	// Enter, but that successful exit still owns the lifecycle intent and must
	// remain correlatable by callback/reaper.
	panePID := livePanePID(sess.TmuxSession)
	// Same harness contract as the selected-target path: a soft exit typed at
	// a busy TUI can be silently discarded, so the prefix keys go in front of
	// it here too (nil for every harness that needs none).
	prefixKeys := harnessForConv(convID).Life.SoftExitPrefixKeys()
	softExitTarget := sess.TmuxSession + ":0.0"
	if err := injectSoftExitTextSerializedBy(softExitTarget, softExitTarget, exitCmd, prefixKeys); err != nil {
		slog.Warn("soft-exit inject failed", "error", err,
			"tmux_session", sess.TmuxSession, "conv_id", convID, "reason", reason)
		return false
	}
	slog.Info("soft-exit injected via send-keys",
		"conv_id", convID, "line", exitCmd, "reason", reason,
		"tmux_session", sess.TmuxSession)
	// Capture the pane's live OS pid so the retry can tell THIS process apart
	// from a later one that reused the same tmux name (a resume re-derives the
	// name from the conv-id — see scheduleSoftExitRetry). 0 = couldn't read
	// it; skip the retry rather than risk re-injecting blind.
	if panePID > 0 {
		scheduleSoftExitRetry(convID, sess.TmuxSession, panePID, exitCmd, prefixKeys, reason, intentRef)
	} else if alive, known := lifecycleSessionAlive(sess.TmuxSession); known && !alive {
		// Confirmed session disappearance: the delivered /exit is landing and
		// the reaper owns attribution.
	} else {
		// An unreadable pid is not a stop failure — the /exit above WAS
		// delivered (a responsive pane can even exit synchronously after
		// Enter, taking its pid with it). Instantly clearing here would erase
		// that delivered exit's attribution, so mirror the retry engines'
		// unknown treatment: retain the intent through the bounded observer
		// window instead.
		scheduleUnknownIntentCleanupCurrent(intentRef)
	}
	return true
}

// softExitRetryDelay is how long the background soft-exit retry waits
// before each re-check of a pane it asked to /exit. A package var so flow
// tests can shrink it (SetSoftExitRetryDelayForTest); production keeps it
// short — long enough for a pane that's honouring the exit to close before
// we bother re-injecting, but tight enough that a batch whose press missed
// the harness's re-press window (Claude Code's is ~0.8 s under load) gets
// its next chance quickly instead of riding most of the way to the 10 s
// escalation deadline.
var softExitRetryDelay = 1500 * time.Millisecond

// Unknown cleanup must remain available for the reaper to observe exits when
// hooks and immediate probes are unavailable.
var unknownIntentCleanupDelay = 65 * time.Second

// softExitMaxAttempts bounds the TOTAL number of soft-exit injections per
// stop (the initial one + retries). The first retry recovers an /exit
// lost to input-buffer junk (see injectSoftExit); the remaining margin
// covers an unlucky pane that was mid-render or whose signal-exit presses
// missed the harness's re-press window. Sized so the batches keep coming for
// the whole softExitEscalationDeadline (a batch is ~1.3 s of key spacing plus
// softExitRetryDelay between batches, so attempts start roughly every 2.8 s),
// and capped so a pane that simply will not exit isn't injected forever —
// the retry engines stand down as soon as the pane dies or the stop settles,
// and the escalation ladder owns the force-kill fallback.
const softExitMaxAttempts = 5

// scheduleSoftExitRetry backgrounds the re-injection of exitCmd into the
// pane that injectSoftExit first targeted. It re-injects ONLY while that
// pane is still the SAME live process — keyed on the tmux pane's OS pid
// (panePID), captured at the first injection.
//
// The pid is the load-bearing guard: a resume re-derives the tmux session
// name from the conv-id (sessionResumeArgs → session new -r, no --label →
// name = conv-id[:8]), so a stop → exit → resume cycle can land a brand
// new agent process under the very same tmux name within the retry window.
// Matching on the name alone would then type /exit at that innocent,
// freshly-resumed pane and drop its input. tmux assigns a fresh pane pid
// to every new process, so a changed (or unreadable → 0) pid means "not my
// pane anymore — stop." Re-injection goes straight to the captured target
// (no conv re-resolution) so the pane we validated is the pane we type at.
//
// Runs through goBackground so it outlives the HTTP handler that asked for
// the stop and flow tests can drain it with WaitForBackgroundForTest.
func scheduleSoftExitRetry(convID, tmuxSession string, panePID int, exitCmd string, prefixKeys []string, reason string, intentRef *db.SessionExitIntentRef) {
	target := tmuxSession + ":0.0"
	goBackground(func() {
		for attempt := 2; attempt <= softExitMaxAttempts; attempt++ {
			time.Sleep(softExitRetryDelay)
			if livePanePID(tmuxSession) != panePID {
				return // exited, force-killed, or a different process now owns the name
			}
			slog.Info("soft-exit retry: pane still alive, re-injecting exit",
				"conv_id", convID,
				"tmux_session", tmuxSession,
				"pane_pid", panePID,
				"attempt", attempt,
				"max_attempts", softExitMaxAttempts,
				"reason", reason)
			if err := injectSoftExitTextSerializedBy(target, target, exitCmd, prefixKeys); err != nil {
				slog.Warn("soft-exit retry inject failed",
					"error", err, "tmux_session", tmuxSession, "reason", reason)
				// The first /exit was already delivered; mirror the
				// selected-pane watchdog's re-send-failure treatment: a
				// vanished session leaves attribution to the reaper, anything
				// else retains the intent through the bounded observer window
				// instead of instantly erasing a delivered exit's attribution.
				if alive, known := lifecycleSessionAlive(tmuxSession); known && !alive {
					return
				}
				scheduleUnknownIntentCleanupCurrent(intentRef)
				return
			}
		}
		if livePanePID(tmuxSession) == panePID {
			// The final re-send was delivered and there is no settle delay
			// before this check, so a pane honoring that /exit is often still
			// alive here. Mirror the target engine's final-attempt treatment:
			// retain attribution through the bounded observer window rather
			// than erasing it moments before the exit lands; a genuinely
			// wedged pane reaches the same cleared end state, just bounded.
			scheduleUnknownIntentCleanupCurrent(intentRef)
		}
	})
}

// livePanePID returns the OS pid tmux reports for tmuxSession's active
// pane, or 0 when the session is gone or the query fails. Unlike the
// sessions-table pid column — written only on the pane's first hook tick,
// so stale right after a resume — tmux knows a pane's pid the instant it
// is created, making this the reliable "is this still the same process?"
// signal the soft-exit retry needs to avoid re-injecting into a resumed
// pane that reused the tmux name.
func livePanePID(tmuxSession string) int {
	out, err := clcommon.TmuxOutput("display-message", "-p", "-t", clcommon.ExactTarget(tmuxSession)+":", "#{pane_dead}|#{pane_pid}")
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.TrimSpace(string(out)), "|")
	if len(parts) != 2 || parts[0] == "1" {
		return 0
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0
	}
	return pid
}

// livePaneCwd returns tmux's view of the live pane process's physical working
// directory. Unlike sessions.cwd, this follows the cwd inode the predecessor
// is actually running in, so retargeting a symlink used at the original launch
// cannot redirect an inherited clone or reincarnation.
func livePaneCwd(tmuxSession string) (string, error) {
	return clcommon.LivePaneCwd(tmuxSession)
}
