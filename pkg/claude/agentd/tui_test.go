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
func stubTUIAPI(h http.HandlerFunc) *inProcessTUIAPI {
	return &inProcessTUIAPI{handler: h, pid: 99999}
}

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

// Starting an agent is how you get to it: a landed spawn goes straight to the
// new agent's pane, the same handover enter makes on its row.
func TestTUISpawnGoesToTheNewAgentsPane(t *testing.T) {
	rec := stubAttach(t)
	m := newTUIModel(nil)
	m.operator = true

	updated, cmd := m.Update(tuiSpawnedMsg{
		group: "dev",
		resp:  agent.SpawnResponse{AgentID: "agt_1", TmuxSession: "cc-dev-1"},
	})
	got := updated.(tuiModel)
	require.NotNil(t, cmd)
	require.True(t, rec.called)
	assert.Equal(t, "cc-dev-1", rec.session)
	assert.Equal(t, "agt_1", rec.agent)
	assert.Contains(t, got.notice, "Spawned agt_1")
	assert.False(t, got.spawning)

	// Coming back off the pane is the ordinary return path, which refreshes.
	back, refresh := got.Update(cmd())
	assert.NotNil(t, refresh)
	assert.True(t, back.(tuiModel).refreshing)
}

// An agent that has no pane yet — a Codex spawn held behind a startup gate —
// has nothing to go to, so the spawn just lands in the listing.
func TestTUISpawnWithoutAPaneJustRefreshes(t *testing.T) {
	rec := stubAttach(t)
	m := newTUIModel(nil)
	m.api = stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []tuiAgentRow{})
	})
	m.operator = true

	updated, cmd := m.Update(tuiSpawnedMsg{group: "dev", resp: agent.SpawnResponse{AgentID: "agt_1"}})
	got := updated.(tuiModel)
	assert.False(t, rec.called)
	assert.True(t, got.refreshing)
	assert.NotNil(t, cmd)
	assert.NotContains(t, got.notice, "attaching")
}

// Going to a pane is an operator move wherever it is triggered from: a
// console the daemon classifies as an agent may spawn, but the terminal
// handover stays closed to it.
func TestTUISpawnDoesNotFocusForANonOperatorConsole(t *testing.T) {
	rec := stubAttach(t)
	m := newTUIModel(nil)
	m.api = stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []tuiAgentRow{})
	})
	m.operator = false

	updated, _ := m.Update(tuiSpawnedMsg{
		group: "dev",
		resp:  agent.SpawnResponse{AgentID: "agt_1", TmuxSession: "cc-dev-1"},
	})
	assert.False(t, rec.called)
	assert.True(t, updated.(tuiModel).refreshing)
	assert.NotContains(t, updated.(tuiModel).renderSpawnForm(), "go to its pane")
}

// A failed spawn has no pane and must not claim otherwise.
func TestTUIFailedSpawnDoesNotFocusAnything(t *testing.T) {
	rec := stubAttach(t)
	m := newTUIModel(nil)
	m.operator = true

	updated, cmd := m.Update(tuiSpawnedMsg{group: "dev", err: errors.New("group is archived")})
	assert.False(t, rec.called)
	assert.Nil(t, cmd)
	assert.Contains(t, updated.(tuiModel).notice, "Spawn failed")
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

// An empty picker has three causes and they ask different things of the
// operator, so the form must not answer all of them with "create one".
func TestTUISpawnProfilePickerExplainsAnEmptyList(t *testing.T) {
	t.Run("the listing landed and there really are none", func(t *testing.T) {
		m := newTUIModel(nil)
		m.lastRefresh = time.Now()
		m = m.openSpawnForm()
		assert.Equal(t, []string{tuiProfileDefault}, m.form.profileNames)
		assert.Contains(t, m.renderSpawnForm(), "profiles create")
	})

	t.Run("nothing has loaded yet", func(t *testing.T) {
		m := newTUIModel(nil).openSpawnForm()
		view := m.renderSpawnForm()
		assert.Contains(t, view, "has not loaded yet")
		assert.NotContains(t, view, "profiles create")
	})

	t.Run("the profile listing failed", func(t *testing.T) {
		m := newTUIModel(nil)
		m.lastRefresh = time.Now()
		m.profilesErr = "database is locked"
		m = m.openSpawnForm()
		view := m.renderSpawnForm()
		assert.Contains(t, view, "profile list unavailable")
		assert.NotContains(t, view, "profiles create")
	})
}

// A profile read that fails costs the spawn form its picker, not the console
// its listing: the agents were fetched successfully and must stay on screen.
func TestTUIProfileListFailureDoesNotBlankTheListing(t *testing.T) {
	failProfiles := false
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/peers":
			writeJSON(w, http.StatusOK, []tuiAgentRow{{ConvID: "c1", Title: "amy", Online: true}})
		case "/v1/groups":
			writeJSON(w, http.StatusOK, []tuiGroupRow{{Name: "dev"}})
		default:
			if failProfiles {
				writeError(w, http.StatusInternalServerError, "io", "database is locked")
				return
			}
			writeJSON(w, http.StatusOK, []tuiProfileRow{{Name: "reviewer-kit"}})
		}
	})

	m := newTUIModel(api)
	m.width = 120
	updated, _ := m.Update(m.refreshCmd()())
	got := updated.(tuiModel)
	require.Len(t, got.profiles, 1)

	failProfiles = true
	updated, _ = got.Update(got.refreshCmd()())
	got = updated.(tuiModel)
	assert.Empty(t, got.refreshErr, "the agent listing succeeded and must not be reported as failed")
	assert.Len(t, got.agents, 1)
	assert.Contains(t, got.renderList(), "amy")
	assert.Contains(t, got.profilesErr, "database is locked")
	assert.Len(t, got.profiles, 1, "the last good picker is kept rather than emptied")

	// And it clears once the read works again.
	failProfiles = false
	updated, _ = got.Update(got.refreshCmd()())
	assert.Empty(t, updated.(tuiModel).profilesErr)
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
// daemon's own profile chain pick the harness. The form opens that way, so a
// profile that selects a harness is not overruled by a field the operator
// never touched.
func TestTUISpawnDefaultHarnessIsLeftUnpinned(t *testing.T) {
	var gotReq agent.SpawnRequest
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		writeJSON(w, http.StatusOK, agent.SpawnResponse{Group: "dev", AgentID: "agt_1"})
	})
	m := newTUIModel(api)
	m.groups = []tuiGroupRow{{Name: "dev"}}
	m.profiles = []tuiProfileRow{{Name: "codex-kit"}}
	m = m.openSpawnForm()

	require.Equal(t, tuiHarnessDefault, m.form.harnessNames[m.form.harnessIdx],
		"the harness picker opens on the sentinel, not on the default harness by name")
	assert.Empty(t, m.selectedHarness())
	assert.Contains(t, m.renderSpawnForm(), "the profile chain decides")

	// Pick a profile and leave the harness alone: the request must carry the
	// profile and no harness, or the profile's own harness would be overruled.
	m = m.moveSpawnField(1)
	m = m.cycleChoice(1)
	_, cmd := m.submitSpawn()
	require.NotNil(t, cmd)
	msg, ok := cmd().(tuiSpawnedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, "codex-kit", gotReq.Profile)
	assert.Empty(t, gotReq.Harness, "an untouched harness field must not pin one")
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

// When this daemon started the tmux server, quitting kills it and every session
// on it (see startTUITmuxServer). The operator decides at the prompt, and the
// slog line announcing ownership goes to output.log where they cannot see it,
// so the prompt itself has to carry the consequence.
func TestTUIQuitPromptWarnsWhenItOwnsTheTmuxServer(t *testing.T) {
	m := newTUIModel(nil)
	m.mode = tuiModeConfirmQuit
	assert.Equal(t, "Quit and shut down agentd? [y / any other key = cancel]", m.confirmPrompt(),
		"a server this daemon did not start keeps running, so the plain wording stands")

	m.ownsTmuxServer = true
	prompt := m.confirmPrompt()
	assert.Contains(t, prompt, "its tmux sessions")
	assert.LessOrEqual(t, lipgloss.Width("  "+prompt), 80,
		"confirmPrompt is budgeted as exactly one line")
}

// Delete on an offline agent retires it. Retiring revokes the agent's
// authority, so the console asks first and then uses the daemon's own verb.
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
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: false}}

	asked, cmd := m.handleKey(tuiDeleteKey())
	got := asked.(tuiModel)
	assert.Nil(t, cmd, "nothing is retired until it is confirmed")
	require.Equal(t, tuiModeConfirmRetire, got.mode)
	assert.Contains(t, got.renderList(), "Retire worker?")

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
	assert.Equal(t, "require_offline=1", gotQuery,
		"the console must not retire an agent resumed while confirmation was open")

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
		{ConvID: "c1", Title: "worker", Online: false},
		{ConvID: "c2", Title: "other", Online: false},
	}

	asked, _ := m.handleKey(tuiDeleteKey())
	got := asked.(tuiModel)

	// A refresh lands under the prompt and reverses the listing.
	shuffled, _ := got.Update(tuiDataMsg{agents: []tuiAgentRow{
		{ConvID: "c2", Title: "other", Online: false},
		{ConvID: "c1", Title: "worker", Online: false},
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

// The prompt is budgeted as exactly one line, and the only variable part of
// it is a conversation title — which is routinely long. It has to be capped,
// or the list view runs past the terminal's last row.
func TestTUIRetirePromptStaysOnOneLine(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 80
	m.height = 24
	m.agents = []tuiAgentRow{{
		ConvID: "c1",
		Title:  "review the whole authentication subsystem and write it up in detail",
		Online: false,
	}}

	asked, _ := m.handleKey(tuiDeleteKey())
	got := asked.(tuiModel)
	require.Equal(t, tuiModeConfirmRetire, got.mode)

	for _, width := range []int{80, 100, 120} {
		got.width = width
		prompt := got.confirmPrompt()
		assert.LessOrEqual(t, lipgloss.Width(prompt)+2, width, "width=%d prompt=%q", width, prompt)
		assert.Contains(t, prompt, "review the", "the operator can still tell which agent it is")
		assert.Contains(t, prompt, "Retire", "and what will happen to it")
		assert.Contains(t, got.renderList(), prompt)
	}

	got.width = 80
	for _, height := range []int{24, 30, 60} {
		got.height = height
		assert.LessOrEqual(t, strings.Count(got.renderList(), "\n"), height, "height=%d", height)
	}
}

func TestTUITruncate(t *testing.T) {
	assert.Equal(t, "worker", tuiTruncate("worker", 10))
	assert.Equal(t, "worker", tuiTruncate("worker", 6))
	assert.Equal(t, "work…", tuiTruncate("worker", 5))
	assert.Equal(t, "", tuiTruncate("worker", 0))
}

// A shutdown that fails says why. On this console there is no browser to go
// and look it up in, and the agent's pane is still running.
func TestTUIRetireReportsWhyAShutdownFailed(t *testing.T) {
	msg := tuiRetiredMsg{agent: "worker"}
	msg.res.Shutdown = memberOpResult{Action: "error", Detail: "tmux send-keys: no server running"}
	summary := tuiRetireSummary(msg)
	assert.Contains(t, summary, "Retired worker")
	assert.Contains(t, summary, "error")
	assert.Contains(t, summary, "no server running")
}

func TestTUIRetireFailureIsReported(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "forbidden", "agent.retire is not granted")
	})
	m := newTUIModel(api)
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: false}}

	asked, _ := m.handleKey(tuiDeleteKey())
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
	updated, cmd := m.handleKey(tuiDeleteKey())
	got := updated.(tuiModel)
	assert.Nil(t, cmd)
	assert.Equal(t, tuiModeList, got.mode)
	assert.NotContains(t, got.keyHintLine(), "del offline / retire")

	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: true}}
	assert.Contains(t, m.keyHintLine(), "del offline / retire")
}

// Delete on a live agent is the inverse of Enter on an offline one: it asks
// first, then gracefully stops the session without retiring the agent.
func TestTUIDeleteOnAnOnlineAgentTakesItOffline(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, map[string]any{
			"conv_id": "c1", "action": "soft_stopped",
		})
	})
	m := newTUIModel(api)
	m.width = 120
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: true}}

	asked, cmd := m.handleKey(tuiDeleteKey())
	got := asked.(tuiModel)
	assert.Nil(t, cmd)
	require.Equal(t, tuiModeConfirmStop, got.mode)
	assert.Contains(t, got.renderList(), "Take worker offline?")

	declined, cmd := got.handleKey(tuiKey("n"))
	assert.Nil(t, cmd)
	assert.Equal(t, tuiModeList, declined.(tuiModel).mode)

	confirmed, cmd := got.handleKey(tuiKey("y"))
	stopping := confirmed.(tuiModel)
	require.NotNil(t, cmd)
	assert.True(t, stopping.stopping)
	assert.Contains(t, stopping.notice, "Taking worker offline")

	msg, ok := cmd().(tuiStoppedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/v1/agent/c1/stop", gotPath)
	assert.Empty(t, gotQuery, "Delete always uses a graceful stop")

	updated, refresh := stopping.Update(msg)
	done := updated.(tuiModel)
	assert.False(t, done.stopping)
	assert.Contains(t, done.notice, "Asked worker to go offline")
	assert.Contains(t, done.notice, "asked to exit")
	assert.NotNil(t, refresh, "the row should reconcile promptly after the exit request")
}

func TestTUIStopSummaryCarriesSuccessfulDetail(t *testing.T) {
	summary := tuiStopSummary(tuiStoppedMsg{
		agent: "worker",
		res: tuiStopResult{
			Action: "soft_stopped",
			Detail: "resume provenance unavailable; human recovery will be required",
		},
	})
	assert.Contains(t, summary, "Asked worker to go offline")
	assert.Contains(t, summary, "human recovery will be required")
}

func TestTUIXNoLongerRetiresAnAgent(t *testing.T) {
	m := newTUIModel(nil)
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: false}}

	updated, cmd := m.handleKey(tuiKey("x"))
	assert.Nil(t, cmd)
	assert.Equal(t, tuiModeList, updated.(tuiModel).mode)
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

// tuiEnterKey is enter, which tuiKey cannot spell: it is a named key rather
// than a character.
func tuiEnterKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyEnter} }

func tuiDeleteKey() tea.KeyPressMsg { return tea.KeyPressMsg{Code: tea.KeyDelete} }

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

	agentConsole := &inProcessTUIAPI{handler: handler, pid: 4242, convID: "conv-1", hasHarnessAncestor: true}
	require.NoError(t, agentConsole.get("/v1/peers", &[]tuiAgentRow{}))
	assert.Equal(t, classAgent, seen, "a console under a harness pane stays that agent")
	assert.Contains(t, agentConsole.identityWarning(), "conv-1")

	operatorConsole := &inProcessTUIAPI{handler: handler, pid: 4242}
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

// A refresh re-sorts the listing under the cursor every two seconds, so the
// cursor follows the agent it was on rather than the index it sat at —
// otherwise the next keystroke acts on whichever agent slid into its place.
// Starting an offline agent is the sharpest case: it moves to the top.
func TestTUIRefreshKeepsTheCursorOnTheSameAgent(t *testing.T) {
	m := newTUIModel(nil)
	m.agents = []tuiAgentRow{
		{ConvID: "c1", Title: "mmm", Online: true},
		{ConvID: "c2", Title: "zzz", Online: true},
		{ConvID: "c3", Title: "aaa", Online: false},
	}
	m.cursor = 2
	selected, ok := m.selectedAgent()
	require.True(t, ok)
	require.Equal(t, "aaa", selected.name())

	// "aaa" came up, so the next poll sorts it to the front.
	updated, _ := m.Update(tuiDataMsg{agents: []tuiAgentRow{
		{ConvID: "c3", Title: "aaa", Online: true},
		{ConvID: "c1", Title: "mmm", Online: true},
		{ConvID: "c2", Title: "zzz", Online: true},
	}})
	got := updated.(tuiModel)
	row, ok := got.selectedAgent()
	require.True(t, ok)
	assert.Equal(t, "aaa", row.name(), "the cursor follows the agent, not the row number")

	// An agent that leaves the listing entirely leaves the cursor to be
	// clamped back into range.
	updated, _ = got.Update(tuiDataMsg{agents: []tuiAgentRow{
		{ConvID: "c1", Title: "mmm", Online: true},
	}})
	got = updated.(tuiModel)
	assert.Equal(t, 0, got.cursor)
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

// The group's prefilled directory is not something to complete either: Tab
// there is how the operator gets off the field, and completing it would move
// the spawn — a directory with exactly one child completes straight into that
// child.
func TestTUISpawnDirTabOnTheGroupsOwnDirectoryMovesOn(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "only-child"), 0o755))

	m := newTUIModel(nil)
	m.operator = true
	m.groups = []tuiGroupRow{{Name: "dev", DefaultCwd: root}}
	m = m.openSpawnForm()
	for m.form.field != tuiFieldDir {
		m = m.moveSpawnField(1)
	}
	require.Equal(t, root+"/", m.form.dir.Value())

	updated, _ := m.handleSpawnKey(tuiTabKey())
	got := updated.(tuiModel)
	assert.Equal(t, tuiFieldHarness, got.form.field, "tab must still reach the next field")
	assert.Equal(t, root+"/", got.form.dir.Value(), "and must not pick a subdirectory nobody chose")

	// Once the operator starts typing a subdirectory, Tab completes it again.
	typing := m
	typing.form.dir.SetValue(filepath.Join(root, "only"))
	completed, _ := typing.handleSpawnKey(tuiTabKey())
	assert.Equal(t, filepath.Join(root, "only-child")+"/", completed.(tuiModel).form.dir.Value())
	assert.Equal(t, tuiFieldDir, completed.(tuiModel).form.field)
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

// The spawn form opens on the selected group's own default directory, so the
// usual "somewhere under the group's tree" spawn is a subdirectory name away
// rather than a full path retyped.
func TestTUISpawnFormPrefillsTheGroupsDirectory(t *testing.T) {
	m := newTUIModel(nil)
	m.groups = []tuiGroupRow{
		{Name: "dev", DefaultCwd: "/work/dev"},
		{Name: "ops", DefaultCwd: "/srv/ops/"},
		{Name: "misc"},
	}
	m = m.openSpawnForm()

	assert.Equal(t, "/work/dev/", m.form.dir.Value(),
		"the prefill ends in a separator so a subdirectory can be typed onto it")
	assert.Contains(t, m.renderSpawnForm(), "the group's directory")

	// Cycling the picker moves the untouched field with it — including over a
	// default that already carries its own trailing separator.
	m = m.cycleChoice(1)
	assert.Equal(t, "/srv/ops/", m.form.dir.Value())

	// A group with no default clears the field, which is the state that means
	// "let the daemon decide".
	m = m.cycleChoice(1)
	assert.Empty(t, m.form.dir.Value())
	assert.Contains(t, m.renderSpawnForm(), "blank = the group's default directory")
}

// Once the operator has typed a path, the group picker must leave it alone:
// they may have Tab-completed their way to it.
func TestTUISpawnPrefillDoesNotOverwriteATypedDirectory(t *testing.T) {
	m := newTUIModel(nil)
	m.groups = []tuiGroupRow{{Name: "dev", DefaultCwd: "/work/dev"}, {Name: "ops", DefaultCwd: "/srv/ops"}}
	m = m.openSpawnForm()
	m.form.dir.SetValue("/work/dev/scratch")

	m = m.cycleChoice(1)
	assert.Equal(t, "/work/dev/scratch", m.form.dir.Value())
	assert.Equal(t, "ops", m.selectedGroup(), "the group itself still changes")
	assert.NotContains(t, m.renderSpawnForm(), "the group's directory",
		"and the hint no longer claims the field is the group's own")

	// Clearing it puts the field back under the prefill's care: an empty
	// field is the daemon's own fallback either way.
	m.form.dir.SetValue("")
	m = m.cycleChoice(1)
	assert.Equal(t, "/work/dev/", m.form.dir.Value())
}

// The prefilled path is what the spawn posts — the same directory a blank
// field would have fallen back to, said out loud.
func TestTUISpawnPostsThePrefilledDirectory(t *testing.T) {
	var gotReq agent.SpawnRequest
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
		writeJSON(w, http.StatusOK, agent.SpawnResponse{Group: "dev", AgentID: "agt_1"})
	})
	m := newTUIModel(api)
	m.groups = []tuiGroupRow{{Name: "dev", DefaultCwd: "/work/dev"}}
	m = m.openSpawnForm()

	_, cmd := m.submitSpawn()
	require.NotNil(t, cmd)
	msg, ok := cmd().(tuiSpawnedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, "/work/dev/", gotReq.Cwd)
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
	line := m.dirSuggestionLine(m.form.dirSuggestions)
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
	setupTestDB(t)
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

func TestTUIReturnFromRemoteTerminalInsideTmuxIsNotReportedAsASwitch(t *testing.T) {
	t.Setenv("TMUX", "/tmp/local-tmux")
	m := newTUIModel(nil)
	updated, cmd := m.Update(tuiAttachedMsg{
		agent: "worker", session: "https://agent-host:8321", remote: true,
	})
	got := updated.(tuiModel)
	assert.Contains(t, got.notice, "Back from the remote terminal for worker")
	assert.NotContains(t, got.notice, "Switched")
	assert.True(t, got.refreshing)
	assert.NotNil(t, cmd)

	got.capabilities.attachLocalPane = false
	help := got.renderHelp()
	assert.Contains(t, help, "ctrl-]")
	assert.NotContains(t, help, "ctrl-b d")
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

// The key line only advertises enter when it will actually do something, and
// names the move the selected row will get.
func TestTUIKeyHintAdvertisesEnterOnlyWhenItWorks(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: true}}
	assert.Contains(t, m.keyHintLine(), "enter")

	m.agents = nil
	assert.NotContains(t, m.keyHintLine(), "enter")

	// A live agent's pane is an operator-only handover, so an agent-class
	// console is not offered it.
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: true}}
	m.operator = false
	assert.NotContains(t, m.keyHintLine(), "enter")

	// Starting an offline one goes through the daemon's own verb, which gates
	// the caller itself — so the key stays advertised whatever this console is.
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: false}}
	assert.Contains(t, m.keyHintLine(), "enter start")
	m.operator = true
	assert.Contains(t, m.keyHintLine(), "enter start")
}

// Enter on an offline agent turns it back on: an offline row has no pane to
// attach to, and starting it is what the operator picked that row for.
func TestTUIEnterOnAnOfflineAgentResumesIt(t *testing.T) {
	var gotMethod, gotPath, gotQuery string
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotQuery = r.Method, r.URL.Path, r.URL.RawQuery
		writeJSON(w, http.StatusOK, map[string]any{"conv_id": "c1", "action": "resumed"})
	})
	rec := stubAttach(t)
	m := newTUIModel(api)
	m.operator = true
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: false}}

	started, cmd := m.handleKey(tuiEnterKey())
	resuming := started.(tuiModel)
	require.NotNil(t, cmd)
	assert.True(t, resuming.resuming)
	assert.Contains(t, resuming.notice, "Starting worker")
	assert.False(t, rec.called, "an offline agent is started, not attached to")

	msg, ok := cmd().(tuiResumedMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.Equal(t, http.MethodPost, gotMethod)
	assert.Equal(t, "/v1/agent/c1/resume", gotPath)
	assert.Empty(t, gotQuery, "the console never asks the daemon to recreate a deleted launch directory")

	updated, refresh := resuming.Update(msg)
	done := updated.(tuiModel)
	assert.False(t, done.resuming)
	assert.Contains(t, done.notice, "Started worker")
	assert.NotNil(t, refresh, "the agent is online now — show that rather than waiting out the tick")
}

// A resume the daemon would not do says why: the reason travels with the
// verdict, since this console has no browser to go and read it in.
func TestTUIResumeReportsWhyItDidNotStart(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"conv_id": "c1", "action": "error:missing_cwd", "detail": "/gone/worktree",
		})
	})
	m := newTUIModel(api)
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker"}}

	_, cmd := m.handleKey(tuiEnterKey())
	require.NotNil(t, cmd)
	updated, _ := m.Update(cmd())
	got := updated.(tuiModel)
	assert.Contains(t, got.notice, "Could not start worker")
	assert.Contains(t, got.notice, "error:missing_cwd")
	assert.Contains(t, got.notice, "/gone/worktree")

	// An agent the daemon found already running is not an error.
	assert.Contains(t,
		tuiResumeSummary(tuiResumedMsg{agent: "worker", res: tuiResumeResult{Action: "skipped:already_online"}}),
		"was already running")

	// Nor is one that was retired out from under the listing — and the raw
	// wire token means nothing to an operator.
	retired := tuiResumeSummary(tuiResumedMsg{
		agent: "worker", res: tuiResumeResult{Action: "skipped:not_active_agent", Detail: "state: retired"},
	})
	assert.Contains(t, retired, "has been retired")
	assert.NotContains(t, retired, "skipped:not_active_agent")

	// A response with no action at all (an older daemon) must not claim a
	// start that may not have happened.
	assert.NotContains(t, tuiResumeSummary(tuiResumedMsg{agent: "worker"}), "Started worker")
}

// A resume that lands can still have something to say — reduced sandbox
// access, say. This console is the only place the operator would read it.
func TestTUIResumeCarriesItsWarnings(t *testing.T) {
	summary := tuiResumeSummary(tuiResumedMsg{agent: "worker", res: tuiResumeResult{
		Action:   "resumed",
		Warnings: []string{"sandbox: read-only /srv", "  ", "profile fell back to claude"},
	}})
	assert.Contains(t, summary, "Started worker")
	assert.Contains(t, summary, "sandbox: read-only /srv")
	assert.Contains(t, summary, "profile fell back to claude")
	assert.NotContains(t, summary, ";  ;", "a blank warning must not render as an empty clause")
}

// A row the listing calls online whose pane has since died is not a resume:
// the listing is up to two seconds stale, and enter says so rather than
// silently starting a second session for an agent that may still be up.
func TestTUIEnterOnALiveRowWithoutAPaneSaysSo(t *testing.T) {
	setupTestDB(t)
	rec := stubAttach(t)
	m := newTUIModel(nil)
	m.operator = true
	m.agents = []tuiAgentRow{{ConvID: "no-such-conv", Title: "worker", Online: true}}

	updated, cmd := m.handleKey(tuiEnterKey())
	got := updated.(tuiModel)
	assert.Nil(t, cmd)
	assert.False(t, rec.called)
	assert.False(t, got.resuming, "an online row is never resumed")
	assert.Contains(t, got.notice, "worker has no live tmux session")
}

// A refused resume releases the in-flight guard and reports the refusal —
// permission is the daemon's call, not the console's.
func TestTUIResumeFailureIsReported(t *testing.T) {
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, http.StatusForbidden, "forbidden", "agent.resume is not granted")
	})
	m := newTUIModel(api)
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker"}}

	started, cmd := m.handleKey(tuiEnterKey())
	require.NotNil(t, cmd)
	updated, _ := started.(tuiModel).Update(cmd())
	got := updated.(tuiModel)
	assert.False(t, got.resuming)
	assert.Contains(t, got.notice, "agent.resume is not granted")

	// A second enter while one is in flight does not stack a request.
	inflight := started.(tuiModel)
	_, cmd = inflight.handleKey(tuiEnterKey())
	assert.Nil(t, cmd)
}

// A placeholder member has no conversation to resume, and a path built from
// an empty selector would only earn a confusing 404.
func TestTUIResumeWithoutAConvSaysSo(t *testing.T) {
	m := newTUIModel(nil)
	m.agents = []tuiAgentRow{{AgentID: "agt_1", Online: false}}

	updated, cmd := m.handleKey(tuiEnterKey())
	got := updated.(tuiModel)
	assert.Nil(t, cmd)
	assert.False(t, got.resuming)
	assert.Contains(t, got.notice, "no conversation to start")
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
	assert.Contains(t, m.renderList(), "web dashboard:")
	assert.Contains(t, m.renderList(), "http://127.0.0.1:44585")
	assert.NotContains(t, m.renderList(), "no web dashboard")
	help := m.renderHelp()
	assert.Contains(t, help, "http://127.0.0.1:44585")
	assert.Contains(t, help, "Messages tab")
	assert.NotContains(t, help, "runs without the web dashboard")
}

// The address the console prints is meant to be opened as-is: it carries an
// init token, which is what the dashboard exchanges for a session cookie, so
// the operator never has to paste the operator token into the sign-in page.
func TestTUIDashboardLinkCarriesAnInitToken(t *testing.T) {
	const base = "http://127.0.0.1:44585"
	m := newTUIModel(nil)
	m.width = 120
	m.operator = true
	m.dashboardURL = base
	start := time.Now()
	m = m.refreshDashboardLink(start)

	require.Contains(t, m.dashboardLink, base+"/?init_token=")
	assert.Contains(t, m.renderList(), m.dashboardLink)
	assert.Contains(t, m.renderHelp(), m.dashboardLink)

	// The token in it is the real thing the dashboard root accepts, and it is
	// single-use — which is why the console mints replacements at all.
	tok := strings.TrimPrefix(m.dashboardLink, base+"/?init_token=")
	require.NotEmpty(t, tok)
	assert.True(t, consumeInitToken(tok, initScopeDashboard))
	assert.False(t, consumeInitToken(tok, initScopeDashboard))

	// A link holds its place for a while: ticking does not rewrite the line
	// out from under an operator selecting it.
	held, _ := m.Update(tuiTickMsg(start.Add(tuiRefreshInterval)))
	assert.Equal(t, m.dashboardLink, held.(tuiModel).dashboardLink)

	// Once it has been up long enough, a fresh token replaces it well before
	// the one on screen could expire.
	rotated, _ := m.Update(tuiTickMsg(start.Add(tuiDashboardLinkRotate)))
	next := rotated.(tuiModel).dashboardLink
	require.NotEqual(t, m.dashboardLink, next)
	assert.True(t, consumeInitToken(strings.TrimPrefix(next, base+"/?init_token="), initScopeDashboard))
	assert.Less(t, tuiDashboardLinkRotate, initTokenTTL,
		"a rotation must land before the link it replaces expires")
}

// The link is a capability — redeeming it buys the dashboard session cookie —
// so it goes on screen only when the screen is the operator's. Everywhere else
// the console still names the dashboard, just without a token on it.
func TestTUIDashboardLinkOnlyForAnOperatorConsole(t *testing.T) {
	const base = "http://127.0.0.1:44585"
	for _, tc := range []struct {
		name  string
		setup func(m *tuiModel)
		addr  string
	}{{
		// agentd started inside a harness pane acts as that agent — which can
		// read its own pane, so a token there is a token handed to the agent.
		name:  "agent-classified console",
		setup: func(m *tuiModel) { m.operator = false },
		addr:  base,
	}, {
		// --no-print-human-token: this terminal's output is scraped or logged.
		name:  "secrets suppressed",
		setup: func(m *tuiModel) { m.suppressSecrets = true },
		addr:  base,
	}, {
		// A token from this process's store is worthless to another daemon.
		name:  "remote console",
		setup: func(m *tuiModel) { m.connectionLabel = "http://10.0.0.4:8712" },
		addr:  "",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTUIModel(nil)
			m.width = 120
			m.operator = true
			m.dashboardURL = base
			tc.setup(&m)
			m = m.refreshDashboardLink(time.Now())

			assert.False(t, m.canMintDashboardLink())
			assert.Empty(t, m.dashboardLink)
			assert.Equal(t, tc.addr, m.dashboardAddressLine())
			assert.NotContains(t, m.renderList(), "init_token")
			assert.NotContains(t, m.renderHelp(), "init_token")
		})
	}
}

// A link already on screen is dropped the moment the console stops being
// allowed to show one, rather than lingering as a live capability.
func TestTUIDashboardLinkDroppedWhenNoLongerAllowed(t *testing.T) {
	m := newTUIModel(nil)
	m.width = 120
	m.operator = true
	m.dashboardURL = "http://127.0.0.1:44585"
	m = m.refreshDashboardLink(time.Now())
	require.NotEmpty(t, m.dashboardLink)

	m.suppressSecrets = true
	m = m.refreshDashboardLink(time.Now())
	assert.Empty(t, m.dashboardLink)
	assert.True(t, m.dashboardLinkMinted.IsZero())
}

// The address is one URL that only works whole, so a narrow terminal wraps it
// — and the row budget has to pay for every row it took, or the list overflows
// the screen bubbletea is diffing.
func TestTUIDashboardAddressRowsCountWrapping(t *testing.T) {
	m := newTUIModel(nil)
	m.operator = true
	m.dashboardURL = "http://127.0.0.1:44585"
	m = m.refreshDashboardLink(time.Now())
	width := lipgloss.Width(m.dashboardAddressLine()) + dashboardAddressIndent

	m.width = width
	assert.Equal(t, 1, m.dashboardAddressRows())

	m.width = width - 1
	assert.Equal(t, 2, m.dashboardAddressRows())

	m.width = (width / 3) + 1
	assert.Equal(t, 3, m.dashboardAddressRows())

	// Every row the header takes is a row the table cannot have.
	m.width, m.height = width, 40
	wide := m.viewportHeight()
	m.width = width - 1
	assert.Equal(t, wide-1, m.viewportHeight())
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
