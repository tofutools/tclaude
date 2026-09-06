package copilot

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	_ "modernc.org/sqlite"
)

func TestCollectCopilotUsageReadsCredentialFreeNativeAccountingStore(t *testing.T) {
	home := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(home, "session-store.db"))
	require.NoError(t, err)
	_, err = db.Exec(`CREATE TABLE schema_version(version INTEGER NOT NULL); INSERT INTO schema_version VALUES(6);
CREATE TABLE assistant_usage_events (
 id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, turn_index INTEGER, agent_id TEXT,
 parent_tool_call_id TEXT, model TEXT NOT NULL, input_tokens INTEGER, output_tokens INTEGER,
 cache_read_tokens INTEGER, cache_write_tokens INTEGER, reasoning_tokens INTEGER, total_nano_aiu INTEGER,
 request_multiplier REAL, duration_ms INTEGER, time_to_first_token_ms INTEGER, inter_token_latency_ms INTEGER,
 initiator TEXT, api_endpoint TEXT, reasoning_effort TEXT, finish_reason TEXT, content_filter_triggered INTEGER,
 token_details_json TEXT, created_at TEXT);
INSERT INTO assistant_usage_events(session_id,turn_index,model,input_tokens,output_tokens,cache_read_tokens,cache_write_tokens,reasoning_tokens,total_nano_aiu,request_multiplier,duration_ms,time_to_first_token_ms,inter_token_latency_ms,reasoning_effort,finish_reason,created_at)
VALUES('native-session',1,'fixture',100,20,40,2,3,7000,1,10,2,1,'medium','stop','2026-09-07T01:00:00Z'),
('native-session',2,'fixture',50,10,5,1,1,NULL,1,10,2,1,'medium','stop','2026-09-07T01:01:00Z')`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	reader := usageReader{provider: &Provider{nativeHome: home}}
	result, err := reader.Collect(context.Background(), ports.UsageCollectionRequest{
		Execution: model.Execution{ConversationID: "conversation", UpdatedAt: time.Now().UTC()},
		Native:    model.NativeConversationEvidence{Namespace: NativeNamespace, Reference: "native-session"},
	})
	require.NoError(t, err)
	require.Equal(t, model.UsageAttributionConversation, result.Attribution)
	require.Equal(t, int64(150), copilotCounter(result.Counters, model.UsageInputTokens))
	require.Equal(t, int64(30), copilotCounter(result.Counters, model.UsageOutputTokens))
	require.Equal(t, int64(2), copilotCounter(result.Counters, model.UsageRequests))
	require.Equal(t, int64(7000), copilotCounter(result.Counters, model.UsageNanoAIU))
	require.Nil(t, result.Cost)
	require.Equal(t, model.UsageCoverageUnsupported, result.Coverage.Cost)
}

func copilotCounter(counters []model.UsageCounter, unit model.UsageUnit) int64 {
	for _, counter := range counters {
		if counter.Unit == unit {
			return counter.Value
		}
	}
	return -1
}
