package transport

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

type publicUsageProvider struct{ lifecycleProvider }

func (*publicUsageProvider) Usage() ports.UsageReader { return publicUsageReader{} }

type publicUsageReader struct{}

func (publicUsageReader) Capabilities() ports.UsageCapabilities {
	return ports.UsageCapabilities{CollectCounters: true}
}
func (publicUsageReader) Collect(_ context.Context, req ports.UsageCollectionRequest) (ports.CollectedUsage, error) {
	return ports.CollectedUsage{SourceKey: "private-file-identity", Source: "native history", SourceRevision: "one", ObservedAt: time.Unix(100, 0), Counters: []model.UsageCounter{{Unit: model.UsageInputTokens, Value: 12}}, Coverage: model.UsageCoverage{Counters: model.UsageCoverageComplete, Cost: model.UsageCoverageUnsupported}, Cumulative: true, Attribution: model.UsageAttributionConversation}, nil
}

func TestPublicUsageAndActivityPreserveAttribution(t *testing.T) {
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	h := testHandler(t, app.New(store, providers.NewRegistry(&publicUsageProvider{})))
	response := request(h, "POST", "/v2/launch", `{"request_id":"launch","target":{"standalone":{"desired":{"Harness":"test-native","WorkingDirectory":"/tmp","Approval":"supervised","Sandbox":"unconfined"}}}}`, testCredential)
	require.Equal(t, 202, response.Code, response.Body.String())
	var launched struct{ Execution executionView }
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &launched))
	target := app.UsageTarget{ExecutionID: launched.Execution.ID}
	data, err := json.Marshal(map[string]any{"target": target})
	require.NoError(t, err)
	require.Equal(t, 401, request(h, "POST", "/v2/usage/refresh", string(data), "").Code)
	for i := 0; i < 2; i++ {
		response = request(h, "POST", "/v2/usage/refresh", string(data), testCredential)
		require.Equal(t, 200, response.Code, response.Body.String())
		require.NotContains(t, response.Body.String(), "private-file-identity")
		var result app.RefreshUsageResult
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Equal(t, i == 1, result.Repeated)
		require.Empty(t, result.Observation.Attribution.ExecutionID)
		require.Equal(t, model.UsageAttributionConversation, result.Observation.Attribution.Precision)
	}
	data, err = json.Marshal(map[string]any{"filter": app.UsageFilter{Target: app.UsageTarget{ConversationID: launched.Execution.ConversationID}, Limit: 10}})
	require.NoError(t, err)
	response = request(h, "POST", "/v2/usage/query", string(data), testCredential)
	require.Equal(t, 200, response.Code, response.Body.String())
	var usage app.UsageResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &usage))
	require.Len(t, usage.Observations, 1)
	data, err = json.Marshal(map[string]any{"filter": app.ActivityFilter{Target: app.ActivityTarget{ExecutionID: launched.Execution.ID}, Limit: 10}})
	require.NoError(t, err)
	_, activityErr := app.New(store, providers.NewRegistry()).QueryActivity(t.Context(), app.QueryActivityRequest{Principal: model.Principal{Kind: model.PrincipalOperator}, Filter: app.ActivityFilter{Target: app.ActivityTarget{ExecutionID: launched.Execution.ID}, Limit: 10}})
	require.NoError(t, activityErr)
	response = request(h, "POST", "/v2/activity/query", string(data), testCredential)
	require.Equal(t, 200, response.Code, response.Body.String())
	var activity app.ActivityResult
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &activity))
	require.NotEmpty(t, activity.Records)
	require.NotContains(t, response.Body.String(), "Generation")
	require.NotContains(t, response.Body.String(), "Delegation")
	bad := request(h, "POST", "/v2/usage/refresh", `{"target":{},"principal":{"Kind":"operator"}}`, testCredential)
	require.Equal(t, 400, bad.Code)
}
