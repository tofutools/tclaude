package harness

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit coverage for the Copilot usage/context follower.
//
// The fixture these tests read is a SANITIZED REAL 1.0.77 event log, recorded
// by pkg/claude/harness/copilotfixture from the pinned binary running
// credential-free (see eventlog_smoke_test.go). Everything here therefore
// asserts against bytes Copilot actually wrote, not against a hand-built idea
// of what it writes. The degraded shapes a live CLI cannot be asked to produce
// — a half-flushed final line, an oversized record, a truncation, a corrupt
// record — are constructed on top of that same real log.

// copilotFixtureLogPath is the recorded log, reached from this package.
const copilotFixtureLogPath = "copilotfixture/testdata/1.0.77/session_events.jsonl"

func copilotFixtureLines(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(copilotFixtureLogPath)
	require.NoError(t, err, "the recorded Copilot event-log fixture must exist")
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	require.NotEmpty(t, lines)
	return lines
}

// copilotTelemetryHome lays out a COPILOT_HOME containing one session whose
// event log holds the given lines, and returns (home, convID).
func copilotTelemetryHome(t *testing.T, lines []string) (string, string) {
	t.Helper()
	const convID = "9a1c2d3e-4f50-4617-8829-0b1c2d3e4f50"
	home := t.TempDir()
	dir := filepath.Join(home, copilotSessionStateDirName, convID)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeCopilotLog(t, filepath.Join(dir, copilotEventsFileName), lines)
	return home, convID
}

func copilotLogPath(home, convID string) string {
	return filepath.Join(home, copilotSessionStateDirName, convID, copilotEventsFileName)
}

func writeCopilotLog(t *testing.T, path string, lines []string) {
	t.Helper()
	body := ""
	if len(lines) > 0 {
		body = strings.Join(lines, "\n") + "\n"
	}
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func appendCopilotLog(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()
	_, err = file.WriteString(text)
	require.NoError(t, err)
}

// TestCopilotTelemetryFollowerProjectsRealLog is the baseline: the recorded
// two-lifetime log (a fresh turn, then a resume that makes a tool call)
// projects the durable fields and nothing else.
func TestCopilotTelemetryFollowerProjectsRealLog(t *testing.T) {
	home, convID := copilotTelemetryHome(t, copilotFixtureLines(t))

	follower := &CopilotTelemetryFollower{}
	snap, ok, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, ok, "a present log must hydrate")

	assert.Equal(t, "copilotfixture-mock-model", snap.Model)
	assert.Equal(t, "1.0.77", snap.CopilotVersion)
	assert.Equal(t, 2, snap.Lifetimes,
		"session.start plus the session.resume appended to the SAME file")
	assert.Equal(t, 2, snap.UserMessages)
	assert.Equal(t, 3, snap.AssistantMessages,
		"the tool-calling turn contributes a second assistant message in one lifetime")
	assert.Positive(t, snap.AssistantOutputTokens,
		"assistant.message.outputTokens is the one per-turn token figure Copilot persists")

	require.NotNil(t, snap.Usage, "the trailing session.shutdown carries lifetime usage")
	assert.Positive(t, snap.Usage.InputTokens)
	assert.Positive(t, snap.Usage.OutputTokens)
	assert.Equal(t, int64(3), snap.Usage.Requests,
		"modelMetrics are cumulative across the resume, so the LAST shutdown is the session total")

	require.True(t, snap.HasContext)
	assert.Equal(t, "shutdown", snap.Context.Source)
	assert.Positive(t, snap.Context.CurrentTokens)
	assert.Zero(t, snap.Context.TokenLimit,
		"session.shutdown discloses occupancy but never the window limit")
	assert.Zero(t, snap.Context.Pct(),
		"no limit means no percentage — 0 is 'unknown', never an invented reading")

	assert.True(t, snap.HasNanoAIU, "the shutdown record states a cost, even when it is zero")
	assert.Nil(t, snap.LastError)
}

// TestCopilotTelemetryFollowerAppendsIncrementally is the ticket's central
// requirement: a growing log must be read forward, not rescanned.
func TestCopilotTelemetryFollowerAppendsIncrementally(t *testing.T) {
	lines := copilotFixtureLines(t)
	split := 8
	require.Greater(t, len(lines), split)

	home, convID := copilotTelemetryHome(t, lines[:split])
	path := copilotLogPath(home, convID)

	follower := &CopilotTelemetryFollower{}
	first, ok, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, first.Lifetimes)
	afterFirst := follower.Stats()
	require.Equal(t, uint64(1), afterFirst.Rebuilds)
	prefixBytes := afterFirst.PayloadBytes

	tail := strings.Join(lines[split:], "\n") + "\n"
	appendCopilotLog(t, path, tail)

	second, ok, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, ok)

	stats := follower.Stats()
	assert.Equal(t, uint64(1), stats.Rebuilds, "an append must not trigger a full rescan")
	assert.Equal(t, uint64(1), stats.Appends)
	assert.Equal(t, int64(len(tail)), stats.PayloadBytes-prefixBytes,
		"the append scan must read exactly the appended tail and nothing before it")

	// A third call with no change must not open the file at all.
	third, ok, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, second, third)
	assert.Equal(t, stats.PayloadBytes, follower.Stats().PayloadBytes,
		"an unchanged file is answered from memory")

	// The incremental answer must equal a cold full scan of the same bytes.
	cold := &CopilotTelemetryFollower{}
	full, ok, err := cold.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, full, second, "incremental and full scans must agree")
}

// TestCopilotTelemetryFollowerIgnoresPartialFinalRecord covers the live-writer
// case: Copilot is between write(2)s, so the file ends mid-record.
func TestCopilotTelemetryFollowerIgnoresPartialFinalRecord(t *testing.T) {
	lines := copilotFixtureLines(t)
	last := lines[len(lines)-1]
	home, convID := copilotTelemetryHome(t, lines[:len(lines)-1])
	path := copilotLogPath(home, convID)

	follower := &CopilotTelemetryFollower{}
	before, _, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)

	// Half a record, with no terminating newline.
	appendCopilotLog(t, path, last[:len(last)/2])
	partial, ok, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, before, partial,
		"an unterminated tail must be left at the cursor, not decoded as a record")

	// The writer finishes the record.
	appendCopilotLog(t, path, last[len(last)/2:]+"\n")
	complete, _, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	assert.NotEqual(t, before, complete, "the completed record must be picked up")

	cold := &CopilotTelemetryFollower{}
	full, _, err := cold.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	assert.Equal(t, full, complete)
}

// TestCopilotTelemetryFollowerRebuildsOnTruncate covers a replaced or truncated
// log — a rotation, a manual edit, or a session directory reused for a
// different conversation.
func TestCopilotTelemetryFollowerRebuildsOnTruncate(t *testing.T) {
	lines := copilotFixtureLines(t)
	home, convID := copilotTelemetryHome(t, lines)
	path := copilotLogPath(home, convID)

	follower := &CopilotTelemetryFollower{}
	before, _, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.Equal(t, 2, before.Lifetimes)

	writeCopilotLog(t, path, lines[:4])

	after, ok, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, 1, after.Lifetimes, "a shorter file must be rebuilt, not appended onto")
	assert.Equal(t, uint64(2), follower.Stats().Rebuilds)

	cold := &CopilotTelemetryFollower{}
	full, _, err := cold.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	assert.Equal(t, full, after)
}

// TestCopilotTelemetryFollowerSurvivesHostileRecords pins the two failures an
// append-only log makes permanent if handled wrongly: an oversized record and
// a corrupt one must both cost exactly one record, never the rest of the log.
func TestCopilotTelemetryFollowerSurvivesHostileRecords(t *testing.T) {
	lines := copilotFixtureLines(t)
	oversized := `{"type":"system.message","data":{"content":"` +
		strings.Repeat("x", maxCopilotEventLineBytes+16) + `"}}`
	corrupt := `{"type":"assistant.message","data":{"outputTokens":`

	hostile := append([]string{}, lines[:4]...)
	hostile = append(hostile, oversized, corrupt)
	hostile = append(hostile, lines[4:]...)

	home, convID := copilotTelemetryHome(t, hostile)
	follower := &CopilotTelemetryFollower{}
	snap, ok, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, ok)

	clean, _, err := (&CopilotTelemetryFollower{}).RuntimeTelemetry(copilotTelemetryHome(t, lines))
	require.NoError(t, err)
	assert.Equal(t, clean, snap,
		"a skipped oversized record and a corrupt record must not change the projection")
}

// TestCopilotTelemetryCheckpointSurvivesDaemonRestart is the restart/repair
// case: a follower rebuilt from a durable checkpoint must continue the same
// fold, and must agree with a cold full scan afterwards.
func TestCopilotTelemetryCheckpointSurvivesDaemonRestart(t *testing.T) {
	lines := copilotFixtureLines(t)
	split := 8
	home, convID := copilotTelemetryHome(t, lines[:split])
	path := copilotLogPath(home, convID)

	original := &CopilotTelemetryFollower{}
	before, _, err := original.RuntimeTelemetry(home, convID)
	require.NoError(t, err)

	data, ok, err := original.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)

	// The daemon restarts: a brand-new follower is primed from the blob.
	restored := &CopilotTelemetryFollower{}
	require.NoError(t, restored.RestoreCheckpoint(data))

	unchanged, ok, err := restored.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, before, unchanged, "a restored checkpoint must reproduce the fold exactly")
	assert.Zero(t, restored.Stats().Rebuilds, "a valid checkpoint must not force a rescan")

	appendCopilotLog(t, path, strings.Join(lines[split:], "\n")+"\n")
	resumedSnap, _, err := restored.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	assert.Zero(t, restored.Stats().Rebuilds,
		"the append after a restore must still be incremental")

	cold := &CopilotTelemetryFollower{}
	full, _, err := cold.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	assert.Equal(t, full, resumedSnap)
}

// TestCopilotTelemetryCheckpointRepairsItself covers the checkpoints a daemon
// must refuse. Each one degrades to "rebuild from byte zero", never to a wrong
// answer carried forward.
func TestCopilotTelemetryCheckpointRepairsItself(t *testing.T) {
	lines := copilotFixtureLines(t)
	home, convID := copilotTelemetryHome(t, lines)
	path := copilotLogPath(home, convID)

	source := &CopilotTelemetryFollower{}
	want, _, err := source.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	data, ok, err := source.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)

	t.Run("rejects a bumped version", func(t *testing.T) {
		var cp map[string]any
		require.NoError(t, json.Unmarshal(data, &cp))
		cp["version"] = copilotTelemetryCheckpointVersion + 1
		bumped, err := json.Marshal(cp)
		require.NoError(t, err)
		assert.Error(t, (&CopilotTelemetryFollower{}).RestoreCheckpoint(bumped))
	})

	t.Run("rejects garbage and empties", func(t *testing.T) {
		assert.Error(t, (&CopilotTelemetryFollower{}).RestoreCheckpoint(nil))
		assert.Error(t, (&CopilotTelemetryFollower{}).RestoreCheckpoint([]byte("{")))
		assert.Error(t, (&CopilotTelemetryFollower{}).RestoreCheckpoint([]byte(`{"version":1}`)))
	})

	t.Run("rebuilds when the log no longer matches the cursor", func(t *testing.T) {
		// The log is rewritten under the checkpoint: same path, different
		// bytes. Restore succeeds (the blob is well-formed), and the next
		// refresh must detect the mismatch and rebuild rather than resume at a
		// byte offset that now means something else.
		rewritten := append([]string{}, lines...)
		rewritten = append(rewritten, `{"type":"user.message","data":{"content":"extra"}}`)
		writeCopilotLog(t, path, rewritten)

		follower := &CopilotTelemetryFollower{}
		require.NoError(t, follower.RestoreCheckpoint(data))
		got, ok, err := follower.RuntimeTelemetry(home, convID)
		require.NoError(t, err)
		require.True(t, ok)
		assert.Equal(t, want.UserMessages+1, got.UserMessages,
			"the rewritten log must be re-read, not resumed from a stale offset")

		writeCopilotLog(t, path, lines)
	})
}

// TestCopilotTelemetryMissingLogIsNotAnError covers a session directory
// Copilot has created but not yet written a log into.
func TestCopilotTelemetryMissingLogIsNotAnError(t *testing.T) {
	home := t.TempDir()
	follower := &CopilotTelemetryFollower{}

	snap, ok, err := follower.RuntimeTelemetry(home, "9a1c2d3e-4f50-4617-8829-0b1c2d3e4f50")
	assert.NoError(t, err, "an absent log is a normal state, not a failure")
	assert.False(t, ok, "and it must not be reported as hydrated data")
	assert.Equal(t, CopilotRuntimeSnapshot{}, snap)
}

// TestCopilotTelemetryRejectsUnsafeConvID keeps the id out of the path join.
func TestCopilotTelemetryRejectsUnsafeConvID(t *testing.T) {
	home := t.TempDir()
	for _, convID := range []string{"", "..", "../escape", "a/b", "a\x00b"} {
		follower := &CopilotTelemetryFollower{}
		_, ok, err := follower.RuntimeTelemetry(home, convID)
		assert.NoError(t, err, "conv id %q", convID)
		assert.False(t, ok, "conv id %q must not resolve to a log", convID)
	}
}

// TestCopilotContextComesOnlyFromAuthoritativeEvents pins the ticket's central
// honesty constraint: between a compaction, a truncation and a shutdown the
// durable log says nothing about the context window, and the follower must say
// nothing too.
func TestCopilotContextComesOnlyFromAuthoritativeEvents(t *testing.T) {
	base := []string{
		`{"type":"session.start","data":{"sessionId":"s","copilotVersion":"1.0.77","selectedModel":"gpt-5.4"}}`,
		`{"type":"user.message","data":{"content":"hello"}}`,
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":17}}`,
	}
	home, convID := copilotTelemetryHome(t, base)
	path := copilotLogPath(home, convID)

	follower := &CopilotTelemetryFollower{}
	snap, ok, err := follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, ok)
	assert.False(t, snap.HasContext,
		"a running session discloses no context window on disk; inventing one is the bug")
	assert.Equal(t, int64(17), snap.AssistantOutputTokens)

	// A compaction is the first authoritative disclosure, and the only place a
	// tokenLimit appears — so it is also what makes a percentage computable.
	appendCopilotLog(t, path,
		`{"type":"session.compaction_start","data":{"currentTokens":90000,"tokenLimit":128000,`+
			`"systemTokens":6000,"conversationTokens":78000,"toolDefinitionsTokens":6000,"trigger":"threshold"}}`+"\n")
	snap, _, err = follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.True(t, snap.HasContext)
	assert.Equal(t, "compaction_start", snap.Context.Source)
	assert.InDelta(t, 70.3125, snap.Context.Pct(), 0.001)

	// A FAILED compaction changed nothing, so it must not overwrite the good
	// reading with its meaningless post-compaction figures.
	appendCopilotLog(t, path,
		`{"type":"session.compaction_complete","data":{"success":false,"error":"boom","postCompactionTokens":0}}`+"\n")
	snap, _, err = follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	assert.Equal(t, "compaction_start", snap.Context.Source)
	assert.Equal(t, int64(90000), snap.Context.CurrentTokens)

	appendCopilotLog(t, path,
		`{"type":"session.compaction_complete","data":{"success":true,"postCompactionTokens":30000,`+
			`"tokenLimit":128000,"systemTokens":6000,"conversationTokens":18000,"toolDefinitionsTokens":6000}}`+"\n")
	snap, _, err = follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	assert.Equal(t, "compaction_complete", snap.Context.Source)
	assert.Equal(t, int64(30000), snap.Context.CurrentTokens)

	// A shutdown carries occupancy but no limit; the compaction's limit is
	// carried forward so the percentage does not silently vanish at exit.
	appendCopilotLog(t, path,
		`{"type":"session.shutdown","data":{"shutdownType":"routine","currentTokens":33000,`+
			`"totalNanoAiu":4200,"totalPremiumRequests":2,"currentModel":"gpt-5.4",`+
			`"modelMetrics":{"gpt-5.4":{"requests":{"count":3},"usage":{"inputTokens":100,"outputTokens":20,`+
			`"cacheReadTokens":5,"cacheWriteTokens":6,"reasoningTokens":7}}}}}`+"\n")
	snap, _, err = follower.RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	assert.Equal(t, "shutdown", snap.Context.Source)
	assert.Equal(t, int64(128000), snap.Context.TokenLimit,
		"a shutdown must not blank a limit an earlier compaction disclosed")
	assert.True(t, snap.HasNanoAIU)
	assert.InDelta(t, 4200, snap.NanoAIU, 0)
	assert.InDelta(t, 2, snap.PremiumRequests, 0)
	require.NotNil(t, snap.Usage)
	assert.Equal(t, int64(100), snap.Usage.InputTokens)
	assert.Equal(t, int64(3), snap.Usage.Requests)
}

// TestCopilotUsageIsNotDoubleCountedAcrossResume pins the one arithmetic trap
// in the shutdown record: Copilot restores its counters on resume, so a second
// shutdown RESTATES the session total rather than reporting a delta.
func TestCopilotUsageIsNotDoubleCountedAcrossResume(t *testing.T) {
	shutdown := func(count, input int) string {
		return `{"type":"session.shutdown","data":{"shutdownType":"routine","currentTokens":1000,` +
			`"modelMetrics":{"gpt-5.4":{"requests":{"count":` +
			strconv.Itoa(count) + `},"usage":{"inputTokens":` + strconv.Itoa(input) + `,"outputTokens":1}}}}}`
	}
	home, convID := copilotTelemetryHome(t, []string{
		`{"type":"session.start","data":{"sessionId":"s","selectedModel":"gpt-5.4"}}`,
		shutdown(1, 11),
		`{"type":"session.resume","data":{"resumeTime":"2026-01-01T00:00:00Z","eventCount":2,"selectedModel":"gpt-5.4"}}`,
		shutdown(2, 22),
	})

	snap, _, err := (&CopilotTelemetryFollower{}).RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.NotNil(t, snap.Usage)
	assert.Equal(t, int64(22), snap.Usage.InputTokens,
		"the last shutdown IS the session total; summing the two would report 33")
	assert.Equal(t, int64(2), snap.Usage.Requests)
	assert.Equal(t, 2, snap.Lifetimes)
}

// TestCopilotErrorObservationIsSanitized pins the TCL-976 finding that
// Copilot's error reporting embeds absolute host paths.
func TestCopilotErrorObservationIsSanitized(t *testing.T) {
	home, convID := copilotTelemetryHome(t, []string{
		`{"type":"session.error","data":{"errorType":"context_limit","errorCode":"too_long",` +
			`"statusCode":413,"message":"failed reading /home/someone/secret/project/file.go",` +
			`"stack":"Error: boom\n    at /home/someone/.copilot/index.js:1:1"}}`,
	})

	snap, _, err := (&CopilotTelemetryFollower{}).RuntimeTelemetry(home, convID)
	require.NoError(t, err)
	require.NotNil(t, snap.LastError)
	assert.Equal(t, "context_limit", snap.LastError.ErrorType)
	assert.Equal(t, "too_long", snap.LastError.ErrorCode)
	assert.Equal(t, 413, snap.LastError.StatusCode)
	assert.NotContains(t, snap.LastError.Message, "/home/someone",
		"absolute host paths must be scrubbed out of anything tclaude retains")
	assert.Contains(t, snap.LastError.Message, copilotRedactedPath)

	// The stack is not merely unused: it must be unrepresentable. Encoding the
	// whole observation is the check that no field smuggles it through.
	encoded, err := json.Marshal(snap.LastError)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "index.js")
	assert.NotContains(t, string(encoded), "at /")
}

// TestCopilotErrorMessageIsBounded stops a provider that returns a whole HTML
// page from pushing it into tclaude's database.
func TestCopilotErrorMessageIsBounded(t *testing.T) {
	got := sanitizeCopilotText(strings.Repeat("a", copilotErrorMessageLimit*3))
	assert.LessOrEqual(t, len(got), copilotErrorMessageLimit+len("…"))
}
