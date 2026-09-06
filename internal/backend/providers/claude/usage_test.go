package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestCollectClaudeUsageFromNativeTranscriptFixture(t *testing.T) {
	raw := []byte(`{"type":"user","timestamp":"2026-09-07T01:00:00Z","message":{"role":"user"}}
{"type":"assistant","timestamp":"2026-09-07T01:01:00Z","message":{"role":"assistant","usage":{"input_tokens":100,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":4}}}
not-json
{"type":"assistant","timestamp":"2026-09-07T01:02:00Z","message":{"role":"assistant","usage":{"input_tokens":50,"output_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":1}}}`)
	counters, observed, partial := collectClaudeUsage(raw)
	require.True(t, partial)
	require.Equal(t, "2026-09-07T01:02:00Z", observed.Format("2006-01-02T15:04:05Z"))
	require.Equal(t, []model.UsageCounter{
		{Unit: model.UsageInputTokens, Value: 150}, {Unit: model.UsageOutputTokens, Value: 30},
		{Unit: model.UsageCacheReadTokens, Value: 35}, {Unit: model.UsageCacheWriteTokens, Value: 5},
	}, counters)
}
