package codex

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestCollectCodexUsageUsesLatestNativeCumulativeCheckpoint(t *testing.T) {
	raw := []byte(`{"timestamp":"2026-09-07T01:00:00Z","type":"session_meta","payload":{"id":"abc"}}
{"timestamp":"2026-09-07T01:01:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":40,"output_tokens":20,"reasoning_output_tokens":3,"total_tokens":120}}}}
{"timestamp":"2026-09-07T01:02:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":250,"cached_input_tokens":90,"cache_write_input_tokens":7,"output_tokens":55,"reasoning_output_tokens":8,"total_tokens":305}}}}`)
	counters, observed, partial := collectCodexUsage(raw)
	require.False(t, partial)
	require.Equal(t, "2026-09-07T01:02:00Z", observed.Format("2006-01-02T15:04:05Z"))
	require.Equal(t, []model.UsageCounter{
		{Unit: model.UsageInputTokens, Value: 250}, {Unit: model.UsageOutputTokens, Value: 55},
		{Unit: model.UsageCacheReadTokens, Value: 90}, {Unit: model.UsageCacheWriteTokens, Value: 7}, {Unit: model.UsageReasoningTokens, Value: 8},
	}, counters)
}

func TestCodexMissingUsageIsNotKnownZero(t *testing.T) {
	counters, _, partial := collectCodexUsage([]byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{}}}}`))
	require.Empty(t, counters)
	require.True(t, partial)
	counters, _, partial = collectCodexUsage([]byte(`{"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":0,"output_tokens":0}}}}`))
	require.NotEmpty(t, counters)
	require.False(t, partial)
}
