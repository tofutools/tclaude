package agentd

import (
	"errors"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
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

// ---- the listing predicate itself ------------------------------------------

// aliveTmuxStub answers the liveness probe from a fixed set and refuses to run
// any tmux command: the predicate under test must read the snapshot, never
// fork per row.
type aliveTmuxStub struct{ alive map[string]struct{} }

func (s *aliveTmuxStub) Command(args ...string) *exec.Cmd {
	return exec.Command("false", args...)
}

func (s *aliveTmuxStub) ListSessions() (map[string]struct{}, error) {
	return s.alive, nil
}

// withAliveTmux points the daemon's liveness probe at a fixed set for the test,
// with the coalescing cache made transparent so each call re-reads it.
func withAliveTmux(t *testing.T, names ...string) {
	t.Helper()
	alive := map[string]struct{}{}
	for _, n := range names {
		alive[n] = struct{}{}
	}
	prevTmux := clcommon.Default
	clcommon.Default = &aliveTmuxStub{alive: alive}
	prevCache := liveTmuxCache
	liveTmuxCache = newTmuxSessionCache(0, time.Now, session.LiveTmuxSessions)
	t.Cleanup(func() {
		clcommon.Default = prevTmux
		liveTmuxCache = prevCache
	})
}

func saveSessionRow(t *testing.T, st *session.SessionState) {
	t.Helper()
	if st.Created.IsZero() {
		st.Created = time.Now()
	}
	require.NoError(t, session.SaveSessionState(st))
}

func listedSessionNames(t *testing.T) []string {
	t.Helper()
	rows, err := listLocalNonAgentSessions()
	require.NoError(t, err)
	var names []string
	for _, r := range rows {
		names = append(names, r.name())
	}
	return names
}

// A plain shell session is exactly what the listing is for.
func TestListLocalNonAgentSessionsListsALiveShell(t *testing.T) {
	setupTestDB(t)
	withAliveTmux(t, "scratch")
	saveSessionRow(t, &session.SessionState{
		ID: "scratch", TmuxSession: "scratch", Cwd: "/home/op",
		Status: session.StatusRunning, Harness: session.ShellHarnessName,
	})

	assert.Equal(t, []string{"scratch"}, listedSessionNames(t))
}

// An agent's own pane belongs to the agent listing. Every generation counts,
// not just the current one: a predecessor conv left over from a reincarnate
// must not resurface here as a plain session.
func TestListLocalNonAgentSessionsExcludesEveryAgentGeneration(t *testing.T) {
	setupTestDB(t)
	withAliveTmux(t, "cc-worker", "cc-old")
	const conv = "11111111-2222-4333-8444-555555555555"
	const oldConv = "99999999-2222-4333-8444-555555555555"
	agentID, _, err := db.EnsureAgentForConv(conv, "test")
	require.NoError(t, err)
	require.NoError(t, db.LinkConvToAgent(oldConv, agentID, "", "test"))
	saveSessionRow(t, &session.SessionState{
		ID: "cc-worker", TmuxSession: "cc-worker", ConvID: conv, Status: session.StatusRunning,
	})
	saveSessionRow(t, &session.SessionState{
		ID: "cc-old", TmuxSession: "cc-old", ConvID: oldConv, Status: session.StatusRunning,
	})

	assert.Empty(t, listedSessionNames(t))
}

// A session whose pane is gone has nothing to go to.
func TestListLocalNonAgentSessionsSkipsDeadPanes(t *testing.T) {
	setupTestDB(t)
	withAliveTmux(t) // nothing alive
	saveSessionRow(t, &session.SessionState{
		ID: "scratch", TmuxSession: "scratch", Status: session.StatusRunning,
		Harness: session.ShellHarnessName,
	})

	assert.Empty(t, listedSessionNames(t))
}

// A row marked exited is not offered as live, matching `session ls` without
// -a. It matters beyond tidiness: an exited row keeps the tmux name it had,
// and dir-derived names are reused, so a dead namesake can match a LIVE
// session's name — and would otherwise be listed beside it as a second,
// indistinguishable row whose delete kills the live one's pane.
func TestListLocalNonAgentSessionsSkipsExitedRowsIncludingDeadNamesakes(t *testing.T) {
	setupTestDB(t)
	withAliveTmux(t, "src")
	saveSessionRow(t, &session.SessionState{
		ID: "dead-namesake", TmuxSession: "src", Cwd: "/home/op/src",
		Status: session.StatusExited, Harness: session.ShellHarnessName,
	})
	saveSessionRow(t, &session.SessionState{
		ID: "live-one", TmuxSession: "src", Cwd: "/home/op/src",
		Status: session.StatusRunning, Harness: session.ShellHarnessName,
	})

	rows, err := listLocalNonAgentSessions()
	require.NoError(t, err)
	require.Len(t, rows, 1, "one live pane is one row")
	assert.Equal(t, "live-one", rows[0].SessionID)
}

// Two rows claiming one live tmux name, neither yet reaped: the row that last
// wrote is the one that owns the pane.
func TestListLocalNonAgentSessionsKeepsOneRowPerLivePane(t *testing.T) {
	setupTestDB(t)
	withAliveTmux(t, "src")
	saveSessionRow(t, &session.SessionState{
		ID: "stale", TmuxSession: "src", Status: session.StatusRunning,
		Harness: session.ShellHarnessName,
	})
	time.Sleep(1100 * time.Millisecond) // the stored stamp has second resolution
	saveSessionRow(t, &session.SessionState{
		ID: "current", TmuxSession: "src", Status: session.StatusRunning,
		Harness: session.ShellHarnessName,
	})

	rows, err := listLocalNonAgentSessions()
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "current", rows[0].SessionID)
}

// A spawn writes its session row — and its pane — before the conversation is
// linked to an actor. For that window nothing durable says the row is an
// agent's, so the pending row is what keeps a launch from flashing past as a
// session the operator could kill.
func TestListLocalNonAgentSessionsSkipsAPendingSpawn(t *testing.T) {
	setupTestDB(t)
	withAliveTmux(t, "spwn-abc123")
	groupID, err := db.CreateAgentGroup("dev", "")
	require.NoError(t, err)
	require.NoError(t, db.InsertPendingSpawn(&db.PendingSpawn{Label: "spwn-abc123", GroupID: groupID}))
	saveSessionRow(t, &session.SessionState{
		ID: "spwn-abc123", TmuxSession: "spwn-abc123", Status: session.StatusRunning,
	})

	require.Empty(t, listedSessionNames(t), "a launching agent is not a session")

	// Once the spawn settles its pending row goes, and the conversation link
	// is what keeps the row out of this listing from then on.
	require.NoError(t, db.DeletePendingSpawn("spwn-abc123"))
	assert.Equal(t, []string{"spwn-abc123"}, listedSessionNames(t),
		"with neither marker left the row really is a plain session")
}

// Reincarnate and clone mint a launch with no pending row, so they carry an
// in-memory claim for the same window instead. Which id they claim depends on
// which one they mint first — a no-copy launch has only a label, a copy clone
// forks the jsonl and so knows the conv-id before the row exists — and the
// listing has to honour a claim on either.
func TestListLocalNonAgentSessionsSkipsAnInFlightRelaunch(t *testing.T) {
	const conv = "77777777-2222-4333-8444-555555555555"
	for _, tc := range []struct {
		name     string
		claim    string
		row      session.SessionState
		wantName string
	}{
		{
			name:     "claimed by label",
			claim:    "spwn-def456",
			row:      session.SessionState{ID: "spwn-def456", TmuxSession: "spwn-def456"},
			wantName: "spwn-def456",
		},
		{
			// The copy path's row carries the conv it was launched with; its
			// label is only discovered from that row afterwards, so the conv
			// is the only id the claim can be made on.
			name:     "claimed by conv-id",
			claim:    conv,
			row:      session.SessionState{ID: "spwn-ghi789", TmuxSession: "spwn-ghi789", ConvID: conv},
			wantName: "spwn-ghi789",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupTestDB(t)
			withAliveTmux(t, tc.row.TmuxSession)
			row := tc.row
			row.Status = session.StatusRunning
			saveSessionRow(t, &row)

			release := claimAgentLaunchIdentity(tc.claim)
			require.Empty(t, listedSessionNames(t), "a relaunching agent is not a session")

			release()
			assert.Equal(t, []string{tc.wantName}, listedSessionNames(t),
				"and the claim is released with the launch that held it")
		})
	}
}

// cloneSpawnOnce hands its launch claim to the caller rather than releasing it
// on return — the clone is only an agent once the caller has linked it, so
// releasing and re-claiming a moment later would reopen the very window the
// claim exists to close. That makes releasing somebody else's job, so every
// caller has to do it; a real clone is driven end to end in
// TestTUIConsoleCloneLeavesNoStrandedLaunchClaim.
func TestEveryCloneSpawnCallerReleasesTheLaunchClaim(t *testing.T) {
	// A cheap structural guard against a new caller silently leaking one: the
	// symbol is only ever read where the release is run.
	for _, file := range []string{"clone.go", "export.go", "groups_clone.go"} {
		src, err := os.ReadFile(file)
		require.NoError(t, err)
		body := string(src)
		if !strings.Contains(body, "cloneSpawnOnce(cloneSpawnParams{") {
			continue
		}
		assert.Contains(t, body, "ReleaseLaunchClaim",
			"%s calls cloneSpawnOnce, so it must release the launch claim it is handed", file)
	}
}

// The claim counts, so a nested or retried claim releases correctly instead of
// the first release clearing both, and releasing twice is a no-op.
func TestAgentLaunchLabelClaimsAreCounted(t *testing.T) {
	first := claimAgentLaunchIdentity("spwn-nested")
	second := claimAgentLaunchIdentity("spwn-nested")
	require.True(t, agentLaunchInFlight("spwn-nested"))

	first()
	first()
	assert.True(t, agentLaunchInFlight("spwn-nested"), "the outstanding claim still holds")

	second()
	assert.False(t, agentLaunchInFlight("spwn-nested"))
	second()
	assert.False(t, agentLaunchInFlight("spwn-nested"))
}

// agentd started inside a tclaude shell session must not offer the operator a
// delete that kills the terminal the console is running in.
func TestListLocalNonAgentSessionsExcludesTheConsolesOwnSession(t *testing.T) {
	setupTestDB(t)
	withAliveTmux(t, "host-shell", "other")
	t.Setenv(tuiSessionIDEnv, "host-shell")
	saveSessionRow(t, &session.SessionState{
		ID: "host-shell", TmuxSession: "host-shell", Status: session.StatusRunning,
		Harness: session.ShellHarnessName,
	})
	saveSessionRow(t, &session.SessionState{
		ID: "other", TmuxSession: "other", Status: session.StatusRunning,
		Harness: session.ShellHarnessName,
	})

	assert.Equal(t, []string{"other"}, listedSessionNames(t))
}
