package agentd

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Pins of the local gates a restricted peer passes, taken before the peer
// gate was derived from the registry's federation reach. A change to them
// must be deliberate: update the pin in the same change and say why.

// pinnedPeerActionGates are the gates a peer action passes under each rule
// grant, held in every group of the target: on the source agent itself
// ("self", "target") and with a group named ("group").
var pinnedPeerActionGates = map[string][]string{
	PermGroupsMembersStop:   {"agent.stop@group", "agent.stop@self", "agent.stop@target"},
	PermGroupsMembersResume: {"agent.resume@group", "agent.resume@self", "agent.resume@target"},
	PermGroupsMembersRetire: {"agent.retire@group", "agent.retire@self", "agent.retire@target"},
	PermGroupsMembersClone:  {"agent.clone@group", "agent.clone@self", "agent.clone@target", "groups.members.add@group"},
	PermAgentMove:           {"agent.bundle.export@group", "agent.bundle.export@self", "agent.bundle.export@target", "agent.move@group", "agent.move@self", "agent.move@target", "agent.share@group", "agent.share@self", "agent.share@target", "self.teleport@group", "self.teleport@self", "self.teleport@target"},
	PermGroupsMembersManage: {"agent.rename@group", "agent.rename@self", "agent.rename@target", "agent.task@group", "agent.task@self", "agent.task@target"},
}

// pinnedPeerMoveWithRetireGates: a move also passes the source's retire
// once the peer holds the retire grant there.
var pinnedPeerMoveWithRetireGates = []string{"agent.bundle.export", "agent.move", "agent.retire", "agent.share", "self.teleport"}

// pinnedPeerGroupGates are the gates a peer-invoked group route answers for
// each group grant held alone (grants not listed answer none).
var pinnedPeerGroupGates = map[string][]string{
	PermGroupsSettings:            {"groups.attachment", "groups.nest", "groups.rename", "groups.settings.default-context", "groups.settings.default-profile", "groups.settings.description", "groups.settings.max-members", "groups.settings.notifications", "groups.settings.remote-control-policy"},
	PermGroupsSettingsEnvironment: {"groups.settings.environment"},
}

// pinnedPeerSettingsWithCwdGates: the default directory also needs the
// grant to choose a spawn's directory.
var pinnedPeerSettingsWithCwdGates = []string{"groups.attachment", "groups.nest", "groups.rename", "groups.settings.default-context", "groups.settings.default-dir", "groups.settings.default-profile", "groups.settings.description", "groups.settings.max-members", "groups.settings.notifications", "groups.settings.remote-control-policy"}

type peerGateFixture struct {
	t      *testing.T
	groups map[string]int64
	src    string
	peers  int
}

func newPeerGateFixture(t *testing.T) *peerGateFixture {
	setupTestDB(t)
	f := &peerGateFixture{t: t, groups: map[string]int64{}, src: "peer-gate-pin-src-0001"}
	for _, name := range []string{"one", "two", "other"} {
		id, err := db.CreateAgentGroup(name, "")
		require.NoError(t, err)
		f.groups[name] = id
	}
	for _, g := range []string{"one", "two"} {
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: f.groups[g], ConvID: f.src}))
	}
	return f
}

// peer trusts a fresh restricted peer holding slugs on groups.
func (f *peerGateFixture) peer(grants map[string][]string) string {
	f.peers++
	label := fmt.Sprintf("pin%d", f.peers)
	id, err := proto.NewIdentity()
	require.NoError(f.t, err)
	require.NoError(f.t, db.TrustFederationPeer(db.FederationPeer{InstanceID: id.ID(), PubKey: id.Pub, Label: label, Name: label, TrustLevel: db.FederationTrustRestricted}))
	for slug, groups := range grants {
		for _, g := range groups {
			require.NoError(f.t, db.UpsertFederationPeerGrant(db.FederationPeerGrant{Peer: id.ID(), Slug: slug, Scope: db.FederationGroupScope(f.groups[g])}))
		}
	}
	return id.ID()
}

func sortedGates(gates map[string]bool) []string {
	var out []string
	for g, ok := range gates {
		if ok {
			out = append(out, g)
		}
	}
	slices.Sort(out)
	return out
}

func TestPeerActionGatesArePinned(t *testing.T) {
	f := newPeerGateFixture(t)
	all := []string{"one", "two", "other"}
	for rule, want := range pinnedPeerActionGates {
		a := &peerActionAuthority{peer: f.peer(map[string][]string{rule: all}), sourceConv: f.src, permission: rule}
		var got []string
		for _, slug := range knownSlugs() {
			for _, c := range []struct{ label, target, group string }{{"self", "", ""}, {"target", f.src, ""}, {"group", "", "other"}} {
				if a.allows(slug, c.target, ActionContext{Group: c.group}) {
					got = append(got, slug+"@"+c.label)
				}
			}
		}
		slices.Sort(got)
		assert.Equal(t, want, got, "gates under %s", rule)
	}
	a := &peerActionAuthority{peer: f.peer(map[string][]string{PermAgentMove: all, PermGroupsMembersRetire: {"one", "two"}}), sourceConv: f.src, permission: PermAgentMove}
	var got []string
	for _, slug := range knownSlugs() {
		if a.allows(slug, f.src, ActionContext{}) {
			got = append(got, slug)
		}
	}
	assert.Equal(t, pinnedPeerMoveWithRetireGates, got, "a move with the retire grant")
	spawn := &peerActionAuthority{peer: f.peer(map[string][]string{PermGroupsMembersSpawn: {"other"}}), groupID: f.groups["other"]}
	got = nil
	for _, slug := range knownSlugs() {
		if spawn.allows(slug, "", ActionContext{Group: "other"}) {
			got = append(got, slug)
		}
	}
	assert.Equal(t, []string{PermGroupsMembersSpawn}, got, "a spawn authority answers its own gate in its group only")
}

func TestPeerGroupGatesArePinned(t *testing.T) {
	f := newPeerGateFixture(t)
	for _, slug := range slices.Sorted(maps.Keys(federationPeerSlugs)) {
		peer := f.peer(map[string][]string{PermGroupsRosterRead: {"other"}, slug: {"other"}})
		assert.Equal(t, pinnedPeerGroupGates[slug], sortedGates(peerGrantGates(peer, f.groups["other"])), "group gates under %s", slug)
	}
	peer := f.peer(map[string][]string{PermGroupsSettings: {"other"}, PermGroupsMembersSpawnCwd: {"other"}})
	assert.Equal(t, pinnedPeerSettingsWithCwdGates, sortedGates(peerGrantGates(peer, f.groups["other"])), "settings with spawn cwd")
}
