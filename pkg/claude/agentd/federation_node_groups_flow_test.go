package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func fedPool(t *testing.T, fh *fedHarness, name string, member bool) *db.FederationNodeGroup {
	t.Helper()
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/nodes/groups", map[string]any{"name": name})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	if member {
		rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/nodes/groups/"+name+"/members", map[string]any{"peer": "bob"})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	g, err := db.GetFederationNodeGroup(name)
	require.NoError(t, err)
	return g
}
func TestFederation_NodeGroupLiveGrantAdmissionAndProvenance(t *testing.T) {
	fh := newFedHarness(t)
	f, p := fh.f, fh.peer
	fedPool(t, fh, "rigs", false)
	rec := fedHuman(t, f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "group:rigs", "slug": "config.offer"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	denied := fedSendOffer(t, p, fedOfferedConfig(t, "Before membership"))
	require.Equal(t, proto.AckRefused, fedAckFor(t, p, denied.ID).Status)
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/nodes/groups/rigs/members", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code)
	offered := fedSendOffer(t, p, fedOfferedConfig(t, "After membership"))
	require.Equal(t, proto.AckAccepted, fedAckFor(t, p, offered.ID).Status)
	rec = fedHuman(t, f, http.MethodGet, "/v1/federation/grants?peer=bob", nil)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"pool_name":"rigs"`)
	rec = fedHuman(t, f, http.MethodDelete, "/v1/federation/nodes/groups/rigs/members", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code)
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/bundle-offers/"+offered.ID+"/import", nil)
	require.Equal(t, 403, rec.Code, rec.Body.String())
	denied = fedSendOffer(t, p, fedOfferedConfig(t, "After removal"))
	require.Equal(t, proto.AckRefused, fedAckFor(t, p, denied.ID).Status)
}
func TestFederation_NodeGroupPeerScopeTracksMembershipAndIdentity(t *testing.T) {
	fh := newFedHarness(t)
	f := fh.f
	g := fedPool(t, fh, "rigs", true)
	const source = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	f.HaveGroup("source")
	f.HaveAliveSession(source, "pool-agent", "pool-agent-pane", testutil.CanonicalTempDir(t))
	f.HaveMember("source", source)
	rec := fedHuman(t, f, http.MethodPost, "/v1/permissions/grant", map[string]any{"target": source, "slug": "agent.share", "scope": map[string]any{"peer": []string{"group:rigs/destination"}}})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	call := func(group string) *httptest.ResponseRecorder {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/share-agent", map[string]any{"peer": "bob", "agent": "self", "group": group}), source))
	}
	require.Equal(t, 200, call("destination").Code)
	require.Equal(t, 403, call("other").Code)
	rec = fedHuman(t, f, http.MethodDelete, "/v1/federation/nodes/groups/rigs/members", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code)
	require.Equal(t, 403, call("destination").Code)
	rec = fedHuman(t, f, http.MethodPost, "/v1/federation/nodes/groups/rigs/members", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code)
	require.Equal(t, 200, call("destination").Code)
	rec = fedHuman(t, f, http.MethodDelete, "/v1/federation/nodes/groups/rigs", nil)
	require.Equal(t, 200, rec.Code)
	replacement := fedPool(t, fh, "rigs", true)
	require.NotEqual(t, g.ID, replacement.ID)
	require.Equal(t, 403, call("destination").Code)
}
func TestFederation_NodeGroupManagementIsOperatorOnly(t *testing.T) {
	fh := newFedHarness(t)
	const caller = "019fe740-43a4-7023-b8ae-1ee64459f2a1"
	rec := testharness.Serve(fh.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/federation/nodes/groups", map[string]any{"name": "rigs"}), caller))
	require.Equal(t, 403, rec.Code)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/nodes/groups", map[string]any{"name": "group:nested"})
	require.Equal(t, 400, rec.Code)
}

func TestFederation_NodeGroupAwayRevocationCannotReviveOldAnswer(t *testing.T) {
	fh := newFedHarness(t)
	fedPool(t, fh, "rigs", true)
	fh.f.HaveConvWithTitle("away-requester", "requester")
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": "group:rigs", "slug": agentd.PermApprovalsAnswer})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	setFedAway(t, fh, time.Time{})
	id := strings.Repeat("c", 32)
	_, cleanup := agentd.StartFederationAwayApprovalForTest(id, "away-requester", 8*time.Second)
	t.Cleanup(cleanup)
	_, epoch := fedAwayTicket(t, fh.peer, id)
	rec = fedHuman(t, fh.f, http.MethodDelete, "/v1/federation/nodes/groups/rigs/members", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code)
	rec = fedHuman(t, fh.f, http.MethodPost, "/v1/federation/nodes/groups/rigs/members", map[string]any{"peer": "bob"})
	require.Equal(t, 200, rec.Code)
	stale := sendFedAwayDecision(t, fh.peer, id, epoch, "approve")
	awaitFedAwayAck(t, fh.peer, stale.ID, proto.AckRefused)
}

func TestFederation_NodeGroupSpawnPolicyPrecedence(t *testing.T) {
	fh := newFedHarness(t)
	fh.f.HaveGroup("team")
	fedPool(t, fh, "one", true)
	fedPool(t, fh, "two", true)
	grant := func(peer, scope string, cap int) {
		rec := fedHuman(t, fh.f, http.MethodPost, "/v1/federation/grants", map[string]any{"peer": peer, "slug": agentd.PermGroupsMembersSpawn, "scope": scope, "spawn_policy": map[string]any{"max_live": cap}})
		require.Equal(t, 200, rec.Code, rec.Body.String())
	}
	spawn := func(want bool) {
		fedEventually(t, "spawn capability follows policy precedence", func() bool {
			catalogs := fh.peer.envelopes(proto.KindCatalog)
			if len(catalogs) == 0 {
				return false
			}
			var cat proto.CatalogPayload
			if catalogs[len(catalogs)-1].DecodePayload(&cat) != nil {
				return false
			}
			for _, g := range cat.Groups {
				if g.Name != "team" {
					continue
				}
				for _, cap := range g.Caps {
					if cap == proto.CapSpawn {
						return want
					}
				}
			}
			return !want
		})
	}
	grant("group:one", "group=team", 1)
	grant("group:two", "group=team", 2)
	spawn(false) // Unequal equally specific pool policies fail closed.
	grant("bob", "", 3)
	spawn(false) // A broad direct policy cannot beat a specific pool policy.
	grant("bob", "group=team", 4)
	spawn(true) // An equally specific direct policy resolves the conflict.
	rec := fedHuman(t, fh.f, http.MethodDelete, "/v1/federation/grants", map[string]any{"peer": "bob", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	spawn(false)
	rec = fedHuman(t, fh.f, http.MethodDelete, "/v1/federation/grants", map[string]any{"peer": "group:two", "slug": agentd.PermGroupsMembersSpawn, "scope": "group=team"})
	require.Equal(t, 200, rec.Code, rec.Body.String())
	grant("group:two", "", 5)
	spawn(true) // A unique specific pool policy beats broader matches.
}
