package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/configbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func TestDashboardFederationBundleOffers(t *testing.T) {
	fh := newFedHarness(t)
	fedGrantConfig(t, fh)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://localhost:12345"))
	h := agentd.BuildDashboardHandlerForTest()
	call := func(method, tail string, body any, status int) []byte {
		t.Helper()
		rec := testharness.Serve(h, testharness.JSONRequest(t, method, "/api/federation/"+tail, body))
		require.Equal(t, status, rec.Code, rec.Body.String())
		require.Contains(t, rec.Header().Get("Cache-Control"), "no-store")
		return rec.Body.Bytes()
	}
	raw := fedOfferedConfig(t, "Read the diff.")
	descriptor := fedSendOffer(t, fh.peer, raw)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, descriptor.ID).Status)
	var offers []db.FederationBundleOffer
	require.NoError(t, json.Unmarshal(call("GET", "bundle-offers?direction=in", nil, 200), &offers))
	require.Len(t, offers, 1)
	require.Equal(t, descriptor.ID, offers[0].Descriptor.ID)
	path := "bundle-offers/" + descriptor.ID
	call("POST", path+"/fetch", nil, 200)
	call("POST", path+"/import?peer="+fh.peer.id.ID(), map[string]any{}, 200)
	role, err := db.GetRole("offered-role")
	require.NoError(t, err)
	require.Nil(t, role, "preview must not import")
	call("POST", path+"/import", map[string]any{"only": []string{"roles"}, "apply": true}, 200)
	role, err = db.GetRole("offered-role")
	require.NoError(t, err)
	require.NotNil(t, role)
	stored, err := db.GetFederationBundleOffer("in", fh.peer.id.ID(), descriptor.ID)
	require.NoError(t, err)
	require.Equal(t, "applied", stored.State)
	other := fedSendOffer(t, fh.peer, raw)
	require.Equal(t, proto.AckAccepted, fedAckFor(t, fh.peer, other.ID).Status)
	call("POST", "bundle-offers/"+other.ID+"/decline?peer="+fh.peer.id.ID(), nil, 200)
	var bundle configbundle.Bundle
	require.NoError(t, json.Unmarshal(raw, &bundle))
	call("POST", "offer-config", map[string]any{"peer": "bob", "bundle": bundle, "only": []string{"roles"}}, 200)
	require.NoError(t, json.Unmarshal(call("GET", "bundle-offers?direction=out", nil, 200), &offers))
	require.Len(t, offers, 1)
	// Share-agent requires an explicitly resolvable source; a browser route does not bypass it.
	call("POST", "share-agent", map[string]any{"peer": "bob", "group": "team", "agent": "missing-agent"}, 404)
	call("POST", "profiles", map[string]any{"name": "demo", "definition": map[string]any{"trust_level": "restricted"}}, 200)
	call("POST", "profiles/demo/offer", map[string]string{"peer": "bob"}, 409)
	peer, err := db.GetFederationPeer(fh.peer.id.ID())
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	for _, route := range []struct{ method, tail string }{{"POST", "offer-config"}, {"POST", "share-agent"}, {"GET", "bundle-offers"}, {"POST", path + "/fetch"}, {"POST", path + "/import"}, {"POST", path + "/decline"}, {"POST", "profiles/demo/offer"}} {
		rec := testharness.Serve(agentd.PeerViewHandler(peer.InstanceID), testharness.JSONRequest(t, route.method, "/api/federation/"+route.tail, nil))
		require.Equal(t, 403, rec.Code, rec.Body.String())
	}
	rawMux := http.NewServeMux()
	agentd.RegisterDashboardRoutesForTest(rawMux)
	rec := testharness.Serve(rawMux, testharness.JSONRequest(t, "GET", "/api/federation/bundle-offers", nil))
	require.Equal(t, 403, rec.Code, rec.Body.String())
}
