package agentd

import (
	"fmt"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Shared terminal-process escalation primitives.
//
// Terminal harness runtime adapters first request a graceful stop. Until
// TCL-1001 that request was the whole plan: the bounded retry was
// the last thing that ever happened to a pane which would not go, and the
// callers that DEPEND on the pane actually closing — retire's agent-directory
// and worktree cleanup — simply waited out their 60 s grace and then skipped
// the cleanup, leaving both the pane and the directories behind. That is the
// operator-reported failure: two Copilot agents retired, panes still running,
// "agent-owned directories kept because agent did not exit within grace".
//
// The adapter now gives its request (and two retries) a real window to work,
// then uses these shared host primitives when the exact pane is still live:
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
// The adapter's bounded retries run on a tighter cadence than this deadline: a
// batch is ~1.3 s of lock-held key spacing plus softExitRetryDelay (1.5 s)
// between batches, so attempts start roughly every 2.8 s and the last one
// that can matter lands around 8.4 s — a pane that honours it gets a short
// window to act before the ladder starts, and a harness whose exit takes
// longer than that from its final prompt will be escalated rather than
// waited for. That is the intended trade: the deadline also has to stay far
// below retireWorktreeExitGrace (60 s), which is what makes retire cleanup
// run at all, and a stop is a request to end the session rather than to
// negotiate about it.
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

// paneScreenTailLines and paneScreenTailClip bound what a pre-kill screen
// capture may add to one warn line. The clip is sized so a full 12-line tail
// survives at the canonical pane width (TCL-1136: 12 × 200-col lines plus
// separators ≈ 2.4 KB) — an undersized clip would eat the tail's HEAD, which
// is where the harness's last real output sits.
const (
	paneScreenTailLines = 12
	paneScreenTailClip  = 3000
)

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
