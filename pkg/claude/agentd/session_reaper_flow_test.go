package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// statusOf reads a session row's persisted status straight from the DB.
func statusOf(t *testing.T, sessionID string) string {
	t.Helper()
	row, err := db.LoadSession(sessionID)
	require.NoError(t, err, "LoadSession %s", sessionID)
	require.NotNil(t, row, "session row %s missing", sessionID)
	return row.Status
}

// Scenario: a live session's tmux pane dies. The reaper sweep must
// stamp status=exited on the row — no SessionEnd hook fires for an
// unclean death, so without the reaper the row stays frozen forever.
func TestSessionReaper_MarksDeadSessionExited(t *testing.T) {
	f := newFlow(t)

	const conv = "reap-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "reaped-worker")
	f.HaveAliveSession(conv, "spwn-reap", "tmux-reap", f.TestCwd("reap"))
	f.MarkOffline("tmux-reap")

	var notified []string
	reaper := agentd.NewSessionReaperForTest(0, func(convID, _ string) {
		notified = append(notified, convID)
	})

	assert.Equal(t, 1, reaper.Tick(), "the dead session should be reaped")
	assert.Equal(t, "exited", statusOf(t, "spwn-reap"),
		"reaper must persist status=exited for the dead session")
}

// Scenario: a live session's pane dies with no SessionEnd hook — an
// unclean death. The reaper must not only mark it exited but stamp
// exit_reason='unexpected', the signal the dashboard renders as
// "crashed" for Claude Code. A graceful /exit would have recorded its
// own reason via the SessionEnd hook and the reaper would have skipped
// the row.
func TestSessionReaper_StampsUnexpectedExitReason(t *testing.T) {
	f := newFlow(t)

	const conv = "uxrs-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "crashed-worker")
	f.HaveAliveSession(conv, "spwn-uxrs", "tmux-uxrs", f.TestCwd("uxrs"))
	f.MarkOffline("tmux-uxrs")

	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	require.Equal(t, 1, reaper.Tick(), "the dead session should be reaped")
	assert.Equal(t, "exited", statusOf(t, "spwn-uxrs"))

	reason, err := db.GetSessionExitReason("spwn-uxrs")
	require.NoError(t, err)
	assert.Equal(t, "unexpected", reason,
		"a session reaped with no recorded SessionEnd reason is stamped unexpected")
}

// Scenario: a Codex pane disappears with no recorded reason. Codex has
// no reliable SessionEnd hook, so a normal terminal close reaches the
// reaper looking exactly like "no reason recorded". That must render as
// plain offline, not crashed, unless another path recorded a specific
// failure reason first.
func TestSessionReaper_CodexCloseWithoutReasonStaysPlainOffline(t *testing.T) {
	f := newFlow(t)

	const conv = "codx-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "codex-worker")
	f.HaveAliveCodexSession(conv, "spwn-codx", "tmux-codx", f.TestCwd("codx"))
	f.MarkOffline("tmux-codx")

	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	require.Equal(t, 1, reaper.Tick(), "the dead Codex session should be reaped")
	assert.Equal(t, "exited", statusOf(t, "spwn-codx"))

	reason, err := db.GetSessionExitReason("spwn-codx")
	require.NoError(t, err)
	assert.Equal(t, "", reason,
		"a Codex session reaped without an explicit reason must stay plain offline")
}

// Scenario: the reaper witnesses a session alive on one tick and dead
// on the next — a genuine alive→dead transition — and notifies.
func TestSessionReaper_NotifiesOnWitnessedTransition(t *testing.T) {
	f := newFlow(t)

	const conv = "trns-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "transition-worker")
	f.HaveAliveSession(conv, "spwn-trns", "tmux-trns", f.TestCwd("trns"))

	var notified []string
	reaper := agentd.NewSessionReaperForTest(0, func(convID, prevStatus string) {
		assert.NotEmpty(t, prevStatus, "notification carries the pre-exit status")
		notified = append(notified, convID)
	})

	// Tick 1: session is alive — seeds the alive-set, reaps nothing.
	assert.Equal(t, 0, reaper.Tick(), "a live session is not reaped")
	assert.Empty(t, notified, "no notification while the session is alive")

	// The pane dies; tick 2 sees the transition.
	f.MarkOffline("tmux-trns")
	assert.Equal(t, 1, reaper.Tick(), "the now-dead session is reaped")
	assert.Equal(t, []string{conv}, notified,
		"a witnessed alive→dead transition must notify exactly once")
	assert.Equal(t, "exited", statusOf(t, "spwn-trns"))
}

// Scenario: a session that is already dead when the reaper starts is a
// pre-existing corpse, not a transition. It must be reaped (DB hygiene)
// but NOT notified — otherwise a daemon restart fires a notification
// storm for the whole backlog of stale rows.
func TestSessionReaper_NoNotifyForPreexistingCorpse(t *testing.T) {
	f := newFlow(t)

	const conv = "corp-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "corpse-worker")
	f.HaveAliveSession(conv, "spwn-corp", "tmux-corp", f.TestCwd("corp"))
	f.MarkOffline("tmux-corp") // dead before the reaper's first sweep

	var notified []string
	reaper := agentd.NewSessionReaperForTest(0, func(convID, _ string) {
		notified = append(notified, convID)
	})

	assert.Equal(t, 1, reaper.Tick(), "a pre-existing corpse is still reaped")
	assert.Equal(t, "exited", statusOf(t, "spwn-corp"))
	assert.Empty(t, notified,
		"the first sweep only seeds — a pre-existing corpse must not notify")
}

func TestSessionReaper_RestartReconcilesRetainedPaneEvidenceThenCleansIt(t *testing.T) {
	f := newFlow(t)
	const conv = "dead-1111-2222-3333-444444444444"
	const tmuxName = "tmux-retained-status"
	f.HaveConvWithTitle(conv, "retained-worker")
	f.HaveAliveSession(conv, "spwn-retained", tmuxName, "/tmp/retained")
	code := 9
	f.World.Tmux.MarkPaneDead(tmuxName, &code, "")
	assert.False(t, session.IsTmuxSessionAlive(tmuxName),
		"a retained dead pane is immediately offline on point-read status paths")
	live, err := session.LiveTmuxSessions()
	require.NoError(t, err)
	assert.NotContains(t, live, tmuxName,
		"snapshot/list status paths must also exclude retained dead panes")

	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	require.Equal(t, 1, reaper.Tick())
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, db.AgentExitObserverReconcile, rows[0].Observer)
	assert.Equal(t, db.AgentExitCauseNormal, rows[0].CauseKind)
	require.NotNil(t, rows[0].ExitCode)
	assert.Equal(t, 9, *rows[0].ExitCode)
	assert.Equal(t, db.AgentExitObservedProcessPaneBootstrap, rows[0].ObservedProcess)
	assert.NotContains(t, f.World.Tmux.Sessions(), tmuxName, "record-first cleanup removes retained corpse")
}

func TestSessionReaper_PredecessorRetainedPaneCannotExitSuccessorGeneration(t *testing.T) {
	f := newFlow(t)
	const conv = "stale-pane-1111-2222-3333-444444444444"
	const tmuxName = "tmux-stale-retained"
	const predecessor = "11111111111111111111111111111111"
	const successor = "22222222222222222222222222222222"
	f.HaveConvWithTitle(conv, "stale-pane-worker")
	f.HaveAliveSession(conv, "spwn-stale-retained", tmuxName, "/tmp/stale-retained")
	originalStatus := statusOf(t, "spwn-stale-retained")
	require.NoError(t, db.SetSessionExitLaunchGeneration("spwn-stale-retained", successor))
	f.World.Tmux.SetPaneExitGeneration(tmuxName, predecessor)
	code := 4
	f.World.Tmux.MarkPaneDead(tmuxName, &code, "")

	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	assert.Equal(t, 0, reaper.Tick())
	assert.Equal(t, originalStatus, statusOf(t, "spwn-stale-retained"),
		"predecessor evidence must not mutate the successor row")
	assert.NotContains(t, f.World.Tmux.Sessions(), tmuxName,
		"the exact predecessor corpse is safe to clean without attributing it to the successor")
	n, err := db.CountAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestSessionReaper_CleanupFailureKeepsSavedEvidenceAndExitedState(t *testing.T) {
	f := newFlow(t)
	const conv = "fail-1111-2222-3333-444444444444"
	const tmuxName = "tmux-retained-fail"
	f.HaveConvWithTitle(conv, "cleanup-fail-worker")
	f.HaveAliveSession(conv, "spwn-retained-fail", tmuxName, "/tmp/retained-fail")
	f.World.Tmux.MarkPaneDead(tmuxName, nil, "15")
	for range 3 {
		f.World.Tmux.FailNextCommand("kill-pane")
	}
	f.World.Tmux.FailNextCommand("kill-session")

	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	require.Equal(t, 1, reaper.Tick())
	assert.Equal(t, "exited", statusOf(t, "spwn-retained-fail"))
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, db.AgentExitCauseSignal, rows[0].CauseKind)
	assert.Equal(t, "15", rows[0].Signal)
	assert.Contains(t, f.World.Tmux.Sessions(), tmuxName, "failed cleanup leaves retained evidence for a later sweep")
	assert.Equal(t, 0, reaper.Tick(), "the already-exited row needs cleanup, not another lifecycle transition")
	assert.NotContains(t, f.World.Tmux.Sessions(), tmuxName,
		"a later sweep retries record-first cleanup even though lifecycle state is already exited")
}

func TestSessionReaper_AuditFailureRemovesRetainedPaneAfterBoundedRetries(t *testing.T) {
	f := newFlow(t)
	const conv = "audit-fail-1111-2222-3333-444444444444"
	const tmuxName = "tmux-retained-audit-fail"
	f.HaveConvWithTitle(conv, "audit-fail-worker")
	f.HaveAliveSession(conv, "spwn-retained-audit-fail", tmuxName, "/tmp/audit-fail")
	originalStatus := statusOf(t, "spwn-retained-audit-fail")
	code := 17
	f.World.Tmux.MarkPaneDead(tmuxName, &code, "")
	d, err := db.Open()
	require.NoError(t, err)
	require.NoError(t, func() error {
		_, dropErr := d.Exec(`DROP TABLE audit_log`)
		return dropErr
	}())

	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	assert.Equal(t, 0, reaper.Tick())
	assert.Equal(t, 0, reaper.Tick())
	assert.Contains(t, f.World.Tmux.Sessions(), tmuxName,
		"retained evidence survives the bounded audit retry window")
	assert.Equal(t, 0, reaper.Tick())
	assert.NotContains(t, f.World.Tmux.Sessions(), tmuxName,
		"the retry bound removes the credential-bearing retained pane even while audit storage is unavailable")
	assert.Equal(t, originalStatus, statusOf(t, "spwn-retained-audit-fail"),
		"failed atomic persistence must not partially change lifecycle state")
}

// Scenario: a session row created moments ago (mid-spawn — its tmux
// session may not be up yet) is exempt from reaping for the grace
// window, so a starting agent never flashes "exited".
func TestSessionReaper_GracePeriodSkipsFreshRow(t *testing.T) {
	f := newFlow(t)

	const conv = "grce-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "fresh-worker")
	f.HaveAliveSession(conv, "spwn-grce", "tmux-grce", f.TestCwd("grce"))
	// Stamp the row as just-created and take its tmux session down, as
	// if the sweep landed in the gap before the pane came up.
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          "spwn-grce",
		TmuxSession: "tmux-grce",
		ConvID:      conv,
		Cwd:         f.TestCwd("grce"),
		Status:      "running",
		CreatedAt:   time.Now(),
	}))
	f.MarkOffline("tmux-grce")

	withGrace := agentd.NewSessionReaperForTest(90*time.Second, func(string, string) {})
	assert.Equal(t, 0, withGrace.Tick(), "a fresh row is exempt from reaping")
	assert.Equal(t, "running", statusOf(t, "spwn-grce"),
		"a row inside the grace window must keep its status")

	// With the grace window disabled the same row is reaped.
	noGrace := agentd.NewSessionReaperForTest(0, func(string, string) {})
	assert.Equal(t, 1, noGrace.Tick(), "past the grace window the dead row is reaped")
	assert.Equal(t, "exited", statusOf(t, "spwn-grce"))
}
