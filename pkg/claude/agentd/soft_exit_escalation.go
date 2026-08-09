package agentd

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The soft-exit escalation ladder.
//
// A soft stop types the harness's exit command into the pane and hopes. Until
// TCL-1001 that hope was the whole plan: the bounded re-injection retry was
// the last thing that ever happened to a pane which would not go, and the
// callers that DEPEND on the pane actually closing — retire's agent-directory
// and worktree cleanup — simply waited out their 60 s grace and then skipped
// the cleanup, leaving both the pane and the directories behind. That is the
// operator-reported failure: two Copilot agents retired, panes still running,
// "agent-owned directories kept because agent did not exit within grace".
//
// So a delivered-or-failed soft exit now starts a watchdog. It gives the
// injection (and its two retries) a real window to work, and when the pane is
// still the same live pane at the end of it, escalates:
//
//	1. tmux kill-pane on the exact pane id (what force-stop already does)
//	2. SIGTERM to the pane process group
//	3. SIGKILL to the pane process group
//
// Ordering is deliberate and not merely tidy. The graceful layers are what let
// a harness write its own end-of-session state — Copilot's durable
// session.shutdown event, which session-lifetime usage totals are computed
// from — so -9 is reached only after the two politer layers have each been
// given their turn and failed.
//
// Every step re-checks the frozen pane identity first. A stop can be followed
// within seconds by a resume that re-derives the same tmux session name, and a
// watchdog that killed on name alone would execute a brand new agent.

// softExitEscalationDeadline is how long the pane has to close on its own
// after the first soft-exit delivery before the ladder starts.
//
// It sits beyond the last bounded re-injection (softExitRetryDelay ×
// softExitMaxAttempts ≈ 8 s) so a pane that honours the third attempt gets a
// chance to act on it before anything is killed — though only about two
// seconds of one, so a harness whose exit takes longer than that from its
// final prompt will be escalated rather than waited for. That is the intended
// trade: the deadline also has to stay far below retireWorktreeExitGrace
// (60 s), which is what makes retire cleanup run at all, and a stop is a
// request to end the session rather than to negotiate about it.
var softExitEscalationDeadline = 10 * time.Second

// softExitEscalationSignalGrace is how long each signal step waits for the
// process to disappear before the next, harsher one.
var softExitEscalationSignalGrace = 2 * time.Second

// softExitEscalationPollInterval is how often the watchdog re-probes the pane
// while waiting.
var softExitEscalationPollInterval = 250 * time.Millisecond

// daemonEscalatedKillReason marks a session the daemon killed because its
// soft exit never took. Without it an escalated kill of a Claude Code pane
// would reach the reaper with no recorded reason and be classified
// "unexpected" — i.e. reported to the operator as a crash, when in fact
// tclaude did it on purpose.
const daemonEscalatedKillReason = "daemon_kill"

// softExitOutcome says how a stop that WAITED for its pane ended. It is the
// return of awaitLifecycleTargetExit; the fire-and-forget scheduler discards
// it.
type softExitOutcome int

const (
	// softExitClosed — the pane closed on its own within the deadline (or was
	// already gone / already replaced). Nothing was killed.
	softExitClosed softExitOutcome = iota
	// softExitEscalated — the pane outlived the deadline, the ladder ran, and
	// the process is gone now.
	softExitEscalated
	// softExitStuck — the ladder ran to the end and the pane process is STILL
	// alive. Nothing further is available; the caller must not assume the
	// agent released its files, cwd or worktree.
	softExitStuck
	// softExitUnattempted — the stop never reached the pane at all: capturing
	// the lifecycle target failed, the selected launch intent went stale under
	// us, or a busy OpenCode TUI refused control input. No exit command was
	// delivered and no rung of the ladder ran, so the ONLY thing known is that
	// we did not stop it.
	//
	// This must never be folded into softExitClosed. It reads as "nothing
	// happened", and "nothing happened" is the opposite of "it exited" — a
	// caller that conflates the two reports a still-running agent as gracefully
	// stopped, and anything gated on a failed stop (the restart paths' abort
	// before relaunch) sails straight past its guard.
	softExitUnattempted
)

// scheduleSoftExitEscalation backgrounds the ladder for one stopped target.
// lifecycleAction/relatedEventID are the same attribution the caller armed
// before injecting, re-armed at the kill so an escalation stays daemon-owned
// even when the injection failed and its intent was cleared.
//
// This is the fire-and-forget half of the pair: the stop returns as soon as
// the exit command is delivered and the pane is closed out-of-band. Callers
// that must not proceed until the process has actually stopped — a retire
// about to delete the agent's directories, a bulk stop reporting what it
// achieved — use awaitLifecycleTargetExit instead.
func scheduleSoftExitEscalation(target *lifecycleTarget, lifecycleAction, relatedEventID, reason, fallbackExitReason string) {
	if target == nil {
		return
	}
	goBackground(func() {
		// By the time this watchdog returns the stop is resolved either way
		// (pane exit verified, or the ladder ran to its end), so release the
		// soft-exit retry watchdog instead of leaving it to sleep out its
		// delay and rediscover the outcome.
		defer target.markSoftExitSettled()
		if waitForLifecycleTargetGone(target, softExitEscalationDeadline) {
			if err := reconcileStoppedLifecycleTarget(target, lifecycleAction, relatedEventID, fallbackExitReason); err != nil {
				slog.Warn("soft-exit: recording verified pane exit failed",
					"session", target.sessionID, "conv", short8(target.convID), "error", err)
			}
			return
		}
		outcome := escalateStuckSoftExit(target, lifecycleAction, relatedEventID, reason)
		if outcome == softExitClosed || outcome == softExitEscalated {
			reconcileReason := fallbackExitReason
			if outcome == softExitEscalated {
				reconcileReason = daemonEscalatedKillReason
			}
			if err := reconcileStoppedLifecycleTarget(target, lifecycleAction, relatedEventID, reconcileReason); err != nil {
				slog.Warn("soft-exit: recording verified pane exit after escalation check failed",
					"session", target.sessionID, "conv", short8(target.convID), "error", err)
			}
		}
	})
}

// awaitLifecycleTargetExit is scheduleSoftExitEscalation run INLINE: it gives
// the delivered soft exit (and its bounded re-injections — the double tap)
// until the deadline to close the pane on its own, then runs the same
// kill-pane → SIGTERM → SIGKILL ladder, and only returns once the pane process
// is actually gone or the ladder is exhausted.
//
// deadline is AUTHORITATIVE, including zero: the power buttons pass their
// per-request grace, and a grace of 0 legitimately means "probe once, then
// escalate". Resolving a default belongs to the caller that has one
// (stopOneConvAndWait), not here.
//
// The caller MUST already hold the conversation's launch lock — the whole
// point is that the stop, the wait and the escalation are one indivisible
// step, so a resume cannot relaunch the conv into the identity being killed
// halfway through.
func awaitLifecycleTargetExit(target *lifecycleTarget, deadline time.Duration, lifecycleAction, relatedEventID, reason string) softExitOutcome {
	if target == nil {
		return softExitClosed
	}
	if waitForLifecycleTargetGone(target, deadline) {
		return softExitClosed
	}
	return escalateStuckSoftExitUnderLaunchLock(target, lifecycleAction, relatedEventID, reason)
}

// waitForLifecycleTargetGone polls until the frozen target is no longer a live
// pane, or the window closes. It reports true when there is nothing left to
// escalate against — the pane died, its session vanished, or the identity
// changed (a successor owns the name now, and it is not ours to kill).
func waitForLifecycleTargetGone(target *lifecycleTarget, window time.Duration) bool {
	deadline := time.Now().Add(window)
	for {
		if softExitEscalationPollForTest != nil {
			softExitEscalationPollForTest()
		}
		probe, err := probeLifecyclePane(target.tmuxSession)
		switch {
		case err != nil || probe.state == paneProbeUnknown:
			// A probe that cannot be read is not evidence of a live pane. Only
			// a confirmed still-listed session keeps the ladder armed.
			if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
				return true
			}
		case probe.state == paneProbeDead:
			return true
		case !lifecycleProbeMatchesTarget(probe, target):
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		sleep := softExitEscalationPollInterval
		if sleep > remaining {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}

// escalateStuckSoftExit runs the ladder under the conversation launch lock, so
// a concurrent resume cannot relaunch the conv into the pane identity being
// killed halfway through. The background scheduler's entry point; a caller
// that already holds the lock uses escalateStuckSoftExitUnderLaunchLock.
func escalateStuckSoftExit(target *lifecycleTarget, lifecycleAction, relatedEventID, reason string) softExitOutcome {
	launchLock := resumeLaunchLock(target.convID)
	launchLock.Lock()
	defer launchLock.Unlock()
	return escalateStuckSoftExitUnderLaunchLock(target, lifecycleAction, relatedEventID, reason)
}

// reconcileStoppedLifecycleTarget publishes the fact a managed stop already
// proved: this launch's frozen tmux pane is gone. Leaving that fact to the
// generic session reaper is both slower and weaker. RefreshSessionStatus falls
// back to the session row's recorded PID after tmux disappears; for Copilot
// that PID can remain readable after the pane has closed, leaving an
// unattachable session displayed as idle indefinitely even though retire
// correctly removed its tmux session.
//
// The status write is generation- and row-version-CASed. A concurrent callback
// may win (already exited), and a resume may rotate the generation; neither can
// be overwritten by this predecessor's stop. A bounded reload closes the
// ordinary race with a last hook update emitted while the process was exiting.
func reconcileStoppedLifecycleTarget(target *lifecycleTarget, lifecycleAction, relatedEventID, fallbackReason string) error {
	if target == nil {
		return nil
	}
	probe, probeErr := probeLifecyclePane(target.tmuxSession)
	switch {
	case probeErr == nil && probe.state == paneProbeDead:
		// A retained dead pane is exact structural proof of exit.
	case probeErr == nil && probe.state == paneProbeLive:
		// The original pane is still live, or a successor now owns the name.
		// Neither permits this predecessor to change the durable status.
		return nil
	default:
		alive, known := lifecycleSessionAlive(target.tmuxSession)
		if !known || alive {
			return nil
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		row, err := db.LoadSession(target.sessionID)
		if err != nil {
			return err
		}
		if row == nil || row.Status == "exited" {
			return nil
		}
		identity, err := db.GetSessionExitLaunchIdentity(target.sessionID)
		if err != nil {
			return err
		}
		if identity.Generation != target.generation {
			return nil // a successor owns the durable row now
		}
		ok, _, err := db.MarkSessionExitedAndRecordObservationIfUnchanged(
			target.sessionID, row.Status, row.UpdatedAt, fallbackReason,
			db.AgentExitObservation{
				At: time.Now(), SessionID: target.sessionID,
				TmuxSession: target.tmuxSession, PaneID: target.paneID,
				Observer:        db.AgentExitObserverReconcile,
				CauseKind:       db.AgentExitCauseDisappeared,
				LifecycleAction: lifecycleAction, RelatedEventID: relatedEventID,
				ExpectedGeneration: target.generation,
			},
		)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
	}
	return fmt.Errorf("session row kept changing after the managed pane exited")
}

// escalateStuckSoftExitUnderLaunchLock is the ladder itself, after its caller
// has taken the conversation launch lock. It reports whether the pane process
// was actually gone by the time it returned, so a synchronous stop can tell
// "killed, and it is really down" from "nothing left to do".
func escalateStuckSoftExitUnderLaunchLock(target *lifecycleTarget, lifecycleAction, relatedEventID, reason string) softExitOutcome {
	if beforeSoftExitEscalationRevalidateForTest != nil {
		beforeSoftExitEscalationRevalidateForTest()
	}
	probe, err := target.revalidate()
	if err != nil {
		// Died, or a successor now owns the name — either way the ladder has
		// nothing of ours to act on.
		slog.Info("soft-exit escalation stood down; target pane is gone or replaced",
			"conv", short8(target.convID), "tmux_session", target.tmuxSession,
			"pane_id", target.paneID, "reason", reason)
		return softExitClosed
	}

	// Attribution BEFORE the kill: whatever the ladder does from here, the
	// reaper must read it as daemon-owned rather than an unexplained close.
	// The intent is (re-)armed because a failed injection clears it, and the
	// reason is recorded because the reaper's per-harness fallback is either
	// silence (Copilot, Codex) or "unexpected" (Claude Code) — neither of
	// which describes a deliberate daemon kill.
	setExitIntentTargetBestEffort(target, lifecycleAction, relatedEventID)
	if err := db.SetSessionExitReason(target.sessionID, daemonEscalatedKillReason); err != nil {
		slog.Warn("soft-exit escalation: recording daemon-owned exit reason failed",
			"session", target.sessionID, "conv", short8(target.convID), "error", err)
	}

	slog.Warn("soft exit did not close the pane; escalating to kill",
		"conv", short8(target.convID), "session", target.sessionID,
		"tmux_session", target.tmuxSession, "pane_id", target.paneID,
		"pane_pid", probe.panePID, "deadline", softExitEscalationDeadline,
		"reason", reason,
		"pane_screen", softExitPaneScreenTail(target))

	// Step 1: the identity-guarded tmux kill force-stop already uses.
	if err := killLifecycleTarget(target); err != nil {
		slog.Warn("soft-exit escalation: tmux kill failed; continuing to signals",
			"conv", short8(target.convID), "pane_id", target.paneID, "error", err)
	}

	// Steps 2 and 3. The pane pid is the harness process tmux started; killing
	// its GROUP is what reaches a harness that spawned children and is being
	// held open by one of them.
	pid := probe.panePID
	if pid <= 0 {
		pid = target.panePID
	}
	if pid <= 0 {
		// No pid to signal — the tmux kill above is all the ladder has. Give
		// the pane one grace window to disappear so the caller still learns
		// whether the process actually went away.
		if waitForLifecycleTargetGone(target, softExitEscalationSignalGrace) {
			return softExitEscalated
		}
		return softExitStuck
	}
	for _, step := range softExitSignalLadder {
		if waitForPaneProcessGone(target, pid, softExitEscalationSignalGrace) {
			return softExitEscalated
		}
		slog.Warn("soft-exit escalation: pane process survived; signalling process group",
			"conv", short8(target.convID), "tmux_session", target.tmuxSession,
			"pane_pid", pid, "signal", step.name, "reason", reason)
		if err := signalLifecycleProcessGroup(pid, step.signal); err != nil {
			slog.Warn("soft-exit escalation: signalling process group failed",
				"conv", short8(target.convID), "pane_pid", pid,
				"signal", step.name, "error", err)
		}
	}
	if waitForPaneProcessGone(target, pid, softExitEscalationSignalGrace) {
		return softExitEscalated
	}
	slog.Error("soft-exit escalation exhausted; pane process still alive",
		"conv", short8(target.convID), "tmux_session", target.tmuxSession,
		"pane_pid", pid, "reason", reason)
	return softExitStuck
}

// paneScreenTailLines and paneScreenTailClip bound what a pre-kill screen
// capture may add to one warn line.
const (
	paneScreenTailLines = 12
	paneScreenTailClip  = 2000
)

// capturePaneScreenTail reads the stuck pane's visible screen so the
// escalation warn can say WHAT the harness was showing when it ignored its
// soft exit — a permission dialog, a modal, a wedged teardown. The soft-exit
// deadline expiring is exactly the moment that state is still on screen and
// about to be destroyed by the kill. The injection attempts each bracket
// themselves with the same capture (logSoftExitPaneState) — necessary because
// a retry's own prefix C-c can clear the very input-box state the escalation
// capture would otherwise have shown.
//
// Best-effort by design: the pane may die between the caller's revalidate and
// this read, and a flow-test tmux sim may not implement capture-pane. Both
// yield "", never an error that could block the ladder. Lines are joined with
// " | " so the capture stays a single log line for line-based log tooling.
func capturePaneScreenTail(paneID string) string {
	out, err := tmuxOutputWithTimeout("capture-pane", "-p", "-J", "-t", paneID)
	if err != nil {
		return ""
	}
	return formatPaneScreenTail(string(out))
}

func formatPaneScreenTail(screen string) string {
	var kept []string
	for line := range strings.SplitSeq(screen, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	if len(kept) > paneScreenTailLines {
		kept = kept[len(kept)-paneScreenTailLines:]
	}
	return auditClip(strings.Join(kept, " | "), paneScreenTailClip)
}

// waitForPaneProcessGone polls the pane process until it is gone or the grace
// closes. The tmux session disappearing counts: a pane whose session tmux no
// longer lists took its process with it, and a pid that has already been
// reaped can be recycled onto an unrelated process.
func waitForPaneProcessGone(target *lifecycleTarget, pid int, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for {
		if alive, known := lifecycleSessionAlive(target.tmuxSession); known && !alive {
			return true
		}
		if !lifecycleProcessAlive(pid) {
			return true
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false
		}
		sleep := softExitEscalationPollInterval
		if sleep > remaining {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
}

// softExitEscalationPollForTest lets a flow test observe (and act between) the
// watchdog's probes.
var softExitEscalationPollForTest func()

// beforeSoftExitEscalationRevalidateForTest lets a flow test act in the
// deadline-to-revalidation window after the watchdog decided escalation was
// needed but before the launch-locked identity check observes the target.
var beforeSoftExitEscalationRevalidateForTest func()

// stopIntendsPaneClosure reports whether a lifecycle stop of this shape means
// "this pane must end", which is what entitles the daemon to escalate. Every
// action the soft-stop path records qualifies; an unattributed call (empty
// action) does not arm the ladder, so no caller acquires a kill it did not ask
// for merely by soft-stopping.
func stopIntendsPaneClosure(lifecycleAction string) bool {
	switch lifecycleAction {
	case db.AgentExitActionStop, db.AgentExitActionForceStop,
		db.AgentExitActionRetire, db.AgentExitActionReincarnate:
		return true
	default:
		return false
	}
}
