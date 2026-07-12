package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexVirtualCostFromRollout_SkipsOversizedRecord(t *testing.T) {
	path := writeOversizedCodexRollout(t,
		[][]byte{codexTestEnvelope(t, "turn_context", map[string]any{"model": "gpt-5.3-codex"})},
		[][]byte{codexTestEnvelope(t, "event_msg", map[string]any{
			"type": "token_count",
			"info": map[string]any{
				"total_token_usage": map[string]any{
					"input_tokens": 1000, "cached_input_tokens": 100, "output_tokens": 25,
				},
				"model_context_window": 200000,
			},
		})},
	)

	cost, ok, err := CodexVirtualCostFromRollout(path, "")
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "gpt-5.3-codex", cost.Model)
	assert.InDelta(t, 0.0019425, cost.CostUSD, 1e-9)
}

func TestCodexUsageFromRollout_SkipsOversizedRecord(t *testing.T) {
	path := writeOversizedCodexRollout(t, nil, [][]byte{codexTestEnvelope(t, "event_msg", map[string]any{
		"type": "token_count",
		"rate_limits": map[string]any{
			"primary": map[string]any{
				"used_percent": 37.0, "window_minutes": 300, "resets_at": 1781442692,
			},
		},
	})})

	usage, err := CodexUsageFromRollout(path)
	require.NoError(t, err)
	require.NotNil(t, usage)
	require.NotNil(t, usage.FiveHour)
	assert.Equal(t, 37.0, usage.FiveHour.UsedPercent)
}

func TestCodexEffortFromRollout_SkipsOversizedRecord(t *testing.T) {
	path := writeOversizedCodexRollout(t,
		[][]byte{codexTestEnvelope(t, "turn_context", map[string]any{"effort": "low"})},
		[][]byte{codexTestEnvelope(t, "turn_context", map[string]any{"effort": "high"})},
	)

	effort, ok, err := CodexEffortFromRollout(path)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "high", effort)
}

func TestParseCodexRolloutHead_SkipsOversizedRecord(t *testing.T) {
	meta := codexTestEnvelope(t, "session_meta", map[string]any{
		"id": "019ec004-4250-79b1-9ade-ebaea41354f1", "cwd": "/home/u/proj",
		"timestamp": "2026-07-12T10:00:00Z",
	})
	user := codexTestEnvelope(t, "event_msg", map[string]any{"type": "user_message", "message": "hello"})
	turn := codexTestEnvelope(t, "turn_context", map[string]any{"model": "gpt-5.3-codex"})

	t.Run("after session metadata", func(t *testing.T) {
		path := writeOversizedCodexRollout(t, [][]byte{meta}, [][]byte{user, turn})
		head, err := parseCodexRolloutHead(path)
		require.NoError(t, err)
		assertCodexRolloutHead(t, head)
	})

	t.Run("before any match", func(t *testing.T) {
		path := writeOversizedCodexRollout(t, nil, [][]byte{meta, user, turn})
		head, err := parseCodexRolloutHead(path)
		require.NoError(t, err)
		assertCodexRolloutHead(t, head)
	})
}

func assertCodexRolloutHead(t *testing.T, head *codexRollout) {
	t.Helper()
	require.NotNil(t, head)
	assert.Equal(t, "019ec004-4250-79b1-9ade-ebaea41354f1", head.SessionID)
	assert.Equal(t, "/home/u/proj", head.Cwd)
	assert.Equal(t, "2026-07-12T10:00:00Z", head.Created)
	assert.Equal(t, "hello", head.FirstUserMsg)
	assert.Equal(t, "gpt-5.3-codex", head.Model)
}

func writeOversizedCodexRollout(t *testing.T, before, after [][]byte) string {
	t.Helper()
	path := t.TempDir() + "/rollout-test.jsonl"
	f, err := os.Create(path) //nolint:gosec // test-owned temporary path
	require.NoError(t, err)
	writeLine := func(line []byte) {
		_, err := f.Write(append(line, '\n'))
		require.NoError(t, err)
	}
	for _, line := range before {
		writeLine(line)
	}
	_, err = f.Write([]byte(`{"timestamp":"2026-07-12T10:00:00Z","type":"compacted","payload":{"replacement_history":"`))
	require.NoError(t, err)
	_, err = f.Write(bytes.Repeat([]byte("x"), maxCodexRolloutLineBytes+1))
	require.NoError(t, err)
	_, err = f.Write([]byte("\"}}\n"))
	require.NoError(t, err)
	for _, line := range after {
		writeLine(line)
	}
	require.NoError(t, f.Close())
	return path
}

func codexTestEnvelope(t *testing.T, typ string, payload any) []byte {
	t.Helper()
	line, err := json.Marshal(map[string]any{
		"timestamp": time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"type":      typ,
		"payload":   payload,
	})
	require.NoError(t, err)
	return line
}
