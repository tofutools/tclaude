package agentd_test

import (
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
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

	return testharness.NewFlow(t, w,
		agentd.BuildHandlerForTest(),
		agentd.AsHumanPeer,
		agentd.AsAgentPeer,
	)
}
