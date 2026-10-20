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
	go func() {
		got <- rt.reservePeerViewWait(context.Background(), "queued", "peer", time.Now().Add(5*time.Second))
	}()
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

func TestPeerViewReplyGzipRoundTripAndLimit(t *testing.T) {
	small := fedPeerViewReply{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"ok":true}`)}
	require.Equal(t, small, gzipPeerViewReply(small), "a small body is sent as is")
	large := fedPeerViewReply{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: bytes.Repeat([]byte(`{"a":1},`), 1000)}
	packed := gzipPeerViewReply(large)
	require.Equal(t, "gzip", packed.Header.Get("Content-Encoding"))
	require.Less(t, len(packed.Body), len(large.Body))
	require.Empty(t, large.Header.Get("Content-Encoding"), "the original reply is left alone")
	unpacked, err := gunzipPeerViewReply(packed)
	require.NoError(t, err)
	require.Equal(t, large.Body, unpacked.Body)
	require.Empty(t, unpacked.Header.Get("Content-Encoding"))
	plain, err := gunzipPeerViewReply(large)
	require.NoError(t, err)
	require.Equal(t, large, plain, "an older peer's plain body passes through")

	huge := gzipPeerViewReply(fedPeerViewReply{Status: 200, Header: http.Header{}, Body: bytes.Repeat([]byte("a"), fedPeerViewResponseBodyLimit+1)})
	_, err = gunzipPeerViewReply(huge)
	require.Error(t, err, "an unpacked body is held to the body limit")
	_, err = gunzipPeerViewReply(fedPeerViewReply{Header: http.Header{"Content-Encoding": {"gzip"}}, Body: []byte("not gzip")})
	require.Error(t, err)
}
