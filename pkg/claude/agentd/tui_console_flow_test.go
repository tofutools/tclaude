package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
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
	require.Contains(t, c.View(), "No agents or sessions yet")

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
	assert.Contains(t, view, "No agents or sessions yet", "and nothing was created")
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

func TestTUIRemoteAttachUsesANonDisplacingTmuxClient(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	spawned := f.Spawn("dev", "worker")

	var command, teardownSession string
	t.Cleanup(agentd.SetTermWSHookForTest(&agentd.TermWSHook{
		RewriteCommand: func(gotCommand, gotSession string) (string, string) {
			command, teardownSession = gotCommand, gotSession
			return gotCommand, gotSession
		},
	}))
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://agent-host:8321"))

	dashboard := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dashboard, httptest.NewRequest(
		http.MethodGet,
		"http://agent-host:8321/api/tui/attach-ws/"+spawned.ConvID,
		nil,
	))

	assert.NotEqual(t, http.StatusNotFound, rec.Code,
		"the enrolled live agent must resolve before the recorder rejects the websocket upgrade")
	require.NotEmpty(t, command, "status=%d body=%s", rec.Code, rec.Body.String())
	assert.Contains(t, command, "exec tmux")
	assert.Contains(t, command, "attach-session")
	assert.Contains(t, command, spawned.TmuxSession)
	assert.False(t, strings.Contains(command, "attach-session -d"),
		"a remote viewer must not displace another attached operator")
	assert.Empty(t, teardownSession,
		"closing this stream must not detach every other client on the session")
}

// Unlike the dashboard's browser terminals, this stream lands on the operator's
// REAL terminal — the remote TUI writes the bytes to its own raw-mode stdout —
// so tmux must not be told the far end renders OSC 8. Nothing here knows what
// emulator the operator is running, and the browser terminals' opt-in must not
// drift into this shared PTY path.
func TestTUIRemoteAttachDoesNotForceHyperlinksOnANativeTerminal(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	spawned := f.Spawn("dev", "worker")

	var command string
	t.Cleanup(agentd.SetTermWSHookForTest(&agentd.TermWSHook{
		RewriteCommand: func(gotCommand, gotSession string) (string, string) {
			command = gotCommand
			return gotCommand, gotSession
		},
	}))
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://agent-host:8321"))

	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), httptest.NewRequest(
		http.MethodGet,
		"http://agent-host:8321/api/tui/attach-ws/"+spawned.ConvID,
		nil,
	))
	require.NotEmpty(t, command, "status=%d body=%s", rec.Code, rec.Body.String())

	assert.NotContains(t, command, "-T hyperlinks",
		"a native terminal must keep tmux's own capability detection: %q", command)
	assert.NotContains(t, command, clcommon.TmuxClientFeaturesEnv,
		"the browser terminals' feature opt-in must not reach this path: %q", command)
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

// Delete moves an agent one step toward removal through the daemon's own
// lifecycle verbs: live → offline, then offline → retired.
func TestTUIConsoleDeleteStepsAnAgentOfflineThenRetiresIt(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	sp := f.Spawn("dev", "worker")

	c := newTUIConsole(t)
	c.Refresh()
	require.Contains(t, c.View(), "1 agents (1 online)")

	// The prompt names the agent, and anything but "y" leaves it alone.
	c.Press(t, "delete")
	require.Contains(t, c.View(), "Take worker offline?")
	c.Press(t, "n")
	c.Refresh()
	state, err := db.AgentState(sp.ConvID)
	require.NoError(t, err)
	require.Equal(t, db.AgentStateActive, state, "a cancelled prompt changes nothing")
	require.True(t, f.World.Tmux.IsAlive(sp.TmuxSession))

	c.Press(t, "delete", "y")
	view := c.View()
	assert.Contains(t, view, "Asked worker to go offline", "the console reports the first step")
	assert.Contains(t, view, "1 agents (0 online)", "the roster keeps the now-offline agent")
	state, err = db.AgentState(sp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state, "taking an agent offline does not retire it")
	assert.True(t, flowGroupHasMember(f, "dev", sp.ConvID), "the offline agent stays in its group")

	// If another client resumes the agent while this confirmation is open,
	// the daemon-side precondition refuses the stale retire rather than
	// skipping Delete's online → offline step.
	c.Press(t, "delete")
	require.Contains(t, c.View(), "Retire worker?")
	f.AssertResumeSpawned(f.AsHuman().Resume(sp.ConvID))
	c.Press(t, "y")
	view = c.View()
	assert.Contains(t, view, "agent is online")
	assert.Contains(t, view, "take it offline before")
	state, err = db.AgentState(sp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateActive, state)
	assert.True(t, flowGroupHasMember(f, "dev", sp.ConvID))

	// Refresh to the externally resumed state, then take the two deliberate
	// lifecycle steps again.
	c.Refresh()
	c.Press(t, "delete", "y")
	c.Press(t, "delete", "y")
	view = c.View()
	assert.Contains(t, view, "Retired worker", "the console reports the outcome")
	assert.Contains(t, view, "left dev", "including the groups the agent gave up")
	assert.Contains(t, view, "No agents or sessions yet", "and the roster drops it right away")

	state, err = db.AgentState(sp.ConvID)
	require.NoError(t, err)
	assert.Equal(t, db.AgentStateRetired, state)
	assert.False(t, flowGroupHasMember(f, "dev", sp.ConvID), "a retired agent leaves its groups")
}

// haveLiveShellSession puts a plain, non-agent session on the host the way
// `tclaude session new --shell` (and the console's own s key) leaves one: a
// session row with no conversation, and a live tmux session to match.
func haveLiveShellSession(t *testing.T, f *testharness.Flow, handle, cwd string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(cwd, 0o755))
	require.NoError(t, session.SaveSessionState(&session.SessionState{
		ID:          handle,
		TmuxSession: handle,
		Cwd:         cwd,
		Status:      session.StatusRunning,
		Harness:     session.ShellHarnessName,
		Created:     time.Now(),
	}))
	f.World.Tmux.MarkAlive(handle)
}

// Scenario: the operator has a shell session going beside their agents — one
// they started with s, or from another terminal with `tclaude session new`.
// The console lists it, marks it as not an agent, and enter goes to it.
func TestTUIConsoleListsAndGoesToANonAgentSession(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	sp := f.Spawn("dev", "worker")
	haveLiveShellSession(t, f, "scratch", f.TestCwd("tui-shell"))
	attach := stubTUIAttach(t)

	c := newTUIConsole(t)
	c.Refresh()

	view := c.View()
	assert.Contains(t, view, "1 agents (1 online)")
	assert.Contains(t, view, "1 sessions", "the shell session is counted apart from the agents")
	assert.Contains(t, view, "scratch")
	assert.Contains(t, view, "(session)", "and is marked as not being an agent")

	// The agent's own session is not re-listed as a plain one: its pane
	// belongs to a conversation the agent listing already owns. One marker on
	// screen means one session row, whatever the agent's own handle renders as.
	assert.Equal(t, 1, strings.Count(view, "(session)"),
		"exactly one row is marked a session: %s", view)
	require.NotEmpty(t, sp.TmuxSession)

	// Down off the agent row and onto the session: enter hands this terminal
	// to its pane, exactly as it does on a live agent's row.
	c.Press(t, "down")
	c.Press(t, "enter")
	require.True(t, attach.called, "enter on a session goes to its pane")
	assert.Equal(t, "scratch", attach.session)
}

// Delete on a session ends it: there is no offline step to take first and
// nothing to retire, so the one move is killing the pane — after a
// confirmation, like every other lifecycle key.
func TestTUIConsoleDeleteEndsANonAgentSession(t *testing.T) {
	f := newFlow(t)
	haveLiveShellSession(t, f, "scratch", f.TestCwd("tui-shell-kill"))

	c := newTUIConsole(t)
	c.Refresh()
	require.Contains(t, c.View(), "scratch")

	// Anything but "y" leaves the session running.
	c.Press(t, "delete")
	require.Contains(t, c.View(), "Kill session scratch?")
	c.Press(t, "n")
	require.True(t, f.World.Tmux.IsAlive("scratch"), "a cancelled prompt changes nothing")

	c.Press(t, "delete", "y")
	view := c.View()
	assert.Contains(t, view, "Ended session scratch")
	assert.False(t, f.World.Tmux.IsAlive("scratch"), "the pane is gone")
	assert.Contains(t, view, "0 sessions", "and the row leaves the listing with it")
}

// A `tclaude session new` is a coding harness with a real conversation, but it
// is still not an agent: no group, no permissions, nothing the agent API
// describes. It belongs in the listing as a session — and it stops being one
// the moment it is enrolled, rather than being listed twice.
func TestTUIConsoleListsAPlainCodingSessionUntilItBecomesAnAgent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	conv := "5b6f2f0e-1111-4222-8333-444444444444"
	f.HaveAliveSession(conv, "solo", "cc-solo", f.TestCwd("tui-solo"))

	c := newTUIConsole(t)
	c.Refresh()
	view := c.View()
	assert.Contains(t, view, "cc-solo")
	assert.Contains(t, view, "(session)", "an unenrolled session is not an agent")
	assert.Contains(t, view, "0 agents (0 online)")
	assert.Contains(t, view, "1 sessions")

	// Joining a group makes it an actor: the agent listing owns it now, and it
	// must not also appear as a plain session.
	f.HaveMember("dev", conv)
	c.Refresh()
	view = c.View()
	assert.Contains(t, view, "1 agents (1 online)")
	assert.Contains(t, view, "0 sessions", "one pane, one row")
	assert.NotContains(t, view, "(session)")
}

// A clone's session row and pane exist before its conversation is linked to an
// actor, so the launch holds an in-flight claim to keep the console from
// listing — and offering to kill — a materialising agent as a plain session.
// That claim is handed from cloneSpawnOnce to its caller, which makes
// releasing it somebody else's job: a caller that forgets strands it, and the
// clone's pane would then be missing from every surface that consults it.
func TestTUIConsoleCloneLeavesNoStrandedLaunchClaim(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	sp := f.Spawn("dev", "worker")
	require.Equal(t, 0, agentd.AgentLaunchClaimsForTest(), "a settled spawn holds nothing")

	cloned := f.CloneFresh(sp.ConvID)
	require.NotEmpty(t, cloned.NewConv)
	assert.Equal(t, 0, agentd.AgentLaunchClaimsForTest(),
		"a finished clone must release the claim its launch held")

	// And the clone is an agent on the console, never a plain session.
	c := newTUIConsole(t)
	c.Refresh()
	view := c.View()
	assert.Contains(t, view, "2 agents (2 online)")
	assert.Contains(t, view, "0 sessions")
	assert.NotContains(t, view, "(session)")
}

// A session whose pane has gone is not listed: there is nothing to go to and
// no resume verb behind it. `tclaude session ls -a` is where those live.
func TestTUIConsoleListsLiveSessionsOnly(t *testing.T) {
	f := newFlow(t)
	haveLiveShellSession(t, f, "scratch", f.TestCwd("tui-shell-dead"))

	c := newTUIConsole(t)
	c.Refresh()
	require.Contains(t, c.View(), "scratch")

	f.World.Tmux.MarkOffline("scratch")
	c.Refresh()
	view := c.View()
	assert.NotContains(t, view, "scratch")
	assert.Contains(t, view, "No agents or sessions yet")
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

// Scenario: the SQLite usage_cache carries a fresh subscription reading — the
// row Claude Code's statusline callback leaves behind — and the operator is
// looking at the terminal console rather than the web dashboard. The console's
// status line must show the same figures the dashboard's top bar does, read
// through the daemon's own /v1/usage handler.
func TestTUIConsoleShowsTheAccountsUsageLimits(t *testing.T) {
	newFlow(t)
	now := time.Now()
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 42, ResetsAt: now.Add(3*time.Hour + 41*time.Minute)},
		SevenDay:      &usageapi.CachedBucket{Pct: 18, ResetsAt: now.Add(2*24*time.Hour + 9*time.Hour)},
		FetchedAt:     now,
		LastAttemptAt: now,
	})

	c := newTUIConsole(t)
	c.RefreshUsage()

	view := c.View()
	assert.Contains(t, view, "usage")
	assert.Contains(t, view, "5h")
	assert.Contains(t, view, "42%")
	assert.Contains(t, view, "(3h40m)", "the daemon's own reset timer, counting down")
	assert.Contains(t, view, "7d")
	assert.Contains(t, view, "18%")
	assert.Contains(t, view, "(2d8h)")
}

// The readout is the operator's own subscription, so the daemon serves it to
// the operator only. A console it does not classify as the human is refused,
// and shows no figures rather than someone else's — or a fabricated blank.
func TestTUIConsoleUsageIsRefusedForANonOperatorConsole(t *testing.T) {
	newFlow(t)
	now := time.Now()
	seedUsageCache(t, usageapi.CachedUsage{
		FiveHour:      &usageapi.CachedBucket{Pct: 42, ResetsAt: now.Add(time.Hour)},
		FetchedAt:     now,
		LastAttemptAt: now,
	})

	// No live operator token: the console falls back to an unconfirmed caller,
	// which is what the daemon's own verifier makes of it.
	t.Cleanup(agentd.SetProcTreeForTest(nil, nil))
	t.Cleanup(agentd.SetOperatorTokenForTest(""))
	c := agentd.NewTUIConsoleForTest()
	c.Resize(140, 30)
	c.RefreshUsage()

	view := c.View()
	assert.NotContains(t, view, "42%")
	assert.NotContains(t, view, "usage ")
}

// The spawn form's worktree picker, end to end: the console cuts a real git
// worktree through the daemon and the agent launches inside it. The branch is
// never typed — it follows the name, which is the point of the field.
func TestTUIConsoleSpawnsIntoANewWorktree(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	repo, parent := initRepoOnMain(t)
	stubTUIAttach(t)

	c := newTUIConsole(t)
	c.Refresh()

	openTUISpawnForm(t, c, "reviewer", repo)
	// Directory → Worktree, then turn the picker onto "create new worktree".
	// Down rather than tab: on a directory the operator has typed, tab is
	// path completion (see the Directory contract).
	c.Press(t, "down", "right")
	view := c.View()
	require.Contains(t, view, "create new worktree")
	require.Contains(t, view, "reviewer", "the branch arrives carrying the name")

	c.Press(t, "enter")

	// A real worktree, on the branch the name gave it.
	wantPath := filepath.Join(parent, "repo-reviewer")
	info, err := os.Stat(wantPath)
	require.NoErrorf(t, err, "worktree should exist at %s; console said: %s", wantPath, c.View())
	assert.True(t, info.IsDir())
	wts, err := worktree.ListWorktreesIn(repo)
	require.NoError(t, err)
	var branches []string
	for _, wt := range wts {
		branches = append(branches, wt.Branch)
	}
	assert.Contains(t, branches, "reviewer")

	// And the agent launched inside it, not in the repo it was cut from.
	members := f.ListGroupMembers("dev")
	require.Len(t, members, 1)
	rows, err := db.FindSessionsByConvID(members[0].ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	assert.Equal(t, wantPath, rows[0].Cwd)

	assert.Contains(t, c.View(), "Spawned")
	assert.Contains(t, c.View(), wantPath, "and the console says where it landed")
}

// A directory that is not a git repo cannot produce a worktree. The form stays
// open on the fields that produced the failure, and nothing is spawned.
func TestTUIConsoleWorktreeFailureKeepsTheFormOpen(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("dev")
	cwd := f.TestCwd("tui-worktree-not-a-repo")
	require.NoError(t, os.MkdirAll(cwd, 0o755))

	c := newTUIConsole(t)
	c.Refresh()

	openTUISpawnForm(t, c, "reviewer", cwd)
	c.Press(t, "down", "right", "enter")

	view := c.View()
	assert.Contains(t, view, "Worktree failed:")
	assert.Contains(t, view, "needs a git repo")
	assert.Contains(t, view, "New agent", "the form is still open")
	assert.Empty(t, f.ListGroupMembers("dev"), "and nothing was spawned")
}
