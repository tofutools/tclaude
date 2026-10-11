package agentd_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Fleet release metadata polling follows the federation runtime: it starts
// when federation is enabled while the daemon runs, and stops when it is
// disabled. With federation off nothing starts.
func TestFleetMetadataFollowsFederationAtRuntime(t *testing.T) {
	f := newFlow(t)
	agentd.ResetFederationForTest()
	t.Cleanup(agentd.ResetFederationForTest)
	var mu sync.Mutex
	var started []context.Context
	t.Cleanup(agentd.SetFleetMetadataStartForTest(func(ctx context.Context) {
		mu.Lock()
		defer mu.Unlock()
		started = append(started, ctx)
	}))
	starts := func() []context.Context { mu.Lock(); defer mu.Unlock(); return append([]context.Context(nil), started...) }

	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": false, "hub_url": "ws://127.0.0.1:1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Empty(t, starts(), "a disabled federation polls nothing")

	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": true, "hub_url": "ws://127.0.0.1:1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	fedEventually(t, "metadata loop started with the runtime", func() bool { return len(starts()) == 1 })
	ctx := starts()[0]
	require.NoError(t, ctx.Err(), "running while federation runs")

	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/config", map[string]any{"enabled": false, "hub_url": "ws://127.0.0.1:1"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("metadata loop still running after federation was disabled")
	}
	require.Len(t, starts(), 1, "disabling starts nothing new")
}
