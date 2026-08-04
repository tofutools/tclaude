package agentd_test

import (
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// newFlow stands up a Flow with the default mocks installed. Every
// flow scenario in this package starts with `f := newFlow(t)`.
//
// Mock installation is direct package-var assignment or an existing test
// cleanup seam — pure Go, no toolchain dependency, runs under bare `go test`.
// clcommon.Default and agentd.Spawn stop at the subprocess boundaries;
// SandboxLayerSim stops at the paired host-capability probe seam. The daemon's
// code paths run unchanged but observe deterministic simulator state.
//
// Callers that want to override a mock further (e.g. count resume
// invocations) can shadow by another assignment right after this
// returns; the t.Cleanup we install here will still restore the
// original LiveSpawner / LiveTmux at end of test.
func newFlow(t *testing.T) *testharness.Flow {
	t.Helper()
	agentd.ResetDeliveryDebounceForTest()

	// The pending approval registry is a package global; reset it so a prior
	// test's pending approvals don't leak into this one's access-requests
	// snapshot. Handled history lives in the per-test DB.
	agentd.ResetApprovalsForTest()

	// Same for the name-derived spawn-label reservations: a process-wide set
	// that outlives the per-scenario DB, so without this a second scenario
	// spawning the same agent name would see it already claimed.
	agentd.ResetSpawnLabelsForTest()

	// The shared LiveTmuxSessions cache (TCL-370) is a daemon-wide global.
	// Set its TTL to 0 for the scenario so every handler re-probes and sees
	// live sim state — a test that flips tmux liveness mid-scenario must not
	// read a stale cached snapshot. The coalescing test opts back into a
	// positive TTL. Cleanup restores the production TTL for the next binary.
	t.Cleanup(agentd.SetTmuxCacheTTLForTest(0))

	// Shrink the production waits to test-scale durations. Production
	// uses 60s alive-timeout + 1s ready-delay to absorb CC startup
	// jitter; under simulator-backed tests the new conv is alive
	// instantly, so the long timing only ever makes test cleanup wait.
	// Worst case (scenario never brings conv online) the post-init
	// goroutine now bails in 200ms instead of 60s.
	t.Cleanup(agentd.SetWaitTimingsForTest(300*time.Millisecond, 20*time.Millisecond))
	// Mirror the shrink on the session-side /clear inject knobs — same
	// "wait for CC's TUI to settle" tax the simulator has no jitter
	// for. Without this, every /clear flow scenario sits on the 1s
	// production ready-delay.
	t.Cleanup(session.SetClearInjectTimingsForTest(300*time.Millisecond, 20*time.Millisecond))
	// And the agentd-side injectTextAndSubmit settle gap (500ms × 2 per
	// call). The simulator processes keystrokes synchronously, so this is
	// pure dead wait — every soft /exit, /rename, welcome and nudge paid
	// ~1s of it. 1ms keeps the two send-keys ordered without the sleep.
	t.Cleanup(agentd.SetInjectSettleDelayForTest(time.Millisecond))
	// Likewise the remote-control disable-confirm pause (700ms in prod).
	t.Cleanup(agentd.SetRemoteControlConfirmDelayForTest(time.Millisecond))
	t.Cleanup(agentd.SetOpenCodeRuntimeForTest(func(sessionID, _, _, _ string) (agentd.OpenCodeRuntimeFixture, error) {
		return agentd.OpenCodeRuntimeFixture{
			SessionID: sessionID,
			ConvID:    "ses_" + sessionID,
			ServerURL: "http://127.0.0.1:43210",
			Password:  "test-password",
			PID:       1234,
		}, nil
	}))
	// And the background soft-exit retry's per-attempt wait (a few seconds
	// in prod). The simulator honours /exit synchronously, so this is pure
	// dead wait that every stop/retire/reincarnate flow — and the
	// WaitForBackgroundForTest drain — would otherwise pay.
	t.Cleanup(agentd.SetSoftExitRetryDelayForTest(time.Millisecond))
	// Delivered-but-not-yet-observed exits retain their lifecycle intent for a
	// production-scale reaper window. Keep that policy testable without making
	// background drains wait for the production duration.
	t.Cleanup(agentd.SetUnknownIntentCleanupDelayForTest(5 * time.Millisecond))
	// And the escalation ladder's waits (10s deadline / 2s per signal step in
	// prod). A simulator pane honours its soft exit immediately, so the
	// watchdog's first probe already finds it gone; the shrink is what keeps
	// the scenarios that DO wedge a pane from paying production seconds.
	t.Cleanup(agentd.SetSoftExitEscalationTimingForTest(
		20*time.Millisecond, 10*time.Millisecond, time.Millisecond))
	// NEUTRALIZE THE ESCALATION LADDER'S TWO OS-PROCESS RUNGS. This one is a
	// safety default, not a speed shrink.
	//
	// The ladder is tmux kill-pane, then SIGTERM to the pane process GROUP,
	// then SIGKILL. Only the first is simulated; the other two were the real
	// syscalls in every scenario that did not opt out. The simulator hands its
	// first session pane pid 1, so a scenario that let the ladder past
	// kill-pane resolved pgid 1 and called kill(-1, …) — and -1 is not "an
	// unlikely pid", it is the kernel's wildcard for every process the caller
	// may signal. That kills the test binary, the `go test` parent, and on a
	// developer's or agent's machine their shell and harness (TCL-1035).
	//
	// Worse than the blast radius: a SIGTERMed process writes no failure and
	// no stack, so the whole event reads as infrastructure trouble rather than
	// as a test doing it.
	//
	// alive=false stands the ladder down after kill-pane, so a scenario that
	// does not opt in cannot reach the signal rungs at all. The scenarios that
	// DO exercise them install their own pair after this one and assert on it;
	// their cleanup restores this neutral pair, then ours restores production.
	t.Cleanup(agentd.SetSoftExitEscalationProcessForTest(
		func(int) bool { return false },
		func(int, syscall.Signal) error { return nil },
	))
	// Neutralize the post-focus auto-tiling pass by default: a bulk focus
	// now runs a tiling gate, and no flow test should read the developer's
	// real config.json or move a real OS window as a side effect of one.
	// Off + no-op dispatch + no-op settle wait keeps every focus scenario
	// hermetic; a test that exercises tiling re-swaps these (its later
	// Cleanup restores to this neutral pair, then ours restores production).
	t.Cleanup(agentd.SetTileConfigForFocusForTest(func() (bool, session.TileOptions) {
		return false, session.TileOptions{}
	}))
	t.Cleanup(agentd.SetTileAgentWindowsForTest(func([]session.TileSpec, session.TileOptions) {}))
	t.Cleanup(agentd.SetTileSettleWaitForTest())

	w := testharness.New(t)
	m := w.DefaultMocks(t)

	// Swap the package-wide tmux + spawner with the simulator-backed
	// fakes. t.Cleanup restores the production singletons so the next
	// test starts clean.
	prevTmux := clcommon.Default
	clcommon.Default = m.Tmux
	t.Cleanup(func() { clcommon.Default = prevTmux })

	prevSpawn := agentd.Spawn
	agentd.Spawn = m.Spawner
	t.Cleanup(func() { agentd.Spawn = prevSpawn })

	// Host capability is independent for interactive panes (which need the
	// terminal relay) and relay-free servers. The default sim makes both
	// available without probing this test runner's bwrap/Seatbelt; scenarios
	// can name one unavailable boundary and assert production's exact refusal.
	t.Cleanup(agentd.SetTclaudeLayerHostAvailabilitiesForTest(
		m.SandboxLayer.InteractiveAvailability,
		m.SandboxLayer.ServerAvailability,
	))

	// Drain any post-init goroutines (spawn rename+welcome, clone
	// rename) before the package-var restores and TempDir teardown
	// run. Registered last → runs first (LIFO), so the goroutines
	// still see the simulator-backed mocks and finish writing into
	// $HOME/.tclaude before RemoveAll, and complete before the next
	// test's db.ResetForTest races them inside db.Open's sync.Once.
	t.Cleanup(agentd.WaitForBackgroundForTest)

	return testharness.NewFlow(t, w,
		agentd.BuildHandlerForTest(),
		agentd.AsHumanPeer,
		agentd.AsAgentPeer,
	)
}

// holdRetiringPane wedges a retiring agent's /exit and hands the moment of its
// death to the test, deterministically. The returned func ends the pane; call
// it once the scenario's drift is in place, and ALWAYS before draining the
// background — the parked watchdog holds a background slot, so a drain that
// precedes the release blocks until the go test timeout.
//
// Scenarios that watch what happens BETWEEN a soft exit and the work it
// unblocks need a pane that is still alive when the retire response is written
// and that exits only after the test says so. Wedging /exit in the simulator
// (TCL-760) is half of that: it stops the simulator from ending the pane on its
// own. It stopped being the whole of it once the soft-exit escalation ladder
// landed (TCL-1001, 40dc0c504) — a pane that ignores /exit is precisely the
// pane that watchdog exists to kill, and flow tests shrink its deadline to 20ms
// (SetSoftExitEscalationTimingForTest above). That kill ends the pane on a
// timer, so a deferred retire cleanup's exit-wait could be satisfied — and its
// live-claimant snapshot taken — before the test had installed the drift it
// asserts about. That is TCL-1019, and TCL-760's identical failure text.
//
// So the ladder's probe loop blocks until the release. Pane death then has
// exactly one cause here: the fixture asking for it, with no wall-clock budget
// on either side. Tests that assert on the ladder itself must NOT use this —
// they wedge /exit directly and let the watchdog run.
func holdRetiringPane(t *testing.T, cc *testharness.CCSim) func() {
	t.Helper()
	require.NotNil(t, cc, "no CCSim to hold")
	cc.OnInput("/exit", func(*testharness.CCSim, string) bool {
		return true
	})
	release := holdSoftExitEscalation(t)
	return func() {
		cc.MarkDead()
		release()
	}
}

// holdRetiringCodexPane is holdRetiringPane for a Codex pane, whose exit
// command is /quit.
func holdRetiringCodexPane(t *testing.T, cx *testharness.CodexSim) func() {
	t.Helper()
	require.NotNil(t, cx, "no CodexSim to hold")
	cx.OnInput("/quit", func(*testharness.CodexSim, string) bool {
		return true
	})
	release := holdSoftExitEscalation(t)
	return func() {
		cx.MarkDead()
		release()
	}
}

// escalationWitness observes whether the soft-exit escalation watchdog has
// reached its first probe, and lets a scenario SAMPLE that at one chosen
// instant.
//
// The park hook fires at the top of waitForLifecycleTargetGone's loop, one
// statement before probeLifecyclePane, so "probed" is exactly "a second issuer
// of the pane-probe verbs now exists". A scenario whose fault is queued inside
// the SYNCHRONOUS injection depends on there being no such issuer yet —
// scheduleSoftExitEscalation is not called until injectSoftExitTarget returns
// — and that is a property of call ORDER, which is the kind of property
// TCL-1028 showed can hold by luck and stop holding silently. Sampling it
// where the fault is queued, and asserting it false, makes a future
// re-ordering fail the test instead of recreating the bug.
type escalationWitness struct {
	mu      sync.Mutex
	probed  bool
	sampled bool
}

func (w *escalationWitness) markProbed() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.probed = true
}

// sample is called from a production-goroutine hook; probedWhenSampled from
// the test goroutine. Hence the mutex — this package runs under -race.
func (w *escalationWitness) sample() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.sampled = w.probed
}

func (w *escalationWitness) probedWhenSampled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sampled
}

// holdSoftExitEscalation parks the escalation watchdog on its first probe
// until the returned func runs, so nothing but the caller can end a wedged
// pane. Only the watchdog's own wait is gated; the ladder's behaviour once it
// runs is untouched.
func holdSoftExitEscalation(t *testing.T) func() {
	t.Helper()
	return holdSoftExitEscalationInto(t, nil)
}

// holdSoftExitEscalationInto is holdSoftExitEscalation, additionally recording
// each probe against w.
//
// REGISTER ANY OTHER HOOK CLEANUP BEFORE CALLING THIS. Cleanup is LIFO, so
// this helper's cleanup — which releases the parked watchdog and only then
// drains — has to run FIRST. A cleanupAfterBackgroundDrain registered after it
// would try to drain while the watchdog is still parked, and block forever.
func holdSoftExitEscalationInto(t *testing.T, w *escalationWitness) func() {
	t.Helper()
	released := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(released) }) }
	restore := agentd.SetSoftExitEscalationPollForTest(func() {
		if w != nil {
			w.markProbed()
		}
		<-released
	})
	// One cleanup, in this order, because all three steps matter on the path
	// where a require fails before the test releases: unpark the watchdog so
	// nothing is left blocked on a finished test, JOIN it, and only then
	// restore the hook — restoring while the loop is still reading it is a
	// data race that would fail the package under -race and bury the real
	// assertion failure.
	t.Cleanup(func() {
		release()
		restoreAfterBackgroundDrain(restore)
	})
	return release
}

// restoreAfterBackgroundDrain joins the background before putting a test hook
// back, and is the whole of what a NON-PARKING hook needs at cleanup.
//
// The hooks in this package are plain package-level func vars: a background
// goroutine reads one while the test's cleanup writes it, with no
// synchronization on either side. On the happy path the test has already
// called WaitForBackgroundForTest, so there is no overlap and no race. The
// exposure is the failure path — a require failing before that join aborts the
// test, cleanup restores immediately, and the goroutine may still be reading.
// Under -race that reports as a data race for the whole package and BURIES the
// real assertion failure, so a genuine test failure arrives as noise in
// unrelated output.
//
// Two steps, not the three in holdSoftExitEscalation above. That one needs a
// release first because its hook PARKS on a channel, so joining before
// releasing would block until the go test timeout. A hook that returns on its
// own has nothing to release, and adding a release with no hold behind it
// would copy the shape of a sibling fix instead of fixing the site — and would
// teach the next reader that three steps is the pattern here. It is not; the
// release is a consequence of parking.
func restoreAfterBackgroundDrain(restore func()) {
	agentd.WaitForBackgroundForTest()
	restore()
}

// cleanupAfterBackgroundDrain registers restoreAfterBackgroundDrain, and is
// what a test installing a hook should reach for. It exists so the correct
// ordering is the SHAPE a new site copies: the broken version and the correct
// one differ only by which function the restore is handed to, and a plain
// t.Cleanup(restore) looks entirely reasonable right up until a require fails.
//
// Registration order matters and is why this takes t rather than being folded
// into the setter. Cleanups run LIFO, so a hook installed AFTER newFlow gets
// its restore run BEFORE newFlow's own drain — which is exactly the window
// this closes. A hook installed BEFORE newFlow is already safe for the
// opposite reason, and that is load-bearing rather than obvious: see
// TestRetire_DeleteWorktreeUnkillableAgentPostsKeptNotice.
func cleanupAfterBackgroundDrain(t *testing.T, restore func()) {
	t.Helper()
	t.Cleanup(func() { restoreAfterBackgroundDrain(restore) })
}
