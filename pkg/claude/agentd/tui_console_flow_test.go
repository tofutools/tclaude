package agentd_test

import (
	"os"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Flow coverage for the `tclaude agentd serve --tui` console. It drives the
// real console model against the real daemon mux, so these exercise the same
// listing and spawn handlers the CLI and the web dashboard go through — the
// console has no private path to either.

// newTUIConsole stands up the console with a deterministic identity: agentd's
// own process resolves to no harness ancestor (a plain operator shell), and a
// known operator token is live. Without the proc-tree stub the console would
// inherit whatever ancestry the `go test` binary happens to have — under a
// harness-run test suite that is a harness ancestor, which classify() rightly
// refuses, and the scenario would fail for a reason that has nothing to do
// with what it is testing.
func newTUIConsole(t *testing.T) *agentd.TUIConsole {
	t.Helper()
	t.Cleanup(agentd.SetProcTreeForTest(nil, nil))
	t.Cleanup(agentd.SetOperatorTokenForTest("tclo_tui_console_test"))
	c := agentd.NewTUIConsoleForTest()
	c.Resize(140, 30)
	return c
}

// openSpawnForm walks the console into the "new agent" prompt with the name
// and directory filled in. The form opens on the group field, so one tab
// reaches the name and another the directory.
func openTUISpawnForm(t *testing.T, c *agentd.TUIConsole, name, dir string) {
	t.Helper()
	c.Press(t, "n")
	c.Press(t, "tab")
	c.Type(t, name)
	c.Press(t, "tab")
	c.Type(t, dir)
}

func TestTUIConsoleSpawnsAnAgentIntoAGroup(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	cwd := f.TestCwd("tui-spawn")
	require.NoError(t, os.MkdirAll(cwd, 0o755))

	c := newTUIConsole(t)
	c.Refresh()
	require.Contains(t, c.View(), "No agents yet")

	openTUISpawnForm(t, c, "reviewer", cwd)
	require.Contains(t, c.View(), "< dev >", "the group picker offers the daemon's groups")
	c.Press(t, "enter")

	view := c.View()
	assert.Contains(t, view, "Spawned", "the console reports the outcome")
	assert.Contains(t, view, "1 agents (1 online)", "and lists the new agent")
	assert.Contains(t, view, "dev", "under the group it was spawned into")
	assert.False(t, c.Quit)
}

func TestTUIConsoleReportsASpawnTheDaemonRefuses(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")

	c := newTUIConsole(t)
	c.Refresh()

	openTUISpawnForm(t, c, "reviewer", "/nonexistent/tui/directory")
	c.Press(t, "enter")

	view := c.View()
	assert.Contains(t, view, "Spawn failed:", "the daemon's refusal reaches the operator")
	assert.Contains(t, view, "No agents yet", "and nothing was created")
}

// An existing agent shows up in the console's listing with the details the
// operator needs to tell agents apart.
func TestTUIConsoleListsExistingAgents(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	sp := f.Spawn("dev", "worker")

	c := newTUIConsole(t)
	c.Refresh()

	view := c.View()
	assert.Contains(t, view, "1 agents (1 online)")
	assert.Contains(t, view, "dev")

	// The agent goes offline when its pane dies, and the console says so
	// rather than keeping the last live status.
	f.MarkOffline(sp.TmuxSession)
	c.Refresh()
	assert.Contains(t, c.View(), "offline")
	assert.Contains(t, c.View(), "1 agents (0 online)")
}

// Enter goes to the selected agent's tmux session, the way `session watch`
// does. The handover itself is stubbed — a flow test has no terminal to give
// away — but the target comes from the real session rows the daemon wrote
// when it spawned the agent.
func TestTUIConsoleEnterGoesToTheAgentsTmuxSession(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	sp := f.Spawn("dev", "worker")

	var gotAgent, gotSession string
	var gotInTmux, called bool
	t.Cleanup(agentd.SetTUIAttachForTest(func(agentName, tmuxSession string, inTmux bool) tea.Cmd {
		called = true
		gotAgent, gotSession, gotInTmux = agentName, tmuxSession, inTmux
		return nil
	}))

	c := newTUIConsole(t)
	c.Refresh()
	c.Press(t, "enter")

	require.True(t, called, "enter on an online agent asks for its pane")
	assert.Equal(t, sp.TmuxSession, gotSession)
	assert.NotEmpty(t, gotAgent)
	assert.Equal(t, os.Getenv("TMUX") != "", gotInTmux,
		"switch-client inside tmux, attach outside it")

	// An agent whose pane is gone has nothing to go to, and the console says
	// so instead of handing the terminal to a dead session.
	called = false
	f.MarkOffline(sp.TmuxSession)
	c.Refresh()
	c.Press(t, "enter")
	assert.False(t, called)
	assert.Contains(t, c.View(), "no live tmux session")
}

// Quitting is confirmed first, because it shuts the daemon down.
func TestTUIConsoleConfirmsQuit(t *testing.T) {
	newFlow(t)

	c := newTUIConsole(t)
	c.Press(t, "q")
	assert.Contains(t, c.View(), "shut down agentd?")
	assert.False(t, c.Quit, "asking is not quitting")

	c.Press(t, "n")
	assert.False(t, c.Quit, "any other key cancels")

	c.Press(t, "q", "y")
	assert.True(t, c.Quit)
}
