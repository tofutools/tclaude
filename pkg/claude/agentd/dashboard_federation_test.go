package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestDashboardFederationStatusSummaryAndLocalAuth(t *testing.T) {
	setupTestDB(t)
	withDashboardAuth(t)
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: id.ID(), PubKey: id.Pub, Label: "laptop", Name: "bob", TrustLevel: db.FederationTrustUnrestricted}))
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: id.ID(), Slug: PermNodeRead}))
	mux := http.NewServeMux()
	registerDashboardFederationRoutes(mux)
	get := func(path string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, dashboardRequest(http.MethodGet, path, ""))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
		var out map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}
	summary := get("/api/federation/status?summary=1")
	require.NotEmpty(t, summary["instance_id"])
	require.NotEmpty(t, summary["fingerprint"])
	for _, key := range []string{"peer_grants", "outbox", "remote"} {
		require.NotContains(t, summary, key)
	}
	peers := summary["peers"].([]any)
	require.Len(t, peers, 1)
	linked := peers[0].(map[string]any)
	require.Equal(t, id.ID(), linked["instance_id"])
	require.Equal(t, "laptop", linked["label"])
	require.Equal(t, "unrestricted", linked["level"])
	require.Equal(t, true, linked["trusted"])
	require.Equal(t, false, linked["online"])
	full := get("/api/federation/status")
	require.Len(t, full["peer_grants"], 1)
	require.Contains(t, full, "outbox")
	require.Len(t, full["remote"], 1)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/federation/status?summary=1", nil))
	require.Equal(t, http.StatusUnauthorized, rec.Code)
	rec = httptest.NewRecorder()
	PeerViewHandler(id.ID()).ServeHTTP(rec, dashboardRequest("GET", "/api/federation/status?summary=1", ""))
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "local")
	_, err = db.UntrustFederationPeer(id.ID())
	require.NoError(t, err)
	require.Empty(t, get("/api/federation/status?summary=1")["peers"])
}
