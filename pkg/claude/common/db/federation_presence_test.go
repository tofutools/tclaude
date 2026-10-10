package db

import (
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestFederationIdentityAwayReturnAndSingleUse(t *testing.T) {
	setupTestDB(t)
	id, _, e := EnsureAgentForConv("source", "test")
	require.NoError(t, e)
	group, e := CreateAgentGroup("home", "")
	require.NoError(t, e)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: group, ConvID: "source", Role: "owner"}))
	require.NoError(t, GrantAgentPermission("source", "human.notify", "test"))
	identity := FederationIdentity{Agent: id, Home: "home", Hops: 1, Proofs: map[string]string{"home": "nonce1"}}
	require.NoError(t, DepartFederationIdentity("source", "home", "peer1", "departure", identity))
	a, e := GetAgent(id)
	require.NoError(t, e)
	require.False(t, a.Active())
	require.True(t, AgentAway(id))
	retired, e := ListRetiredAgents()
	require.NoError(t, e)
	require.Empty(t, retired)
	overrides, e := ListAgentPermissionOverridesForConv("source")
	require.NoError(t, e)
	require.Contains(t, overrides, "human.notify")
	_, e = ReinstateAgentByID(id)
	require.ErrorContains(t, e, "continuation")
	identity.Hops = 3
	identity.Proofs["intermediate"] = "transit"
	returning, e := ReserveFederationIdentity(identity, "home", "peer3", "return")
	require.NoError(t, e)
	require.True(t, returning)
	got, created, e := EnsureAgentForConvWithID("returned", id, "federation")
	require.NoError(t, e)
	require.Equal(t, id, got)
	require.False(t, created)
	a, e = GetAgent(id)
	require.NoError(t, e)
	require.True(t, a.Active())
	require.Equal(t, "returned", a.CurrentConvID)
	groups, e := ListGroupsForConv("returned")
	require.NoError(t, e)
	require.Len(t, groups, 1)
	overrides, e = ListAgentPermissionOverridesForConv("returned")
	require.NoError(t, e)
	require.Contains(t, overrides, "human.notify")
	_, e = ReserveFederationIdentity(identity, "home", "peer3", "replayed")
	require.Error(t, e)
	identity.Proofs["home"] = "nonce2"
	identity.Hops++
	require.NoError(t, DepartFederationIdentity("returned", "home", "peer3", "departure2", identity))
	stale := identity
	stale.Proofs = map[string]string{"home": "nonce1"}
	_, e = ReserveFederationIdentity(stale, "home", "peer3", "stale")
	require.Error(t, e)
	_, e = ReserveFederationIdentity(identity, "home", "peer3", "return2")
	require.NoError(t, e)
}
func TestFederationIdentityCollisionsAndTerminalDelete(t *testing.T) {
	setupTestDB(t)
	id, _, e := EnsureAgentForConv("source", "test")
	require.NoError(t, e)
	identity := FederationIdentity{Agent: id, Home: "home", Hops: 1, Proofs: map[string]string{"home": "nonce"}}
	_, e = ReserveFederationIdentity(identity, "home", "peer", "bad")
	require.ErrorContains(t, e, "unrelated")
	require.NoError(t, DepartFederationIdentity("source", "home", "peer", "out", identity))
	_, e = RetireAgentAuthorizationByConv("source", "human", "done")
	require.NoError(t, e)
	_, e = ReserveFederationIdentity(identity, "home", "peer", "return")
	require.ErrorContains(t, e, "explicitly retired")
	_, e = DeleteAgentByConvID("source")
	require.NoError(t, e)
	_, e = ReserveFederationIdentity(identity, "home", "peer", "return-after-delete")
	require.ErrorContains(t, e, "explicitly retired")
}
func TestFederationIdentityVisitRevokesAuthority(t *testing.T) {
	setupTestDB(t)
	identity := FederationIdentity{Agent: NewAgentID(), Home: "origin", Hops: 1, Proofs: map[string]string{"origin": "homeproof"}}
	home, e := ReserveFederationIdentity(identity, "visit", "origin", "in")
	require.NoError(t, e)
	require.False(t, home)
	_, _, e = EnsureAgentForConvWithID("visitor", identity.Agent, "visit")
	require.NoError(t, e)
	require.NoError(t, GrantAgentPermission("visitor", "human.notify", "local"))
	_, e = RetireAgentAuthorizationAtGeneration("visitor", "system:federation-move", "leaving")
	require.NoError(t, e)
	identity.Proofs["visit"] = "visitproof"
	identity.Hops++
	require.NoError(t, DepartFederationIdentity("visitor", "visit", "next", "out", identity))
	perms, e := ListAgentPermissionOverridesForConv("visitor")
	require.NoError(t, e)
	require.Empty(t, perms)
	_, e = ReserveFederationIdentity(identity, "visit", "next", "back")
	require.NoError(t, e)
	_, _, e = EnsureAgentForConvWithID("visitor2", identity.Agent, "visit")
	require.NoError(t, e)
	cancelled, e := CancelAgentMessageNudge(0, identity.Agent, time.Now(), "test")
	require.NoError(t, e)
	require.False(t, cancelled)
}

func TestFederationIdentityUnlaunchedReturnRestoresHome(t *testing.T) {
	setupTestDB(t)
	id, _, e := EnsureAgentForConv("source", "test")
	require.NoError(t, e)
	group, e := CreateAgentGroup("home", "")
	require.NoError(t, e)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: group, ConvID: "source"}))
	require.NoError(t, GrantAgentPermission("source", "human.notify", "human"))
	identity := FederationIdentity{Agent: id, Home: "home", Hops: 1, Proofs: map[string]string{"home": "nonce"}}
	require.NoError(t, DepartFederationIdentity("source", "home", "first-hop", "out", identity))
	_, e = ReserveFederationIdentity(identity, "home", "third-hop", "return")
	require.NoError(t, e)
	_, _, e = EnsureAgentForConvWithID("failed-launch", id, "federation")
	require.NoError(t, e)
	restored, e := RollbackFederationArrival("failed-launch")
	require.NoError(t, e)
	require.True(t, restored)
	a, e := GetAgent(id)
	require.NoError(t, e)
	require.Equal(t, "source", a.CurrentConvID)
	require.False(t, a.Active())
	p, e := GetAgentFederationPresence(id)
	require.NoError(t, e)
	require.Equal(t, "away", p.State)
	require.Equal(t, "first-hop", p.CurrentInstance)
	perms, e := ListAgentPermissionOverridesForConv("source")
	require.NoError(t, e)
	require.Contains(t, perms, "human.notify")
	groups, e := ListGroupsForConv("source")
	require.NoError(t, e)
	require.Len(t, groups, 1)
	_, e = ReserveFederationIdentity(identity, "home", "third-hop", "retry")
	require.NoError(t, e)
	_, _, e = EnsureAgentForConvWithID("retry-launch", id, "federation")
	require.NoError(t, e)
	_, e = ReserveFederationIdentity(identity, "home", "third-hop", "replay")
	require.Error(t, e)
}

func TestFederationIdentityPausedBackupAndIndependentClone(t *testing.T) {
	setupTestDB(t)
	identity := FederationIdentity{Agent: NewAgentID(), Home: "origin", Hops: 1, Proofs: map[string]string{"origin": "homeproof"}}
	_, e := ReserveFederationIdentity(identity, "visit", "origin", "in")
	require.NoError(t, e)
	_, _, e = EnsureAgentForConvWithID("visitor", identity.Agent, "visit")
	require.NoError(t, e)
	require.NoError(t, GrantAgentPermission("visitor", "human.notify", "local"))
	clone, e := RemintFederationClone(identity.Agent, "visitor")
	require.NoError(t, e)
	require.NotEqual(t, identity.Agent, clone)
	again, e := RemintFederationClone(identity.Agent, "")
	require.NoError(t, e)
	require.Equal(t, clone, again)
	resolved, e := AgentIDForConv("visitor")
	require.NoError(t, e)
	require.Equal(t, clone, resolved)
	perms, e := ListAgentPermissionOverridesForConv("visitor")
	require.NoError(t, e)
	require.Contains(t, perms, "human.notify")
	_, e = ReserveFederationIdentity(identity, "visit", "origin", "replayed")
	require.ErrorContains(t, e, "explicitly retired")
	home, _, e := EnsureAgentForConv("homebackup", "test")
	require.NoError(t, e)
	backup := FederationIdentity{Agent: home, Home: "origin", Hops: 1, Proofs: map[string]string{"origin": "backupnonce"}}
	require.NoError(t, DepartFederationIdentity("homebackup", "origin", "remote", "backupout", backup))
	require.NoError(t, RestoreFederationBackup(home, "backupout", "origin"))
	a, e := GetAgent(home)
	require.NoError(t, e)
	require.True(t, a.Active())
	_, e = ReserveFederationIdentity(backup, "origin", "remote", "late-return")
	require.Error(t, e)
}

func TestFederationIdentityVisitorDepartureIsAtomicAndDefaultsReplace(t *testing.T) {
	setupTestDB(t)
	identity := FederationIdentity{Agent: NewAgentID(), Home: "origin", Hops: 1, Proofs: map[string]string{"origin": "home-proof"}}
	_, e := ReserveFederationIdentity(identity, "visit", "origin", "first")
	require.NoError(t, e)
	require.NoError(t, ReplaceFederationArrivalWorkerDefaults(identity.Agent, "first", &FederationWorkerDefaults{Peer: "origin", ProfileName: "first"}))
	_, _, e = EnsureAgentForConvWithID("visitor", identity.Agent, "visit")
	require.NoError(t, e)
	require.NoError(t, GrantAgentPermission("visitor", "human.notify", "local"))
	_, e = RetireFederationVisitor("visitor", "visit", "origin", "return", identity)
	require.ErrorContains(t, e, "continuation")
	a, e := GetAgent(identity.Agent)
	require.NoError(t, e)
	require.True(t, a.Active())
	perms, e := ListAgentPermissionOverridesForConv("visitor")
	require.NoError(t, e)
	require.Contains(t, perms, "human.notify")
	identity.Proofs["visit"] = "visit-proof"
	identity.Hops++
	_, e = RetireFederationVisitor("visitor", "visit", "origin", "return", identity)
	require.NoError(t, e)
	require.True(t, AgentAway(identity.Agent))
	perms, e = ListAgentPermissionOverridesForConv("visitor")
	require.NoError(t, e)
	require.Empty(t, perms)
	_, e = ReserveFederationIdentity(identity, "visit", "origin", "second")
	require.NoError(t, e)
	require.NoError(t, ReplaceFederationArrivalWorkerDefaults(identity.Agent, "second", &FederationWorkerDefaults{Peer: "origin", ProfileName: "second"}))
	defaults, e := GetFederationWorkerDefaults(identity.Agent)
	require.NoError(t, e)
	require.Equal(t, "second", defaults.ProfileName)
	require.Error(t, ReplaceFederationArrivalWorkerDefaults(identity.Agent, "wrong", nil))
	_, _, e = EnsureAgentForConvWithID("visitor2", identity.Agent, "visit")
	require.NoError(t, e)
}

func TestFederationIdentityBackupReturnRetainsVisitContinuation(t *testing.T) {
	setupTestDB(t)
	id, _, e := EnsureAgentForConv("home-conv", "test")
	require.NoError(t, e)
	identity := FederationIdentity{Agent: id, Home: "home", Hops: 1, Proofs: map[string]string{"home": "departure"}}
	require.NoError(t, DepartFederationIdentity("home-conv", "home", "visit", "offer", identity))
	require.Error(t, AcceptFederationBackupReturnProof(id, "wrong", "visit", "visit-proof"))
	require.NoError(t, AcceptFederationBackupReturnProof(id, "offer", "visit", "visit-proof"))
	require.NoError(t, RestoreFederationBackup(id, "offer", "home"))
	p, e := GetAgentFederationPresence(id)
	require.NoError(t, e)
	require.Equal(t, "visit-proof", p.Transfer.Proofs["visit"])
	require.NoError(t, AcceptFederationBackupReturnProof(id, "offer", "visit", "visit-proof"))
	identity = p.Transfer
	identity.Proofs["home"] = "next-departure"
	identity.Hops++
	require.NoError(t, DepartFederationIdentity("home-conv", "home", "visit", "next-offer", identity))
	require.Error(t, AcceptFederationBackupReturnProof(id, "offer", "visit", "late"))
}
