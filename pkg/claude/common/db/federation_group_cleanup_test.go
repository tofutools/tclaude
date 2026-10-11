package db

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"testing"
)

func TestDeleteAgentGroupSweepsPeerAndPoolGrantAuthority(t *testing.T) {
	setupTestDB(t)
	target, err := CreateAgentGroup("target", "")
	require.NoError(t, err)
	other, err := CreateAgentGroup("other", "")
	require.NoError(t, err)
	peer, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, TrustFederationPeer(FederationPeer{InstanceID: peer.ID(), PubKey: peer.Pub}))
	pool, err := CreateFederationNodeGroup("fleet")
	require.NoError(t, err)
	for _, scope := range []string{FederationGroupScope(target), FederationGroupScope(other), "", "http_proxy=provider"} {
		g := FederationPeerGrant{Peer: peer.ID(), Slug: "message.direct", Scope: scope}
		require.NoError(t, UpsertFederationPeerGrant(g))
		require.NoError(t, UpsertFederationNodeGroupGrant(pool.ID, g))
	}
	require.NoError(t, DeleteAgentGroup("target"))
	direct, err := ListFederationPeerGrants(peer.ID())
	require.NoError(t, err)
	require.Len(t, direct, 3)
	inherited, err := ListFederationNodeGroupGrants(pool.ID)
	require.NoError(t, err)
	require.Len(t, inherited, 3)
	for _, grants := range [][]FederationPeerGrant{direct, inherited} {
		for _, g := range grants {
			require.NotEqual(t, FederationGroupScope(target), g.Scope)
		}
	}
	require.NoError(t, DeleteAgentGroup("target"))
}
