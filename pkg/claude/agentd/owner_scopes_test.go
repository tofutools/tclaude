package agentd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestParseOwnerScopes(t *testing.T) {
	for _, tc := range []struct {
		name          string
		raw           string
		wantCanonical string
		wantErr       string
	}{
		{name: "absent is no narrowing", raw: ""},
		{name: "null is no narrowing", raw: "null"},
		{name: "empty object is no narrowing", raw: "{}"},
		{name: "canonicalizes and sorts",
			raw:           `{"groups.spawn":{"spawn_profile":["p2","p1"]}}`,
			wantCanonical: `{"groups.spawn":{"spawn_profile":["p1","p2"]}}`},
		{name: "not an object", raw: `["groups.spawn"]`,
			wantErr: "owner scopes must be a JSON object"},
		{name: "unknown slug", raw: `{"groups.teleport":{"group":["g1"]}}`,
			wantErr: `unknown permission slug "groups.teleport"`},
		{name: "slug with no owner bypass", raw: `{"permissions.grant":{"group":["g1"]}}`,
			wantErr: `permission "permissions.grant" has no owner-implied bypass to narrow`},
		{name: "dimension the slug does not declare", raw: `{"groups.spawn":{"remote":["github.com"]}}`,
			wantErr: `permission slug "groups.spawn" does not declare scope dimension "remote"`},
		{name: "unknown dimension", raw: `{"groups.spawn":{"phase":["design"]}}`,
			wantErr: `unknown permission scope dimension "phase"`},
		// The whole point of writing one of these is to narrow, so the value
		// that would silently mean "unrestricted" is refused rather than
		// stored as the widest possible reading of a narrowing.
		{name: "empty scope for a slug is refused", raw: `{"groups.spawn":{}}`,
			wantErr: "an owner scope must name at least one dimension"},
		{name: "slug with no owner-implied entry and empty scope reports the slug first",
			raw:     `{"permissions.grant":{}}`,
			wantErr: `permission "permissions.grant" has no owner-implied bypass to narrow`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, canonical, err := parseOwnerScopes(json.RawMessage(tc.raw))
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantCanonical, canonical)
		})
	}
}

// A stored map this build cannot decode must deny the bypass, not read as
// "unrestricted". The inversion would be the worst possible failure: a
// narrowing written by a newer daemon becoming a wildcard on an older one.
func TestOwnerBypassPermittedForGroupFailsClosedOnUndecodableMap(t *testing.T) {
	g := &db.AgentGroup{ID: 1, Name: "g1", OwnerScopesJSON: `{"groups.spawn":{"mystery":["x"]}}`}
	assert.False(t, ownerBypassPermittedForGroup(g, "conv-1", PermGroupsSpawn, ActionContext{Group: "g1", SpawnProfile: "p1"}),
		"an undecodable owner-scope map must deny the bypass")
	assert.False(t, ownerBypassPermittedForGroup(g, "conv-1", PermHumanNotify, ActionContext{}),
		"and it denies it for EVERY slug on that group, not just the malformed entry")
}

func TestOwnerBypassPermittedForGroup(t *testing.T) {
	narrowed := &db.AgentGroup{ID: 1, Name: "g1",
		OwnerScopesJSON: `{"groups.spawn":{"spawn_profile":["p1"]}}`}
	unnarrowed := &db.AgentGroup{ID: 2, Name: "g2"}

	assert.True(t, ownerBypassPermittedForGroup(narrowed, "c", PermGroupsSpawn,
		ActionContext{Group: "g1", SpawnProfile: "p1"}), "the named profile passes")
	assert.False(t, ownerBypassPermittedForGroup(narrowed, "c", PermGroupsSpawn,
		ActionContext{Group: "g1", SpawnProfile: "p2"}), "another profile is refused")
	assert.False(t, ownerBypassPermittedForGroup(narrowed, "c", PermGroupsSpawn,
		ActionContext{Group: "g1"}), "an inline/unnamed profile leaves the dimension undescribed and fails closed")
	assert.True(t, ownerBypassPermittedForGroup(narrowed, "c", PermGroupsStop,
		ActionContext{Group: "g1"}), "a slug the map does not mention keeps the unrestricted bypass")
	assert.True(t, ownerBypassPermittedForGroup(unnarrowed, "c", PermGroupsSpawn, ActionContext{Group: "g2"}),
		"a group with no map is exactly today's bypass")
	assert.False(t, ownerBypassPermittedForGroup(nil, "c", PermGroupsSpawn, ActionContext{}),
		"no group is no bypass")
}

// The multi-group rule: narrowing is PER GROUP, so an owner of a narrowed g1
// and an unnarrowed g2 keeps its full reach over g2. This is the subtlety a
// site that only asked "is this caller an owner" would get wrong.
func TestOwnerImpliedTierUnionsAcrossOwnedGroups(t *testing.T) {
	setupTestDB(t)
	const owner = "owner-tier-conv-0001"
	_, _, err := db.EnsureAgentForConv(owner, "spawn")
	require.NoError(t, err)

	narrowedID, err := db.CreateAgentGroup("tier-narrow", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(narrowedID, owner, "test"))
	_, err = db.SetAgentGroupOwnerScopes("tier-narrow", `{"groups.spawn":{"spawn_profile":["p1"]}}`)
	require.NoError(t, err)

	tier := ownerImpliedTierFor(owner)
	require.Contains(t, tier, PermGroupsSpawn)
	assert.False(t, tier[PermGroupsSpawn].Unrestricted, "the only owned group narrows spawn")
	assert.True(t, tier[PermGroupsStop].Unrestricted, "an unmentioned slug is unrestricted")
	assert.True(t, tier.satisfiedBy(owner, PermGroupsSpawn, ActionContext{Group: "tier-narrow", SpawnProfile: "p1"}))
	assert.False(t, tier.satisfiedBy(owner, PermGroupsSpawn, ActionContext{Group: "tier-narrow", SpawnProfile: "p2"}))

	// Owning a SECOND, unnarrowed group restores the unrestricted reading —
	// union with unrestricted is unrestricted, the same rule an unscoped row
	// applies within a grant tier.
	wideID, err := db.CreateAgentGroup("tier-wide", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(wideID, owner, "test"))

	tier = ownerImpliedTierFor(owner)
	assert.True(t, tier[PermGroupsSpawn].Unrestricted,
		"an unnarrowed owned group must not be suppressed by a narrowed sibling")
	assert.True(t, tier.satisfiedBy(owner, PermGroupsSpawn, ActionContext{Group: "tier-wide", SpawnProfile: "p2"}))
}

// The listing must state the reach the gate will actually allow. An owner
// whose bypass is pinned to one profile is not holding the fleet-wide slug the
// bare "owner:group" provenance implies.
func TestOwnerProvenanceRendersTheNarrowing(t *testing.T) {
	assert.Equal(t, "owner:group", ownerProvenance(PermGroupsSpawn, ownerTierEntry{Unrestricted: true}))
	assert.Equal(t, "owner:group [spawn_profile=p1]",
		ownerProvenance(PermGroupsSpawn, ownerTierEntry{
			Scopes: []PermissionScope{{ScopeDimSpawnProfile: {"p1"}}}}))
	assert.Equal(t, "owner:group [spawn_profile=p1] OR [spawn_profile=p2]",
		ownerProvenance(PermGroupsSpawn, ownerTierEntry{Scopes: []PermissionScope{
			{ScopeDimSpawnProfile: {"p2"}}, {ScopeDimSpawnProfile: {"p1"}}}}),
		"two owned groups' narrowings render as the OR the gate evaluates")
	assert.Equal(t, "owner:any", ownerProvenance(PermHumanNotify, ownerTierEntry{Unrestricted: true}))
}

// Attenuation must consult the NARROWED owner tier where the plain resolver is
// undecided. Without this an owner pinned to one profile could mint a child an
// unscoped groups.spawn and act through it — the escalation attenuation exists
// to stop, arriving through the bypass instead of a grant.
func TestGranterScopesForSlugUsesNarrowedOwnerTier(t *testing.T) {
	setupTestDB(t)
	const granter = "owner-atten-conv-0001"
	src := loadPermSources(granter)
	cfg, err := config.Load()
	require.NoError(t, err)

	narrow := ownerImpliedTier{PermGroupsSpawn: {Scopes: []PermissionScope{
		{ScopeDimSpawnProfile: {"p1"}}}}}
	scopes, scoped := granterScopesForSlug(src, cfg, narrow, PermGroupsSpawn)
	require.True(t, scoped, "a narrowed owner bypass is a shape to attenuate against")
	require.Len(t, scopes, 1)
	assert.False(t, permissionScopeCovers(scopes[0], nil),
		"a narrowed owner may not confer an UNSCOPED grant")
	assert.True(t, permissionScopeCovers(scopes[0], PermissionScope{ScopeDimSpawnProfile: {"p1"}}),
		"but may confer within its own narrowing")

	_, scoped = granterScopesForSlug(src, cfg, ownerImpliedTier{
		PermGroupsSpawn: {Unrestricted: true}}, PermGroupsSpawn)
	assert.False(t, scoped, "an unrestricted owner tier stays unconstrained, as before Phase 6")

	_, scoped = granterScopesForSlug(src, cfg, nil, PermGroupsSpawn)
	assert.False(t, scoped, "a non-owner with no grant is unconstrained (permissions.grant is recursive)")
}

// An EXPLICIT grant resolves first and is untouched by any narrowing: the
// operator narrowed the structural bypass, not the grant they separately
// issued. Settled design decision, guarded here so a later refactor that
// merges the two tiers fails loudly.
func TestGranterScopesForSlugPrefersExplicitGrantOverOwnerNarrowing(t *testing.T) {
	setupTestDB(t)
	const granter = "owner-atten-explicit-0001"
	require.NoError(t, db.GrantAgentPermission(granter, PermGroupsSpawn, "test"))
	src := loadPermSources(granter)
	cfg, err := config.Load()
	require.NoError(t, err)

	narrow := ownerImpliedTier{PermGroupsSpawn: {Scopes: []PermissionScope{
		{ScopeDimSpawnProfile: {"p1"}}}}}
	_, scoped := granterScopesForSlug(src, cfg, narrow, PermGroupsSpawn)
	assert.False(t, scoped,
		"an unscoped explicit grant wins over a narrowed bypass, so delegation stays unconstrained")
}

// The group dimension is filled in by the site that KNOWS which group carries
// the bypass. Otherwise the obvious narrowing — {"agent.retire": {"group":
// ["g1"]}} on g1 — would refuse every time and read as a revoke rather than the
// confinement the operator wrote. The cross-agent gate cannot fill it in: it
// targets an agent, and which owned group carries the authority is only decided
// while enumerating candidates.
func TestOwnerBypassFillsTheGroupDimensionFromTheCarryingGroup(t *testing.T) {
	setupTestDB(t)
	const owner = "owner-groupdim-conv-0001"
	const target = "owner-groupdim-target-0001"
	for _, conv := range []string{owner, target} {
		_, _, err := db.EnsureAgentForConv(conv, "spawn")
		require.NoError(t, err)
	}
	id, err := db.CreateAgentGroup("groupdim", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(id, owner, "test"))
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: id, ConvID: target, Role: "worker"}))

	_, err = db.SetAgentGroupOwnerScopes("groupdim", `{"agent.retire":{"group":["groupdim"]}}`)
	require.NoError(t, err)
	assert.True(t, ownerOfGroupContainingPermitting(owner, target, PermAgentRetire, ActionContext{}),
		"the candidate group IS the group the bypass flows through")

	// Naming a DIFFERENT group still refuses: filling the dimension states the
	// truth, it does not wave the matcher through.
	_, err = db.SetAgentGroupOwnerScopes("groupdim", `{"agent.retire":{"group":["elsewhere"]}}`)
	require.NoError(t, err)
	assert.False(t, ownerOfGroupContainingPermitting(owner, target, PermAgentRetire, ActionContext{}))

	// And a group-scoped gate that passes no context at all still knows its own
	// group — the same rule applied one site earlier.
	_, err = db.SetAgentGroupOwnerScopes("groupdim", `{"groups.spawn":{"group":["groupdim"]}}`)
	require.NoError(t, err)
	g, err := db.GetAgentGroupByName("groupdim")
	require.NoError(t, err)
	assert.True(t, ownerOfGroupPermitting(g, owner, PermGroupsSpawn, ActionContext{}))
}

// The failure the degraded flag exists for. A group whose stored map this
// build cannot decode confers NOTHING at the gate — so the owner must not be
// read as an UNCONSTRAINED granter, which is what "no tier entry" would mean.
// Without this, an owner narrowed by a newer daemon's map could, after a
// downgrade, mint a child an unscoped groups.spawn and act through it.
func TestOwnerTierDegradesRatherThanVanishingOnUndecodableMap(t *testing.T) {
	setupTestDB(t)
	const owner = "owner-degraded-conv-0001"
	_, _, err := db.EnsureAgentForConv(owner, "spawn")
	require.NoError(t, err)
	id, err := db.CreateAgentGroup("degraded", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(id, owner, "test"))
	// Written straight to the row: the HTTP boundary would refuse this, which
	// is precisely why it can only arrive from a build that knew the dimension.
	_, err = db.SetAgentGroupOwnerScopes("degraded", `{"groups.spawn":{"mystery":["x"]}}`)
	require.NoError(t, err)

	tier := ownerImpliedTierFor(owner)
	entry := tier[PermGroupsSpawn]
	assert.True(t, entry.Degraded, "the unreadable group is recorded, not skipped")
	assert.False(t, entry.confers(), "and it authorizes nothing")

	// Listing == gate: the slug must NOT be reported effective.
	effective, ownerAdded, _, _ := effectivePermsFor(permissionsState{}, owner, tier)
	assert.NotContains(t, effective, PermGroupsSpawn)
	assert.NotContains(t, ownerAdded, PermGroupsSpawn)
	g, err := db.GetAgentGroupByName("degraded")
	require.NoError(t, err)
	assert.False(t, ownerOfGroupPermitting(g, owner, PermGroupsSpawn,
		ActionContext{Group: "degraded", SpawnProfile: "p1"}), "and the gate refuses it too")

	// Attenuation must read it as "confers nothing", NOT as unconstrained.
	cfg, err := config.Load()
	require.NoError(t, err)
	scopes, scoped := granterScopesForSlug(loadPermSources(owner), cfg, tier, PermGroupsSpawn)
	require.True(t, scoped, "a tier the daemon failed to read is not an absence of constraint")
	assert.Empty(t, scopes, "so the owner may confer nothing through it")
}

// A second, UNNARROWED owned group still carries the owner: a group that could
// not be read must not suppress one that could.
func TestOwnerTierDegradedGroupDoesNotSuppressAReadableOne(t *testing.T) {
	setupTestDB(t)
	const owner = "owner-degraded-conv-0002"
	_, _, err := db.EnsureAgentForConv(owner, "spawn")
	require.NoError(t, err)
	for _, name := range []string{"deg-bad", "deg-good"} {
		id, err := db.CreateAgentGroup(name, "")
		require.NoError(t, err)
		require.NoError(t, db.AddAgentGroupOwner(id, owner, "test"))
	}
	_, err = db.SetAgentGroupOwnerScopes("deg-bad", `{"groups.spawn":{"mystery":["x"]}}`)
	require.NoError(t, err)

	tier := ownerImpliedTierFor(owner)
	entry := tier[PermGroupsSpawn]
	assert.True(t, entry.Unrestricted, "the readable, unnarrowed group still confers")
	assert.True(t, entry.confers())

	cfg, err := config.Load()
	require.NoError(t, err)
	_, scoped := granterScopesForSlug(loadPermSources(owner), cfg, tier, PermGroupsSpawn)
	assert.False(t, scoped, "and an unrestricted tier stays unconstrained for delegation")
}

// The cross-agent bypass unions per candidate group: an owner of a narrowed g1
// and an unnarrowed g2 that BOTH contain the target still passes through g2.
// This is the multi-group rule at the one site that has no group in its
// ActionContext at all.
func TestOwnerOfGroupContainingPermittingUnionsCandidates(t *testing.T) {
	setupTestDB(t)
	const owner = "owner-xagent-conv-0001"
	const target = "owner-xagent-target-0001"
	for _, conv := range []string{owner, target} {
		_, _, err := db.EnsureAgentForConv(conv, "spawn")
		require.NoError(t, err)
	}
	var ids []int64
	for _, name := range []string{"xa-narrow", "xa-wide"} {
		id, err := db.CreateAgentGroup(name, "")
		require.NoError(t, err)
		require.NoError(t, db.AddAgentGroupOwner(id, owner, "test"))
		require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{GroupID: id, ConvID: target, Role: "worker"}))
		ids = append(ids, id)
	}
	require.Len(t, ids, 2)
	// xa-narrow pins agent.retire to descendants only; the caller did not spawn
	// the target, so that arm can never match.
	_, err := db.SetAgentGroupOwnerScopes("xa-narrow", `{"agent.retire":{"target_agent":["@descendants"]}}`)
	require.NoError(t, err)

	targetAgent, err := db.AgentIDForConv(target)
	require.NoError(t, err)
	actx := ActionContext{TargetAgent: targetAgent}
	assert.True(t, ownerOfGroupContainingPermitting(owner, target, PermAgentRetire, actx),
		"the unnarrowed candidate group still carries the bypass")

	// Narrow the second group the same way and the bypass is gone: every
	// candidate now refuses.
	_, err = db.SetAgentGroupOwnerScopes("xa-wide", `{"agent.retire":{"target_agent":["@descendants"]}}`)
	require.NoError(t, err)
	assert.False(t, ownerOfGroupContainingPermitting(owner, target, PermAgentRetire, actx),
		"with every candidate narrowed away, nothing carries it")
}
