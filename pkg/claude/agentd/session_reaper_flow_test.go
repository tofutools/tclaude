package agentd_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
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

func TestSessionReaper_BackgroundActivityGatesStableIdleNotification(t *testing.T) {
	f := newFlow(t)

	const (
		conv      = "idlework-1111-2222-3333-444444444444"
		sessionID = "spwn-idlework"
	)
	f.HaveConvWithTitle(conv, "background-worker")
	f.HaveAliveSession(conv, sessionID, "tmux-idlework", f.TestCwd("idlework"))

	row, err := db.LoadSession(sessionID)
	require.NoError(t, err)
	row.Status = session.StatusIdle
	row.StatusDetail = ""
	row.SubagentCount = 1
	row.SubagentsJSON = db.SubagentSet{
		"agent-live": {Type: "Explore", Seen: time.Now()},
	}.Encode()
	require.NoError(t, db.SaveSession(row))
	row, err = db.LoadSession(sessionID)
	require.NoError(t, err)

	var idleNotifications []string
	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	reaper.SetIdleNotify(func(convID, _ string) {
		idleNotifications = append(idleNotifications, convID)
	})

	reaper.TickAt(row.UpdatedAt.Add(time.Second))
	assert.Equal(t, session.StatusMainAgentIdle, statusOf(t, sessionID),
		"a live sub-agent must hold a raw idle row at idle + work")
	assert.Empty(t, idleNotifications, "background work must suppress the Idle notification")

	// Model a lost SubagentStop whose ledger entry has now crossed its TTL.
	// The first zero-activity sweep settles the row but intentionally does not
	// notify; only a later sweep may confirm that the idle state stayed stable.
	row, err = db.LoadSession(sessionID)
	require.NoError(t, err)
	row.SubagentsJSON = db.SubagentSet{
		"agent-lost-stop": {
			Type: "Explore",
			Seen: time.Now().Add(-db.SubagentTTL - time.Minute),
		},
	}.Encode()
	require.NoError(t, db.SaveSession(row))
	row, err = db.LoadSession(sessionID)
	require.NoError(t, err)

	settledAt := row.UpdatedAt.Add(time.Second)
	reaper.TickAt(settledAt)
	assert.Equal(t, session.StatusIdle, statusOf(t, sessionID))
	assert.Empty(t, idleNotifications,
		"the reconcile tick that first sees zero activity must not announce idle")

	reaper.TickAt(settledAt.Add(6 * time.Second))
	assert.Equal(t, []string{conv}, idleNotifications,
		"a later sweep may announce the unchanged, activity-free idle state")
}

func TestSessionReaper_ExpiredBackgroundShellSettlesBeforeIdleNotification(t *testing.T) {
	f := newFlow(t)

	const (
		conv      = "idleshell-1111-2222-3333-444444444444"
		sessionID = "spwn-idleshell"
	)
	f.HaveAliveSession(conv, sessionID, "tmux-idleshell", f.TestCwd("idleshell"))

	row, err := db.LoadSession(sessionID)
	require.NoError(t, err)
	row.Status = session.StatusMainAgentIdle
	row.StatusDetail = "1 background shell running"
	row.BgShellsJSON = db.BgShellSet{
		"shell-lost-exit": {
			Command: "go test ./...",
			Seen:    time.Now().Add(-db.BgShellTTL - time.Minute),
		},
	}.Encode()
	require.NoError(t, db.SaveSession(row))
	row, err = db.LoadSession(sessionID)
	require.NoError(t, err)

	var idleNotifications []string
	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	reaper.SetIdleNotify(func(convID, _ string) {
		idleNotifications = append(idleNotifications, convID)
	})

	settledAt := row.UpdatedAt.Add(time.Second)
	reaper.TickAt(settledAt)
	assert.Equal(t, session.StatusIdle, statusOf(t, sessionID))
	assert.Empty(t, idleNotifications,
		"the first shell-free observation only starts the stability window")

	reaper.TickAt(settledAt.Add(6 * time.Second))
	assert.Equal(t, []string{conv}, idleNotifications,
		"an unchanged shell-free idle row is announced on the later sweep")
}

func TestSessionReaper_StartupDoesNotNotifyHistoricalIdleRows(t *testing.T) {
	f := newFlow(t)

	const (
		conv      = "oldidle-1111-2222-3333-444444444444"
		sessionID = "spwn-oldidle"
	)
	f.HaveAliveSession(conv, sessionID, "tmux-oldidle", f.TestCwd("oldidle"))
	row, err := db.LoadSession(sessionID)
	require.NoError(t, err)
	row.Status = session.StatusIdle
	require.NoError(t, db.SaveSession(row))
	row, err = db.LoadSession(sessionID)
	require.NoError(t, err)

	var idleNotifications []string
	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	reaper.SetIdleNotify(func(convID, _ string) {
		idleNotifications = append(idleNotifications, convID)
	})
	reaper.TickAt(row.UpdatedAt.Add(time.Hour))

	assert.Empty(t, idleNotifications,
		"a fresh daemon must not announce the backlog of already-idle sessions")
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

// Scenario: a spawn's pane comes up, so an async spawn reports "launched" and
// stops watching, and the tclaude-layer supervisor behind it dies a few seconds
// later — refused by the host's own confinement, or standing up a
// filtered-network gateway that never came up. tmux has closed the pane's pty
// but not yet reaped its child, so the corpse reports dead with NO exit status,
// and the pane-died callback never records: this sweep is the only observer.
//
// Expected: the supervisor's own error reaches the log before the corpse is
// reaped. Without it the operator gets one audit line reading
// "cause_kind=unknown, exit_code=unavailable" — the failure with its
// explanation deleted — and nothing anywhere else.
func TestSessionReaper_DeadPaneOutputIsLoggedBeforeTheCorpseIsReaped(t *testing.T) {
	f := newFlow(t)

	const conv = "aapr-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "private-network-worker")
	f.HaveAliveSession(conv, "spwn-aapr", "tmux-aapr", f.TestCwd("aapr"))
	const paneError = "tclaude: terminal resize relay: inspect filtered-network " +
		"namespace identity: readlink /proc/74344/ns/net: permission denied"
	f.World.Tmux.SetDeadPaneOutput("tmux-aapr", paneError)
	f.World.Tmux.MarkPaneDead("tmux-aapr", nil, "")

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	require.Equal(t, 1, reaper.Tick(), "the dead pane should be reaped")
	assert.Equal(t, "exited", statusOf(t, "spwn-aapr"))

	// Assert against THIS line, not the buffer. The pre-existing "managed pane
	// exit observed" audit line lands in the same buffer and renders a nil exit
	// code as "unavailable" too, so a whole-buffer assertion on that value would
	// pass with the capture removed entirely.
	captured := startupFailureLogRecord(t, logs.Bytes())
	assert.Contains(t, captured["pane_output"], "readlink /proc/74344/ns/net: permission denied",
		"the supervisor's own error is the whole point of the capture")
	// The audit row's own word for "tmux had not reaped the child yet", so the
	// log line and the audit line describe the same exit in the same terms.
	assert.Equal(t, "unavailable", captured["exit_code"])
	assert.Equal(t, "reconcile", captured["observer"])
	assert.NotEmpty(t, captured["event_id"],
		"the capture must name the audit row it explains")
}

// startupFailureLogRecord returns the one JSON log record emitted by the
// startup-failure capture, failing the test if there is not exactly one. Every
// field is flattened to a string; the capture emits only string values.
func startupFailureLogRecord(t *testing.T, logs []byte) map[string]string {
	t.Helper()
	var found []map[string]string
	for _, line := range bytes.Split(bytes.TrimSpace(logs), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record map[string]string
		if err := json.Unmarshal(line, &record); err != nil {
			continue // a record with non-string values is not this one
		}
		if record["msg"] == "managed pane failed during startup" {
			found = append(found, record)
		}
	}
	require.Len(t, found, 1, "expected exactly one startup-failure capture in:\n%s", logs)
	return found[0]
}

// requireNoStartupFailureCapture fails unless the sweep captured nothing.
func requireNoStartupFailureCapture(t *testing.T, logs []byte, paneText string) {
	t.Helper()
	got := string(logs)
	assert.NotContains(t, got, `"msg":"managed pane failed during startup"`)
	assert.NotContains(t, got, paneText,
		"this pane's contents must never be copied into the log")
}

// Scenario: an agent is stopped on purpose — a lifecycle exit with a recorded
// intent — and the reaper, not the pane callback, is the observer of the
// resulting pane death. The pane exits non-zero, so the clean-exit gate does
// NOT stop it; only the recorded lifecycle action does.
//
// Expected: no capture. The startup-failure path exists for launches that died
// with something to explain; a deliberate stop has nothing to explain, and its
// pane holds a working agent's terminal, which must not be copied into the log
// just because the reaper happened to be the one who noticed.
func TestSessionReaper_ExpectedLifecycleExitIsNotCaptured(t *testing.T) {
	f := newFlow(t)

	const conv = "lcyc-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "stopped-worker")
	f.HaveAliveSession(conv, "spwn-lcyc", "tmux-lcyc", f.TestCwd("lcyc"))
	const paneText = "a deliberately stopped agent's conversation"
	f.World.Tmux.SetDeadPaneOutput("tmux-lcyc", paneText)
	// Non-zero, so the clean-exit gate cannot be what excludes this.
	code := 1
	f.World.Tmux.MarkPaneDead("tmux-lcyc", &code, "")
	_, err := db.SetSessionExitIntent("spwn-lcyc", db.AgentExitActionStop, "", time.Now())
	require.NoError(t, err)

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	require.Equal(t, 1, reaper.Tick(), "the dead pane should still be reaped")
	require.Contains(t, logs.String(), `"lifecycle_action":"stop"`,
		"the exit must actually have been recorded as a lifecycle stop")

	requireNoStartupFailureCapture(t, logs.Bytes(), paneText)
}

// Scenario: an agent's pane exits 0 with no lifecycle intent recorded — an
// ordinary clean close the reaper happens to observe.
//
// Expected: no capture. Only a literal 0 is a clean exit, and a clean exit has
// nothing to diagnose.
func TestSessionReaper_CleanExitIsNotCaptured(t *testing.T) {
	f := newFlow(t)

	const conv = "clen-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "clean-worker")
	f.HaveAliveSession(conv, "spwn-clen", "tmux-clen", f.TestCwd("clen"))
	const paneText = "a working agent's conversation"
	f.World.Tmux.SetDeadPaneOutput("tmux-clen", paneText)
	code := 0
	f.World.Tmux.MarkPaneDead("tmux-clen", &code, "")

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	require.Equal(t, 1, reaper.Tick(), "the dead pane should still be reaped")

	requireNoStartupFailureCapture(t, logs.Bytes(), paneText)
}

// Scenario: a launch fails the same way as the capture test above, but the
// sweep does not reach it until well past the startup window — a long-offline
// daemon catching up, say.
//
// Expected: no capture. Past the window the sweep can no longer tell a launch
// failure from an agent that worked for a while and then crashed, and the
// second must not have its terminal copied into the log.
func TestSessionReaper_DeathObservedPastTheWindowIsNotCaptured(t *testing.T) {
	f := newFlow(t)

	const conv = "oldw-1111-2222-3333-444444444444"
	f.HaveConvWithTitle(conv, "late-observed-worker")
	f.HaveAliveSession(conv, "spwn-oldw", "tmux-oldw", f.TestCwd("oldw"))
	const paneText = "tclaude: terminal resize relay: something went wrong"
	f.World.Tmux.SetDeadPaneOutput("tmux-oldw", paneText)
	f.World.Tmux.MarkPaneDead("tmux-oldw", nil, "")

	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
	// Reachable only because the window is measured against the sweep's own
	// clock rather than time.Now().
	reaper.TickAt(time.Now().Add(time.Hour))

	requireNoStartupFailureCapture(t, logs.Bytes(), paneText)
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

func TestSessionReaper_SessionEndRaceEnrichesBeforeRetainedPaneCleanup(t *testing.T) {
	f := newFlow(t)
	const conv = "hook-race-1111-2222-3333-444444444444"
	const sessionID = "spwn-hook-race"
	const tmuxName = "tmux-hook-race"
	const generation = "abababababababababababababababab"
	f.HaveConvWithTitle(conv, "hook-race-worker")
	f.HaveAliveSession(conv, sessionID, tmuxName, "/tmp/hook-race")
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID, generation))
	f.World.Tmux.SetPaneExitGeneration(tmuxName, generation)
	accepted, _, err := db.RecordSessionEndExitObservation(db.AgentExitObservation{
		SessionID: sessionID, TmuxSession: tmuxName,
		Observer: db.AgentExitObserverHook, CauseKind: db.AgentExitCauseNormal,
		Reason: "logout", ObservedState: session.StatusExited, ExpectedGeneration: generation,
	})
	require.NoError(t, err)
	require.True(t, accepted)
	code := 9
	f.World.Tmux.MarkPaneDead(tmuxName, &code, "")

	reaper := agentd.NewSessionReaperForTest(90*time.Second, func(string, string) {})
	assert.Equal(t, 0, reaper.Tick(), "SessionEnd already committed the lifecycle transition")
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1, "hook and retained-pane observations converge on one launch event")
	assert.Equal(t, db.AgentExitObserverHook, rows[0].Observer)
	require.NotNil(t, rows[0].ExitCode)
	assert.Equal(t, code, *rows[0].ExitCode, "exact pane evidence enriches the hook event before cleanup")
	assert.Equal(t, "logout", rows[0].Reason)
	assert.NotContains(t, f.World.Tmux.Sessions(), tmuxName, "enriched retained evidence is then cleaned")
}

func TestSessionReaper_SessionEndRaceBoundsEnrichmentPersistenceFailure(t *testing.T) {
	f := newFlow(t)
	const conv = "hook-fail-1111-2222-3333-444444444444"
	const sessionID = "spwn-hook-fail"
	const tmuxName = "tmux-hook-fail"
	const generation = "bcbcbcbcbcbcbcbcbcbcbcbcbcbcbcbc"
	f.HaveConvWithTitle(conv, "hook-fail-worker")
	f.HaveAliveSession(conv, sessionID, tmuxName, "/tmp/hook-fail")
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID, generation))
	f.World.Tmux.SetPaneExitGeneration(tmuxName, generation)
	accepted, _, err := db.RecordSessionEndExitObservation(db.AgentExitObservation{
		SessionID: sessionID, TmuxSession: tmuxName,
		Observer: db.AgentExitObserverHook, CauseKind: db.AgentExitCauseNormal,
		Reason: "logout", ObservedState: session.StatusExited, ExpectedGeneration: generation,
	})
	require.NoError(t, err)
	require.True(t, accepted)
	code := 23
	f.World.Tmux.MarkPaneDead(tmuxName, &code, "")
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TRIGGER fail_exit_audit_enrichment
		BEFORE UPDATE ON audit_log BEGIN
			SELECT RAISE(FAIL, 'forced exit audit enrichment failure');
		END`)
	require.NoError(t, err)

	reaper := agentd.NewSessionReaperForTest(90*time.Second, func(string, string) {})
	assert.Equal(t, 0, reaper.Tick())
	assert.Equal(t, 0, reaper.Tick())
	assert.Contains(t, f.World.Tmux.Sessions(), tmuxName,
		"exact evidence survives the bounded audit enrichment retry window")
	assert.Equal(t, 0, reaper.Tick())
	assert.NotContains(t, f.World.Tmux.Sessions(), tmuxName,
		"the credential-bearing retained pane is removed after the bounded failure policy")
}

func TestSessionReaper_FreshRetainedDeadPaneBypassesSpawnGrace(t *testing.T) {
	f := newFlow(t)
	const conv = "fresh-dead-1111-2222-3333-444444444444"
	const sessionID = "spwn-fresh-dead"
	const tmuxName = "tmux-fresh-dead"
	const generation = "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	f.HaveConvWithTitle(conv, "fresh-dead-worker")
	f.HaveAliveSession(conv, sessionID, tmuxName, "/tmp/fresh-dead")
	require.NoError(t, db.SetSessionExitLaunchGeneration(sessionID, generation))
	f.World.Tmux.SetPaneExitGeneration(tmuxName, generation)
	code := 17
	f.World.Tmux.MarkPaneDead(tmuxName, &code, "")

	reaper := agentd.NewSessionReaperForTest(90*time.Second, func(string, string) {})
	require.Equal(t, 1, reaper.Tick(), "exact dead-pane evidence is not an ambiguous mid-spawn absence")
	assert.Equal(t, session.StatusExited, statusOf(t, sessionID))
	rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].ExitCode)
	assert.Equal(t, code, *rows[0].ExitCode)
	assert.NotContains(t, f.World.Tmux.Sessions(), tmuxName, "exact evidence is recorded before cleanup")
}

func TestSessionReaper_DoesNotInferSignalsFromConventionalExitCodes(t *testing.T) {
	tests := []struct {
		name, conv, sessionID, tmuxName string
		code                            int
	}{
		{name: "exit-137", conv: "code-137-1111-2222-3333-444444444444", sessionID: "spwn-code-137", tmuxName: "tmux-code-137", code: 137},
		{name: "exit-143", conv: "code-143-1111-2222-3333-444444444444", sessionID: "spwn-code-143", tmuxName: "tmux-code-143", code: 143},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveConvWithTitle(tc.conv, tc.name)
			f.HaveAliveSession(tc.conv, tc.sessionID, tc.tmuxName, "/tmp/"+tc.name)
			f.World.Tmux.MarkPaneDead(tc.tmuxName, &tc.code, "")

			reaper := agentd.NewSessionReaperForTest(0, func(string, string) {})
			require.Equal(t, 1, reaper.Tick())
			rows, err := db.ListAuditLog(db.AuditLogFilter{Verb: db.AuditVerbAgentExit})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.NotNil(t, rows[0].ExitCode)
			assert.Equal(t, tc.code, *rows[0].ExitCode)
			assert.Equal(t, db.AgentExitCauseNormal, rows[0].CauseKind)
			assert.Empty(t, rows[0].Signal)
			assert.Contains(t, rows[0].Detail, "signal=unavailable")
		})
	}
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
