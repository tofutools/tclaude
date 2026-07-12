package harness

import (
	"bytes"
	"encoding/json"
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
