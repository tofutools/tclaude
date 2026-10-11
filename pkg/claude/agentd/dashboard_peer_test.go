package agentd

import (
	"bytes"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"

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
