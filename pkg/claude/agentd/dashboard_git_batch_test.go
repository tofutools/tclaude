package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDashboardGitBatchConcurrency(t *testing.T) {
	for _, limit := range []int{1, 7, 100} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			requests := make([]dashboardGitRequest, 110)
			for i := range requests {
				requests[i].Path = fmt.Sprint(i)
			}
			gate := make(chan struct{})
			var active, peak atomic.Int32
			run := func(ctx context.Context, r dashboardGitRequest) dashboardGitResult {
				n := active.Add(1)
				for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
				}
				if n == int32(limit) {
					select {
					case <-gate:
					default:
						close(gate)
					}
				}
				select {
				case <-gate:
				case <-ctx.Done():
				}
				active.Add(-1)
				status := "updated"
				if r.Path == "2" {
					status = "failed"
				}
				return dashboardGitResult{Path: r.Path, Status: status}
			}
			counts := map[string]int{}
			err := streamDashboardGitBatch(ctx, requests, limit, run, func(r dashboardGitResult) error { counts[r.Status]++; return nil })
			require.NoError(t, err)
			require.Equal(t, int32(limit), peak.Load())
			require.Equal(t, map[string]int{"running": 110, "updated": 109, "failed": 1}, counts)
		})
	}
}

func TestDashboardGitBatchDisconnectCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	ended := make(chan error, 1)
	var calls atomic.Int32
	go func() {
		ended <- streamDashboardGitBatch(ctx, make([]dashboardGitRequest, 10), 1, func(ctx context.Context, r dashboardGitRequest) dashboardGitResult {
			calls.Add(1)
			close(started)
			<-ctx.Done()
			return dashboardGitResult{}
		}, func(dashboardGitResult) error { return nil })
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never started")
	}
	cancel()
	select {
	case err := <-ended:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not cancel")
	}
	require.Equal(t, int32(1), calls.Load())
}

func TestDashboardGitBatchFlushThroughAudit(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(auditRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-ndjson")
		_ = json.NewEncoder(w).Encode(dashboardGitResult{Status: "running"})
		if err := http.NewResponseController(w).Flush(); err != nil {
			return
		}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		_ = json.NewEncoder(w).Encode(dashboardGitResult{Status: "complete"})
	})))
	defer server.Close()
	defer close(release)
	client := &http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(server.URL + "/api/git-repositories/batch")
	require.NoError(t, err)
	defer response.Body.Close()
	var result dashboardGitResult
	require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
	require.Equal(t, "running", result.Status, "record must arrive while handler is still waiting")
}
