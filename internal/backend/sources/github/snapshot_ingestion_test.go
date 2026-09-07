package github

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestSnapshotIdentityIncludesFallbackTimeAcrossPRUpdates(t *testing.T) {
	ctx := context.Background()
	closed := false
	now := testTime
	source := setup(t, func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.Contains(r.URL.Path, "/pulls/"):
			value := pullJSON(testSHA)
			if closed {
				value["state"] = "closed"
				value["updated_at"] = now.Add(-time.Second)
			}
			respondJSON(w, value)
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			respondJSON(w, map[string]any{"total_count": 0, "check_runs": []any{}})
		case strings.HasSuffix(r.URL.Path, "/status"):
			respondJSON(w, map[string]any{"sha": testSHA, "total_count": 0, "statuses": []any{}})
		default:
			return false
		}
		return true
	})
	source.clock = func() time.Time { return now }
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	req := request()
	first, err := source.CollectAutomationFacts(ctx, req)
	require.NoError(t, err)
	require.NoError(t, service.IngestTrustedAutomationFacts(ctx, source.SourceID(), first.Facts))
	closed = true
	now = now.Add(2 * time.Minute)
	req.Now = now
	second, err := source.CollectAutomationFacts(ctx, req)
	require.NoError(t, err)
	require.NotEqual(t, first.Facts[1].EventID, second.Facts[1].EventID)
	require.NoError(t, service.IngestTrustedAutomationFacts(ctx, source.SourceID(), second.Facts))
	// Re-observe the exact second batch to prove response-loss retry is safe.
	require.NoError(t, service.IngestTrustedAutomationFacts(ctx, source.SourceID(), second.Facts))
	facts, err := store.AutomationFactsAfter(ctx, source.SourceID(), req.Resource, 0, 256)
	require.NoError(t, err)
	require.Len(t, facts, 4)
	require.Equal(t, "closed", second.Facts[0].Value)
}

func TestCollectionCompletionClockAcceptsNewlyCompletedCheck(t *testing.T) {
	ctx := context.Background()
	started := testTime
	observed := started
	source := setup(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			// Simulate a check completing while the sequential HTTP snapshot is read.
			observed = started.Add(2 * time.Second)
			respondJSON(w, map[string]any{"total_count": 1, "check_runs": []map[string]any{{"id": 1, "status": "completed", "conclusion": "success", "completed_at": observed.Add(-time.Second)}}})
			return true
		}
		return false
	})
	source.clock = func() time.Time { return observed }
	batch, err := source.CollectAutomationFacts(ctx, request())
	require.NoError(t, err)
	require.True(t, batch.Facts[1].OccurredAt.After(started))
	require.Equal(t, observed, batch.Facts[1].ObservedAt)
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	require.NoError(t, app.New(store, providers.NewRegistry()).IngestTrustedAutomationFacts(ctx, source.SourceID(), batch.Facts))
}

func TestFutureRemoteTimestampIsAnIsolatedCollectionFailure(t *testing.T) {
	source := setup(t, func(w http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/check-runs") {
			respondJSON(w, map[string]any{"total_count": 1, "check_runs": []map[string]any{{"id": 1, "status": "completed", "conclusion": "success", "completed_at": testTime.Add(time.Hour)}}})
			return true
		}
		return false
	})
	source.clock = func() time.Time { return testTime }
	batch, err := source.CollectAutomationFacts(context.Background(), request())
	require.Error(t, err)
	require.Equal(t, ports.AutomationFactBatch{}, batch)
}
