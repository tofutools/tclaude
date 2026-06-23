package agentd_test

import (
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

	// Shrink the production waits to test-scale durations. Every flow test
	// runs inside a synctest bubble, so these sleeps collapse on the fake
	// clock either way; the shrink is kept because a couple of scenarios
	// (deferred worktree retirement) assert on the *relative* ordering of
	// post-init work, which the shrunk durations preserve. Exercising the
	// genuine 60s/1s production durations under the bubble is a follow-up
	// (it needs those few ordering-sensitive tests adjusted first).
	t.Cleanup(agentd.SetWaitTimingsForTest(300*time.Millisecond, 20*time.Millisecond))
	t.Cleanup(session.SetClearInjectTimingsForTest(300*time.Millisecond, 20*time.Millisecond))
	t.Cleanup(agentd.SetInjectSettleDelayForTest(time.Millisecond))
	t.Cleanup(agentd.SetRemoteControlConfirmDelayForTest(time.Millisecond))

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

	// Close the DB after the drain so the database/sql pool's
	// connectionOpener/Resetter goroutines exit. Registered before the
	// drain → runs after it (LIFO): goroutines finish their writes, then
	// the pool shuts down. Harmless under a real clock (the next test's
	// ResetForTest reopens from the template); REQUIRED under a synctest
	// bubble, where any goroutine still alive at bubble-exit is a deadlock.
	t.Cleanup(db.Close)

	// Drain any post-init goroutines (spawn rename+welcome, clone
	// rename) before the package-var restores and TempDir teardown run.
	// Registered last → runs first (LIFO), so the goroutines still see
	// the simulator-backed mocks and finish writing into $HOME/.tclaude
	// before RemoveAll, and before the db.Close above closes the pool.
	// WaitForBackgroundForTest advances the bubble's fake clock past the
	// post-init goroutines' bounded lifetimes so they exit (see its doc).
	t.Cleanup(agentd.WaitForBackgroundForTest)

	return testharness.NewFlow(t, w,
		agentd.BuildHandlerForTest(),
		agentd.AsHumanPeer,
		agentd.AsAgentPeer,
	)
}
