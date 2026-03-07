package stats

import (
	"encoding/json"
	"testing"
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
		if result != tt.expected {
			t.Errorf("formatNumber(%d) = %q, want %q", tt.input, result, tt.expected)
		}
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
		if result != tt.expected {
			t.Errorf("formatModelName(%q) = %q, want %q", tt.input, result, tt.expected)
		}
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
	if err := json.Unmarshal([]byte(jsonData), &stats); err != nil {
		t.Fatalf("Failed to parse stats JSON: %v", err)
	}

	if stats.TotalSessions != 10 {
		t.Errorf("TotalSessions = %d, want 10", stats.TotalSessions)
	}

	if stats.TotalMessages != 100 {
		t.Errorf("TotalMessages = %d, want 100", stats.TotalMessages)
	}

	if len(stats.DailyActivity) != 1 {
		t.Errorf("DailyActivity length = %d, want 1", len(stats.DailyActivity))
	}

	if usage, ok := stats.ModelUsage["claude-opus-4-5-20251101"]; ok {
		if usage.InputTokens != 1000 {
			t.Errorf("InputTokens = %d, want 1000", usage.InputTokens)
		}
	} else {
		t.Error("Expected model usage entry not found")
	}
}
