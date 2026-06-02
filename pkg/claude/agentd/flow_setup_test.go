package agentd_test

import (
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// newFlow stands up a Flow with the default mocks installed. Every
// flow scenario in this package starts with `f := newFlow(t)`.
//
// Mock installation is direct package-var assignment with t.Cleanup
// restoring the previous value — pure Go, no toolchain dependency,
// runs under bare `go test`. clcommon.Default and agentd.Spawn are
// the two boundary handles every production caller routes through;
// swap them and the daemon's code paths run unchanged but observe
// the simulator's state machine instead of real subprocesses.
//
// Callers that want to override a mock further (e.g. count resume
// invocations) can shadow by another assignment right after this
// returns; the t.Cleanup we install here will still restore the
// original LiveSpawner / LiveTmux at end of test.
func newFlow(t *testing.T) *testharness.Flow {
	t.Helper()

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
