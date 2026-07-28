package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// stubTUIAPI builds a console API client whose "daemon" is h, so a test can
// drive the console's request shaping and response handling without a DB. The
// identity is the ordinary one: a real pid, no harness ancestry — the shape a
// console started from the operator's own shell resolves to.
func stubTUIAPI(h http.HandlerFunc) *tuiAPI { return &tuiAPI{handler: h, pid: 99999} }

// withOperatorToken installs tok as the live operator token for the duration
// of the test, restoring whatever was there before.
func withOperatorToken(t *testing.T, tok string) {
	t.Helper()
	t.Cleanup(SetOperatorTokenForTest(tok))
}

func TestDashboardRequested(t *testing.T) {
	t.Run("without --tui the dashboard always runs", func(t *testing.T) {
		assert.True(t, dashboardRequested(&serveParams{}))
		assert.True(t, dashboardRequested(&serveParams{NoTray: true}))
	})

	t.Run("--tui alone is the terminal-only daemon", func(t *testing.T) {
		assert.False(t, dashboardRequested(&serveParams{TUI: true}))
	})

	t.Run("a dashboard flag alongside --tui runs both surfaces", func(t *testing.T) {
		cases := map[string]serveParams{
			"--auto-launch-dashboard": {AutoLaunchDashboard: true},
			"--dashboard-port":        {DashboardPort: 9000},
			"--dashboard-bind":        {DashboardBind: "0.0.0.0"},
		}
		for flag, p := range cases {
			p.TUI = true
			assert.True(t, dashboardRequested(&p), flag)
		}
	})

	t.Run("the theme flags are cosmetic and do not ask for a listener", func(t *testing.T) {
		// They re-skin an auto-launched dashboard; on their own they say
		// nothing about whether one should exist, and they never reach the
		// terminal console.
		assert.False(t, dashboardRequested(&serveParams{TUI: true, Slop: true}))
		assert.False(t, dashboardRequested(&serveParams{TUI: true, Wizard: true}))
	})

	t.Run("an unset port or bind is not a request", func(t *testing.T) {
		assert.False(t, dashboardRequested(&serveParams{TUI: true, DashboardPort: 0}))
		assert.False(t, dashboardRequested(&serveParams{TUI: true, DashboardBind: "   "}))
	})

	t.Run("suppressing the token banner is unrelated", func(t *testing.T) {
		assert.False(t, dashboardRequested(&serveParams{TUI: true, NoPrintHumanToken: true}))
	})
}

// The console authenticates like any other operator client: it presents the
// live operator token and lets the daemon's own verifier classify it. It
// must NOT be able to pass itself off as the human without one.
func TestTUIAPIAuthenticatesWithTheOperatorToken(t *testing.T) {
	var seen callerClass
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		seen = classify(peerFromContext(r.Context()))
		writeJSON(w, http.StatusOK, []tuiAgentRow{})
	})

	withOperatorToken(t, "tclo_test-token")
	var rows []tuiAgentRow
	require.NoError(t, api.get("/v1/peers", &rows))
	assert.Equal(t, classHuman, seen, "console holding the operator token is the human")

	// No live token (the state a daemon that could not mint one is in):
	// the console falls back to an unconfirmed caller, not to the human.
	setOperatorToken("")
	require.NoError(t, api.get("/v1/peers", &rows))
	assert.Equal(t, classUnconfirmed, seen)
}

func TestTUIAPISurfacesDaemonErrors(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "forbidden", "group is archived")
	})
	err := api.get("/v1/peers", &[]tuiAgentRow{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "group is archived")
}

func TestTUIAPIFallsBackToTheStatusTextWithoutAnErrorBody(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	err := api.get("/v1/peers", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Bad Gateway")
}

func TestTUIRefreshOrdersOnlineAgentsFirst(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/peers":
			writeJSON(w, http.StatusOK, []tuiAgentRow{
				{ConvID: "c1", Title: "zoe", Online: false},
				{ConvID: "c2", Title: "bob", Online: true},
				{ConvID: "c3", Title: "amy", Online: false},
			})
		case "/v1/groups":
			writeJSON(w, http.StatusOK, []tuiGroupRow{{Name: "dev"}})
		case "/v1/spawn-profiles":
			writeJSON(w, http.StatusOK, []tuiProfileRow{})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	msg, ok := newTUIModel(api).refreshCmd()().(tuiDataMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	var names []string
	for _, a := range msg.agents {
		names = append(names, a.name())
	}
	assert.Equal(t, []string{"bob", "amy", "zoe"}, names)
	require.Len(t, msg.groups, 1)
}

func TestTUIRefreshFailureIsReportedThenClearedByASuccess(t *testing.T) {
	fail := true
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			writeError(w, http.StatusInternalServerError, "io", "database is locked")
			return
		}
		switch r.URL.Path {
		case "/v1/peers":
			writeJSON(w, http.StatusOK, []tuiAgentRow{{ConvID: "c1", Title: "amy", Online: true}})
		default:
			writeJSON(w, http.StatusOK, []tuiGroupRow{{Name: "dev"}})
		}
	})

	m := newTUIModel(api)
	m.refreshing = true
	m.notice = "Spawned agt_1 in group dev"
	updated, _ := m.Update(m.refreshCmd()())
	got := updated.(tuiModel)
	assert.False(t, got.refreshing)
	assert.Contains(t, got.refreshErr, "database is locked")
	assert.Equal(t, "Spawned agt_1 in group dev", got.notice,
		"a poll failure must not overwrite the last action's outcome")

	// A transient failure must not leave the operator reading "refresh
	// failed" over a listing that is in fact live.
	fail = false
	updated, _ = got.Update(got.refreshCmd()())
	got = updated.(tuiModel)
	assert.Empty(t, got.refreshErr)
	assert.Len(t, got.agents, 1)
}

// The spawn form is the console's one mutating surface: what the operator
// typed must reach POST /v1/groups/{group}/spawn unchanged.
func TestTUISpawnFormPostsWhatTheOperatorTyped(t *testing.T) {
	var gotPath string
	var gotReq agent.SpawnRequest
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		writeJSON(w, http.StatusOK, agent.SpawnResponse{
			Group: "dev", AgentID: "agt_1", TmuxSession: "cc-dev-1",
		})
	})

	m := newTUIModel(api)
	m.groups = []tuiGroupRow{{Name: "dev"}, {Name: "ops"}}
	m = m.openSpawnForm()
	// Second group, a typed name and brief, and a pinned harness.
	m = m.cycleChoice(1)
	m.form.name.SetValue("reviewer")
	m.form.dir.SetValue("/tmp/repo")
	m.form.brief.SetValue("review the open PRs")
	m.form.harnessNames = []string{tuiHarnessDefault, "codex"}
	m.form.harnessIdx = 1

	spawned, cmd := m.submitSpawn()
	require.NotNil(t, cmd)
	assert.True(t, spawned.spawning)
	assert.Equal(t, tuiModeList, spawned.mode, "the form closes while the spawn runs")

	msg, ok := cmd().(tuiSpawnedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, "/v1/groups/ops/spawn", gotPath)
	assert.Equal(t, "reviewer", gotReq.Name)
	assert.Equal(t, "/tmp/repo", gotReq.Cwd)
	assert.Equal(t, "codex", gotReq.Harness)
	assert.Equal(t, "review the open PRs", gotReq.InitialMessage)

	// The outcome lands on the status line and pulls a fresh listing.
	updated, refresh := spawned.Update(msg)
	got := updated.(tuiModel)
	assert.False(t, got.spawning)
	assert.Contains(t, got.notice, "agt_1")
	assert.Contains(t, got.notice, "ops")
	assert.NotNil(t, refresh)
}

// The profile picker is the console's way of saying "one of these kinds of
// agent" — the name has to reach the request the daemon resolves against.
func TestTUISpawnFormPostsTheChosenProfile(t *testing.T) {
	var gotReq agent.SpawnRequest
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		writeJSON(w, http.StatusOK, agent.SpawnResponse{Group: "dev", AgentID: "agt_1"})
	})

	m := newTUIModel(api)
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m.profiles = []tuiProfileRow{{Name: "reviewer-kit"}, {Name: "scribe-kit"}}
	m = m.openSpawnForm()
	require.Equal(t, []string{tuiProfileDefault, "reviewer-kit", "scribe-kit"}, m.form.profileNames)

	// Tab off the group onto the profile, then pick the second one.
	m = m.moveSpawnField(1)
	require.Equal(t, tuiFieldProfile, m.form.field)
	m = m.cycleChoice(1)
	m = m.cycleChoice(1)
	assert.Equal(t, "scribe-kit", m.selectedProfile())
	assert.Contains(t, m.renderSpawnForm(), "< scribe-kit >")

	_, cmd := m.submitSpawn()
	require.NotNil(t, cmd)
	msg, ok := cmd().(tuiSpawnedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, "scribe-kit", gotReq.Profile)
}

// "(default)" is not "no profile": it names none and leaves the group's and
// the global default profile in force, which the form says out loud because
// the two readings differ in what the agent ends up being.
func TestTUISpawnDefaultProfileIsLeftUnpinned(t *testing.T) {
	m := newTUIModel(nil)
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m.profiles = []tuiProfileRow{{Name: "reviewer-kit"}}
	m = m.openSpawnForm()

	assert.Empty(t, m.selectedProfile())
	view := m.renderSpawnForm()
	assert.Contains(t, view, "Profile:")
	assert.Contains(t, view, "< "+tuiProfileDefault+" >")
	assert.Contains(t, view, "default profile still applies")
}

// A daemon with no saved profiles offers only the sentinel, and says where
// profiles come from rather than showing an empty picker.
func TestTUISpawnProfilePickerWithoutAnyProfiles(t *testing.T) {
	m := newTUIModel(nil)
	m = m.openSpawnForm()
	assert.Equal(t, []string{tuiProfileDefault}, m.form.profileNames)
	assert.Contains(t, m.renderSpawnForm(), "profiles create")
}

// A disabled profile is refused at spawn and cannot be enabled from here, so
// the picker does not offer it.
func TestTUISpawnProfilePickerSkipsDisabledProfiles(t *testing.T) {
	on, off := true, false
	got := tuiProfileOptions([]tuiProfileRow{
		{Name: "live-kit", Disabled: &off},
		{Name: "retired-kit", Disabled: &on},
		{Name: "legacy-kit"},
	})
	assert.Equal(t, []string{tuiProfileDefault, "live-kit", "legacy-kit"}, got)
}

// A blank harness must stay blank on the wire: that is what lets the
// daemon's own profile chain pick the harness.
func TestTUISpawnDefaultHarnessIsLeftUnpinned(t *testing.T) {
	m := newTUIModel(nil)
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m = m.openSpawnForm()
	m.form.harnessNames = []string{tuiHarnessDefault}
	m.form.harnessIdx = 0
	assert.Empty(t, m.selectedHarness())
}

func TestTUISpawnWithoutAGroupExplainsHowToMakeOne(t *testing.T) {
	m := newTUIModel(nil)
	m.lastRefresh = time.Now() // the listing landed; there really are no groups
	m = m.openSpawnForm()
	got, cmd := m.submitSpawn()
	assert.Nil(t, cmd, "nothing is posted without a group")
	assert.False(t, got.spawning)
	assert.Contains(t, got.notice, "groups create")
}

func TestTUISpawnFailureIsReported(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusBadRequest, "invalid_name", "name must be 1-32 characters")
	})
	m := newTUIModel(api)
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m = m.openSpawnForm()
	spawned, cmd := m.submitSpawn()
	require.NotNil(t, cmd)

	updated, _ := spawned.Update(cmd())
	got := updated.(tuiModel)
	assert.False(t, got.spawning, "a failed spawn releases the in-flight guard")
	assert.Contains(t, got.notice, "name must be 1-32 characters")
}

// Quitting the console stops the daemon, so it is always confirmed first.
func TestTUIQuitAsksBeforeShuttingTheDaemonDown(t *testing.T) {
	m := newTUIModel(nil)
	updated, cmd := m.handleKey(tuiKey("q"))
	got := updated.(tuiModel)
	assert.Equal(t, tuiModeConfirmQuit, got.mode)
	assert.Nil(t, cmd, "no quit until it is confirmed")
	assert.Contains(t, got.renderList(), "shut down agentd?")

	declined, cmd := got.handleKey(tuiKey("n"))
	assert.Equal(t, tuiModeList, declined.(tuiModel).mode)
	assert.Nil(t, cmd)

	// Enter is the reflexive dismiss key and the prompt says anything but
	// "y" cancels — it must not shut the daemon down.
	entered, cmd := got.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	assert.Equal(t, tuiModeList, entered.(tuiModel).mode)
	assert.Nil(t, cmd)

	confirmed, cmd := got.handleKey(tuiKey("y"))
	assert.Equal(t, tuiModeConfirmQuit, confirmed.(tuiModel).mode)
	require.NotNil(t, cmd)
	assert.IsType(t, tea.QuitMsg{}, cmd())
}

// Retiring stops an agent and revokes its authority, so the console asks
// first — and then goes through the daemon's own retire verb.
func TestTUIRetireAsksThenPostsToTheDaemon(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, map[string]any{
			"conv_id":  "c1",
			"outcome":  retireConvOutcome{GroupsLeft: []string{"dev"}, Retired: true},
			"shutdown": memberOpResult{ConvID: "c1", Action: "soft_stopped"},
		})
	})
	m := newTUIModel(api)
	m.width = 120
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: true}}

	asked, cmd := m.handleKey(tuiKey("x"))
	got := asked.(tuiModel)
	assert.Nil(t, cmd, "nothing is retired until it is confirmed")
	require.Equal(t, tuiModeConfirmRetire, got.mode)
	assert.Contains(t, got.renderList(), "Retire worker and stop its session?")

	// Anything but "y" cancels.
	declined, cmd := got.handleKey(tuiKey("n"))
	assert.Nil(t, cmd)
	assert.Equal(t, tuiModeList, declined.(tuiModel).mode)

	confirmed, cmd := got.handleKey(tuiKey("y"))
	retiring := confirmed.(tuiModel)
	require.NotNil(t, cmd)
	assert.Equal(t, tuiModeList, retiring.mode)
	assert.True(t, retiring.retiring)

	msg, ok := cmd().(tuiRetiredMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/v1/agent/c1/retire", gotPath)
	assert.Empty(t, gotQuery, "the console keeps the endpoint's own defaults: stop the pane, keep the worktree")

	updated, refresh := retiring.Update(msg)
	done := updated.(tuiModel)
	assert.False(t, done.retiring)
	assert.Contains(t, done.notice, "Retired worker")
	assert.Contains(t, done.notice, "left dev")
	assert.Contains(t, done.notice, "asked to exit")
	assert.NotNil(t, refresh, "the retired agent leaves the listing right away")
}

// The prompt names one agent and must retire that one: the listing re-sorts
// under the cursor every couple of seconds.
func TestTUIRetireActsOnTheAgentTheOperatorConfirmed(t *testing.T) {
	var gotPath string
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		writeJSON(w, http.StatusOK, map[string]any{"conv_id": "c1"})
	})
	m := newTUIModel(api)
	m.agents = []tuiAgentRow{
		{ConvID: "c1", Title: "worker", Online: true},
		{ConvID: "c2", Title: "other", Online: true},
	}

	asked, _ := m.handleKey(tuiKey("x"))
	got := asked.(tuiModel)

	// A refresh lands under the prompt and reverses the listing.
	shuffled, _ := got.Update(tuiDataMsg{agents: []tuiAgentRow{
		{ConvID: "c2", Title: "other", Online: true},
		{ConvID: "c1", Title: "worker", Online: true},
	}})
	got = shuffled.(tuiModel)
	require.Equal(t, tuiModeConfirmRetire, got.mode)

	_, cmd := got.handleKey(tuiKey("y"))
	require.NotNil(t, cmd)
	msg, ok := cmd().(tuiRetiredMsg)
	require.True(t, ok)
	assert.Equal(t, "worker", msg.agent)
	assert.Equal(t, "/v1/agent/c1/retire", gotPath)
}

func TestTUIRetireFailureIsReported(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "forbidden", "agent.retire is not granted")
	})
	m := newTUIModel(api)
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: true}}

	asked, _ := m.handleKey(tuiKey("x"))
	confirmed, cmd := asked.(tuiModel).handleKey(tuiKey("y"))
	require.NotNil(t, cmd)

	updated, _ := confirmed.(tuiModel).Update(cmd())
	got := updated.(tuiModel)
	assert.False(t, got.retiring, "a refused retire releases the in-flight guard")
	assert.Contains(t, got.notice, "agent.retire is not granted")
}

// An empty list has nothing to retire, and the key line does not offer it.
func TestTUIRetireOnAnEmptyListDoesNothing(t *testing.T) {
	m := newTUIModel(nil)
	updated, cmd := m.handleKey(tuiKey("x"))
	got := updated.(tuiModel)
	assert.Nil(t, cmd)
	assert.Equal(t, tuiModeList, got.mode)
	assert.NotContains(t, got.keyHintLine(), "x retire")

	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: true}}
	assert.Contains(t, m.keyHintLine(), "x retire")
}

func TestTUIListRendersTheAgentsItWasGiven(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 120
	m.agents = []tuiAgentRow{{
		ConvID:     "c1",
		Title:      "reviewer",
		Online:     true,
		Groups:     []string{"dev"},
		Branch:     "feat/x",
		CurrentDir: "/home/op/src/tclaude",
		State:      tuiAgentState{Harness: "codex", Status: "working"},
	}}
	m.groups = []tuiGroupRow{{Name: "dev"}}

	view := m.renderList()
	for _, want := range []string{"reviewer", "dev", "working", "codex", "tclaude", "feat/x", "1 agents (1 online)"} {
		assert.Contains(t, view, want)
	}
	assert.NotContains(t, view, "\x1b[3", "the console renders without colors")
}

func TestTUIOfflineWinsOverAStaleStatus(t *testing.T) {
	row := tuiAgentRow{Title: "worker"}
	row.State.Status = "working"
	assert.Equal(t, "offline", row.status())
	row.Online = true
	assert.Equal(t, "working", row.status())
}

func TestTUIEmptyListStillExplainsTheKeys(t *testing.T) {
	view := newTUIModel(nil).renderList()
	assert.Contains(t, view, "No agents yet")
	assert.Contains(t, view, "n new agent")
}

// tuiKey builds the KeyPressMsg for a single printable key.
func tuiKey(s string) tea.KeyPressMsg {
	r := []rune(s)[0]
	return tea.KeyPressMsg{Code: r, Text: string(r)}
}

func TestTUIHelpMentionsTheMissingDashboard(t *testing.T) {
	m := newTUIModel(nil)
	updated, _ := m.handleKey(tuiKey("?"))
	got := updated.(tuiModel)
	require.Equal(t, tuiModeHelp, got.mode)
	help := got.renderHelp()
	assert.True(t, strings.Contains(help, "without the web dashboard"))
	assert.Contains(t, help, "permissions grant")

	// Any key closes it again.
	closed, _ := got.handleKey(tuiKey("x"))
	assert.Equal(t, tuiModeList, closed.(tuiModel).mode)
}

// A handler panic must cost one request, not the daemon: on the socket path
// net/http contains it, and the console has to match that.
func TestTUIAPIContainsAHandlerPanic(t *testing.T) {
	api := stubTUIAPI(func(_ http.ResponseWriter, _ *http.Request) {
		panic("boom")
	})
	var rows []tuiAgentRow
	err := api.get("/v1/peers", &rows)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "panicked")

	// And the console keeps working afterwards.
	m := newTUIModel(nil)
	m.api = api
	m.refreshing = true
	updated, _ := m.Update(m.refreshCmd()())
	assert.Contains(t, updated.(tuiModel).refreshErr, "panicked")
}

// The console does not get to declare itself the operator: a daemon started
// from inside a harness session is classified by its ancestry, exactly as
// classify() treats every other caller holding the token.
func TestTUIAPIDoesNotOutrankItsHarnessAncestry(t *testing.T) {
	withOperatorToken(t, "tclo_test-token")
	var seen callerClass
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = classify(peerFromContext(r.Context()))
		writeJSON(w, http.StatusOK, []tuiAgentRow{})
	})

	agentConsole := &tuiAPI{handler: handler, pid: 4242, convID: "conv-1", hasHarnessAncestor: true}
	require.NoError(t, agentConsole.get("/v1/peers", &[]tuiAgentRow{}))
	assert.Equal(t, classAgent, seen, "a console under a harness pane stays that agent")
	assert.Contains(t, agentConsole.identityWarning(), "conv-1")

	operatorConsole := &tuiAPI{handler: handler, pid: 4242}
	require.NoError(t, operatorConsole.get("/v1/peers", &[]tuiAgentRow{}))
	assert.Equal(t, classHuman, seen)
	assert.Empty(t, operatorConsole.identityWarning(), "the ordinary case says nothing")
}

// The list view must fit the terminal: overflowing it scrolls the screen out
// from under bubbletea's diff renderer.
func TestTUIListFitsTheTerminalWithEveryOptionalLineShowing(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 120
	m.identityWarning = "Note: something about identity"
	m.refreshErr = "Refresh failed: database is locked"
	m.notice = "Spawned agt_1 in group dev"
	m.mode = tuiModeConfirmQuit
	for i := range 50 {
		m.agents = append(m.agents, tuiAgentRow{ConvID: fmt.Sprintf("c%d", i), Title: fmt.Sprintf("a%d", i)})
	}

	for _, height := range []int{24, 30, 60} {
		m.height = height
		lines := strings.Count(m.renderList(), "\n")
		assert.LessOrEqual(t, lines, height, "height=%d", height)
	}
}

func TestTUIListWithNarrowWidthIncludesWrappedTextInBudget(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 40
	// identityWarning is indented 2 spaces, so it has 38 chars width.
	// 80+ chars should take 3+ lines.
	m.identityWarning = "Note: this is a very long identity warning that should definitely wrap when the width is only forty characters."
	m.refreshErr = "Refresh failed: another long error message that should wrap in a narrow terminal."
	m.notice = "Spawned agt_1 in group dev with a very long notice message that will also wrap."
	m.mode = tuiModeConfirmQuit
	for i := range 50 {
		m.agents = append(m.agents, tuiAgentRow{ConvID: fmt.Sprintf("c%d", i), Title: fmt.Sprintf("a%d", i)})
	}

	for _, height := range []int{24, 30, 60} {
		m.height = height
		lines := strings.Count(m.renderList(), "\n")
		assert.LessOrEqual(t, lines, height, "height=%d width=%d", height, m.width)
	}
}

func TestTUIFilterActive(t *testing.T) {
	m := newTUIModel(nil)
	m.agents = []tuiAgentRow{
		{ConvID: "c1", Title: "online-1", Online: true},
		{ConvID: "c2", Title: "offline-1", Online: false},
		{ConvID: "c3", Title: "online-2", Online: true},
	}
	assert.Len(t, m.visibleAgents(), 3)

	// Toggle filter on.
	updated, _ := m.handleKey(tuiKey("f"))
	m = updated.(tuiModel)
	assert.True(t, m.filterActive)
	visible := m.visibleAgents()
	assert.Len(t, visible, 2)
	assert.Equal(t, "online-1", visible[0].name())
	assert.Equal(t, "online-2", visible[1].name())

	// Toggle filter off.
	updated, _ = m.handleKey(tuiKey("f"))
	m = updated.(tuiModel)
	assert.False(t, m.filterActive)
	assert.Len(t, m.visibleAgents(), 3)
}

func TestTUIFilterPreservesCursor(t *testing.T) {
	m := newTUIModel(nil)
	m.agents = []tuiAgentRow{
		{ConvID: "c1", Title: "a", Online: true},
		{ConvID: "c2", Title: "b", Online: true},
		{ConvID: "c3", Title: "c", Online: false},
	}
	m.cursor = 1 // on "b"

	// Filter active (only a and b show).
	updated, _ := m.handleKey(tuiKey("f"))
	m = updated.(tuiModel)
	assert.Equal(t, 1, m.cursor, "cursor stays on 'b'")

	// Move to 'a' and toggle off.
	m.cursor = 0
	updated, _ = m.handleKey(tuiKey("f"))
	m = updated.(tuiModel)
	assert.Equal(t, 0, m.cursor, "cursor stays on 'a'")
}

func TestTUIUpdateClampsCursorAgainstVisibleAgents(t *testing.T) {
	m := newTUIModel(nil)
	m.filterActive = true
	m.cursor = 10

	// Refresh returns only 2 online agents. Cursor must be clamped.
	msg := tuiDataMsg{
		agents: []tuiAgentRow{
			{ConvID: "c1", Online: true},
			{ConvID: "c2", Online: true},
			{ConvID: "c3", Online: false},
		},
	}
	updated, _ := m.Update(msg)
	got := updated.(tuiModel)
	assert.Equal(t, 1, got.cursor)
}

// The spawn form must not tell an operator to create a group when the group
// list simply has not arrived yet.
func TestTUISpawnBeforeTheFirstRefreshSaysSo(t *testing.T) {
	m := newTUIModel(nil)
	m = m.openSpawnForm()
	got, cmd := m.submitSpawn()
	assert.Nil(t, cmd)
	assert.Contains(t, got.notice, "not loaded yet")
	assert.NotContains(t, got.notice, "groups create")
}

// spawnFormOnDir opens the spawn form on an operator console with the
// cursor on the Directory field and dir prefilled, the state every
// completion test starts from.
func spawnFormOnDir(t *testing.T, dir string) tuiModel {
	t.Helper()
	m := newTUIModel(nil)
	m.width = 120
	m.operator = true
	m = m.openSpawnForm()
	for m.form.field != tuiFieldDir {
		m = m.moveSpawnField(1)
	}
	m.form.dir.SetValue(dir)
	return m
}

func tuiTabKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyTab} }

// Tab on the Directory field completes the path instead of leaving the
// field, the same way the `session watch` new-session prompt does.
func TestTUISpawnDirTabCompletesAnUnambiguousPath(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "project-beta"), 0o755))

	m := spawnFormOnDir(t, filepath.Join(root, "project-b"))
	updated, _ := m.handleSpawnKey(tuiTabKey())
	got := updated.(tuiModel)

	assert.Equal(t, filepath.Join(root, "project-beta")+"/", got.form.dir.Value())
	assert.Empty(t, got.form.dirSuggestions)
	assert.Equal(t, tuiFieldDir, got.form.field, "completing must not move off the field")
}

// Several matches extend as far as they can and list the candidates under
// the field, which is where the operator reads them.
func TestTUISpawnDirTabListsAmbiguousCandidates(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "project-alpha"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "project-beta"), 0o755))

	m := spawnFormOnDir(t, filepath.Join(root, "proj"))
	updated, _ := m.handleSpawnKey(tuiTabKey())
	got := updated.(tuiModel)

	assert.Equal(t, filepath.Join(root, "project-"), got.form.dir.Value())
	assert.Equal(t, []string{"project-alpha", "project-beta"}, got.form.dirSuggestions)
	view := got.renderSpawnForm()
	assert.Contains(t, view, "project-alpha")
	assert.Contains(t, view, "project-beta")

	// The next keystroke retires the list — it answers a path that is no
	// longer what the field says.
	typed, _ := got.handleSpawnKey(tuiKey("x"))
	assert.Empty(t, typed.(tuiModel).form.dirSuggestions)
}

// An empty Directory means "the group's default", so there is nothing to
// complete and Tab keeps its ordinary next-field job.
func TestTUISpawnDirTabOnAnEmptyFieldMovesOn(t *testing.T) {
	m := spawnFormOnDir(t, "")
	updated, _ := m.handleSpawnKey(tuiTabKey())
	got := updated.(tuiModel)
	assert.Equal(t, tuiFieldHarness, got.form.field)
	assert.Empty(t, got.form.dir.Value())
}

// A console the daemon does not treat as the human gets no completion: it
// reads the filesystem as agentd's own user, outside any sandbox the
// driving agent is under. Tab stays plain field navigation there, and the
// form does not advertise a key that would do nothing.
func TestTUISpawnDirTabIsRefusedForANonOperatorConsole(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "project-beta"), 0o755))

	m := spawnFormOnDir(t, filepath.Join(root, "project-b"))
	m.operator = false

	updated, _ := m.handleSpawnKey(tuiTabKey())
	got := updated.(tuiModel)
	assert.Equal(t, filepath.Join(root, "project-b"), got.form.dir.Value(),
		"the typed path must be left alone")
	assert.Empty(t, got.form.dirSuggestions)
	assert.Equal(t, tuiFieldHarness, got.form.field, "tab falls back to next-field")
	assert.NotContains(t, got.renderSpawnForm(), "complete dir")
}

// Tab anywhere else in the form is still plain field navigation.
func TestTUISpawnTabOnAnotherFieldMovesOn(t *testing.T) {
	m := newTUIModel(nil)
	m = m.openSpawnForm()
	require.Equal(t, tuiFieldGroup, m.form.field)
	updated, _ := m.handleSpawnKey(tuiTabKey())
	assert.Equal(t, tuiFieldProfile, updated.(tuiModel).form.field)
}

// The candidate list is one line by contract: the form renders it whether
// or not there are candidates so the fields below hold still, which only
// works if a long list is trimmed rather than wrapped.
func TestTUISpawnDirSuggestionsStayOnOneLine(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 40
	m = m.openSpawnForm()
	m.form.dirSuggestions = []string{
		"alpha-service", "beta-service", "gamma-service", "delta-service",
	}
	line := m.dirSuggestionLine()
	assert.LessOrEqual(t, lipgloss.Width(line), m.width)
	assert.Contains(t, line, "alpha-service")
	assert.Contains(t, line, "more)")
}

// stubAttach swaps the terminal handover for a recorder and returns the
// target the console asked for.
func stubAttach(t *testing.T) *tuiAttachRecord {
	t.Helper()
	rec := &tuiAttachRecord{}
	prev := tuiAttachToPane
	tuiAttachToPane = func(agentName, tmuxSession string, inTmux bool) tea.Cmd {
		rec.called = true
		rec.agent, rec.session, rec.inTmux = agentName, tmuxSession, inTmux
		return func() tea.Msg {
			return tuiAttachedMsg{agent: agentName, session: tmuxSession}
		}
	}
	t.Cleanup(func() { tuiAttachToPane = prev })
	return rec
}

type tuiAttachRecord struct {
	called  bool
	agent   string
	session string
	inTmux  bool
}

// Enter on a row with no live pane must say so, not silently do nothing.
func TestTUIAttachWithoutALivePaneSaysSo(t *testing.T) {
	rec := stubAttach(t)
	m := newTUIModel(nil)
	m.operator = true
	m.agents = []tuiAgentRow{{ConvID: "no-such-conv", Title: "worker"}}

	got, cmd := m.attachSelected()
	assert.Nil(t, cmd)
	assert.False(t, rec.called)
	assert.Contains(t, got.notice, "worker has no live tmux session")
}

// Attaching is an operator move: a console the daemon classifies as an agent
// must not be able to reach a peer's pane through it.
func TestTUIAttachIsRefusedForANonOperatorConsole(t *testing.T) {
	rec := stubAttach(t)
	m := newTUIModel(nil)
	m.operator = false
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker"}}

	got, cmd := m.attachSelected()
	assert.Nil(t, cmd)
	assert.False(t, rec.called, "no session lookup, no handover")
	assert.Contains(t, got.notice, "Only an operator console")
}

func TestTUIAttachOnAnEmptyListDoesNothing(t *testing.T) {
	rec := stubAttach(t)
	m := newTUIModel(nil)
	m.operator = true
	got, cmd := m.attachSelected()
	assert.Nil(t, cmd)
	assert.False(t, rec.called)
	assert.Empty(t, got.notice)
}

// Coming back from a pane refreshes: the agent may have exited while the
// operator was looking at it.
func TestTUIReturnFromAPaneRefreshes(t *testing.T) {
	m := newTUIModel(nil)
	updated, cmd := m.Update(tuiAttachedMsg{agent: "worker", session: "cc-dev-1"})
	got := updated.(tuiModel)
	assert.Contains(t, got.notice, "cc-dev-1")
	assert.Contains(t, got.notice, "worker")
	assert.True(t, got.refreshing)
	assert.NotNil(t, cmd)
}

func TestTUIAttachFailureIsReported(t *testing.T) {
	m := newTUIModel(nil)
	updated, cmd := m.Update(tuiAttachedMsg{
		agent: "worker", session: "cc-dev-1", err: errors.New("no server running"),
	})
	got := updated.(tuiModel)
	assert.Contains(t, got.notice, "Could not reach cc-dev-1")
	assert.Contains(t, got.notice, "no server running")
	assert.Nil(t, cmd)
}

// The key line only advertises enter when it will actually do something.
func TestTUIKeyHintAdvertisesEnterOnlyWhenItWorks(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker"}}
	assert.Contains(t, m.keyHintLine(), "enter")

	m.agents = nil
	assert.NotContains(t, m.keyHintLine(), "enter")

	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker"}}
	m.operator = false
	assert.NotContains(t, m.keyHintLine(), "enter")
}

// The console says which surfaces are live: on its own it is the only one,
// and beside a dashboard it points at it rather than claiming there is none.
func TestTUIViewNamesACoRunningDashboard(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 120

	assert.Contains(t, m.renderList(), "no web dashboard in this mode")
	assert.Contains(t, m.renderHelp(), "runs without the web dashboard")
	assert.Contains(t, m.renderHelp(), "permissions grant")

	m.dashboardURL = "http://127.0.0.1:44585"
	assert.Contains(t, m.renderList(), "web dashboard: http://127.0.0.1:44585")
	assert.NotContains(t, m.renderList(), "no web dashboard")
	help := m.renderHelp()
	assert.Contains(t, help, "http://127.0.0.1:44585")
	assert.Contains(t, help, "Messages tab")
	assert.NotContains(t, help, "runs without the web dashboard")
}

func TestTokenBannerInTUI(t *testing.T) {
	t.Run("any console takes the banner, dashboard or not", func(t *testing.T) {
		// stdout is discarded under --tui, so the console is the only place
		// the token can be read from.
		assert.True(t, tokenBannerInTUI(&serveParams{TUI: true}))
		assert.True(t, tokenBannerInTUI(&serveParams{TUI: true, AutoLaunchDashboard: true}))
		assert.True(t, tokenBannerInTUI(&serveParams{TUI: true, DashboardPort: 8321}))
		assert.True(t, tokenBannerInTUI(&serveParams{TUI: true, DashboardBind: "0.0.0.0"}))
	})

	t.Run("no console keeps the stdout banner", func(t *testing.T) {
		assert.False(t, tokenBannerInTUI(&serveParams{}))
		assert.False(t, tokenBannerInTUI(&serveParams{DashboardPort: 8321}))
	})

	t.Run("--no-print-human-token still means no banner anywhere", func(t *testing.T) {
		assert.False(t, tokenBannerInTUI(&serveParams{TUI: true, NoPrintHumanToken: true}))
		assert.False(t, tokenBannerInTUI(&serveParams{
			TUI: true, AutoLaunchDashboard: true, NoPrintHumanToken: true,
		}))
	})
}

// Startup narration goes to the terminal only when the console is not using
// it. Discarding is what keeps `--tui` from opening on a screen full of
// migration progress and socket paths.
func TestServeStdout(t *testing.T) {
	assert.Equal(t, os.Stdout, serveStdout(&serveParams{}))
	assert.Equal(t, os.Stdout, serveStdout(&serveParams{DashboardPort: 8321}))
	assert.Equal(t, io.Discard, serveStdout(&serveParams{TUI: true}))
	assert.Equal(t, io.Discard, serveStdout(&serveParams{TUI: true, DashboardPort: 8321}))
}

func TestTUIOperatorTokenLines(t *testing.T) {
	t.Run("no token, no block", func(t *testing.T) {
		assert.Nil(t, tuiOperatorTokenLines("", tokenSource{}))
	})

	t.Run("ephemeral token is exportable and unannotated", func(t *testing.T) {
		lines := tuiOperatorTokenLines("tclo_secret", tokenSource{kind: tokenSourceEphemeral})
		require.Len(t, lines, 2)
		assert.Contains(t, lines[1], `export TCLAUDE_HUMAN_TOKEN="tclo_secret"`)
	})

	t.Run("a persisted token says where it lives", func(t *testing.T) {
		lines := tuiOperatorTokenLines("tclo_secret",
			tokenSource{kind: tokenSourceFile, path: "/home/op/.tclaude/data/operator_token"})
		require.Len(t, lines, 3)
		assert.Contains(t, lines[2], "/home/op/.tclaude/data/operator_token")

		lines = tuiOperatorTokenLines("tclo_secret", tokenSource{kind: tokenSourceKeychain})
		require.Len(t, lines, 3)
		assert.Contains(t, lines[2], "keychain")
	})
}

// The token is shown once at startup, retired by the first keystroke, and
// reachable from the help view for as long as the console runs.
func TestTUITokenBannerIsShownThenRecallable(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 120
	m.height = 40
	m.tokenLines = tuiOperatorTokenLines("tclo_secret", tokenSource{kind: tokenSourceEphemeral})
	m.showTokenBanner = true

	assert.Contains(t, m.renderList(), "tclo_secret")
	assert.Contains(t, m.renderList(), "press ? to see it again")

	// Any keystroke retires it — here one that does nothing else.
	updated, _ := m.Update(tuiKey("z"))
	got := updated.(tuiModel)
	assert.False(t, got.showTokenBanner)
	assert.NotContains(t, got.renderList(), "tclo_secret")

	// …and the help view still has it.
	assert.Contains(t, got.renderHelp(), "tclo_secret")
}

// A console that did not take over the banner never renders the secret.
func TestTUIWithoutATokenShowsNoTokenBlock(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 120
	assert.NotContains(t, m.renderList(), "TCLAUDE_HUMAN_TOKEN")
	assert.NotContains(t, m.renderHelp(), "TCLAUDE_HUMAN_TOKEN")
}

// The banner is several lines tall, so it has to be paid for out of the
// viewport like every other optional block.
func TestTUIListWithTheTokenBannerStillFitsTheTerminal(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 120
	m.tokenLines = tuiOperatorTokenLines("tclo_secret",
		tokenSource{kind: tokenSourceFile, path: "/home/op/.tclaude/data/operator_token"})
	m.showTokenBanner = true
	m.identityWarning = "Note: something about identity"
	m.refreshErr = "Refresh failed: database is locked"
	m.notice = "Spawned agt_1 in group dev"
	m.mode = tuiModeConfirmQuit
	for i := range 50 {
		m.agents = append(m.agents, tuiAgentRow{ConvID: fmt.Sprintf("c%d", i), Title: fmt.Sprintf("a%d", i)})
	}

	for _, height := range []int{24, 30, 60} {
		m.height = height
		lines := strings.Count(m.renderList(), "\n")
		assert.LessOrEqual(t, lines, height, "height=%d", height)
	}
}
