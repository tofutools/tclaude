package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
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
// and directory filled in. The form opens on the group field, so two tabs
// (past the profile picker) reach the name and another the directory.
func openTUISpawnForm(t *testing.T, c *agentd.TUIConsole, name, dir string) {
	t.Helper()
	c.Press(t, "n")
	c.Press(t, "tab", "tab")
	c.Type(t, name)
	c.Press(t, "tab")
	c.Type(t, dir)
}

// tuiAttachLog records where the console asked to send this terminal.
type tuiAttachLog struct {
	called  bool
	agent   string
	session string
	inTmux  bool
}

// stubTUIAttach swaps the console's terminal handover for a recorder: a flow
// test has no terminal to give away, but the target the console picks is real
// and worth asserting on. The returned command feeds the ordinary
// came-back-from-the-pane message in, so the console finishes the move.
func stubTUIAttach(t *testing.T) *tuiAttachLog {
	t.Helper()
	log := &tuiAttachLog{}
	t.Cleanup(agentd.SetTUIAttachForTest(func(agentName, tmuxSession string, inTmux bool) tea.Cmd {
		log.called = true
		log.agent, log.session, log.inTmux = agentName, tmuxSession, inTmux
		return nil
	}))
	return log
}

func TestTUIConsoleSpawnsAnAgentIntoAGroup(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	cwd := f.TestCwd("tui-spawn")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	attach := stubTUIAttach(t)

	c := newTUIConsole(t)
	c.Refresh()
	require.Contains(t, c.View(), "No agents yet")

	openTUISpawnForm(t, c, "reviewer", cwd)
	require.Contains(t, c.View(), "< dev >", "the group picker offers the daemon's groups")
	c.Press(t, "enter")

	view := c.View()
	assert.Contains(t, view, "Spawned", "the console reports the outcome")
	assert.Contains(t, view, "dev", "under the group it was spawned into")
	assert.False(t, c.Quit)

	// Starting an agent goes straight to its pane — the same handover enter
	// makes on its row, aimed at the session the daemon just created.
	require.Len(t, f.ListGroupMembers("dev"), 1)
	require.True(t, attach.called, "a landed spawn goes to the new agent's pane")
	assert.NotEmpty(t, attach.agent)
	assert.True(t, f.World.Tmux.IsAlive(attach.session),
		"and it is the live session the daemon just created: %q", attach.session)

	c.Refresh()
	assert.Contains(t, c.View(), "1 agents (1 online)", "and lists the new agent")
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

	attach := stubTUIAttach(t)

	c := newTUIConsole(t)
	c.Refresh()
	c.Press(t, "enter")

	require.True(t, attach.called, "enter on an online agent asks for its pane")
	assert.Equal(t, sp.TmuxSession, attach.session)
	assert.NotEmpty(t, attach.agent)
	assert.Equal(t, os.Getenv("TMUX") != "", attach.inTmux,
		"switch-client inside tmux, attach outside it")

	// An agent whose pane is gone has nothing to go to. Enter turns it back
	// on instead — through the daemon's own resume verb, so the agent really
	// is running again afterwards.
	attach.called = false
	f.MarkOffline(sp.TmuxSession)
	c.Refresh()
	require.Contains(t, c.View(), "offline")
	c.Press(t, "enter")
	assert.False(t, attach.called, "an offline agent is started, not attached to")
	assert.Contains(t, c.View(), "Started")
	assert.Contains(t, c.View(), "1 agents (1 online)", "and the listing shows it running again")
}

// Enter on an offline agent is the console's "turn this back on": it goes
// through the daemon's resume verb, in the directory and conversation the
// agent was last running.
func TestTUIConsoleEnterStartsAnOfflineAgent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	sp := f.Spawn("dev", "worker")
	f.MarkOffline(sp.TmuxSession)

	attach := stubTUIAttach(t)
	c := newTUIConsole(t)
	c.Refresh()
	require.Contains(t, c.View(), "offline")

	c.Press(t, "enter")

	assert.False(t, attach.called, "starting an agent does not take over this terminal")
	assert.Contains(t, c.View(), "Started worker")
	members := f.ListGroupMembers("dev")
	require.Len(t, members, 1, "resuming does not create a second member")

	// A second enter now has a live pane to go to.
	c.Press(t, "enter")
	assert.True(t, attach.called, "once it is running, enter goes to its pane")
	assert.True(t, f.World.Tmux.IsAlive(attach.session), "and the session is live: %q", attach.session)
}

// The spawn form's profile picker offers the daemon's saved profiles, and the
// one the operator lands on reaches the launch — asserted on the model the
// profile pins, which nothing else in this spawn asks for.
func TestTUIConsoleSpawnsWithTheChosenProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	rec := createProfile(t, f, map[string]any{"name": "haiku-kit", "harness": "claude", "model": "haiku"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	cwd := f.TestCwd("tui-profile")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	stubTUIAttach(t)

	c := newTUIConsole(t)
	c.Refresh()

	c.Press(t, "n")
	c.Press(t, "tab") // group → profile
	require.Contains(t, c.View(), "< (default) >", "the picker starts on the daemon's own chain")
	c.Press(t, "right")
	require.Contains(t, c.View(), "< haiku-kit >", "and offers the saved profiles")
	c.Press(t, "tab")
	c.Type(t, "reviewer")
	c.Press(t, "tab")
	c.Type(t, cwd)
	c.Press(t, "enter")

	require.Contains(t, c.View(), "Spawned", "the console reports the outcome")
	members := f.ListGroupMembers("dev")
	require.Len(t, members, 1)
	model, ok := f.World.SpawnModel(members[0].ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Equal(t, "haiku", model, "the chosen profile's model must reach the launch")
}

// A profile that selects a harness must not be overruled by a harness field
// the operator never touched — the form opens on "(default)", which is the
// only setting that leaves the daemon's chain free to apply the profile.
func TestTUIConsoleProfileHarnessSurvivesTheUntouchedHarnessField(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	rec := createProfile(t, f, map[string]any{"name": "other-harness-kit", "harness": "codex"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	// A directory name with nothing harness-shaped in it: the listing shows
	// the working directory, and a "codex" in the path would make the
	// assertion below pass for the wrong reason.
	cwd := f.TestCwd("tui-profile-harness")
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	stubTUIAttach(t)

	c := newTUIConsole(t)
	c.Refresh()

	c.Press(t, "n")
	c.Press(t, "tab") // group → profile
	c.Press(t, "right")
	require.Contains(t, c.View(), "< other-harness-kit >")
	require.Contains(t, c.View(), "Harness:   < (default) >",
		"the harness field must not pin one on the operator's behalf")
	c.Press(t, "tab")
	c.Type(t, "reviewer")
	c.Press(t, "tab")
	c.Type(t, cwd)
	c.Press(t, "enter")

	require.Contains(t, c.View(), "Spawned", "the console reports the outcome")
	members := f.ListGroupMembers("dev")
	require.Len(t, members, 1)
	sessions, err := db.FindSessionsByConvID(members[0].ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)
	assert.Equal(t, "codex", sessions[0].Harness,
		"the profile's harness must reach the launch, not the form's default")
}

// The spawn form starts on the group's own default directory, read off the
// real /v1/groups listing, so spawning into a subdirectory of it is a name
// typed onto the end rather than a whole path.
func TestTUIConsoleSpawnFormPrefillsTheGroupsDirectory(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	root := f.TestCwd("tui-group-dir")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "service"), 0o755))
	_, err := db.SetAgentGroupDefaultCwd("dev", root)
	require.NoError(t, err)
	stubTUIAttach(t)

	c := newTUIConsole(t)
	c.Refresh()

	c.Press(t, "n")
	// The field is narrower than a temp-dir path, so it shows the tail — which
	// is the end the operator types the subdirectory onto.
	require.Contains(t, c.View(), filepath.Base(root)+"/",
		"the directory field opens on the group's own")
	require.Contains(t, c.View(), "the group's directory")

	// The cursor sits at the end of the prefill, so the subdirectory is just
	// typed on.
	c.Press(t, "tab", "tab") // group → profile → name
	c.Type(t, "reviewer")
	c.Press(t, "tab") // → directory
	c.Type(t, "service")
	c.Press(t, "enter")

	require.Contains(t, c.View(), "Spawned", "the console reports the outcome")
	members := f.ListGroupMembers("dev")
	require.Len(t, members, 1)
	sessions, err := db.FindSessionsByConvID(members[0].ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, sessions)
	assert.Equal(t, filepath.Join(root, "service"), sessions[0].Cwd,
		"the spawn lands in the subdirectory of the group's directory")
}

// A group's default directory is a path on the operator's own filesystem, and
// /v1/groups is the one listing that asks nothing of its caller. An agent
// reading it gets the group, not the path.
func TestGroupsListServesTheDefaultDirToHumansOnly(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	_, err := db.SetAgentGroupDefaultCwd("dev", "/work/dev")
	require.NoError(t, err)

	human := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/groups", nil)))
	require.Equal(t, http.StatusOK, human.Code)
	assert.Contains(t, human.Body.String(), "/work/dev", "the operator reads back what they configured")

	agentSide := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/groups", nil), "peer-1111-2222-3333-444444444444"))
	require.Equal(t, http.StatusOK, agentSide.Code)
	assert.Contains(t, agentSide.Body.String(), `"dev"`, "the group itself is still listed")
	assert.NotContains(t, agentSide.Body.String(), "/work/dev", "but not the operator's directory")
}

// x retires the selected agent through the daemon's own retire verb: the
// demotion, the group exit and the pane shutdown are the daemon's, so this
// asserts on enrollment state and tmux liveness rather than on the console.
func TestTUIConsoleRetiresAnAgent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	sp := f.Spawn("dev", "worker")

	c := newTUIConsole(t)
	c.Refresh()
	require.Contains(t, c.View(), "1 agents (1 online)")

	// The prompt names the agent, and anything but "y" leaves it alone.
	c.Press(t, "x")
	require.Contains(t, c.View(), "and stop its session?")
	c.Press(t, "n")
	c.Refresh()
	state, err := db.AgentState(sp.ConvID)
	require.NoError(t, err)
	require.Equal(t, db.AgentStateActive, state, "a cancelled prompt retires nothing")
	require.True(t, f.World.Tmux.IsAlive(sp.TmuxSession))

	c.Press(t, "x", "y")

	view := c.View()
	assert.Contains(t, view, "Retired worker", "the console reports the outcome")
	assert.Contains(t, view, "left dev", "including the groups the agent gave up")
	assert.Contains(t, view, "No agents yet", "and the roster drops it right away")

	state, err = db.AgentState(sp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, state)
	assert.False(t, flowGroupHasMember(f, "dev", sp.ConvID), "a retired agent leaves its groups")
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
