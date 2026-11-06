package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// Pins: a local agent whose title has the member@peer shape stays local, and
// a peer operator's view metadata names its real trust and grants.
func TestPeerMemberVerbPins(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	f.HaveGroup("crew")
	f.HaveAliveSession(moveSourceConv, "pin-local", "pin-local-pane", testutil.CanonicalTempDir(t))
	f.HaveConvWithTitle(moveSourceConv, "worker@bob")
	f.HaveMember("crew", moveSourceConv)
	rec := fedHuman(t, f, http.MethodPost, "/v1/agent/worker@bob/stop", nil)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), moveSourceConv, "a local title wins over a peer label")

	inst := fh.peer.id.ID()
	rec = fedHuman(t, f, "POST", "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersStop, "scope": "group=crew"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	meta := func() map[string]any {
		t.Helper()
		out := testharness.Serve(agentd.PeerViewHandler(inst), testharness.JSONRequest(t, "GET", "/api/groups", nil))
		require.Equal(t, 200, out.Code, out.Body.String())
		var body struct {
			PeerView map[string]any `json:"peer_view"`
		}
		require.NoError(t, json.Unmarshal(out.Body.Bytes(), &body))
		return body.PeerView
	}
	m := meta()
	require.Equal(t, "restricted", m["level"])
	require.Contains(t, m["groups"], "crew")
	peer, err := db.GetFederationPeer(inst)
	require.NoError(t, err)
	peer.TrustLevel = db.FederationTrustUnrestricted
	require.NoError(t, db.TrustFederationPeer(*peer))
	m = meta()
	require.Equal(t, "unrestricted", m["level"])
	require.Nil(t, m["groups"], "unrestricted trust holds every grant; none is listed")
}
