package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardFederationHealthPolicy(t *testing.T) {
	fh := newFedHarness(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	h := agentd.BuildDashboardHandlerForTest()
	read := func(method, path string, body any) config.FederationHealthPolicy {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, path, body))
		require.Equal(t, 200, rec.Code, rec.Body.String())
		require.Equal(t, "private, no-store", rec.Header().Get("Cache-Control"))
		var p config.FederationHealthPolicy
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
		return p
	}
	p := read("GET", "/api/federation/nodes/health?peer=", nil)
	require.NotNil(t, p.Presence)
	require.True(t, *p.Presence)
	require.Equal(t, 15, p.DebounceSeconds, "the effective policy fills in built-in defaults")

	// A peer's own policy replaces, and the defaults stay as they were.
	p = read("POST", "/api/federation/nodes/health?peer=bob", map[string]any{"presence": false, "resources": true, "disk_free_percent": 5})
	require.False(t, *p.Presence)
	require.True(t, p.Resources)
	require.Equal(t, 5.0, p.DiskFreePercent)
	require.Equal(t, 10.0, p.RAMFreePercent)
	require.True(t, read("GET", "/api/federation/nodes/health?peer=bob", nil).Resources)
	require.False(t, read("GET", "/api/federation/nodes/health?peer=", nil).Resources)

	rec := testharness.Serve(h, testharness.JSONRequest(t, "POST", "/api/federation/nodes/health?peer=", map[string]any{"failure_count": 300}))
	require.Equal(t, 400, rec.Code, rec.Body.String())

	// Peer views never reach the policy, even for an unrestricted peer.
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	for _, method := range []string{"GET", "POST"} {
		rec := testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, method, "/api/federation/nodes/health?peer=", map[string]any{}))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
}
