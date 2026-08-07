package session_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// corpseWorld installs TmuxSim as the tmux boundary and returns it. TmuxSim is
// used rather than a hand-rolled fake on purpose: it models tmux's unique-PREFIX
// target matching, so a launch-name probe that dropped clcommon.ExactTarget
// would reach a neighbouring session here exactly as it would in production. A
// fake that only string-matches cannot fail that way, which would make the
// "unrelated live session is untouched" assertion below vacuous.
func corpseWorld(t *testing.T) *testharness.TmuxSim {
	t.Helper()
	w := testharness.New(t)
	prev := clcommon.Default
	clcommon.Default = w.Tmux
	t.Cleanup(func() { clcommon.Default = prev })
	return w.Tmux
}

// listed reports whether tmux would still refuse the name — dead pane or not.
// That is the question new-session asks, and the one the fix turns on, so the
// assertions below test it rather than pane liveness.
func listed(tm *testharness.TmuxSim, name string) bool {
	for _, n := range tm.Sessions() {
		if n == name {
			return true
		}
	}
	return false
}

// markCorpse leaves name in the state an exited agent pane actually leaves
// behind: still LISTED (remain-on-exit keeps the session), with a dead pane.
func markCorpse(t *testing.T, tm *testharness.TmuxSim, name string, exitCode int) {
	t.Helper()
	tm.MarkAlive(name)
	tm.MarkPaneDead(name, &exitCode, "")
}

// Scenario: a resume whose previous generation left a remain-on-exit corpse
// must still get its canonical name.
//
// The regression this pins is operator-reported and looked intermittent:
//
//	Error: failed to create tmux session: exit status 1: duplicate session: 019fde64
//
// The name was chosen by asking "is an agent alive there?", which a dead pane
// correctly answers NO — while tmux, which only cares that the name is listed,
// then refused to create it. The resume died after the daemon had already
// launched the spawn, and a later attempt succeeded once something else reaped
// the corpse.
func TestUniqueTmuxSessionName_ReapsARemainOnExitCorpseHoldingTheName(t *testing.T) {
	tm := corpseWorld(t)
	markCorpse(t, tm, "019fde64", 1)

	assert.Equal(t, "019fde64", session.UniqueTmuxSessionName("019fde64"),
		"the canonical name must come back free; suffixing here would drift the agent onto -2, -3, ... for good")
	assert.False(t, listed(tm, "019fde64"), "the name is actually free afterwards")
}

// The reap is strictly for corpses: a name held by a LIVE agent falls through
// to the -N suffix and is never killed. Getting this wrong would take a session
// out from under a running agent, which is far worse than the bug being fixed.
func TestUniqueTmuxSessionName_NeverReapsALiveSession(t *testing.T) {
	tm := corpseWorld(t)
	tm.MarkAlive("019fde64")

	assert.Equal(t, "019fde64-2", session.UniqueTmuxSessionName("019fde64"),
		"a live holder must be worked around, not killed")
	assert.True(t, listed(tm, "019fde64"), "the running agent keeps its session")
}

// Mixed: the base is a corpse and the first suffix is LIVE. The base is reaped
// and reused, and the live neighbour — whose name the base is a strict PREFIX
// of — must survive. That is the assertion TmuxSim's prefix matching makes real.
func TestUniqueTmuxSessionName_PrefersTheReapedBaseAndSparesItsLiveNeighbour(t *testing.T) {
	tm := corpseWorld(t)
	markCorpse(t, tm, "019fde64", 0)
	tm.MarkAlive("019fde64-2")

	assert.Equal(t, "019fde64", session.UniqueTmuxSessionName("019fde64"))
	assert.False(t, listed(tm, "019fde64"), "the corpse is gone")
	assert.True(t, listed(tm, "019fde64-2"), "the unrelated live session is untouched")
}

// A name whose pane cannot be PROVEN dead is left alone. IsTmuxSessionAlive
// reports alive when it cannot read the pane, so an unreadable probe must
// suffix rather than reap — the reap is only ever entitled by positive
// evidence.
func TestUniqueTmuxSessionName_LeavesAnUnprovableSessionAlone(t *testing.T) {
	tm := corpseWorld(t)
	markCorpse(t, tm, "019fde64", 1)
	// IsTmuxSessionAlive's pane_dead probe is the first display-message the
	// launch-name check issues; failing it models a pane that cannot be read.
	tm.FailNextCommand("display-message")

	assert.Equal(t, "019fde64-2", session.UniqueTmuxSessionName("019fde64"),
		"without readable proof the pane is dead, the launch must work around the name")
	assert.True(t, listed(tm, "019fde64"), "nothing unproven is killed")
}
