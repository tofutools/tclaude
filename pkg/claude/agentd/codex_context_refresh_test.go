package agentd

import (
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
	codexContextRefreshMu.Unlock()
}

func resetCodexRefreshThrottleForTest(sessionID string) {
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
	line, err := json.Marshal(map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "type": typ, "payload": payload,
	})
	require.NoError(t, err)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	require.NoError(t, err)
	_, writeErr := file.Write(append(line, '\n'))
	require.NoError(t, writeErr)
	require.NoError(t, file.Close())
}
