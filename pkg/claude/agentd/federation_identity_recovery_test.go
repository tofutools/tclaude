package agentd

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestFederationIdentityRecoveryRetiresAuthorityBeforeKeyAndResumes(t *testing.T) {
	setupTestDB(t)
	ResetFederationForTest()
	t.Cleanup(ResetFederationForTest)
	old, err := federationIdentity()
	require.NoError(t, err)
	require.NoError(t, db.IssueModelProxyLease(db.ModelProxyLease{ID: "lease", Peer: "peer", Request: "request", Proxy: "model", IdleSeconds: 3600}))
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`CREATE TRIGGER fail_identity_retirement BEFORE UPDATE ON model_proxy_leases BEGIN SELECT RAISE(ABORT,'retirement unavailable'); END`)
	require.NoError(t, err)
	_, err = recoverLocalIdentity()
	require.ErrorContains(t, err, "retirement unavailable")
	current, err := proto.LoadIdentity(FederationKeyPath())
	require.NoError(t, err)
	require.Equal(t, old.ID(), current.ID(), "failed retirement must not activate replacement")
	_, err = federationIdentity()
	require.ErrorContains(t, err, "recovery is pending")
	intent, err := loadIdentityRecovery()
	require.NoError(t, err)
	require.True(t, intent.Pending)
	_, err = d.Exec(`DROP TRIGGER fail_identity_retirement`)
	require.NoError(t, err)
	// Startup resumes the durable target rather than generating a second key.
	next, err := completeLocalIdentityRecovery(false)
	require.NoError(t, err)
	require.Equal(t, intent.NewID, next.ID())
	lease, err := db.GetModelProxyLease("lease")
	require.NoError(t, err)
	require.True(t, lease.Revoked)
	current, err = federationIdentity()
	require.NoError(t, err)
	require.Equal(t, next.ID(), current.ID())
}

func TestFederationIdentityRecoveryAfterKeyRenameAndLostRotationKeys(t *testing.T) {
	setupTestDB(t)
	ResetFederationForTest()
	t.Cleanup(ResetFederationForTest)
	old, err := federationIdentity()
	require.NoError(t, err)
	next, err := proto.NewIdentity()
	require.NoError(t, err)
	r, err := proto.NewRotation(old, next, "", 1, time.Now(), time.Hour)
	require.NoError(t, err)
	require.NoError(t, saveIdentityJournal(localIdentityRotation{Chain: []proto.Rotation{r}, Pending: true}))
	require.NoError(t, os.Remove(FederationKeyPath()))
	// Explicit recovery can abandon public rotation evidence even when both
	// original private keys are gone.
	replacement, err := recoverLocalIdentity()
	require.NoError(t, err)
	require.NotEqual(t, next.ID(), replacement.ID())
	_, err = os.Stat(identityJournalPath() + ".recovered-" + replacement.ID())
	require.NoError(t, err)
	// Simulate a crash after the replacement rename, before final metadata.
	intent := localIdentityRecovery{NewID: replacement.ID(), NewKey: replacement.Pub, Pending: true}
	require.NoError(t, saveIdentityPublicFile(identityRecoveryPath(), intent))
	ResetFederationForTest()
	_, err = federationIdentity()
	require.ErrorContains(t, err, "recovery is pending")
	resumed, err := completeLocalIdentityRecovery(false)
	require.NoError(t, err)
	require.Equal(t, replacement.ID(), resumed.ID())
}

func TestFederationIdentityModelBlocksFollowExplicitRecovery(t *testing.T) {
	setupTestDB(t)
	old, err := proto.NewIdentity()
	require.NoError(t, err)
	next, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: old.ID(), PubKey: old.Pub, Label: "blocked", TrustLevel: db.FederationTrustUnrestricted}))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{HTTPProxies: map[string]config.HTTPProxyConfig{"model": {
		URL: "https://example.test", Header: "X-Api-Key", ModelPolicy: &config.ModelProxyPolicy{Enabled: true, BlockedPeers: []string{old.ID()}, Models: []string{"test"}, DailyRequests: 10, DailyTokens: 100, PeerDailyRequests: 10, PeerDailyTokens: 100, SessionDailyRequests: 10, SessionDailyTokens: 100, MaxInputTokens: 10, MaxOutputTokens: 10, MaxConcurrent: 1, RequestsPerMinute: 10},
	}}}}))
	_, err = modelProxyPolicy("model")
	require.NoError(t, err)
	require.False(t, fedPeerModelAllows(old.ID(), "model"))
	require.NoError(t, db.RebindFederationIdentity(old.ID(), next.ID(), next.Pub, time.Now()))
	require.True(t, db.FederationPeerUnrestricted(next.ID()))
	require.False(t, fedPeerModelAllows(next.ID(), "model"))
	require.False(t, fedPeerLeasedModelAllows(next.ID(), "model"))
}

func TestFederationIdentityTeleportCapacityFollowsPredecessor(t *testing.T) {
	setupTestDB(t)
	previous := clcommon.Default
	clcommon.Default = &commandRecordingTmux{}
	t.Cleanup(func() { clcommon.Default = previous })
	old, err := proto.NewIdentity()
	require.NoError(t, err)
	next, err := proto.NewIdentity()
	require.NoError(t, err)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: old.ID(), PubKey: old.Pub, Label: "old"}))
	require.NoError(t, db.RecordFederationTeleport(db.FederationTeleport{Direction: "in", Peer: old.ID(), Offer: "offer", State: "uncertain", TargetAgent: "reserved"}, config.TeleportLimits{}))
	require.NoError(t, db.RebindFederationIdentity(old.ID(), next.ID(), next.Pub, time.Now()))
	require.ErrorContains(t, teleportPeerCapacity(next.ID(), 1), "worker limit reached")
}
