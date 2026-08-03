package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// resetCopilotContextRefreshStateForTest drops the memoized followers and the
// per-session throttle so a test can drive consecutive refreshes, and clears
// the shutdown latch a stop() in an earlier test would otherwise leave set.
func resetCopilotContextRefreshStateForTest() {
	copilotContextRefreshMu.Lock()
	defer copilotContextRefreshMu.Unlock()
	copilotContextRefreshMu.states = nil
	copilotContextRefreshMu.stopping = false
}

const copilotRefreshConvID = "3f2a1b0c-9d8e-4f76-8a55-1c2d3e4f5a6b"

// copilotRefreshHome lays out a COPILOT_HOME with one session log and points
// the harness at it.
func copilotRefreshHome(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "copilot-home")
	require.NoError(t, os.MkdirAll(
		filepath.Join(home, "session-state", copilotRefreshConvID), 0o755))
	t.Setenv(harness.CopilotHomeEnvVar, home)
	return home
}

func copilotRefreshLogPath(home string) string {
	return filepath.Join(home, "session-state", copilotRefreshConvID, "events.jsonl")
}

func appendCopilotRefreshEvents(t *testing.T, path string, lines ...string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	_, err = file.WriteString(strings.Join(lines, "\n") + "\n")
	require.NoError(t, err)
}

func copilotRefreshSession(t *testing.T, sessionID string) *db.SessionRow {
	t.Helper()
	sess := &db.SessionRow{
		ID: sessionID, ConvID: copilotRefreshConvID, TmuxSession: "copilot-pane",
		Status: "idle", Harness: harness.CopilotName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))
	return sess
}

// TestCopilotContextRefreshWritesOnlyDisclosedFields is the honesty test for
// the read path. A running Copilot session persists per-turn output tokens and
// nothing else about its context window, so the row must show exactly that —
// tokens_output advancing, and context_pct/context_window_size still zero
// rather than filled in with a guess.
func TestCopilotContextRefreshWritesOnlyDisclosedFields(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	home := copilotRefreshHome(t)
	path := copilotRefreshLogPath(home)
	appendCopilotRefreshEvents(t, path,
		`{"type":"session.start","data":{"sessionId":"s","copilotVersion":"1.0.77","selectedModel":"gpt-5.4"}}`,
		`{"type":"user.message","data":{"content":"hi"}}`,
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":40}}`)

	sess := copilotRefreshSession(t, "copilot-live-session")
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(40), snap.TokensOutput,
		"assistant.message.outputTokens is durable and must reach the row")
	assert.Zero(t, snap.TokensInput,
		"Copilot persists no per-turn input tokens; a fabricated one is the bug")
	assert.Zero(t, snap.ContextWindowSize,
		"no compaction has disclosed a window limit yet")
	assert.Zero(t, snap.ContextPct)

	// A compaction is the first authoritative context disclosure. Only now may
	// a percentage and a window appear.
	resetCopilotContextRefreshStateForTest()
	appendCopilotRefreshEvents(t, path,
		`{"type":"session.compaction_start","data":{"currentTokens":64000,"tokenLimit":128000,"trigger":"threshold"}}`)
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(128000), snap.ContextWindowSize)
	assert.InDelta(t, 50.0, snap.ContextPct, 0.001)
	assert.Equal(t, int64(40), snap.TokensOutput, "the earlier reading must survive")
}

// TestCopilotContextRefreshResumesFromCheckpoint is the daemon-restart case:
// the in-memory follower disappears, the durable checkpoint and the
// append-only log survive, and the next refresh continues the same fold
// instead of starting over or losing the earlier lifetime.
func TestCopilotContextRefreshResumesFromCheckpoint(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	const sessionID = "copilot-checkpoint-session"
	home := copilotRefreshHome(t)
	path := copilotRefreshLogPath(home)
	appendCopilotRefreshEvents(t, path,
		`{"type":"session.start","data":{"sessionId":"s","selectedModel":"gpt-5.4"}}`,
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":25}}`)

	sess := copilotRefreshSession(t, sessionID)
	refreshCopilotContextSnapshotOnRead(sess, true)

	checkpoint, err := db.LoadCopilotTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, checkpoint, "a successful scan must leave a durable checkpoint")
	require.NotEmpty(t, checkpoint.Data)

	// The daemon restarts. Every follower is gone; the DB and the log are not.
	resetCopilotContextRefreshStateForTest()
	appendCopilotRefreshEvents(t, path,
		`{"type":"session.resume","data":{"resumeTime":"2026-01-01T00:00:00Z","eventCount":2,"selectedModel":"gpt-5.4"}}`,
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":15}}`)
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(40), snap.TokensOutput,
		"25 from before the restart plus 15 after it — the fold must be continued, not restarted")
}

// TestCopilotContextRefreshRepairsCorruptCheckpoint pins the repair path: a
// checkpoint the follower refuses must be dropped and the log rebuilt, not
// reloaded and rejected forever.
func TestCopilotContextRefreshRepairsCorruptCheckpoint(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	const sessionID = "copilot-corrupt-checkpoint-session"
	home := copilotRefreshHome(t)
	path := copilotRefreshLogPath(home)
	appendCopilotRefreshEvents(t, path,
		`{"type":"session.start","data":{"sessionId":"s","selectedModel":"gpt-5.4"}}`,
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":7}}`)

	sess := copilotRefreshSession(t, sessionID)
	require.NoError(t, db.SaveCopilotTelemetryCheckpoint(sessionID, []byte(`{"version":999}`)))

	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(7), snap.TokensOutput,
		"an unusable checkpoint must degrade to a full rebuild, never to no data")

	stored, err := db.LoadCopilotTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	assert.NotContains(t, string(stored.Data), `"version":999`,
		"the refused checkpoint must be replaced, not left to be refused again")
}

// TestCopilotContextRefreshIgnoresOtherHarnessesAndDeadRows keeps the
// read-through from touching rows it has no business touching.
func TestCopilotContextRefreshIgnoresOtherHarnessesAndDeadRows(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	home := copilotRefreshHome(t)
	appendCopilotRefreshEvents(t, copilotRefreshLogPath(home),
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":99}}`)

	codexRow := copilotRefreshSession(t, "not-copilot")
	codexRow.Harness = harness.CodexName
	require.NoError(t, db.SaveSession(codexRow))
	refreshCopilotContextSnapshotOnRead(codexRow, true)
	snap, err := db.GetContextSnapshot(codexRow.ID)
	require.NoError(t, err)
	assert.Zero(t, snap.TokensOutput, "a Codex row must not be written by the Copilot path")

	deadRow := copilotRefreshSession(t, "copilot-dead")
	refreshCopilotContextSnapshotOnRead(deadRow, false)
	snap, err = db.GetContextSnapshot(deadRow.ID)
	require.NoError(t, err)
	assert.Zero(t, snap.TokensOutput,
		"a dead session's log cannot grow; its final row must be left alone")
}

// TestCopilotContextRefreshToleratesMissingLog covers the absent-log state:
// Copilot creates the session directory before it writes any events.
func TestCopilotContextRefreshToleratesMissingLog(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	copilotRefreshHome(t) // session dir exists, log does not
	sess := copilotRefreshSession(t, "copilot-no-log")

	refreshCopilotContextSnapshotOnRead(sess, true)
	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Zero(t, snap.TokensOutput)

	checkpoint, err := db.LoadCopilotTelemetryCheckpoint(sess.ID)
	require.NoError(t, err)
	assert.Nil(t, checkpoint, "a session with no log must not leave a checkpoint behind")
}
