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

// expireCopilotContextRefreshThrottle lets a test drive consecutive refreshes
// WITHOUT dropping the follower or the persisted mirror.
//
// This distinction is load-bearing: resetting the whole state makes the second
// refresh a fresh full rescan, so every carry-forward and supersede rule in
// persistCopilotContextSnapshot would go untested while still being reported
// as covered.
func expireCopilotContextRefreshThrottle() {
	copilotContextRefreshMu.Lock()
	defer copilotContextRefreshMu.Unlock()
	for _, state := range copilotContextRefreshMu.states {
		state.lastRefresh = time.Time{}
	}
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
	saved, err := db.SaveCopilotTelemetryCheckpoint(sessionID, sess.ConvID, sess.CreatedAt, []byte(`{"version":999}`))
	require.NoError(t, err)
	require.True(t, saved)

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

// TestCopilotOutputTokensAdvanceAfterAResume is the regression the cold review
// found. A log that still grows AND already contains a session.shutdown is
// exactly a resumed session — the case this follower exists for. Taking the
// shutdown total unconditionally pinned tokens_output to its pre-resume value
// and stopped the one durable per-turn figure from ever advancing again.
func TestCopilotOutputTokensAdvanceAfterAResume(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	home := copilotRefreshHome(t)
	path := copilotRefreshLogPath(home)
	appendCopilotRefreshEvents(t, path,
		`{"type":"session.start","data":{"sessionId":"s","selectedModel":"gpt-5.4"}}`,
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":100}}`,
		`{"type":"session.shutdown","data":{"shutdownType":"routine","currentTokens":1000,`+
			`"modelMetrics":{"gpt-5.4":{"requests":{"count":1},"usage":{"inputTokens":500,"outputTokens":100}}}}}`,
		`{"type":"session.resume","data":{"resumeTime":"2026-01-01T00:00:00Z","eventCount":3,"selectedModel":"gpt-5.4"}}`)

	sess := copilotRefreshSession(t, "copilot-resumed-session")
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100), snap.TokensOutput)
	require.Equal(t, int64(500), snap.TokensInput)

	// The resumed lifetime produces real work. Note: no state reset, so the
	// persisted mirror and the follower are the same ones as above.
	expireCopilotContextRefreshThrottle()
	appendCopilotRefreshEvents(t, path,
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":900}}`)
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), snap.TokensOutput,
		"the running per-turn sum must overtake the pre-resume shutdown total")
	assert.Equal(t, int64(500), snap.TokensInput,
		"input has no live source, so the shutdown figure is carried forward, not zeroed")
}

// TestCopilotContextWindowAndPctMoveTogether is the second cold-review
// regression: session.truncation discloses a token LIMIT but no total
// occupancy, so updating the window while keeping the previous percentage
// produced a row describing an occupancy Copilot never reported.
func TestCopilotContextWindowAndPctMoveTogether(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	home := copilotRefreshHome(t)
	path := copilotRefreshLogPath(home)
	appendCopilotRefreshEvents(t, path,
		`{"type":"session.start","data":{"sessionId":"s","selectedModel":"gpt-5.4"}}`,
		`{"type":"session.compaction_start","data":{"currentTokens":90000,"tokenLimit":128000,"trigger":"threshold"}}`)

	sess := copilotRefreshSession(t, "copilot-window-session")
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	require.Equal(t, int64(128000), snap.ContextWindowSize)
	require.InDelta(t, 70.3125, snap.ContextPct, 0.001)

	// A truncation under a much wider window. Carrying the old 70.3% forward
	// against 400000 would render ~281k tokens occupied out of nothing.
	expireCopilotContextRefreshThrottle()
	appendCopilotRefreshEvents(t, path,
		`{"type":"session.truncation","data":{"tokenLimit":400000,"preTruncationTokensInMessages":9000,`+
			`"preTruncationMessagesLength":9,"postTruncationTokensInMessages":1000,`+
			`"postTruncationMessagesLength":2,"tokensRemovedDuringTruncation":8000,`+
			`"messagesRemovedDuringTruncation":7,"performedBy":"BasicTruncator"}}`)
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(400000), snap.ContextWindowSize, "the fresh limit is recorded")
	assert.Zero(t, snap.ContextPct,
		"a limit with no disclosed occupancy must drop the percentage, never keep a stale one")
}

// TestCopilotTruncationAtTheSameWindowClearsPct is the common case, and the
// one a "only clear when the limit CHANGED" rule silently missed.
//
// session.truncation normally restates the SAME limit — the window moves only
// on a model change — so gating the clear on a changed denominator leaves the
// ordinary truncation rendering the occupancy the previous compaction
// measured, long after the conversation was cut out from under it.
func TestCopilotTruncationAtTheSameWindowClearsPct(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	home := copilotRefreshHome(t)
	path := copilotRefreshLogPath(home)
	appendCopilotRefreshEvents(t, path,
		`{"type":"session.start","data":{"sessionId":"s","selectedModel":"gpt-5.4"}}`,
		`{"type":"session.compaction_start","data":{"currentTokens":120000,"tokenLimit":128000,"trigger":"threshold"}}`)

	sess := copilotRefreshSession(t, "copilot-same-window")
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	require.InDelta(t, 93.75, snap.ContextPct, 0.001)

	// The truncation cuts the conversation to 1000 tokens under the SAME limit.
	expireCopilotContextRefreshThrottle()
	appendCopilotRefreshEvents(t, path,
		`{"type":"session.truncation","data":{"tokenLimit":128000,"preTruncationTokensInMessages":119000,`+
			`"preTruncationMessagesLength":40,"postTruncationTokensInMessages":1000,`+
			`"postTruncationMessagesLength":2,"tokensRemovedDuringTruncation":118000,`+
			`"messagesRemovedDuringTruncation":38,"performedBy":"BasicTruncator"}}`)
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(128000), snap.ContextWindowSize, "the limit is unchanged and stays")
	assert.Zero(t, snap.ContextPct,
		"the truncation invalidated the occupancy; keeping 93.75% would render a window "+
			"that was just emptied as nearly full")
}

// TestCopilotContextRefreshIsSerializedPerSession pins the in-flight guard.
// The 2s throttle alone does not serialize anything: it measures from the
// START of the previous refresh, so a full rebuild that outlives its own
// throttle window would let a second caller in concurrently.
func TestCopilotContextRefreshIsSerializedPerSession(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	copilotRefreshHome(t)
	sess := copilotRefreshSession(t, "copilot-serialized")

	first, ok := claimCopilotContextRefresh(sess, time.Now())
	require.True(t, ok, "the first caller claims the session")

	// A second caller arriving long after the throttle window, while the first
	// refresh is still running, must be refused.
	_, ok = claimCopilotContextRefresh(sess, time.Now().Add(time.Hour))
	assert.False(t, ok, "a refresh already in flight must exclude a concurrent one")

	releaseCopilotContextRefresh(first)
	_, ok = claimCopilotContextRefresh(sess, time.Now().Add(time.Hour))
	assert.True(t, ok, "and the next caller proceeds once it is released")
}

// TestCopilotContextRefreshRefusesAStaleGeneration pins the guard on the row
// write itself: a session pruned and recreated between the log read and the
// write must not receive the previous conversation's tokens.
func TestCopilotContextRefreshRefusesAStaleGeneration(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	home := copilotRefreshHome(t)
	appendCopilotRefreshEvents(t, copilotRefreshLogPath(home),
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":77}}`)

	const sessionID = "copilot-generation"
	sess := copilotRefreshSession(t, sessionID)

	// The row is recreated under the same id with a different generation,
	// exactly as a prune-and-respawn would leave it, while `sess` still
	// describes the old one.
	recreated := &db.SessionRow{
		ID: sessionID, ConvID: "11111111-2222-4333-8444-999999999999",
		TmuxSession: "copilot-pane", Status: "idle",
		Harness: harness.CopilotName, CreatedAt: sess.CreatedAt.Add(time.Minute),
	}
	require.NoError(t, db.SaveSession(recreated))

	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, snap.TokensOutput,
		"a stale generation's projection must not land on the recreated row")

	checkpoint, err := db.LoadCopilotTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	assert.Nil(t, checkpoint,
		"nor may its follower cursor be attached to the recreated row")
}

// TestCopilotLivePctSurvivesDurableRefresh is the TCL-1048 follow-up
// regression, straight from the operator's screenshot: "context: 19k / 200k
// tokens (assumed cap) — 0%".
//
// The sweep and this follower are two writers of the same row. The sweep
// resolves the effective denominator (configured cap, else the observed
// model's static assumption) and wrote a real percentage; the follower then
// recomputed against only the durable log's disclosed window — which Copilot
// rarely provides — and overwrote that percentage with 0 on every token
// advance, while the token counts kept landing. Both writers must resolve the
// SAME effective denominator.
func TestCopilotLivePctSurvivesDurableRefresh(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)
	t.Cleanup(resetCopilotUsageStateForTest)

	// A durable log that HYDRATES the follower but discloses no window. This
	// matters to the test's validity: an ABSENT log stops the refresh at its
	// !hydrated gate before the recompute under test ever runs — the first
	// version of this test made exactly that mistake and passed without the
	// fix.
	home := copilotRefreshHome(t)
	appendCopilotRefreshEvents(t, copilotRefreshLogPath(home),
		`{"type":"session.start","data":{"sessionId":"s","selectedModel":"gpt-5-mini"}}`,
		`{"type":"assistant.message","data":{"model":"gpt-5-mini","outputTokens":245}}`)

	sess := copilotRefreshSession(t, "copilot-live-pct")
	// One real swept call: model observed (200k static band), 18762-token
	// prompt — the sweep persists ~9.4%.
	call := copilotUsageCall(1, 18762, 245)
	call.SessionID = sess.ConvID
	call.Model = "gpt-5-mini"
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{call})

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	require.InDelta(t, 9.381, snap.ContextPct, 0.001)

	// The durable read-through refresh hydrates, finds the live entry, and has
	// no disclosed window to offer. It must recompute against the same
	// effective denominator, not clobber the sweep's percentage back to 0.
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.InDelta(t, 9.381, snap.ContextPct, 0.001,
		"the follower must measure the live numerator against the effective cap")
	assert.Equal(t, int64(18762), snap.TokensInput)
	assert.Equal(t, int64(245), snap.TokensOutput)
	assert.Zero(t, snap.ContextWindowSize,
		"the observed window column still reports only what Copilot disclosed")
}

// TestCopilotDisclosedWindowIsTheLastResort pins the bottom of the effective
// denominator chain: with no configured cap and no recognizable model, a
// window Copilot actually disclosed is better than nothing and is what the
// percentage measures against — matching the dashboard tooltip's own display
// fallback.
func TestCopilotDisclosedWindowIsTheLastResort(t *testing.T) {
	setupTestDB(t)
	resetCopilotContextRefreshStateForTest()
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotContextRefreshStateForTest)
	t.Cleanup(resetCopilotUsageStateForTest)

	home := copilotRefreshHome(t)
	appendCopilotRefreshEvents(t, copilotRefreshLogPath(home),
		`{"type":"session.start","data":{"sessionId":"s"}}`,
		`{"type":"session.compaction_start","data":{"currentTokens":64000,"tokenLimit":128000,"trigger":"threshold"}}`)

	sess := copilotRefreshSession(t, "copilot-last-resort")
	// A swept call with NO model id: no static band resolves, so the disclosed
	// 128k window is all tclaude knows.
	call := copilotUsageCall(1, 32000, 100)
	call.SessionID = sess.ConvID
	call.Model = ""
	call.ReasoningEffort = ""
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{call})

	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(128000), snap.ContextWindowSize)
	assert.InDelta(t, 25.0, snap.ContextPct, 0.001,
		"32k of a disclosed 128k window, with nothing better to measure against")
}
