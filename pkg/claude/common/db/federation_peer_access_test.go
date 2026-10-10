package db

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"testing"
	"time"
)

func TestPeerAccessGrantLifetimeUnionAndRevoke(t *testing.T) {
	setupTestDB(t)
	id, err := proto.NewIdentity()
	require.NoError(t, err)
	peer := id.ID()
	require.NoError(t, TrustFederationPeer(FederationPeer{InstanceID: peer, PubKey: id.Pub}))
	known := []string{"groups.roster.read", "message.direct", "groups.members.spawn"}
	require.NoError(t, UpsertFederationPeerGrant(FederationPeerGrant{Peer: peer, Slug: "groups.roster.read"}))
	expiry := time.Now().Add(time.Minute)
	policy := FederationSpawnPolicy{Profile: "safe", MaxLive: 3}
	require.NoError(t, UpsertFederationPeerGrant(FederationPeerGrant{Peer: peer, Slug: "groups.members.spawn", SpawnPolicy: policy, ExpiresAt: &expiry}))
	r := FederationPeerAccessRequest{ID: "approval", Peer: peer, Slug: "groups.members.spawn", GrantTTLSeconds: 7200}
	require.NoError(t, UpsertFederationPeerAccessRequest(r))
	approved, err := ApproveFederationPeerAccessRequest(&r, known, known)
	require.NoError(t, err)
	require.True(t, approved)
	grants, err := ListFederationPeerGrants(peer)
	require.NoError(t, err)
	require.Len(t, grants, 2)
	require.Equal(t, policy, grants[0].SpawnPolicy)
	require.NotNil(t, grants[0].ExpiresAt)
	require.True(t, grants[0].ExpiresAt.After(expiry))
	r.GrantTTLSeconds = 0
	approved, err = ApproveFederationPeerAccessRequest(&r, known, known)
	require.NoError(t, err)
	require.True(t, approved)
	grants, err = ListFederationPeerGrants(peer)
	require.NoError(t, err)
	require.Nil(t, grants[0].ExpiresAt)
	require.Equal(t, policy, grants[0].SpawnPolicy)
	r.GrantTTLSeconds = 1
	approved, err = ApproveFederationPeerAccessRequest(&r, known, known)
	require.NoError(t, err)
	require.True(t, approved)
	require.Nil(t, r.ExpiresAt, "approval cannot shorten permanent authority")
	_, err = UntrustFederationPeer(peer)
	require.NoError(t, err)
	r.Slug = "message.direct"
	approved, err = ApproveFederationPeerAccessRequest(&r, known, known)
	require.NoError(t, err)
	require.False(t, approved, "revoked trust cannot create an orphan grant")
	require.NoError(t, TrustFederationPeer(FederationPeer{InstanceID: peer, PubKey: id.Pub}))
	grants, err = ListFederationPeerGrants(peer)
	require.NoError(t, err)
	require.Empty(t, grants)
}
