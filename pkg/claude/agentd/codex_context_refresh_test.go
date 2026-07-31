package agentd

import (
	"context"
	"encoding/json"
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

func TestCodexContextRefreshPersistsAndRestoresFollowerCheckpoint(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-checkpoint-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354f3"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 1000, 100)

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle",
		Harness: harness.CodexName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))
	refreshCodexContextSnapshotOnRead(sess, true)
	firstCheckpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, firstCheckpoint)
	require.NotEmpty(t, firstCheckpoint.Data)

	contextSnapshot, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), contextSnapshot.TokensInput)

	// Simulate a daemon restart: all follower objects disappear, while the DB
	// and append-only rollout survive. The next refresh restores the checkpoint
	// before consuming only the new records and replaces it with the new cursor.
	resetCodexContextRefreshStateForTest()
	appendCodexRefreshTokenCount(t, path, 9000, 900)
	refreshCodexContextSnapshotOnRead(sess, true)

	contextSnapshot, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(9000), contextSnapshot.TokensInput)
	secondCheckpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, secondCheckpoint)
	require.NotEmpty(t, secondCheckpoint.Data)
	assert.NotEqual(t, string(firstCheckpoint.Data), string(secondCheckpoint.Data))
}

func TestCodexContextRefreshFollowerPersistsEffortUsageAndCost(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-follower-fold-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354f4"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshEnvelope(t, path, "turn_context", map[string]any{
		"model": "gpt-5.6-terra", "effort": "high",
	})
	usage := map[string]any{
		"input_tokens": 2000, "cached_input_tokens": 400,
		"output_tokens": 100, "total_tokens": 2100,
	}
	yesterday := time.Now().AddDate(0, 0, -1)
	appendCodexRefreshEnvelopeAt(t, path, yesterday, "event_msg", map[string]any{
		"type": "token_count",
		"info": map[string]any{
			"total_token_usage": usage, "last_token_usage": usage,
			"model_context_window": 200000,
		},
		"rate_limits": map[string]any{
			"limit_id": "codex",
			"primary": map[string]any{
				"used_percent": 31, "window_minutes": 300,
				"resets_at": time.Now().Add(2 * time.Hour).Unix(),
			},
			"secondary": map[string]any{
				"used_percent": 45, "window_minutes": 10080,
				"resets_at": time.Now().Add(5 * 24 * time.Hour).Unix(),
			},
		},
	})
	appendCodexRefreshEnvelope(t, path, "event_msg", map[string]any{
		"type": "token_count",
		"info": map[string]any{
			"total_token_usage": map[string]any{
				"input_tokens": 4000, "cached_input_tokens": 800,
				"output_tokens": 200, "total_tokens": 4200,
			},
			"last_token_usage":     usage,
			"model_context_window": 200000,
		},
	})

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle",
		Harness: harness.CodexName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))
	require.NoError(t, db.ReplaceSessionVirtualCostHistory(sessionID, 1,
		[]db.VirtualCostDailySnapshot{
			{Day: yesterday.In(time.Local).Format("2006-01-02"), CostUSD: 0.5, UpdatedAt: yesterday},
			{Day: time.Now().In(time.Local).Format("2006-01-02"), CostUSD: 1, UpdatedAt: time.Now()},
		}), "seed the inflated multi-day projection produced by the old pricing table")
	refreshCodexContextSnapshotOnRead(sess, true)

	snapshot, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(2000), snapshot.TokensInput)
	assert.Equal(t, "high", snapshot.EffortLevel)
	assert.InDelta(t, 0.00896, snapshot.VirtualCostUSD, 1e-9)
	assert.Zero(t, snapshot.CostUSD, "subscription cost remains hypothetical")
	rows, err := db.AllCostDailyRows()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.InDelta(t, 0.00448, rows[0].VirtualCostUSD, 1e-9,
		"the authoritative fold replaces the old first-day cumulative prefix")
	assert.InDelta(t, 0.00896, rows[1].VirtualCostUSD, 1e-9,
		"the authoritative fold replaces, rather than MAXes, the final daily total")

	cache, err := db.LoadCodexUsageCache()
	require.NoError(t, err)
	require.NotNil(t, cache)
	assert.Equal(t, "telemetry-follower", cache.Source)
	var got harness.CodexUsage
	require.NoError(t, json.Unmarshal(cache.Data, &got))
	require.NotNil(t, got.FiveHour)
	assert.Equal(t, 31.0, got.FiveHour.UsedPercent)
	require.NotNil(t, got.Weekly)
	assert.Equal(t, 45.0, got.Weekly.UsedPercent)

	// Replacement with a valid but unpriced rollout is authoritative: stale
	// session and daily projections must clear instead of surviving forever.
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshEnvelope(t, path, "turn_context", map[string]any{
		"model": "unpriced-preview", "effort": "medium",
	})
	appendCodexRefreshTokenCount(t, path, 100, 10)
	resetCodexRefreshThrottleForTest(sessionID)
	refreshCodexContextSnapshotOnRead(sess, true)
	snapshot, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, snapshot.VirtualCostUSD)
	rows, err = db.AllCostDailyRows()
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Zero(t, rows[0].VirtualCostUSD)
	assert.Zero(t, rows[1].VirtualCostUSD)
}

func TestCodexContextRefreshRejectsStaleSessionGenerationTelemetry(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-stale-generation-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354f5"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshEnvelope(t, path, "turn_context", map[string]any{
		"model": "gpt-5.6-terra", "effort": "high",
	})
	appendCodexRefreshTokenCount(t, path, 2000, 100)

	stale := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle",
		Harness: harness.CodexName, CreatedAt: time.Now().Add(-time.Minute),
	}
	require.NoError(t, db.SaveSession(stale))
	require.NoError(t, db.DeleteSession(sessionID))
	current := *stale
	current.CreatedAt = time.Now()
	require.NoError(t, db.SaveSession(&current))
	require.NoError(t, db.UpdateSessionEffort(sessionID, "low"))
	require.NoError(t, db.UpdateSessionVirtualCost(sessionID, 9))

	refreshCodexContextSnapshotOnRead(stale, true)
	snapshot, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, "low", snapshot.EffortLevel,
		"stale follower effort must not attach to a recreated session row")
	assert.Equal(t, 9.0, snapshot.VirtualCostUSD,
		"stale follower cost must not attach to a recreated session row")
	checkpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	assert.Nil(t, checkpoint,
		"stale follower checkpoint must not attach to a recreated session row")
}

func TestCodexContextWritePerfPhasesOnlyIncludeExecutedPaths(t *testing.T) {
	timing := codexTelemetryTiming{
		contextWrite: 32 * time.Millisecond,
		contextReset: contextWriteTiming{
			total: 2 * time.Millisecond, open: 100 * time.Microsecond,
			execCommit: 1800 * time.Microsecond,
		},
		contextFast: contextWriteTiming{
			total: 10 * time.Millisecond, open: 100 * time.Microsecond,
			execCommit: 9800 * time.Microsecond, result: 50 * time.Microsecond,
		},
		contextProject: contextWriteTiming{
			total: 18 * time.Millisecond, open: 100 * time.Microsecond,
			begin: 2 * time.Millisecond, update: 500 * time.Microsecond,
			projection: 5 * time.Millisecond, commit: 10 * time.Millisecond,
		},
	}
	var contextWrite perfPhase
	for _, phase := range timing.perfPhases() {
		if phase.Name == "context_write" {
			contextWrite = phase
			break
		}
	}
	var names []string
	for _, child := range contextWrite.Children {
		names = append(names, child.Name)
	}
	assert.Equal(t, []string{"reset", "fast_update", "full_projection", "other"}, names)
	require.Len(t, contextWrite.Children[1].Children, 4)
	assert.Equal(t, "exec_commit", contextWrite.Children[1].Children[1].Name)
	require.Len(t, contextWrite.Children[2].Children, 6)
	assert.Equal(t, "profile_projection", contextWrite.Children[2].Children[3].Name)
	assert.Equal(t, "commit", contextWrite.Children[2].Children[4].Name)

	for _, phase := range (codexTelemetryTiming{}).perfPhases() {
		if phase.Name == "context_write" {
			assert.Empty(t, phase.Children,
				"idle polls must not dilute sparse path quantiles with zero samples")
		}
	}
}

func TestDashboardCodexContextWriteBatchKeepsResponseFresh(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-dashboard-batch-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354b0"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 12_000, 800)

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle",
		Harness: harness.CodexName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))
	batch := &codexContextWriteBatch{}
	state := stateForConvInSessionsBatched(
		[]*db.SessionRow{sess},
		map[string]struct{}{sess.TmuxSession: {}},
		batch,
		nil,
	)
	assert.Equal(t, int64(12_000), state.TokensInput,
		"the dashboard response must use rollout telemetry before the batch commits")

	stored, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, stored.TokensInput, "row assembly should only enqueue the context write")

	// Remove the normal one-second refresh throttle: the pending batch itself
	// must retain the claim so a slow/overlapping snapshot cannot queue and
	// later commit an older rollout value after this one.
	codexContextRefreshMu.Lock()
	pendingState := codexContextRefreshMu.last[sessionID]
	pendingState.at = time.Time{}
	codexContextRefreshMu.last[sessionID] = pendingState
	codexContextRefreshMu.Unlock()
	overlappingBatch := &codexContextWriteBatch{}
	overlappingState := stateForConvInSessionsBatched(
		[]*db.SessionRow{sess},
		map[string]struct{}{sess.TmuxSession: {}},
		overlappingBatch,
		nil,
	)
	assert.Equal(t, int64(12_000), overlappingState.TokensInput,
		"an overlapping snapshot should use the cached rollout value before commit")
	assert.Nil(t, overlappingBatch.dbBatch, "the claimed refresh should remain owned by the first snapshot")

	timing, err := batch.flush()
	require.NoError(t, err)
	assert.Positive(t, timing.contextProject.total)
	assert.Positive(t, timing.contextBatch.commit)
	stored, err = db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(12_000), stored.TokensInput)
	codexContextRefreshMu.Lock()
	assert.False(t, codexContextRefreshMu.last[sessionID].refreshing)
	codexContextRefreshMu.Unlock()
}

func TestCodexContextRefreshSkipsUnchangedContextWrite(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-unchanged-context-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354fd"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 1000, 100)

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle",
		Harness: harness.CodexName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))

	var first codexTelemetryTiming
	refreshCodexContextSnapshotOnReadTimed(sess, true, func(timing codexTelemetryTiming) {
		first = timing
	})
	assert.Positive(t, first.contextWrite, "the first observed snapshot is persisted")
	assert.Positive(t, first.contextProject.total,
		"the first observation records the full self-healing projection")
	assert.Zero(t, first.contextFast.total)

	resetCodexRefreshThrottleForTest(sessionID)
	var unchanged codexTelemetryTiming
	refreshCodexContextSnapshotOnReadTimed(sess, true, func(timing codexTelemetryTiming) {
		unchanged = timing
	})
	assert.Zero(t, unchanged.contextWrite,
		"an unchanged rollout must not rewrite the same context snapshot on every dashboard poll")

	require.NoError(t, db.DeleteSession(sessionID))
	recreated := *sess
	recreated.CreatedAt = sess.CreatedAt.Add(time.Second)
	require.NoError(t, db.SaveSession(&recreated))
	blank, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Zero(t, blank.TokensInput, "the recreated row starts without the prior generation's context")

	resetCodexRefreshThrottleForTest(sessionID)
	var recreatedTiming codexTelemetryTiming
	refreshCodexContextSnapshotOnReadTimed(&recreated, true, func(timing codexTelemetryTiming) {
		recreatedTiming = timing
	})
	assert.Positive(t, recreatedTiming.contextWrite,
		"a new session-row generation must be populated even when its rollout is unchanged")
	recreatedSnapshot, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(1000), recreatedSnapshot.TokensInput)
	recreatedCheckpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	assert.NotNil(t, recreatedCheckpoint,
		"session recreation also repopulates the checkpoint removed by the delete cascade")

	appendCodexRefreshTokenCount(t, path, 9000, 900)
	resetCodexRefreshThrottleForTest(sessionID)
	var changed codexTelemetryTiming
	refreshCodexContextSnapshotOnReadTimed(&recreated, true, func(timing codexTelemetryTiming) {
		changed = timing
	})
	assert.Positive(t, changed.contextWrite, "new token telemetry is persisted")
	assert.Positive(t, changed.contextFast.total,
		"same-generation token changes record the conditional fast update")
	assert.Positive(t, changed.contextFast.execCommit,
		"fast update exposes its SQLite execution and implicit commit")
	assert.Zero(t, changed.contextProject.total)
	contextSnapshot, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(9000), contextSnapshot.TokensInput)
}

func TestCodexContextRefreshFirstObservationRepairsRelaunchProjection(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-context-projection-repair-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea4135501"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 1000, 100)

	agentID, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle",
		Harness: harness.CodexName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))
	refreshCodexContextSnapshotOnRead(sess, true)

	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`DELETE FROM conversation_resume_profiles WHERE conv_id = ?`, convID)
	require.NoError(t, err)
	require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion,
	}))

	// A daemon restart clears proof that this generation/window was projected.
	// The first unchanged observation must therefore use the full self-healing
	// path before later token-only updates become eligible for the fast path.
	resetCodexContextRefreshStateForTest()
	refreshCodexContextSnapshotOnRead(sess, true)

	conversation, err := db.ConversationResumeProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, conversation)
	require.NotNil(t, conversation.FallbackRelaunch)
	require.NotNil(t, conversation.FallbackRelaunch.ContextWindowSize)
	assert.Equal(t, int64(200000), *conversation.FallbackRelaunch.ContextWindowSize)
	agent, err := db.AgentRelaunchProfileForConv(convID)
	require.NoError(t, err)
	require.NotNil(t, agent)
	require.NotNil(t, agent.ContextWindowSize)
	assert.Equal(t, int64(200000), *agent.ContextWindowSize)
}

func TestCodexContextRefreshRateLimitsDurableCheckpointWrites(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-rate-limited-checkpoint-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354fe"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 1000, 100)

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle",
		Harness: harness.CodexName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))
	refreshCodexContextSnapshotOnRead(sess, true)
	firstCheckpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, firstCheckpoint)

	appendCodexRefreshTokenCount(t, path, 9000, 900)
	resetCodexRefreshPollThrottleForTest(sessionID)
	var deferred codexTelemetryTiming
	refreshCodexContextSnapshotOnReadTimed(sess, true, func(timing codexTelemetryTiming) {
		deferred = timing
	})
	assert.Zero(t, deferred.checkpointEncode)
	assert.Zero(t, deferred.checkpointWrite,
		"a fresh durable cursor suppresses checkpoint work on the dashboard poll")
	contextSnapshot, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(9000), contextSnapshot.TokensInput,
		"live context persistence is independent from checkpoint durability")
	stillFirst, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, stillFirst)
	assert.Equal(t, string(firstCheckpoint.Data), string(stillFirst.Data))

	resetCodexRefreshThrottleForTest(sessionID)
	var due codexTelemetryTiming
	refreshCodexContextSnapshotOnReadTimed(sess, true, func(timing codexTelemetryTiming) {
		due = timing
	})
	assert.Positive(t, due.checkpointEncode)
	assert.Positive(t, due.checkpointWrite)
	updated, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, updated)
	assert.NotEqual(t, string(firstCheckpoint.Data), string(updated.Data),
		"the latest in-memory cursor is persisted once its interval is due")
}

func TestCodexContextRefreshFlushesDeferredCheckpointOnShutdown(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-shutdown-checkpoint-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354ff"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 1000, 100)

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle",
		Harness: harness.CodexName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))
	refreshCodexContextSnapshotOnRead(sess, true)
	firstCheckpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, firstCheckpoint)

	appendCodexRefreshTokenCount(t, path, 9000, 900)
	resetCodexRefreshPollThrottleForTest(sessionID)
	refreshCodexContextSnapshotOnRead(sess, true)
	deferred, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, deferred)
	assert.Equal(t, string(firstCheckpoint.Data), string(deferred.Data))

	// One in-flight follower must not keep an otherwise-ready checkpoint from
	// using the shutdown window.
	codexContextRefreshMu.Lock()
	codexContextRefreshMu.last["codex-stuck-refresh"] = codexReadThroughSnapshot{refreshing: true}
	codexContextRefreshMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	saved, err := flushCodexTelemetryCheckpoints(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, saved)
	flushed, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, flushed)
	assert.NotEqual(t, string(firstCheckpoint.Data), string(flushed.Data),
		"graceful shutdown flushes the latest cursor even while its interval is deferred")

	// A clean follower does not add another shutdown write.
	codexContextRefreshMu.Lock()
	delete(codexContextRefreshMu.last, "codex-stuck-refresh")
	codexContextRefreshMu.Unlock()
	saved, err = flushCodexTelemetryCheckpoints(context.Background())
	require.NoError(t, err)
	assert.Zero(t, saved)
}

func TestCodexContextRefreshShutdownFlushSkipsRecreatedSession(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-recreated-shutdown-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea4135500"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 1000, 100)

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle",
		Harness: harness.CodexName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))
	refreshCodexContextSnapshotOnRead(sess, true)

	require.NoError(t, db.DeleteSession(sessionID))
	recreated := *sess
	recreated.CreatedAt = sess.CreatedAt.Add(time.Second)
	require.NoError(t, db.SaveSession(&recreated))

	saved, err := flushCodexTelemetryCheckpoints(context.Background())
	require.NoError(t, err)
	assert.Zero(t, saved)
	checkpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	assert.Nil(t, checkpoint,
		"shutdown must not attach an old follower to a recreated session row")
}

func TestCodexContextRefreshReplacesMalformedFollowerCheckpoint(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-bad-checkpoint-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354f4"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 700, 70)

	sess := &db.SessionRow{ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle", Harness: harness.CodexName}
	require.NoError(t, db.SaveSession(sess))
	require.NoError(t, db.SaveCodexTelemetryCheckpoint(sessionID, json.RawMessage(`{"version":99}`)))

	refreshCodexContextSnapshotOnRead(sess, true)
	checkpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)
	require.NotEmpty(t, checkpoint.Data)
	assert.NotEqual(t, `{"version":99}`, string(checkpoint.Data))
	contextSnapshot, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(700), contextSnapshot.TokensInput)
}

func TestCodexContextRefreshEvictsCheckpointAfterRepeatedProcessingFailures(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-failing-checkpoint-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354f6"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 500, 50)

	sess := &db.SessionRow{ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle", Harness: harness.CodexName}
	require.NoError(t, db.SaveSession(sess))
	refreshCodexContextSnapshotOnRead(sess, true)
	checkpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)

	// A directory at the memoized rollout path produces a persistent read
	// error. Unlike an incomplete final JSON line, this is a genuine processing
	// failure and should eventually evict the durable cursor.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	for failure := 1; failure <= codexCheckpointFailureEvictThreshold; failure++ {
		resetCodexRefreshThrottleForTest(sessionID)
		refreshCodexContextSnapshotOnRead(sess, true)
		checkpoint, err = db.LoadCodexTelemetryCheckpoint(sessionID)
		require.NoError(t, err)
		if failure < codexCheckpointFailureEvictThreshold {
			require.NotNil(t, checkpoint)
			assert.Equal(t, failure, checkpoint.FailureCount)
		} else {
			assert.Nil(t, checkpoint)
		}
	}

	// Failure does not blank the last good dashboard context. Once the path is
	// readable again, the reset follower rebuilds and creates a fresh checkpoint.
	contextSnapshot, err := db.GetContextSnapshot(sessionID)
	require.NoError(t, err)
	assert.Equal(t, int64(500), contextSnapshot.TokensInput)
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 900, 90)
	resetCodexRefreshThrottleForTest(sessionID)
	refreshCodexContextSnapshotOnRead(sess, true)
	checkpoint, err = db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)
	assert.Zero(t, checkpoint.FailureCount)
}

func TestCodexContextRefreshDeletesCheckpointThatGrowsTooLarge(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-oversized-checkpoint-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354f9"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 500, 50)
	sess := &db.SessionRow{ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle", Harness: harness.CodexName}
	require.NoError(t, db.SaveSession(sess))
	refreshCodexContextSnapshotOnRead(sess, true)
	checkpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)

	appendCodexRefreshEnvelope(t, path, "response_item", map[string]any{
		"type": "function_call", "name": "followup_task", "call_id": strings.Repeat("x", 2<<20),
	})
	resetCodexRefreshThrottleForTest(sessionID)
	refreshCodexContextSnapshotOnRead(sess, true)
	checkpoint, err = db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	assert.Nil(t, checkpoint, "an unrestorable oversized checkpoint must not survive in the DB")

	resetCodexRefreshThrottleForTest(sessionID)
	refreshCodexContextSnapshotOnRead(sess, true)
	codexContextRefreshMu.Lock()
	follower := codexContextRefreshMu.last[sessionID].follower
	codexContextRefreshMu.Unlock()
	checkpointData, ok, checkpointErr := follower.Checkpoint()
	assert.NoError(t, checkpointErr, "a second unchanged refresh must not re-encode the oversized state")
	assert.False(t, ok)
	assert.Nil(t, checkpointData)
}

func TestCodexContextRefreshRecreatesCheckpointAfterTokenStateShrinks(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-shrinking-checkpoint-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354fb"
		large     = int64(999999999999999999)
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	largeUsage := map[string]any{
		"input_tokens": large, "cached_input_tokens": large, "output_tokens": large,
		"reasoning_output_tokens": large, "total_tokens": large,
	}
	appendCodexRefreshEnvelope(t, path, "event_msg", map[string]any{
		"type": "token_count",
		"info": map[string]any{
			"total_token_usage": largeUsage, "last_token_usage": largeUsage,
			"model_context_window": large,
		},
	})
	appendCodexRefreshEnvelope(t, path, "response_item", map[string]any{
		"type": "function_call", "name": "followup_task",
		"call_id": strings.Repeat("x", (1<<20)-2000),
	})

	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle", Harness: harness.CodexName,
	}
	require.NoError(t, db.SaveSession(sess))
	refreshCodexContextSnapshotOnRead(sess, true)
	checkpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)
	require.Less(t, len(checkpoint.Data), 1<<20)

	// Start from the actual durable size so temp-directory path lengths cannot
	// make this boundary test flaky. At this rollout size, adding the second ID
	// does not change the digit width of the cursor fields; its checkpoint JSON
	// contribution is one comma, two quotes, and the ID bytes.
	secondIDLen := (1 << 20) + 64 - len(checkpoint.Data) - 3
	require.Positive(t, secondIDLen)
	appendCodexRefreshEnvelope(t, path, "response_item", map[string]any{
		"type": "function_call", "name": "followup_task",
		"call_id": strings.Repeat("y", secondIDLen),
	})
	resetCodexRefreshThrottleForTest(sessionID)
	refreshCodexContextSnapshotOnRead(sess, true)
	checkpoint, err = db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	assert.Nil(t, checkpoint, "the large token snapshot makes the checkpoint barely oversized")

	appendCodexRefreshTokenCount(t, path, 1, 1)
	resetCodexRefreshThrottleForTest(sessionID)
	refreshCodexContextSnapshotOnRead(sess, true)
	checkpoint, err = db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, checkpoint, "token-only shrink recreates the durable checkpoint row")
	assert.NotEmpty(t, checkpoint.Data)
}

func TestCodexContextRefreshDeletesCheckpointWhenRolloutDisappears(t *testing.T) {
	setupTestDB(t)
	resetCodexContextRefreshStateForTest()
	t.Cleanup(resetCodexContextRefreshStateForTest)

	const (
		sessionID = "codex-missing-rollout-session"
		convID    = "019ec004-4250-79b1-9ade-ebaea41354fa"
	)
	path := filepath.Join(os.Getenv("HOME"), ".codex", "sessions", "2026", "07", "16",
		"rollout-2026-07-16T10-00-00-"+convID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendCodexRefreshEnvelope(t, path, "session_meta", map[string]any{"id": convID})
	appendCodexRefreshTokenCount(t, path, 500, 50)
	sess := &db.SessionRow{ID: sessionID, ConvID: convID, TmuxSession: "codex-pane", Status: "idle", Harness: harness.CodexName}
	require.NoError(t, db.SaveSession(sess))
	refreshCodexContextSnapshotOnRead(sess, true)
	checkpoint, err := db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	require.NotNil(t, checkpoint)

	require.NoError(t, os.Remove(path))
	resetCodexRefreshThrottleForTest(sessionID)
	refreshCodexContextSnapshotOnRead(sess, true)
	checkpoint, err = db.LoadCodexTelemetryCheckpoint(sessionID)
	require.NoError(t, err)
	assert.Nil(t, checkpoint)
}

func resetCodexContextRefreshStateForTest() {
	codexContextRefreshMu.Lock()
	codexContextRefreshMu.last = nil
	codexContextRefreshMu.stopping = false
	codexContextRefreshMu.Unlock()
}

func resetCodexRefreshThrottleForTest(sessionID string) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	if codexContextRefreshMu.last == nil {
		codexContextRefreshMu.last = make(map[string]codexReadThroughSnapshot)
	}
	state := codexContextRefreshMu.last[sessionID]
	state.at = time.Time{}
	state.checkpointPersistedAt = time.Time{}
	codexContextRefreshMu.last[sessionID] = state
}

func resetCodexRefreshPollThrottleForTest(sessionID string) {
	codexContextRefreshMu.Lock()
	defer codexContextRefreshMu.Unlock()
	state := codexContextRefreshMu.last[sessionID]
	state.at = time.Time{}
	codexContextRefreshMu.last[sessionID] = state
}

func appendCodexRefreshTokenCount(t *testing.T, path string, input, output int64) {
	t.Helper()
	usage := map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": input + output}
	appendCodexRefreshEnvelope(t, path, "event_msg", map[string]any{
		"type": "token_count",
		"info": map[string]any{
			"total_token_usage": usage, "last_token_usage": usage, "model_context_window": 200000,
		},
	})
}

func appendCodexRefreshEnvelope(t *testing.T, path, typ string, payload any) {
	t.Helper()
	appendCodexRefreshEnvelopeAt(t, path, time.Now(), typ, payload)
}

func appendCodexRefreshEnvelopeAt(t *testing.T, path string, at time.Time, typ string, payload any) {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"timestamp": at.UTC().Format(time.RFC3339Nano), "type": typ, "payload": payload,
	})
	require.NoError(t, err)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, writeErr := file.Write(append(line, '\n'))
	require.NoError(t, writeErr)
	require.NoError(t, file.Close())
}
