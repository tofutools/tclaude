package harness

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexUsageRepairFollowerReadsOnlyAppendedPayload(t *testing.T) {
	home := t.TempDir()
	const id = "019ec004-4250-79b1-9ade-ebaea41354aa"
	path := newFollowerTestRollout(t, home, id)
	appendUsageSnapshot := func(percent float64) {
		appendRolloutEnvelope(t, path, "event_msg", map[string]any{
			"type": "token_count",
			"rate_limits": map[string]any{
				"limit_id": "codex",
				"primary": map[string]any{
					"used_percent": percent, "window_minutes": 300,
				},
			},
		})
	}
	appendUsageSnapshot(10)

	first, err := followCodexUsage(path)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.NotNil(t, first.FiveHour)
	assert.Equal(t, 10.0, first.FiveHour.UsedPercent)

	codexUsageFollowers.Lock()
	firstStats := codexUsageFollowers.byPath[path].stream.Stats()
	codexUsageFollowers.Unlock()
	unchanged, err := followCodexUsage(path)
	require.NoError(t, err)
	assert.Equal(t, first, unchanged)
	codexUsageFollowers.Lock()
	unchangedStats := codexUsageFollowers.byPath[path].stream.Stats()
	codexUsageFollowers.Unlock()
	assert.Equal(t, firstStats.PayloadBytes, unchangedStats.PayloadBytes)
	assert.Equal(t, firstStats.Unchanged+1, unchangedStats.Unchanged)

	before, err := os.Stat(path)
	require.NoError(t, err)
	appendUsageSnapshot(25)
	after, err := os.Stat(path)
	require.NoError(t, err)
	latest, err := followCodexUsage(path)
	require.NoError(t, err)
	require.NotNil(t, latest)
	require.NotNil(t, latest.FiveHour)
	assert.Equal(t, 25.0, latest.FiveHour.UsedPercent)
	codexUsageFollowers.Lock()
	appendedStats := codexUsageFollowers.byPath[path].stream.Stats()
	codexUsageFollowers.Unlock()
	assert.Equal(t, after.Size()-before.Size(), appendedStats.PayloadBytes-unchangedStats.PayloadBytes)
	assert.Equal(t, unchangedStats.Appends+1, appendedStats.Appends)
}
