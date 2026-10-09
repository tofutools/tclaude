package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type dashFedLink struct {
	Peer      string   `json:"peer"`
	Label     string   `json:"label"`
	Level     string   `json:"level"`
	Kind      string   `json:"kind"`
	Direction string   `json:"direction"`
	Slugs     []string `json:"slugs"`
	Pool      string   `json:"pool"`
}

// The Groups tab's linked-group marker reads each local group's federation
// links from the snapshot: group-scoped direct and pool peer grants, but not
// unscoped (all-groups) grants, which would mark every group.
func TestDashboardSnapshot_GroupFederationLinks(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	builders := f.HaveGroup("builders")
	f.HaveGroup("quiet")

	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))
	scope := db.FederationGroupScope(builders.ID)
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: bob.ID(), Slug: agentd.PermMessageDirect, Scope: scope}))
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: bob.ID(), Slug: "groups.roster.read", Scope: scope}))
	require.NoError(t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: bob.ID(), Slug: "node.read", Scope: ""}))
	pool, err := db.CreateFederationNodeGroup("rigs")
	require.NoError(t, err)
	require.NoError(t, db.AddFederationNodeGroupPeer(pool.ID, bob.ID()))
	require.NoError(t, db.UpsertFederationNodeGroupGrant(pool.ID, db.FederationPeerGrant{Slug: "routes.consume", Scope: scope}))

	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var snap struct {
		Groups []struct {
			Name            string        `json:"name"`
			FederationLinks []dashFedLink `json:"federation_links"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	links := map[string][]dashFedLink{}
	for _, g := range snap.Groups {
		links[g.Name] = g.FederationLinks
	}
	require.Empty(t, links["quiet"], "an unscoped grant must not mark every group")
	require.Equal(t, []dashFedLink{
		{Peer: bob.ID(), Label: "bob", Level: db.FederationTrustRestricted, Kind: "grant", Direction: "in", Slugs: []string{"groups.roster.read", agentd.PermMessageDirect}},
		{Peer: bob.ID(), Label: "bob", Level: db.FederationTrustRestricted, Kind: "grant", Direction: "in", Slugs: []string{"routes.consume"}, Pool: "rigs"},
	}, links["builders"])
}

// A route mirror links the consuming group once, however many members opened
// the same peer route, and peers never see any group's federation links — not
// even an unrestricted one served the full snapshot.
func TestDashboardSnapshot_GroupFederationLinksRoutesAndPeerExclusion(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	builders := f.HaveGroup("builders")
	var err error
	convs := []string{"f2000000-0000-4000-8000-000000000001", "f2000000-0000-4000-8000-000000000002"}
	for _, c := range convs {
		f.HaveMember("builders", c)
	}
	builders, err = db.GetAgentGroupByID(builders.ID) // membership bumped the route generation
	require.NoError(t, err)

	bob, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: bob.ID(), PubKey: bob.Pub, Label: "bob"}))
	for i, c := range convs {
		agentID, err := db.AgentIDForConv(c)
		require.NoError(t, err)
		_, err = db.CreateFederationRouteMirror(builders.ID, agentID, c, "gen1", builders.RouteGeneration, "bob-api-"+string(rune('a'+i)),
			db.FederationRouteMirror{Peer: bob.ID(), RemoteRoute: "rt_api"})
		require.NoError(t, err)
	}

	read := func(h http.Handler) []dashFedLink {
		rec := testharness.Serve(h, testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var snap struct {
			Groups []struct {
				Name            string        `json:"name"`
				FederationLinks []dashFedLink `json:"federation_links"`
			} `json:"groups"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
		for _, g := range snap.Groups {
			if g.Name == "builders" {
				return g.FederationLinks
			}
		}
		t.Fatalf("builders missing from snapshot: %s", rec.Body.String())
		return nil
	}
	links := read(agentd.BuildDashboardHandlerForTest())
	require.Len(t, links, 1, "two mirrors of one remote route link the group once")
	require.Equal(t, "route", links[0].Kind)
	require.Equal(t, "out", links[0].Direction)

	carol, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: carol.ID(), PubKey: carol.Pub, Label: "carol", TrustLevel: db.FederationTrustUnrestricted}))
	require.True(t, db.FederationPeerUnrestricted(carol.ID()))
	require.Empty(t, read(agentd.PeerViewHandler(carol.ID())), "an unrestricted peer's snapshot omits this node's trust links")
}
