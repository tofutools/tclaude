package agentd

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDashboardPeerRequiresCookieBeforePeerLookup(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	mux := http.NewServeMux()
	registerDashboardPeerRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/peer/unknown/snapshot", nil))
	require.Equal(t, 403, rec.Code)
	require.NotContains(t, rec.Body.String(), "not_trusted")
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, dashboardRequest("GET", "/api/peer/unknown/snapshot", ""))
	require.Equal(t, 403, rec.Code)
	require.Contains(t, rec.Body.String(), "not_trusted")
}

func TestPeerViewFrameAndAdmissionBounds(t *testing.T) {
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(fedPeerViewRequestLimit+1))
	var req fedPeerViewRequest
	require.Error(t, readPeerViewFrame(bytes.NewReader(prefix[:]), fedPeerViewRequestLimit, &req))
	require.Error(t, writePeerViewFrame(&bytes.Buffer{}, 8, fedPeerViewRequest{URI: "/api/snapshot"}))
	rt := &fedRuntime{}
	for i := 0; i < fedPeerViewPerPeerLimit; i++ {
		require.True(t, rt.reservePeerView(string(rune('a'+i)), "peer"))
	}
	require.False(t, rt.reservePeerView("overflow", "peer"))
	rt.releasePeerView("a")
	require.True(t, rt.reservePeerView("next", "peer"))
	require.False(t, rt.reservePeerView("next", "other"))
}

// A request finding every slot taken waits for one instead of failing busy,
// and gives up at its deadline.
func TestPeerViewAdmissionQueuesForAFreedSlot(t *testing.T) {
	rt := &fedRuntime{}
	for i := 0; i < fedPeerViewPerPeerLimit; i++ {
		require.True(t, rt.reservePeerView(string(rune('a'+i)), "peer"))
	}
	require.False(t, rt.reservePeerViewWait(context.Background(), "late", "peer", time.Now().Add(20*time.Millisecond)))

	got := make(chan bool, 1)
	go func() { got <- rt.reservePeerViewWait(context.Background(), "queued", "peer", time.Now().Add(5*time.Second)) }()
	time.Sleep(20 * time.Millisecond)
	rt.releasePeerView("a")
	select {
	case ok := <-got:
		require.True(t, ok, "the queued request takes the freed slot")
	case <-time.After(2 * time.Second):
		t.Fatal("the queued request was not woken by the release")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.False(t, rt.reservePeerViewWait(ctx, "cancelled", "peer", time.Now().Add(5*time.Second)))
}
