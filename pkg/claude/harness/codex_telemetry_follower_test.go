package harness

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexTelemetryFollower_IncrementalMatchesFullScan(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354c1"
	path := newFollowerTestRollout(t, home, id)
	appendSubagentActivity(t, path, "child-a", "interrupted", "")
	appendTokenCount(t, path, 1000, 100, 1100)

	follower := &CodexTelemetryFollower{}
	assertFollowerMatchesFull(t, follower, home, id, path)
	firstOffset := follower.offset

	// The function call and its correlated activity deliberately land in
	// separate reads. The pending call-id map must survive the boundary.
	appendRolloutEnvelope(t, path, "response_item", map[string]any{
		"type": "function_call", "name": "followup_task", "call_id": "call-split",
	})
	assertFollowerMatchesFull(t, follower, home, id, path)
	assert.Greater(t, follower.offset, firstOffset)

	appendRolloutEnvelope(t, path, "event_msg", map[string]any{
		"type": "sub_agent_activity", "event_id": "call-split",
		"agent_thread_id": "child-a", "kind": "interacted",
	})
	appendTokenCount(t, path, 9000, 1000, 10000)
	got := assertFollowerMatchesFull(t, follower, home, id, path)
	assert.NotContains(t, got.InterruptedSubagents, "child-a")
	assert.Equal(t, int64(9000), got.Context.TokensInput, "latest token_count survives incremental scan")

	// A malformed complete append is decode doubt: rebuild from zero (whose
	// legacy contract skips malformed history) rather than keeping partial
	// incremental mutations.
	appendBytes(t, path, []byte("{not-json}\n"))
	assertFollowerMatchesFull(t, follower, home, id, path)
}

func TestCodexTelemetryFollower_CheckpointSurvivesRestartWithFoldState(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354f1"
	path := newFollowerTestRollout(t, home, id)
	appendSubagentActivity(t, path, "child-a", "interrupted", "")
	appendTokenCount(t, path, 1000, 100, 1100)
	// The correlated activity lands after the simulated daemon restart. The
	// pending call-id therefore proves the checkpoint carries parser state, not
	// merely an offset that would be meaningless on its own.
	appendRolloutEnvelope(t, path, "response_item", map[string]any{
		"type": "function_call", "name": "followup_task", "call_id": "call-after-restart",
	})

	beforeRestart := &CodexTelemetryFollower{}
	first := assertFollowerMatchesFull(t, beforeRestart, home, id, path)
	assert.Contains(t, first.InterruptedSubagents, "child-a")
	checkpoint, ok, err := beforeRestart.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)
	checkpointOffset := beforeRestart.offset

	restored := &CodexTelemetryFollower{}
	require.NoError(t, restored.RestoreCheckpoint(checkpoint))
	unchanged, err := restored.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Equal(t, first, unchanged)
	assert.Equal(t, checkpointOffset, restored.offset)
	assert.False(t, restored.restored, "the first read validates and adopts the durable cursor")

	appendRolloutEnvelope(t, path, "event_msg", map[string]any{
		"type": "sub_agent_activity", "event_id": "call-after-restart",
		"agent_thread_id": "child-a", "kind": "interacted",
	})
	appendTokenCount(t, path, 9000, 900, 9900)
	got := assertFollowerMatchesFull(t, restored, home, id, path)
	assert.NotContains(t, got.InterruptedSubagents, "child-a",
		"followup call-id from before restart clears the interrupted child")
	assert.Equal(t, int64(9000), got.Context.TokensInput)
	assert.Greater(t, restored.offset, checkpointOffset)

	nextCheckpoint, ok, err := restored.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)
	assert.NotEqual(t, string(checkpoint), string(nextCheckpoint), "advanced state produces a new checkpoint")
}

func TestCodexTelemetryFollower_CheckpointInvalidationRebuilds(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354f2"
	path := newFollowerTestRollout(t, home, id)
	appendSubagentActivity(t, path, "old-child", "interrupted", "")
	appendTokenCount(t, path, 100, 10, 110)

	beforeRotation := &CodexTelemetryFollower{}
	_, err := beforeRotation.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	checkpoint, ok, err := beforeRotation.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, os.Rename(path, path+".old"))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendRolloutEnvelope(t, path, "session_meta", map[string]any{"id": id})
	appendSubagentActivity(t, path, "new-child", "interrupted", "")
	appendTokenCount(t, path, 800, 80, 880)

	restored := &CodexTelemetryFollower{}
	require.NoError(t, restored.RestoreCheckpoint(checkpoint))
	got, err := restored.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.NotContains(t, got.InterruptedSubagents, "old-child")
	assert.Contains(t, got.InterruptedSubagents, "new-child")
	assert.Equal(t, int64(800), got.Context.TokensInput,
		"rotated/truncated path rejects the durable cursor and rebuilds")
}

func TestCodexTelemetryFollower_CheckpointShrinkRebuilds(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354f7"
	path := newFollowerTestRollout(t, home, id)
	appendSubagentActivity(t, path, "old-child-with-a-long-name", "interrupted", "")
	appendTokenCount(t, path, 1000, 100, 1100)

	beforeShrink := &CodexTelemetryFollower{}
	_, err := beforeShrink.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	checkpoint, ok, err := beforeShrink.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)
	oldInfo, err := os.Stat(path)
	require.NoError(t, err)

	shrunk := append(rolloutEnvelope(t, "event_msg", map[string]any{
		"type": "sub_agent_activity", "agent_thread_id": "new", "kind": "interrupted",
	}), '\n')
	require.Less(t, int64(len(shrunk)), oldInfo.Size())
	require.NoError(t, os.WriteFile(path, shrunk, 0o600))
	newInfo, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(oldInfo, newInfo), "shrink keeps the same file identity")

	restored := &CodexTelemetryFollower{}
	require.NoError(t, restored.RestoreCheckpoint(checkpoint))
	got, err := restored.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.NotContains(t, got.InterruptedSubagents, "old-child-with-a-long-name")
	assert.Contains(t, got.InterruptedSubagents, "new", "size shrink rejects the durable cursor")
}

func TestCodexTelemetryFollower_CheckpointSameSizeRewriteRebuilds(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354f5"
	path := newFollowerTestRollout(t, home, id)
	appendSubagentActivity(t, path, "old-child", "interrupted", "")
	appendTokenCount(t, path, 100, 10, 110)

	beforeRewrite := &CodexTelemetryFollower{}
	_, err := beforeRewrite.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	checkpoint, ok, err := beforeRewrite.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)
	originalInfo, err := os.Stat(path)
	require.NoError(t, err)

	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	rewritten := bytes.Replace(raw, []byte("old-child"), []byte("new-child"), 1)
	require.Equal(t, len(raw), len(rewritten), "precondition: rewrite keeps file size")
	require.NoError(t, os.WriteFile(path, rewritten, 0o600))
	changedTime := originalInfo.ModTime().Add(time.Second)
	require.NoError(t, os.Chtimes(path, changedTime, changedTime))

	restored := &CodexTelemetryFollower{}
	require.NoError(t, restored.RestoreCheckpoint(checkpoint))
	got, err := restored.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.NotContains(t, got.InterruptedSubagents, "old-child")
	assert.Contains(t, got.InterruptedSubagents, "new-child",
		"same-size rewrite with changed mtime rejects the checkpoint")
}

func TestCodexTelemetryFollower_RejectsMalformedCheckpoint(t *testing.T) {
	follower := &CodexTelemetryFollower{}
	assert.Error(t, follower.RestoreCheckpoint([]byte(`{"version":99}`)))
	assert.Error(t, follower.RestoreCheckpoint([]byte(`{"version":1,"offset":42}`)))
	assert.Error(t, follower.RestoreCheckpoint(nil))
}

func TestCodexTelemetryFollower_RejectsOversizedCheckpoint(t *testing.T) {
	follower := &CodexTelemetryFollower{
		home: "/tmp/home", convID: "conv", path: "/tmp/rollout.jsonl", offset: 64,
		checkpointSize: 64, checkpointAnchor: bytes.Repeat([]byte("a"), 64),
		state: newCodexRuntimeScanState(),
	}
	follower.state.followupCallIDs[string(bytes.Repeat([]byte("x"), maxCodexTelemetryCheckpointBytes))] = struct{}{}
	checkpoint, ok, err := follower.Checkpoint()
	assert.ErrorIs(t, err, ErrCodexTelemetryCheckpointTooLarge)
	assert.False(t, ok)
	assert.Nil(t, checkpoint)

	checkpoint, ok, err = follower.Checkpoint()
	assert.NoError(t, err, "unchanged oversized state is suppressed instead of re-encoded")
	assert.False(t, ok)
	assert.Nil(t, checkpoint)

	revisionChangingLine := rolloutEnvelope(t, "response_item", map[string]any{
		"type": "function_call", "name": "followup_task", "call_id": "another-call",
	})
	require.True(t, follower.state.consumeLine(revisionChangingLine))
	checkpoint, ok, err = follower.Checkpoint()
	assert.ErrorIs(t, err, ErrCodexTelemetryCheckpointTooLarge,
		"a collaboration-ledger change makes the follower retry encoding")
	assert.False(t, ok)
	assert.Nil(t, checkpoint)
}

func TestCaptureCodexTelemetryCheckpointUsesScannedDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	fileA := append(rolloutEnvelope(t, "event_msg", map[string]any{
		"type": "sub_agent_activity", "agent_thread_id": "file-a", "kind": "interrupted",
	}), '\n')
	require.NoError(t, os.WriteFile(path, fileA, 0o600))
	opened, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = opened.Close() })
	fileAInfo, err := opened.Stat()
	require.NoError(t, err)

	// Replace the pathname after opening A. Metadata and the anchor must still
	// come from the descriptor that supplied the fold state, and the caller's
	// final SameFile check must detect that the pathname now resolves to B.
	require.NoError(t, os.Rename(path, path+".old"))
	fileB := append(rolloutEnvelope(t, "event_msg", map[string]any{
		"type": "sub_agent_activity", "agent_thread_id": "file-b", "kind": "interrupted",
	}), '\n')
	require.NoError(t, os.WriteFile(path, fileB, 0o600))
	metadata, err := captureCodexTelemetryCheckpoint(opened, int64(len(fileA)))
	require.NoError(t, err)
	assert.True(t, os.SameFile(fileAInfo, metadata.info))
	pathInfo, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, os.SameFile(metadata.info, pathInfo), "replacement is rejected before state is committed")
	assert.Equal(t, fileA[max(len(fileA)-codexTelemetryAnchorBytes, 0):], metadata.anchor)
}

func TestScanCodexTelemetryToStableConsumesAppendAfterEOF(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354fb"
	path := newFollowerTestRollout(t, home, id)
	appendTokenCount(t, path, 100, 10, 110)
	file, err := os.Open(path)
	require.NoError(t, err)
	defer func() { _ = file.Close() }()

	state := newCodexRuntimeScanState()
	injected := false
	scanner := func(r io.Reader, rolloutPath string, scanState *codexRuntimeScanState, strict bool) (int64, bool, error) {
		read, doubt, scanErr := scanCompleteCodexLines(r, rolloutPath, scanState, strict)
		if !injected {
			injected = true
			appendTokenCount(t, path, 900, 90, 990)
		}
		return read, doubt, scanErr
	}
	offset, metadata, doubt, err := scanCodexTelemetryToStableWithScanner(file, path, &state, 0, false, scanner)
	require.NoError(t, err)
	assert.False(t, doubt)
	assert.True(t, injected)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, info.Size(), offset, "the complete append racing with EOF is consumed")
	assert.Equal(t, offset, metadata.size)
	assert.Equal(t, int64(900), state.snapshot().Context.TokensInput)
}

func TestCodexTelemetryFollower_MissingRolloutClearsCheckpoint(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354f8"
	path := newFollowerTestRollout(t, home, id)
	appendTokenCount(t, path, 100, 10, 110)
	follower := &CodexTelemetryFollower{}
	_, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	_, ok, err := follower.Checkpoint()
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, os.Remove(path))
	got, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Equal(t, CodexRuntimeSnapshot{}, got)
	checkpoint, ok, err := follower.Checkpoint()
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Nil(t, checkpoint)
}

func TestCodexTelemetryFollower_PartialLineIsRetried(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354c2"
	path := newFollowerTestRollout(t, home, id)
	follower := &CodexTelemetryFollower{}
	before, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	offset := follower.offset

	line := rolloutEnvelope(t, "event_msg", map[string]any{
		"type": "sub_agent_activity", "agent_thread_id": "partial-child", "kind": "interrupted",
	})
	cut := len(line) / 2
	appendBytes(t, path, line[:cut])
	got, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Equal(t, before, got)
	assert.Equal(t, offset, follower.offset, "unterminated bytes are not consumed")

	appendBytes(t, path, append(line[cut:], '\n'))
	got = assertFollowerMatchesFull(t, follower, home, id, path)
	assert.Contains(t, got.InterruptedSubagents, "partial-child")
}

func TestCodexTelemetryFollower_CompleteEOFAndConversationChange(t *testing.T) {
	home := t.TempDir()
	const firstID = "019ec004-4250-79b1-9ade-ebaea41354d1"
	firstPath := newFollowerTestRollout(t, home, firstID)
	line := rolloutEnvelope(t, "event_msg", map[string]any{
		"type": "sub_agent_activity", "agent_thread_id": "complete-eof", "kind": "interrupted",
	})
	appendBytes(t, firstPath, line) // deliberately no trailing newline

	follower := &CodexTelemetryFollower{}
	got := assertFollowerMatchesFull(t, follower, home, firstID, firstPath)
	assert.Contains(t, got.InterruptedSubagents, "complete-eof")
	info, err := os.Stat(firstPath)
	require.NoError(t, err)
	assert.Equal(t, info.Size(), follower.offset, "syntactically complete EOF is consumed")

	// A follower is normally session-scoped, but a session row's conversation
	// can transition. Its still-existing old rollout must not win path memoization.
	const secondID = "019ec004-4250-79b1-9ade-ebaea41354d2"
	secondPath := newFollowerTestRollout(t, home, secondID)
	appendSubagentActivity(t, secondPath, "second-conv", "interrupted", "")
	got = assertFollowerMatchesFull(t, follower, home, secondID, secondPath)
	assert.NotContains(t, got.InterruptedSubagents, "complete-eof")
	assert.Contains(t, got.InterruptedSubagents, "second-conv")
	assert.Equal(t, secondPath, follower.path)
}

func TestCodexTelemetryFollower_ShrinkAndRotationRebuild(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354c3"
	path := newFollowerTestRollout(t, home, id)
	appendSubagentActivity(t, path, "old", "interrupted", "")
	follower := &CodexTelemetryFollower{}
	got, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Contains(t, got.InterruptedSubagents, "old")

	shrunk := append(rolloutEnvelope(t, "event_msg", map[string]any{
		"type": "sub_agent_activity", "agent_thread_id": "new", "kind": "interrupted",
	}), '\n')
	require.NoError(t, os.WriteFile(path, shrunk, 0o600))
	got, err = follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.NotContains(t, got.InterruptedSubagents, "old")
	assert.Contains(t, got.InterruptedSubagents, "new", "size shrink rebuilds from byte zero")

	oldInfo := follower.info
	require.NoError(t, os.Rename(path, path+".old"))
	rotated := append(rolloutEnvelope(t, "event_msg", map[string]any{
		"type": "sub_agent_activity", "agent_thread_id": "rotated", "kind": "interrupted",
	}), '\n')
	require.NoError(t, os.WriteFile(path, rotated, 0o600))
	got, err = follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.False(t, os.SameFile(oldInfo, follower.info), "rotation changes file identity")
	assert.NotContains(t, got.InterruptedSubagents, "new")
	assert.Contains(t, got.InterruptedSubagents, "rotated")
}

func TestCodexTelemetryFollower_StatSkipAndZstdPathRefresh(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354c4"
	path := newFollowerTestRollout(t, home, id)
	appendTokenCount(t, path, 100, 10, 110)
	follower := &CodexTelemetryFollower{}
	first, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	info := follower.info

	// Change a same-width token value, then restore mtime. Size+mtime+identity
	// match, so the follower must return its cached snapshot without opening.
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	replaced := []byte(string(raw))
	for i := 0; i+3 <= len(replaced); i++ {
		if string(replaced[i:i+3]) == "100" {
			copy(replaced[i:i+3], "900")
		}
	}
	require.NoError(t, os.WriteFile(path, replaced, 0o600))
	require.NoError(t, os.Chtimes(path, info.ModTime(), info.ModTime()))
	cached, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Equal(t, first, cached)

	// Archiving removes the memoized live path. The follower re-walks once,
	// resolves .zst, full-scans it, and thereafter uses stat-skip only.
	enc, err := zstd.NewWriter(nil)
	require.NoError(t, err)
	compressed := enc.EncodeAll(raw, nil)
	enc.Close()
	require.NoError(t, os.WriteFile(path+".zst", compressed, 0o600))
	require.NoError(t, os.Remove(path))
	archived, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Equal(t, first, archived)
	assert.Equal(t, path+".zst", follower.path)
	assert.Zero(t, follower.offset, ".zst is never tailed")
	archivedAgain, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Equal(t, archived, archivedAgain)
}

func TestCodexTelemetryFollower_SkipsOversizedRecordAndContinues(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354e1"
	path := newFollowerTestRollout(t, home, id)
	appendTokenCount(t, path, 100, 10, 110)

	follower := &CodexTelemetryFollower{}
	first, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Equal(t, int64(100), first.Context.TokensInput)

	// Current Codex compaction replacement-history records can exceed 10 MiB.
	// The runtime follower must chunk-discard the complete irrelevant record,
	// commit its newline offset, and still observe telemetry appended after it.
	prefix := []byte(`{"timestamp":"2026-07-12T10:00:00Z","type":"compacted","payload":{"replacement_history":"`)
	suffix := []byte("\"}}\n")
	oversized := make([]byte, 0, len(prefix)+maxCodexRolloutLineBytes+1)
	oversized = append(oversized, prefix...)
	oversized = append(oversized, bytes.Repeat([]byte("x"), maxCodexRolloutLineBytes+1)...)
	offsetBeforeTail := follower.offset
	appendBytes(t, path, oversized)
	whilePartial, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Equal(t, first, whilePartial)
	assert.Equal(t, offsetBeforeTail, follower.offset, "unterminated oversized tail is not consumed")

	appendBytes(t, path, suffix)
	reset, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.True(t, reset.ContextReset,
		"oversized compacted record invalidates pre-compaction telemetry")
	assert.False(t, reset.HasContext)

	appendTokenCount(t, path, 900, 90, 990)

	got, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Equal(t, int64(900), got.Context.TokensInput, "incremental scan reaches telemetry after oversized record")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, info.Size(), follower.offset, "offset advances beyond oversized record newline")

	full, err := CodexRuntimeTelemetryFromRollout(path)
	require.NoError(t, err)
	assert.Equal(t, got, full, "full runtime scan also reaches later telemetry")
	fromFreshFollower, err := (&CodexTelemetryFollower{}).RuntimeTelemetry(home, id)
	require.NoError(t, err)
	assert.Equal(t, got, fromFreshFollower, "follower full scan matches incremental scan")
}

func assertFollowerMatchesFull(t *testing.T, follower *CodexTelemetryFollower, home, id, path string) CodexRuntimeSnapshot {
	t.Helper()
	got, err := follower.RuntimeTelemetry(home, id)
	require.NoError(t, err)
	want, err := CodexRuntimeTelemetryFromRollout(path)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	return got
}

func appendRolloutEnvelope(t *testing.T, path, typ string, payload any) {
	t.Helper()
	appendBytes(t, path, append(rolloutEnvelope(t, typ, payload), '\n'))
}

func newFollowerTestRollout(t *testing.T, home, id string) string {
	t.Helper()
	dir := filepath.Join(home, ".codex", "sessions", "2026", "07", "12")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "rollout-2026-07-12T10-00-00-"+id+".jsonl")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	appendRolloutEnvelope(t, path, "session_meta", map[string]any{
		"id": id, "cwd": "/tmp/project", "timestamp": "2026-07-12T10:00:00Z",
	})
	return path
}

func appendSubagentActivity(t *testing.T, path, child, kind, eventID string) {
	t.Helper()
	appendRolloutEnvelope(t, path, "event_msg", map[string]any{
		"type": "sub_agent_activity", "event_id": eventID,
		"agent_thread_id": child, "kind": kind,
	})
}

func appendTokenCount(t *testing.T, path string, input, output, total int64) {
	t.Helper()
	usage := map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": total}
	appendRolloutEnvelope(t, path, "event_msg", map[string]any{
		"type": "token_count",
		"info": map[string]any{
			"total_token_usage": usage, "last_token_usage": usage, "model_context_window": 200000,
		},
	})
}

func rolloutEnvelope(t *testing.T, typ string, payload any) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"type":      typ,
		"payload":   payload,
	})
	require.NoError(t, err)
	return b
}

func appendBytes(t *testing.T, path string, b []byte) {
	t.Helper()
	f, err := os.OpenFile(filepath.Clean(path), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	defer func() { require.NoError(t, f.Close()) }()
	_, err = f.Write(b)
	require.NoError(t, err)
}
