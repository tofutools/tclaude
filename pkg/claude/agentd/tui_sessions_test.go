package agentd

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubLocalSessions swaps the host's non-agent session listing for a fixed
// answer, so the console can be driven without a session database or a tmux
// server. It reports whether the console actually asked.
func stubLocalSessions(t *testing.T, rows []tuiSessionRow, err error) *bool {
	t.Helper()
	asked := false
	prev := tuiListLocalSessions
	tuiListLocalSessions = func() ([]tuiSessionRow, error) {
		asked = true
		return rows, err
	}
	t.Cleanup(func() { tuiListLocalSessions = prev })
	return &asked
}

// stubKillSession swaps the session kill for a recorder.
func stubKillSession(t *testing.T, err error) *tuiKillRecord {
	t.Helper()
	rec := &tuiKillRecord{}
	prev := tuiKillSession
	tuiKillSession = func(tmuxSession string) error {
		rec.called = true
		rec.session = tmuxSession
		return err
	}
	t.Cleanup(func() { tuiKillSession = prev })
	return rec
}

type tuiKillRecord struct {
	called  bool
	session string
}

// operatorSessionConsole is the console non-agent sessions are offered to: the
// daemon treats it as the human and it shares the daemon's host.
func operatorSessionConsole(sessions ...tuiSessionRow) tuiModel {
	m := newTUIModel(nil)
	m.operator = true
	m.sessions = sessions
	return m
}

func shellSessionRow(handle, cwd string) tuiSessionRow {
	return tuiSessionRow{
		SessionID:   handle,
		TmuxSession: handle,
		Cwd:         cwd,
		Harness:     "shell",
		Status:      "running",
	}
}

// A session is not an agent, and the listing says so on the row itself: the
// GROUP column — which a session has nothing to put in — carries the marker.
func TestTUISessionsAreListedBelowTheAgentsAndMarked(t *testing.T) {
	m := operatorSessionConsole(shellSessionRow("scratch", "/home/op/src"))
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: true, Groups: []string{"dev"}}}
	m.width, m.height = 140, 30

	rows := m.visibleRows()
	require.Len(t, rows, 2)
	assert.False(t, rows[0].isSession(), "agents lead the listing")
	assert.Equal(t, "worker", rows[0].name())
	assert.True(t, rows[1].isSession())
	assert.Equal(t, "scratch", rows[1].name())

	view := m.renderList()
	assert.Contains(t, view, "scratch")
	assert.Contains(t, view, tuiSessionGroupCell)
	assert.Contains(t, view, "/home/op/src")
	assert.Contains(t, view, "1 agents (1 online)")
	assert.Contains(t, view, "1 sessions", "sessions are counted apart from the agents")
}

// The active filter is about agents that have gone offline. Every listed
// session is live by construction, so filtering leaves them alone.
func TestTUIActiveFilterLeavesSessionsListed(t *testing.T) {
	m := operatorSessionConsole(shellSessionRow("scratch", "/home/op"))
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: false}}

	filtered, _ := m.handleKey(tuiKey("f"))
	got := filtered.(tuiModel)
	require.True(t, got.filterActive)
	rows := got.visibleRows()
	require.Len(t, rows, 1)
	assert.True(t, rows[0].isSession(), "the offline agent went, the live session stayed")
}

// The listing is the operator's own host state — their shells, their working
// directories — so a console the daemon does not classify as the human never
// even asks for it.
func TestTUISessionsAreNotReadByANonOperatorConsole(t *testing.T) {
	asked := stubLocalSessions(t, []tuiSessionRow{shellSessionRow("scratch", "/home/op")}, nil)
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []tuiAgentRow{})
	})

	m := newTUIModel(api) // operator stays false
	msg, ok := m.refreshCmd()().(tuiDataMsg)
	require.True(t, ok)
	require.NoError(t, msg.err)
	assert.False(t, *asked)
	assert.Empty(t, msg.sessions)
	assert.Contains(t, m.renderList(), "No agents yet",
		"and the empty listing does not claim to have looked for sessions")
}

// A remote console drives a daemon on another host: it can neither read that
// host's session store nor hand this terminal to a pane on it.
func TestTUISessionsAreNotReadByARemoteConsole(t *testing.T) {
	asked := stubLocalSessions(t, []tuiSessionRow{shellSessionRow("scratch", "/home/op")}, nil)
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []tuiAgentRow{})
	})

	m := newTUIModel(api)
	m.operator = true
	m.capabilities = tuiCapabilities{attachAgent: true}

	msg, ok := m.refreshCmd()().(tuiDataMsg)
	require.True(t, ok)
	assert.False(t, *asked)
	assert.Empty(t, msg.sessions)
}

// Enter on a session row hands this terminal to its pane — the same move it
// makes on a live agent's row, aimed at the tmux handle the session is
// attached by.
func TestTUIEnterOnASessionGoesToItsPane(t *testing.T) {
	attached := stubAttach(t)
	// A stub daemon that fails the test if an agent verb is issued: a session
	// has no conversation, so nothing here may reach /v1/agent/…
	api := stubTUIAPI(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			t.Errorf("a session row must not reach the agent API: %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, []tuiAgentRow{})
	})

	m := operatorSessionConsole(shellSessionRow("scratch", "/home/op"))
	m.api = api

	updated, cmd := m.handleKey(tuiEnterKey())
	got := updated.(tuiModel)
	require.NotNil(t, cmd)
	require.True(t, attached.called)
	assert.Equal(t, "scratch", attached.session)
	assert.Equal(t, "scratch", attached.agent)
	assert.False(t, got.resuming, "a session is not resumed like an offline agent")
}

// Only an operator console on the daemon's host may take over a pane. The
// listing already withholds session rows from every other console, but the
// action carries its own gate rather than relying on that.
func TestTUIEnterOnASessionIsRefusedWithoutTheHost(t *testing.T) {
	attached := stubAttach(t)

	m := operatorSessionConsole(shellSessionRow("scratch", "/home/op"))
	m.capabilities.localSessions = false
	updated, _ := m.enterSelected()
	assert.False(t, attached.called)
	assert.Contains(t, updated.notice, "does not share the daemon's host")

	m = operatorSessionConsole(shellSessionRow("scratch", "/home/op"))
	m.operator = false
	updated, _ = m.enterSelected()
	assert.False(t, attached.called)
	assert.Contains(t, updated.notice, "operator console")
}

// Delete on a session ends it — there is no offline state to park it in and
// nothing to retire — and like every other lifecycle key it asks first.
func TestTUIDeleteOnASessionKillsItAfterConfirmation(t *testing.T) {
	rec := stubKillSession(t, nil)
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []tuiAgentRow{})
	})
	m := operatorSessionConsole(shellSessionRow("scratch", "/home/op"))
	m.api = api
	m.width = 120

	asked, cmd := m.handleKey(tuiDeleteKey())
	got := asked.(tuiModel)
	require.Nil(t, cmd)
	require.Equal(t, tuiModeConfirmKillSession, got.mode)
	assert.Contains(t, got.confirmPrompt(), "Kill session scratch?")

	// Anything but "y" leaves it running.
	cancelled, cmd := got.handleKey(tuiKey("n"))
	assert.Nil(t, cmd)
	assert.Equal(t, tuiModeList, cancelled.(tuiModel).mode)
	assert.False(t, rec.called)

	asked, _ = m.handleKey(tuiDeleteKey())
	confirmed, cmd := asked.(tuiModel).handleKey(tuiKey("y"))
	killing := confirmed.(tuiModel)
	require.NotNil(t, cmd)
	assert.True(t, killing.killingSession)
	assert.Contains(t, killing.summaryLine(), "ending a session…")

	msg, ok := cmd().(tuiSessionKilledMsg)
	require.True(t, ok)
	require.True(t, rec.called)
	assert.Equal(t, "scratch", rec.session)

	done, refresh := killing.Update(msg)
	final := done.(tuiModel)
	assert.False(t, final.killingSession)
	assert.Contains(t, final.notice, "Ended session scratch")
	assert.NotNil(t, refresh, "the row is gone, so the listing is pulled straight away")
}

// A kill that fails says why and leaves the console usable, rather than
// stranding it on "ending…".
func TestTUISessionKillFailureIsReported(t *testing.T) {
	m := operatorSessionConsole(shellSessionRow("scratch", "/home/op"))
	m.killingSession = true

	updated, cmd := m.Update(tuiSessionKilledMsg{session: "scratch", err: errors.New("no server running")})
	got := updated.(tuiModel)
	assert.Nil(t, cmd)
	assert.False(t, got.killingSession)
	assert.Contains(t, got.notice, "Could not end session scratch")
	assert.Contains(t, got.notice, "no server running")
}

// The prompt names what it will act on even after the listing re-sorts under
// the cursor, and the confirmation acts on that captured row.
func TestTUISessionKillPromptActsOnTheRowItNamed(t *testing.T) {
	rec := stubKillSession(t, nil)
	api := stubTUIAPI(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, []tuiAgentRow{})
	})
	m := operatorSessionConsole(
		shellSessionRow("alpha", "/home/op/a"),
		shellSessionRow("beta", "/home/op/b"),
	)
	m.api = api
	m.cursor = 1

	asked, _ := m.handleKey(tuiDeleteKey())
	got := asked.(tuiModel)
	require.Contains(t, got.confirmPrompt(), "Kill session beta?")

	// A poll lands while the prompt is open and reorders the listing.
	got.sessions = []tuiSessionRow{
		shellSessionRow("beta", "/home/op/b"),
		shellSessionRow("alpha", "/home/op/a"),
	}
	confirmed, cmd := got.handleKey(tuiKey("y"))
	require.NotNil(t, cmd)
	cmd()
	require.True(t, rec.called)
	assert.Equal(t, "beta", rec.session, "the confirmation acts on the session it named")
	assert.False(t, confirmed.(tuiModel).mode == tuiModeConfirmKillSession)
}

// The listing re-sorts every two seconds, so the cursor follows the row it was
// on rather than the row number — including across the agent/session boundary,
// where a session id and a conv-id must never be confused for one another.
func TestTUICursorFollowsASessionAcrossARefresh(t *testing.T) {
	m := operatorSessionConsole(
		shellSessionRow("alpha", "/home/op/a"),
		shellSessionRow("beta", "/home/op/b"),
	)
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: true}}
	m.cursor = 2 // "beta"
	selected, ok := m.selectedRow()
	require.True(t, ok)
	require.Equal(t, "beta", selected.name())

	// A second agent comes up, pushing every session row down one.
	updated, _ := m.Update(tuiDataMsg{
		agents: []tuiAgentRow{
			{ConvID: "c1", Title: "worker", Online: true},
			{ConvID: "c2", Title: "another", Online: true},
		},
		sessions: []tuiSessionRow{
			shellSessionRow("alpha", "/home/op/a"),
			shellSessionRow("beta", "/home/op/b"),
		},
	})
	got := updated.(tuiModel)
	row, ok := got.selectedRow()
	require.True(t, ok)
	assert.Equal(t, "beta", row.name(), "the cursor follows the session, not the row number")
	assert.True(t, row.isSession())
}

// A conv-id and a session id come from different id spaces, so the row key
// namespaces them: a session must never inherit the cursor from an agent that
// happens to share its id.
func TestTUIRowKeysNamespaceAgentsAndSessions(t *testing.T) {
	same := "shared-id"
	agent := agentListRow(tuiAgentRow{ConvID: same})
	sess := sessionListRow(tuiSessionRow{SessionID: same})
	assert.NotEqual(t, agent.key(), sess.key())
}

// One failed read of the host's session store must not drop the rows out from
// under the cursor — the agent listing beside them is live and correct.
func TestTUISessionListingFailureKeepsTheLastRows(t *testing.T) {
	m := operatorSessionConsole(shellSessionRow("scratch", "/home/op"))
	m.lifecycleTarget = tuiListRow{}

	updated, _ := m.Update(tuiDataMsg{sessionsErr: errors.New("database is locked")})
	got := updated.(tuiModel)
	require.Len(t, got.sessions, 1, "the last known sessions are kept")
	assert.Contains(t, got.summaryLine(), "1 sessions (stale)")
	assert.Empty(t, got.refreshErr, "one part of the listing failing is not the listing failing")

	settled, _ := got.Update(tuiDataMsg{sessions: []tuiSessionRow{}})
	assert.Empty(t, settled.(tuiModel).sessions)
	assert.NotContains(t, settled.(tuiModel).summaryLine(), "stale")
}

// The keys mean different things on the two kinds of row, so the key line says
// which one the cursor is on.
func TestTUIKeyHintsFollowTheSelectedRowKind(t *testing.T) {
	m := operatorSessionConsole(shellSessionRow("scratch", "/home/op"))
	m.agents = []tuiAgentRow{{ConvID: "c1", Title: "worker", Online: true}}

	m.cursor = 0
	assert.Contains(t, m.keyHintLine(), "del offline / retire")
	assert.NotContains(t, m.keyHintLine(), "kill session")

	m.cursor = 1
	assert.Contains(t, m.keyHintLine(), "del kill session")
	assert.NotContains(t, m.keyHintLine(), "offline / retire")
	assert.True(t, strings.Contains(m.enterHint(), "enter attach") ||
		strings.Contains(m.enterHint(), "enter switch to"))
}

// The help explains the second kind of row where it exists, and says nothing
// about it on a console that has none.
func TestTUIHelpExplainsSessionRowsOnlyWhereTheyAppear(t *testing.T) {
	help := operatorSessionConsole().renderHelp()
	assert.Contains(t, help, tuiSessionGroupCell)
	assert.Contains(t, help, "kill-session")
	assert.Contains(t, help, "session ls -a")

	remote := newTUIModel(nil)
	remote.operator = true
	remote.capabilities = tuiCapabilities{attachAgent: true}
	assert.NotContains(t, remote.renderHelp(), "session ls -a")
}
