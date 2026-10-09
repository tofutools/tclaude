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
