//go:build rewire

package agentd_test

import (
	"testing"

	"github.com/GiGurra/rewire/pkg/rewire"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// newFlow stands up a Flow with the default mocks rewired. Every flow
// scenario in this package starts with `f := newFlow(t)` — the rewire
// installs are centralised here both for readability and because
// rewire's _test.go scanner walks for rewire.Func calls and they have
// to live in `_test.go` files.
//
// Callers that want to override a mock (e.g. count resume invocations)
// can shadow with another rewire.Func right after this returns; later
// installs win because rewire keys on the function name.
func newFlow(t *testing.T) *testharness.Flow {
	t.Helper()
	w := testharness.New(t)
	m := w.DefaultMocks(t)

	rewire.Func(t, clcommon.TmuxCommand, m.TmuxCmd)
	rewire.Func(t, agentd.SpawnDetachedTclaudeNew, m.SpawnNew)
	rewire.Func(t, agentd.SpawnDetachedTclaudeResume, m.SpawnResume)

	return testharness.NewFlow(t, w,
		agentd.BuildHandlerForTest(),
		agentd.AsHumanPeer,
		agentd.AsAgentPeer,
	)
}
