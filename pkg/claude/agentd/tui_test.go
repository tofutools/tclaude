package agentd

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// stubTUIAPI builds a console API client whose "daemon" is h, so a test can
// drive the console's request shaping and response handling without a DB.
func stubTUIAPI(h http.HandlerFunc) *tuiAPI { return &tuiAPI{handler: h} }

// withOperatorToken installs tok as the live operator token for the duration
// of the test, restoring whatever was there before.
func withOperatorToken(t *testing.T, tok string) {
	t.Helper()
	t.Cleanup(SetOperatorTokenForTest(tok))
}

func TestValidateTUIFlags(t *testing.T) {
	t.Run("no --tui accepts every dashboard flag", func(t *testing.T) {
		p := &serveParams{
			AutoLaunchDashboard: true,
			Slop:                true,
			Wizard:              true,
			DashboardPort:       8080,
			DashboardBind:       "0.0.0.0",
			NoPrintHumanToken:   true,
		}
		require.NoError(t, validateTUIFlags(p))
	})

	t.Run("--tui alone is fine", func(t *testing.T) {
		require.NoError(t, validateTUIFlags(&serveParams{TUI: true}))
	})

	t.Run("--tui reports every conflicting flag at once", func(t *testing.T) {
		err := validateTUIFlags(&serveParams{
			TUI:                 true,
			AutoLaunchDashboard: true,
			Slop:                true,
			Wizard:              true,
			DashboardPort:       8080,
			DashboardBind:       "0.0.0.0",
			NoPrintHumanToken:   true,
		})
		require.Error(t, err)
		for _, flag := range []string{
			"--auto-launch-dashboard", "--slop", "--wizard",
			"--dashboard-port", "--dashboard-bind", "--no-print-human-token",
		} {
			assert.Contains(t, err.Error(), flag)
		}
	})

	t.Run("--dashboard-port 0 and a blank bind are 'unset', not conflicts", func(t *testing.T) {
		require.NoError(t, validateTUIFlags(&serveParams{
			TUI:           true,
			DashboardPort: 0,
			DashboardBind: "   ",
		}))
	})

	t.Run("each conflicting flag trips on its own", func(t *testing.T) {
		cases := map[string]serveParams{
			"--auto-launch-dashboard": {AutoLaunchDashboard: true},
			"--slop":                  {Slop: true},
			"--wizard":                {Wizard: true},
			"--dashboard-port":        {DashboardPort: 9000},
			"--dashboard-bind":        {DashboardBind: "127.0.0.1"},
			"--no-print-human-token":  {NoPrintHumanToken: true},
		}
		for flag, p := range cases {
			p.TUI = true
			err := validateTUIFlags(&p)
			require.Error(t, err, flag)
			assert.Contains(t, err.Error(), flag)
		}
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

func TestTUIRefreshFailureBecomesANotice(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusInternalServerError, "io", "database is locked")
	})
	m := newTUIModel(api)
	m.refreshing = true
	updated, _ := m.Update(m.refreshCmd()())
	got := updated.(tuiModel)
	assert.False(t, got.refreshing)
	assert.Contains(t, got.notice, "database is locked")
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

	confirmed, cmd := got.handleKey(tuiKey("y"))
	assert.Equal(t, tuiModeConfirmQuit, confirmed.(tuiModel).mode)
	require.NotNil(t, cmd)
	assert.IsType(t, tea.QuitMsg{}, cmd())
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
