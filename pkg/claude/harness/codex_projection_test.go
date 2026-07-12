package harness

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexHookProjection_EquivalentToForwardScans(t *testing.T) {
	path := t.TempDir() + "/rollout-2026-07-12T10-00-00-019ec004-4250-79b1-9ade-ebaea41354ff.jsonl"
	f, err := os.Create(path) //nolint:gosec // test-owned temporary path
	require.NoError(t, err)
	write := func(line []byte) {
		_, err := f.Write(append(line, '\n'))
		require.NoError(t, err)
	}
	write(codexProjectionEnvelope(t, "2026-07-12T10:00:00Z", "turn_context", map[string]any{
		"model": "gpt-5.3-codex", "effort": "low",
	}))
	write(codexProjectionTokenCount(t, "2026-07-12T14:00:00Z", 10, 100, 10))
	// File position, not timestamp, defines latest. This deliberately has an
	// older timestamp than the preceding populated rate-limit record.
	write(codexProjectionTokenCount(t, "2026-07-12T12:00:00Z", 55, 200, 20))
	write(codexProjectionEnvelope(t, "2026-07-12T14:30:00Z", "turn_context", map[string]any{
		"model": "gpt-5.3-codex", "effort": "high",
	}))
	_, err = f.Write([]byte(`{"timestamp":"2026-07-12T14:40:00Z","type":"compacted","payload":{"replacement_history":"`))
	require.NoError(t, err)
	_, err = f.Write(bytes.Repeat([]byte("x"), maxCodexRolloutLineBytes+1))
	require.NoError(t, err)
	_, err = f.Write([]byte("\"}}\n"))
	require.NoError(t, err)
	write(codexProjectionTokenCount(t, "2026-07-12T15:00:00Z", -1, 3000, 300))
	// Simulate Codex being interrupted midway through its next append.
	_, err = f.Write([]byte(`{"timestamp":"2026-07-12T15:01:00Z","type":"event_msg","payload":`))
	require.NoError(t, err)
	require.NoError(t, f.Close())

	wantContext, wantContextOK, err := CodexTelemetryFromRollout(path)
	require.NoError(t, err)
	wantEffort, wantEffortOK, err := CodexEffortFromRollout(path)
	require.NoError(t, err)
	wantUsage, err := CodexUsageFromRollout(path)
	require.NoError(t, err)
	wantCost, wantCostOK, err := CodexVirtualCostFromRollout(path, "")
	require.NoError(t, err)

	got, err := CodexHookProjectionFromRollout(path, "")
	require.NoError(t, err)
	assert.Equal(t, wantContextOK, got.HasContext)
	assert.Equal(t, wantContext, got.Context)
	assert.Equal(t, wantEffortOK, got.HasEffort)
	assert.Equal(t, wantEffort, got.Effort)
	assert.Equal(t, wantUsage, got.Usage)
	assert.Equal(t, wantCostOK, got.HasCost)
	assert.Equal(t, wantCost, got.Cost)
	require.NotNil(t, got.Usage)
	require.NotNil(t, got.Usage.FiveHour)
	assert.Equal(t, 55.0, got.Usage.FiveHour.UsedPercent)
	assert.Equal(t, time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC), got.Usage.Observed,
		"last populated record wins even when its timestamp moves backwards")

	fromHook, discovered, err := CodexHookProjection(t.TempDir(), "unused", path, "")
	require.NoError(t, err)
	assert.Equal(t, path, discovered, "hook transcript_path bypasses rollout-tree discovery")
	assert.Equal(t, got, fromHook)
}

func TestCodexRuntimeTelemetry_PrefersThreadsRolloutPath(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354ee"
	rolloutDir := t.TempDir() // deliberately outside ~/.codex/sessions
	path := filepath.Join(rolloutDir, "rollout-2026-07-12T10-00-00-"+id+".jsonl")
	require.NoError(t, os.WriteFile(path, append(codexProjectionTokenCount(t,
		"2026-07-12T15:00:00Z", -1, 3000, 300), '\n'), 0o600))

	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o700))
	d, err := sql.Open("sqlite", filepath.Join(home, ".codex", "state_5.sqlite"))
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT);
		INSERT INTO threads (id, rollout_path) VALUES (?, ?)`, id, path)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	snap, err := CodexRuntimeTelemetry(home, id)
	require.NoError(t, err)
	require.True(t, snap.HasContext, "threads.rollout_path resolves before the absent date tree")
	assert.Equal(t, int64(1500), snap.Context.TokensInput)
}

func TestFindCodexRollout_ThreadsArchivePathPrefersLiveSibling(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354ed"
	dir := t.TempDir()
	live := filepath.Join(dir, "rollout-2026-07-12T10-00-00-"+id+".jsonl")
	archive := live + ".zst"
	require.NoError(t, os.WriteFile(live, []byte("live\n"), 0o600))
	require.NoError(t, os.WriteFile(archive, []byte("archive\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o700))
	d, err := sql.Open("sqlite", filepath.Join(home, ".codex", "state_5.sqlite"))
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TABLE threads (id TEXT PRIMARY KEY, rollout_path TEXT);
		INSERT INTO threads (id, rollout_path) VALUES (?, ?)`, id, archive)
	require.NoError(t, err)
	require.NoError(t, d.Close())

	got, err := findCodexRollout(home, id)
	require.NoError(t, err)
	assert.Equal(t, live, got)
}

func BenchmarkCodexHookProjection(b *testing.B) {
	path := b.TempDir() + "/rollout.jsonl"
	f, err := os.Create(path) //nolint:gosec // benchmark-owned temporary path
	if err != nil {
		b.Fatal(err)
	}
	filler := append(codexProjectionEnvelope(b, "2026-07-12T10:00:00Z", "response_item", map[string]any{
		"type": "message", "content": string(bytes.Repeat([]byte("x"), 1024)),
	}), '\n')
	for written := 0; written < 8*1024*1024; written += len(filler) {
		if _, err := f.Write(filler); err != nil {
			b.Fatal(err)
		}
	}
	for _, line := range [][]byte{
		codexProjectionEnvelope(b, "2026-07-12T14:30:00Z", "turn_context", map[string]any{"model": "gpt-5.3-codex", "effort": "high"}),
		codexProjectionTokenCount(b, "2026-07-12T15:00:00Z", 55, 3000, 300),
	} {
		if _, err := f.Write(append(line, '\n')); err != nil {
			b.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}

	b.Run("four-forward-scans", func(b *testing.B) {
		for range b.N {
			if _, _, err := CodexTelemetryFromRollout(path); err != nil {
				b.Fatal(err)
			}
			if _, _, err := CodexEffortFromRollout(path); err != nil {
				b.Fatal(err)
			}
			if _, err := CodexUsageFromRollout(path); err != nil {
				b.Fatal(err)
			}
			if _, _, err := CodexVirtualCostFromRollout(path, ""); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("one-reverse-tail", func(b *testing.B) {
		for range b.N {
			if _, err := CodexHookProjectionFromRollout(path, ""); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func codexProjectionTokenCount(t testing.TB, timestamp string, usagePercent float64, totalInput, totalOutput int64) []byte {
	payload := map[string]any{
		"type": "token_count",
		"info": map[string]any{
			"total_token_usage": map[string]any{
				"input_tokens": totalInput, "cached_input_tokens": totalInput / 10,
				"output_tokens": totalOutput, "total_tokens": totalInput + totalOutput,
			},
			"last_token_usage": map[string]any{
				"input_tokens": totalInput / 2, "output_tokens": totalOutput / 2,
				"total_tokens": totalInput/2 + totalOutput/2,
			},
			"model_context_window": 200000,
		},
	}
	if usagePercent >= 0 {
		payload["rate_limits"] = map[string]any{"primary": map[string]any{
			"used_percent": usagePercent, "window_minutes": 300, "resets_at": 1781442692,
		}}
	}
	return codexProjectionEnvelope(t, timestamp, "event_msg", payload)
}

func codexProjectionEnvelope(t testing.TB, timestamp, typ string, payload any) []byte {
	t.Helper()
	line, err := json.Marshal(map[string]any{"timestamp": timestamp, "type": typ, "payload": payload})
	require.NoError(t, err)
	return line
}
