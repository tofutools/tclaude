package stats

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatNumber(t *testing.T) {
	tests := []struct {
		input    int
		expected string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0K"},
		{1500, "1.5K"},
		{10000, "10.0K"},
		{999999, "1000.0K"},
		{1000000, "1.0M"},
		{1500000, "1.5M"},
		{1000000000, "1.0B"},
	}

	for _, tt := range tests {
		result := formatNumber(tt.input)
		assert.Equalf(t, tt.expected, result, "formatNumber(%d)", tt.input)
	}
}

func TestFormatModelName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"claude-opus-4-5-20251101", "Opus-4-5"},
		{"claude-sonnet-4-20250929", "Sonnet-4"},
		{"claude-haiku-3-5-20241022", "Haiku-3-5-20241022"},
		{"opus", "Opus"},
	}

	for _, tt := range tests {
		result := formatModelName(tt.input)
		assert.Equalf(t, tt.expected, result, "formatModelName(%q)", tt.input)
	}
}

func TestStatsCache_Parsing(t *testing.T) {
	// Test that we can parse the expected JSON structure
	jsonData := `{
		"version": 2,
		"lastComputedDate": "2026-01-31",
		"dailyActivity": [
			{"date": "2026-01-31", "messageCount": 100, "sessionCount": 5, "toolCallCount": 20}
		],
		"modelUsage": {
			"claude-opus-4-5-20251101": {
				"inputTokens": 1000,
				"outputTokens": 500,
				"cacheReadInputTokens": 10000,
				"cacheCreationInputTokens": 5000
			}
		},
		"totalSessions": 10,
		"totalMessages": 100
	}`

	var stats StatsCache
	require.NoError(t, json.Unmarshal([]byte(jsonData), &stats), "Failed to parse stats JSON")

	assert.Equal(t, 10, stats.TotalSessions)
	assert.Equal(t, 100, stats.TotalMessages)
	assert.Len(t, stats.DailyActivity, 1)

	usage, ok := stats.ModelUsage["claude-opus-4-5-20251101"]
	if assert.True(t, ok, "Expected model usage entry not found") {
		assert.Equal(t, 1000, usage.InputTokens)
	}
}
