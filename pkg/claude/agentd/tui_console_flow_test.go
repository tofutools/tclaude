package agentd_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Flow coverage for the `tclaude agentd serve --tui` console. It drives the
// real console model against the real daemon mux, so these exercise the same
// listing and spawn handlers the CLI and the web dashboard go through — the
// console has no private path to either.

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
	t.Cleanup(agentd.SetOperatorTokenForTest("tclo_tui_console_test"))
	f.HaveGroup("dev")
	cwd := f.TestCwd("tui-spawn")
	require.NoError(t, os.MkdirAll(cwd, 0o755))

	c := agentd.NewTUIConsoleForTest()
	c.Resize(140, 30)
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
	t.Cleanup(agentd.SetOperatorTokenForTest("tclo_tui_console_test"))
	f.HaveGroup("dev")

	c := agentd.NewTUIConsoleForTest()
	c.Resize(140, 30)
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
	t.Cleanup(agentd.SetOperatorTokenForTest("tclo_tui_console_test"))
	f.HaveGroup("dev")
	sp := f.Spawn("dev", "worker")

	c := agentd.NewTUIConsoleForTest()
	c.Resize(140, 30)
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

// Quitting is confirmed first, because it shuts the daemon down.
func TestTUIConsoleConfirmsQuit(t *testing.T) {
	newFlow(t)
	t.Cleanup(agentd.SetOperatorTokenForTest("tclo_tui_console_test"))

	c := agentd.NewTUIConsoleForTest()
	c.Resize(140, 30)
	c.Press(t, "q")
	assert.Contains(t, c.View(), "shut down agentd?")
	assert.False(t, c.Quit, "asking is not quitting")

	c.Press(t, "n")
	assert.False(t, c.Quit, "any other key cancels")

	c.Press(t, "q", "y")
	assert.True(t, c.Quit)
}
