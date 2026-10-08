package db

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func enrollmentFixture(t *testing.T, uses int) (*proto.Identity, *FederationNodeProfile, *proto.EnrollmentToken, string) {
	t.Helper()
	master, e := proto.NewIdentity()
	require.NoError(t, e)
	p := &FederationNodeProfile{Name: "rigs", Definition: FederationNodeProfileSpec{TrustLevel: FederationTrustRestricted, PeerGrants: []FederationPeerGrant{{Slug: "config.offer"}}}}
	require.NoError(t, SaveFederationNodeProfile(p))
	bearer, tok, e := proto.NewEnrollmentToken(master, p.ID, p.Name, p.Revision, "restricted", time.Now().Add(time.Hour))
	require.NoError(t, e)
	require.NoError(t, SaveFederationEnrollmentToken(tok, uses))
	return master, p, tok, bearer
}
func TestFederationEnrollmentAtomicBindingAndTombstone(t *testing.T) {
	setupTestDB(t)
	master, _, tok, bearer := enrollmentFixture(t, 1)
	node, e := proto.NewIdentity()
	require.NoError(t, e)
	created, e := RedeemFederationEnrollment(tok, node.Pub, master.Pub)
	require.NoError(t, e)
	require.True(t, created)
	grants, e := ListFederationPeerGrants(node.ID())
	require.NoError(t, e)
	require.Len(t, grants, 1)
	peer, e := GetFederationPeer(node.ID())
	require.NoError(t, e)
	peer.TrustLevel = FederationTrustUnrestricted
	require.NoError(t, TrustFederationPeer(*peer))
	require.NoError(t, RevokeFederationEnrollmentToken(tok.Claims.TokenID))
	created, e = RedeemFederationEnrollment(tok, node.Pub, master.Pub)
	require.NoError(t, e)
	require.False(t, created)
	peer, e = GetFederationPeer(node.ID())
	require.NoError(t, e)
	require.Equal(t, FederationTrustUnrestricted, peer.TrustLevel)
	other, e := proto.NewIdentity()
	require.NoError(t, e)
	_, e = RedeemFederationEnrollment(tok, other.Pub, master.Pub)
	require.ErrorIs(t, e, ErrEnrollmentRefused)
	tokens, e := ListFederationEnrollmentTokens()
	require.NoError(t, e)
	require.Equal(t, 1, tokens[0].Used)
	require.NotContains(t, tokens[0].Public, strings.Split(bearer, ".")[3])
	_, e = UntrustFederationPeer(node.ID())
	require.NoError(t, e)
	_, e = RedeemFederationEnrollment(tok, node.Pub, master.Pub)
	require.ErrorIs(t, e, ErrEnrollmentRefused)
	require.NoError(t, TrustFederationPeer(*peer))
	_, e = RedeemFederationEnrollment(tok, node.Pub, master.Pub)
	require.ErrorIs(t, e, ErrEnrollmentRefused)
}
func TestFederationEnrollmentProfileChangeAndFailedApplyConsumeNothing(t *testing.T) {
	setupTestDB(t)
	master, p, tok, _ := enrollmentFixture(t, 2)
	node, e := proto.NewIdentity()
	require.NoError(t, e)
	p.Definition.Pools = []string{"missing"}
	require.NoError(t, SaveFederationNodeProfile(p))
	_, e = RedeemFederationEnrollment(tok, node.Pub, master.Pub)
	require.Error(t, e)
	// A current revision with an unavailable pool still rolls back use and trust.
	_, current, e := proto.NewEnrollmentToken(master, p.ID, p.Name, p.Revision, "restricted", time.Now().Add(time.Hour))
	require.NoError(t, e)
	require.NoError(t, SaveFederationEnrollmentToken(current, 1))
	_, e = RedeemFederationEnrollment(current, node.Pub, master.Pub)
	require.Error(t, e)
	tokens, e := ListFederationEnrollmentTokens()
	require.NoError(t, e)
	for _, v := range tokens {
		require.Zero(t, v.Used)
	}
	peer, e := GetFederationPeer(node.ID())
	require.NoError(t, e)
	require.Nil(t, peer)
}
func TestFederationEnrollmentConcurrentUseLimit(t *testing.T) {
	setupTestDB(t)
	master, _, tok, _ := enrollmentFixture(t, 1)
	var wg sync.WaitGroup
	results := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		id, e := proto.NewIdentity()
		require.NoError(t, e)
		wg.Add(1)
		go func() {
			defer wg.Done()
			created, _ := RedeemFederationEnrollment(tok, id.Pub, master.Pub)
			results <- created
		}()
	}
	wg.Wait()
	close(results)
	n := 0
	for ok := range results {
		if ok {
			n++
		}
	}
	require.Equal(t, 1, n)
	tokens, e := ListFederationEnrollmentTokens()
	require.NoError(t, e)
	require.Equal(t, 1, tokens[0].Used)
}
func TestFederationEnrollmentNodeConsentAndRetry(t *testing.T) {
	setupTestDB(t)
	master, e := proto.NewIdentity()
	require.NoError(t, e)
	node, e := proto.NewIdentity()
	require.NoError(t, e)
	_, tok, e := proto.NewEnrollmentToken(master, "p", "rigs", 1, "restricted", time.Now().Add(time.Hour))
	require.NoError(t, e)
	preview, e := FederationEnrollmentNodePreview(tok)
	require.NoError(t, e)
	require.Error(t, CompleteFederationEnrollment(tok, node.Pub, "wrong"))
	require.NoError(t, CompleteFederationEnrollment(tok, node.Pub, preview))
	peer, e := GetFederationPeer(master.ID())
	require.NoError(t, e)
	peer.TrustLevel = "unrestricted"
	require.NoError(t, TrustFederationPeer(*peer))
	require.Error(t, CompleteFederationEnrollment(tok, node.Pub, preview), "concurrent manual change invalidates consent")
	preview, e = FederationEnrollmentNodePreview(tok)
	require.NoError(t, e)
	require.NoError(t, CompleteFederationEnrollment(tok, node.Pub, preview))
	peer, e = GetFederationPeer(master.ID())
	require.NoError(t, e)
	require.Equal(t, "unrestricted", peer.TrustLevel)
	_, e = UntrustFederationPeer(master.ID())
	require.NoError(t, e)
	_, e = FederationEnrollmentNodePreview(tok)
	require.ErrorIs(t, e, ErrEnrollmentRefused)
}
