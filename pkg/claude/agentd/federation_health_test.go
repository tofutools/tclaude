package agentd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func healthFixture(t *testing.T) (*fedRuntime, string) {
	t.Helper()
	t.Setenv("HOME", testutil.CanonicalTempDir(t))
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	id, e := proto.NewIdentity()
	require.NoError(t, e)
	require.NoError(t, db.TrustFederationPeer(db.FederationPeer{InstanceID: id.ID(), PubKey: id.Pub, Label: "remote"}))
	require.NoError(t, config.Save(&config.Config{Federation: &config.FederationConfig{Health: &config.FederationHealthConfig{Defaults: config.FederationHealthPolicy{Resources: true, Failures: true, DebounceSeconds: 1, MemorySeconds: 2}}}}))
	return &fedRuntime{online: map[string]bool{id.ID(): true}, inLimiter: map[string][]time.Time{}}, id.ID()
}
func healthMessages(t *testing.T) []*db.HumanMessage {
	t.Helper()
	m, e := db.ListHumanMessages()
	require.NoError(t, e)
	return m
}
func TestFleetHealthPresenceDebounceAndRecovery(t *testing.T) {
	rt, peer := healthFixture(t)
	now := time.Now()
	rt.observeFleetPresence(map[string]bool{peer: true}, now)
	rt.flushFleetHealth(now.Add(time.Hour))
	require.Empty(t, healthMessages(t), "initial directory is a baseline")
	rt.observeFleetPresence(map[string]bool{}, now)
	rt.flushFleetHealth(now.Add(500 * time.Millisecond))
	require.Empty(t, healthMessages(t))
	rt.observeFleetPresence(map[string]bool{peer: true}, now.Add(600*time.Millisecond))
	rt.flushFleetHealth(now.Add(2 * time.Second))
	require.Empty(t, healthMessages(t), "short flap suppressed")
	rt.observeFleetPresence(map[string]bool{}, now.Add(3*time.Second))
	rt.flushFleetHealth(now.Add(5 * time.Second))
	require.Len(t, healthMessages(t), 1)
	rt.flushFleetHealth(now.Add(6 * time.Second))
	require.Len(t, healthMessages(t), 1)
	rt.observeFleetPresence(map[string]bool{peer: true}, now.Add(7*time.Second))
	rt.flushFleetHealth(now.Add(9 * time.Second))
	require.Len(t, healthMessages(t), 2)
	require.Contains(t, healthMessages(t)[0].Body, "back online")
}
func TestFleetHealthFreshResourcesSustainAndUnknown(t *testing.T) {
	rt, peer := healthFixture(t)
	now := time.Now()
	cat := &proto.CatalogPayload{Node: &proto.NodeMetadata{Resources: proto.NodeResources{Status: "current", ObservedAt: &now, DataDisk: &proto.NodeDisk{TotalBytes: 100, AvailableBytes: 5}, RAM: &proto.NodeRAM{TotalBytes: 100, AvailableBytes: 5}}}}
	rt.observeFleetNode(peer, cat, now)
	rt.flushFleetHealth(now.Add(2 * time.Second))
	require.Len(t, healthMessages(t), 1)
	require.Contains(t, healthMessages(t)[0].Subject, "disk")
	// Replayed readings cannot manufacture a sustained memory interval.
	rt.observeFleetNode(peer, cat, now.Add(4*time.Second))
	rt.flushFleetHealth(now.Add(5 * time.Second))
	require.Len(t, healthMessages(t), 1)
	later := now.Add(3 * time.Second)
	cat.Node.Resources.ObservedAt = &later
	rt.observeFleetNode(peer, cat, later)
	rt.flushFleetHealth(later.Add(2 * time.Second))
	require.Len(t, healthMessages(t), 2)
	require.Contains(t, healthMessages(t)[0].Subject, "memory")
	// Unknown observations neither recover nor create resource notices.
	cat.Node = nil
	rt.observeFleetNode(peer, cat, later.Add(time.Second))
	rt.flushFleetHealth(later.Add(3 * time.Second))
	require.Len(t, healthMessages(t), 2)
	later = later.Add(5 * time.Second)
	cat.Node = &proto.NodeMetadata{Resources: proto.NodeResources{Status: "current", ObservedAt: &later, DataDisk: &proto.NodeDisk{TotalBytes: 100, AvailableBytes: 50}, RAM: &proto.NodeRAM{TotalBytes: 100, AvailableBytes: 50}}}
	rt.observeFleetNode(peer, cat, later)
	rt.flushFleetHealth(later.Add(2 * time.Second))
	require.Len(t, healthMessages(t), 4)
}
func TestFleetHealthFailureDedupCooldownAndDisabledDefaults(t *testing.T) {
	rt, peer := healthFixture(t)
	now := time.Now()
	for i := 0; i < 10; i++ {
		rt.observeFleetFailure(peer, "same-attempt", now)
	}
	rt.flushFleetHealth(now.Add(2 * time.Second))
	require.Empty(t, healthMessages(t))
	rt.observeFleetFailure(peer, "second", now)
	rt.observeFleetFailure(peer, "third", now)
	rt.flushFleetHealth(now.Add(2 * time.Second))
	require.Len(t, healthMessages(t), 1)
	rt.observeFleetFailure(peer, "fourth", now.Add(3*time.Second))
	rt.flushFleetHealth(now.Add(5 * time.Second))
	require.Len(t, healthMessages(t), 1)
	require.NoError(t, config.Save(&config.Config{Federation: &config.FederationConfig{}}))
	rt.observeFleetFailure(peer, "fifth", now.Add(time.Hour))
	rt.flushFleetHealth(now.Add(time.Hour + 2*time.Second))
	require.Len(t, healthMessages(t), 1)
	_, err := db.UntrustFederationPeer(peer)
	require.NoError(t, err)
	publishFleetEvent(peer, "offline", "must not notify", true)
	require.Len(t, healthMessages(t), 1)
}

func TestFleetHealthSpawnTelemetryCorrelatesAndNeverSettles(t *testing.T) {
	rt, peer := healthFixture(t)
	now := time.Now()
	require.NoError(t, db.InsertFederationOutbox(db.FederationOutboxRow{Sealed: []byte{}, EnvelopeID: "request", Kind: proto.KindSpawnReq, ToInstance: peer, ExpiresAt: now.Add(time.Hour)}))
	p, e := db.GetFederationPeer(peer)
	require.NoError(t, e)
	receive := func(request, attempt string) {
		raw, e := json.Marshal(spawnAttemptFailure{Request: request, Attempt: attempt})
		require.NoError(t, e)
		rt.acceptSpawnAttemptFailure(p, &proto.Envelope{Payload: raw})
	}
	receive("unknown", "agt_one")
	receive("request", "bad-control\n")
	receive("request", "agt_one")
	receive("request", "agt_one")
	rt.flushFleetHealth(now.Add(2 * time.Second))
	require.Empty(t, healthMessages(t))
	receive("request", "agt_two")
	receive("request", "agt_three")
	rt.flushFleetHealth(time.Now().Add(2 * time.Second))
	require.Len(t, healthMessages(t), 1)
	row, e := db.GetFederationOutbox("request")
	require.NoError(t, e)
	require.Equal(t, db.FedOutboxQueued, row.State)
}

func TestFleetHealthMemoryFlapCancelsRecovery(t *testing.T) {
	rt, peer := healthFixture(t)
	now := time.Now()
	sample := func(at time.Time, free uint64) {
		rt.observeFleetNode(peer, &proto.CatalogPayload{Node: &proto.NodeMetadata{Resources: proto.NodeResources{Status: "current", ObservedAt: &at, RAM: &proto.NodeRAM{TotalBytes: 100, AvailableBytes: free}}}}, at)
	}
	sample(now, 5)
	sample(now.Add(2*time.Second), 5)
	rt.flushFleetHealth(now.Add(4 * time.Second))
	require.Len(t, healthMessages(t), 1)
	sample(now.Add(5*time.Second), 50)
	sample(now.Add(5500*time.Millisecond), 5)
	rt.flushFleetHealth(now.Add(7 * time.Second))
	require.Len(t, healthMessages(t), 1, "no false recovery when latest reading is low")
}
func TestFleetHealthStaleGapCancelsPendingMemory(t *testing.T) {
	rt, peer := healthFixture(t)
	now := time.Now()
	_, err := config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Federation.Health.Defaults.DebounceSeconds = 120
		return nil
	})
	require.NoError(t, err)
	sample := func(at time.Time) {
		rt.observeFleetNode(peer, &proto.CatalogPayload{Node: &proto.NodeMetadata{Resources: proto.NodeResources{Status: "current", ObservedAt: &at, RAM: &proto.NodeRAM{TotalBytes: 100, AvailableBytes: 5}}}}, at)
	}
	sample(now)
	sample(now.Add(2 * time.Second))
	rt.flushFleetHealth(now.Add(100 * time.Second))
	require.Empty(t, healthMessages(t))
	sample(now.Add(125 * time.Second))
	rt.flushFleetHealth(now.Add(126 * time.Second))
	require.Empty(t, healthMessages(t), "fresh low observation must start a new sustained interval")
}
func TestFleetHealthUnknownWorkDiskCannotRecover(t *testing.T) {
	rt, peer := healthFixture(t)
	now := time.Now()
	work := 5.0
	cat := &proto.CatalogPayload{Node: &proto.NodeMetadata{Resources: proto.NodeResources{Status: "current", ObservedAt: &now, DataDisk: &proto.NodeDisk{TotalBytes: 100, AvailableBytes: 50}, WorkDiskMinAvailablePercent: &work}}}
	rt.observeFleetNode(peer, cat, now)
	rt.flushFleetHealth(now.Add(2 * time.Second))
	require.Len(t, healthMessages(t), 1)
	later := now.Add(3 * time.Second)
	cat.Node.Resources.ObservedAt = &later
	cat.Node.Resources.WorkDiskMinAvailablePercent = nil
	rt.observeFleetNode(peer, cat, later)
	rt.flushFleetHealth(later.Add(2 * time.Second))
	require.Len(t, healthMessages(t), 1)
}

func TestFleetHealthDuplicateAfterWindowPruneDoesNotInflateCount(t *testing.T) {
	rt, peer := healthFixture(t)
	now := time.Now()
	rt.observeFleetFailure(peer, "expired", now)
	rt.observeFleetFailure(peer, "current", now.Add(599*time.Second))
	rt.observeFleetFailure(peer, "current", now.Add(601*time.Second))
	rt.observeFleetFailure(peer, "second-current", now.Add(602*time.Second))
	rt.flushFleetHealth(now.Add(604 * time.Second))
	require.Empty(t, healthMessages(t), "only two distinct failures remain in the window")
}
func TestFleetHealthNearestPolicyAcrossTwoRotations(t *testing.T) {
	_, old := healthFixture(t)
	peer, err := db.GetFederationPeer(old)
	require.NoError(t, err)
	// The fixture's private key is intentionally not retained; generate a fresh
	// pair for a real signed two-hop continuation chain.
	first, err := proto.NewIdentity()
	require.NoError(t, err)
	second, err := proto.NewIdentity()
	require.NoError(t, err)
	third, err := proto.NewIdentity()
	require.NoError(t, err)
	peer.InstanceID = first.ID()
	peer.PubKey = first.Pub
	peer.Label = "rotating"
	require.NoError(t, db.TrustFederationPeer(*peer))
	now := time.Now()
	for _, pair := range [][2]*proto.Identity{{first, second}, {second, third}} {
		r, err := proto.NewRotation(pair[0], pair[1], "", 1, now, time.Second)
		require.NoError(t, err)
		_, err = db.ObserveFederationRotation(r, now, time.Second)
		require.NoError(t, err)
		require.NoError(t, db.AcceptFederationRotation(pair[0].ID(), now.Add(2*time.Second)))
	}
	_, err = config.Update(func(c *config.Config, e error) error {
		if e != nil {
			return e
		}
		c.Federation.Health.Peers = map[string]config.FederationHealthPolicy{first.ID(): {Resources: true}, second.ID(): {Failures: true}}
		return nil
	})
	require.NoError(t, err)
	p, ok := fleetPolicy(third.ID())
	require.True(t, ok)
	require.True(t, p.Failures)
	require.False(t, p.Resources)
}
