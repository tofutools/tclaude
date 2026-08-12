package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
			raw:           `{"groups.members.spawn":{"spawn_profile":["p2","p1"]}}`,
			wantCanonical: `{"groups.members.spawn":{"spawn_profile":["p1","p2"]}}`},
		{name: "not an object", raw: `["groups.members.spawn"]`,
			wantErr: "owner scopes must be a JSON object"},
		{name: "unknown slug", raw: `{"groups.teleport":{"group":["g1"]}}`,
			wantErr: `unknown permission slug "groups.teleport"`},
		{name: "slug with no owner bypass", raw: `{"permissions.grant":{"group":["g1"]}}`,
			wantErr: `permission "permissions.grant" is not contributed by group ownership`},
		{name: "dimension the slug does not declare", raw: `{"groups.members.spawn":{"remote":["github.com"]}}`,
			wantErr: `permission slug "groups.members.spawn" does not declare scope dimension "remote"`},
		{name: "unknown dimension", raw: `{"groups.members.spawn":{"phase":["design"]}}`,
			wantErr: `unknown permission scope dimension "phase"`},
		// The whole point of writing one of these is to narrow, so the value
		// that would silently mean "unrestricted" is refused rather than
		// stored as the widest possible reading of a narrowing.
		{name: "empty scope for a slug is refused", raw: `{"groups.members.spawn":{}}`,
			wantErr: "an owner scope must name at least one dimension"},
		{name: "slug with no owner-implied entry and empty scope reports the slug first",
			raw:     `{"permissions.grant":{}}`,
			wantErr: `permission "permissions.grant" is not contributed by group ownership`},
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

func TestSpawnWorktreePermissionUsesTargetGroupOwnership(t *testing.T) {
	setupTestDB(t)
	const owner = "worktree-owner-conv-0001"
	_, _, err := db.EnsureAgentForConv(owner, "spawn")
	require.NoError(t, err)
	ownedID, err := db.CreateAgentGroup("worktree-owned", "")
	require.NoError(t, err)
	_, err = db.CreateAgentGroup("worktree-unowned", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(ownedID, owner, "test"))

	request := func() *http.Request {
		return requestWithPeer(&peer{PID: 999, HasClaudeAncestor: true, ConvID: owner})
	}
	w := httptest.NewRecorder()
	_, ok := requireSpawnWorktreePermission(w, request(), "worktree-owned")
	assert.True(t, ok, "ownership should confer the spawn slug for that group's worktree; body=%s", w.Body.String())

	w = httptest.NewRecorder()
	_, ok = requireSpawnWorktreePermission(w, request(), "worktree-unowned")
	assert.False(t, ok, "owning an unrelated group must not authorize worktree preparation")
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
	_, err = db.SetAgentGroupOwnerScopes("tier-narrow", `{"groups.members.spawn":{"spawn_profile":["p1"]}}`)
	require.NoError(t, err)

	tier := ownerImpliedTierFor(owner)
	require.Contains(t, tier, PermGroupsMembersSpawn)
	assert.False(t, tier[PermGroupsMembersSpawn].Unrestricted, "the only owned group narrows spawn")
	assert.True(t, tier.satisfiedBy(owner, PermGroupsMembersStop, ActionContext{Group: "tier-narrow"}),
		"an unmentioned slug still gets the mandatory owned-group scope")
	assert.True(t, tier.satisfiedBy(owner, PermGroupsMembersSpawn, ActionContext{Group: "tier-narrow", SpawnProfile: "p1"}))
	assert.False(t, tier.satisfiedBy(owner, PermGroupsMembersSpawn, ActionContext{Group: "tier-narrow", SpawnProfile: "p2"}))

	// Owning a second group contributes a second scoped row; it never turns the
	// group permission into a global one.
	wideID, err := db.CreateAgentGroup("tier-wide", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(wideID, owner, "test"))

	tier = ownerImpliedTierFor(owner)
	assert.False(t, tier[PermGroupsMembersSpawn].Unrestricted)
	assert.Len(t, tier[PermGroupsMembersSpawn].Scopes, 2)
	assert.True(t, tier.satisfiedBy(owner, PermGroupsMembersSpawn, ActionContext{Group: "tier-wide", SpawnProfile: "p2"}))
}

func TestOwnerImpliedTierIgnoresArchivedGroups(t *testing.T) {
	setupTestDB(t)
	const owner = "owner-archived-conv-0001"
	_, _, err := db.EnsureAgentForConv(owner, "spawn")
	require.NoError(t, err)
	id, err := db.CreateAgentGroup("archived-owner-grant", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupOwner(id, owner, "test"))
	require.Contains(t, ownerImpliedTierFor(owner), PermGroupsMembersSpawn)
	require.NoError(t, db.ArchiveAgentGroup("archived-owner-grant"))
	assert.Empty(t, ownerImpliedTierFor(owner), "archived ownership is forensic history, not live authority")
}

// The listing must state the reach the gate will actually allow. An owner
// whose bypass is pinned to one profile is not holding the fleet-wide slug the
// bare "owner:group" provenance implies.
func TestOwnerProvenanceRendersTheNarrowing(t *testing.T) {
	assert.Equal(t, "owner", ownerProvenance(PermHumanNotify, ownerTierEntry{Unrestricted: true}))
	assert.Equal(t, "owner [group=g1 spawn_profile=p1]",
		ownerProvenance(PermGroupsMembersSpawn, ownerTierEntry{
			Scopes: []PermissionScope{{ScopeDimGroup: {"g1"}, ScopeDimSpawnProfile: {"p1"}}}}))
	assert.Equal(t, "owner [group=g1 spawn_profile=p1] OR [group=g2 spawn_profile=p2]",
		ownerProvenance(PermGroupsMembersSpawn, ownerTierEntry{Scopes: []PermissionScope{
			{ScopeDimGroup: {"g2"}, ScopeDimSpawnProfile: {"p2"}},
			{ScopeDimGroup: {"g1"}, ScopeDimSpawnProfile: {"p1"}}}}),
		"two owned groups' narrowings render as the OR the gate evaluates")
}

// Attenuation must consult the NARROWED owner tier where the plain resolver is
// undecided. Without this an owner pinned to one profile could mint a child an
// unscoped groups.members.spawn and act through it — the escalation attenuation exists
// to stop, arriving through the bypass instead of a grant.
func TestGranterScopesForSlugUsesNarrowedOwnerTier(t *testing.T) {
	setupTestDB(t)
	const granter = "owner-atten-conv-0001"
	src := loadPermSources(granter)
	cfg, err := config.Load()
	require.NoError(t, err)

	narrow := ownerImpliedTier{PermGroupsMembersSpawn: {Scopes: []PermissionScope{
		{ScopeDimSpawnProfile: {"p1"}}}}}
	scopes, scoped := granterScopesForSlug(src, cfg, narrow, PermGroupsMembersSpawn)
	require.True(t, scoped, "a narrowed owner bypass is a shape to attenuate against")
	require.Len(t, scopes, 1)
	assert.False(t, permissionScopeCovers(scopes[0], nil),
		"a narrowed owner may not confer an UNSCOPED grant")
	assert.True(t, permissionScopeCovers(scopes[0], PermissionScope{ScopeDimSpawnProfile: {"p1"}}),
		"but may confer within its own narrowing")

	_, scoped = granterScopesForSlug(src, cfg, ownerImpliedTier{
		PermGroupsMembersSpawn: {Unrestricted: true}}, PermGroupsMembersSpawn)
	assert.False(t, scoped, "an unrestricted owner tier stays unconstrained, as before Phase 6")

	_, scoped = granterScopesForSlug(src, cfg, nil, PermGroupsMembersSpawn)
	assert.False(t, scoped, "a non-owner with no grant is unconstrained (permissions.grant is recursive)")
}

// An EXPLICIT grant resolves first and is untouched by any narrowing: the
// operator narrowed the structural bypass, not the grant they separately
// issued. Settled design decision, guarded here so a later refactor that
// merges the two tiers fails loudly.
func TestGranterScopesForSlugPrefersExplicitGrantOverOwnerNarrowing(t *testing.T) {
	setupTestDB(t)
	const granter = "owner-atten-explicit-0001"
	require.NoError(t, db.GrantAgentPermission(granter, PermGroupsMembersSpawn, "test"))
	src := loadPermSources(granter)
	cfg, err := config.Load()
	require.NoError(t, err)

	narrow := ownerImpliedTier{PermGroupsMembersSpawn: {Scopes: []PermissionScope{
		{ScopeDimSpawnProfile: {"p1"}}}}}
	_, scoped := granterScopesForSlug(src, cfg, narrow, PermGroupsMembersSpawn)
	assert.False(t, scoped,
		"an unscoped explicit grant wins over a narrowed bypass, so delegation stays unconstrained")
}

func TestOwnerDerivedGrantAlwaysCarriesItsOwnedGroup(t *testing.T) {
	scope, ok := ownerDerivedGroupScope("groupdim", PermissionScope{ScopeDimSpawnProfile: {"reviewer"}})
	require.True(t, ok)
	assert.Equal(t, []string{"groupdim"}, scope[ScopeDimGroup])
	assert.Equal(t, []string{"reviewer"}, scope[ScopeDimSpawnProfile])

	_, ok = ownerDerivedGroupScope("groupdim", PermissionScope{ScopeDimGroup: {"elsewhere"}})
	assert.False(t, ok, "a constraint cannot redirect an owner grant to another group")
}

// The failure the degraded flag exists for. A group whose stored map this
// build cannot decode confers NOTHING at the gate — so the owner must not be
// read as an UNCONSTRAINED granter, which is what "no tier entry" would mean.
// Without this, an owner narrowed by a newer daemon's map could, after a
// downgrade, mint a child an unscoped groups.members.spawn and act through it.
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
	_, err = db.SetAgentGroupOwnerScopes("degraded", `{"groups.members.spawn":{"mystery":["x"]}}`)
	require.NoError(t, err)

	tier := ownerImpliedTierFor(owner)
	entry := tier[PermGroupsMembersSpawn]
	assert.True(t, entry.Degraded, "the unreadable group is recorded, not skipped")
	assert.False(t, entry.confers(), "and it authorizes nothing")

	// Listing == gate: the slug must NOT be reported effective.
	effective, ownerAdded, _, _ := effectivePermsFor(permissionsState{}, owner, tier)
	assert.NotContains(t, effective, PermGroupsMembersSpawn)
	assert.NotContains(t, ownerAdded, PermGroupsMembersSpawn)
	allowed, _ := ownerPermissionMatch(owner, PermGroupsMembersSpawn,
		ActionContext{Group: "degraded", SpawnProfile: "p1"})
	assert.False(t, allowed, "and the ordinary owner-derived grant refuses it too")

	// Attenuation must read it as "confers nothing", NOT as unconstrained.
	cfg, err := config.Load()
	require.NoError(t, err)
	scopes, scoped := granterScopesForSlug(loadPermSources(owner), cfg, tier, PermGroupsMembersSpawn)
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
	_, err = db.SetAgentGroupOwnerScopes("deg-bad", `{"groups.members.spawn":{"mystery":["x"]}}`)
	require.NoError(t, err)

	tier := ownerImpliedTierFor(owner)
	entry := tier[PermGroupsMembersSpawn]
	assert.False(t, entry.Unrestricted, "group owner grants are never global")
	assert.True(t, entry.confers())
	assert.True(t, tier.satisfiedBy(owner, PermGroupsMembersSpawn,
		ActionContext{Group: "deg-good"}), "the readable group still contributes its scoped grant")

	cfg, err := config.Load()
	require.NoError(t, err)
	scopes, scoped := granterScopesForSlug(loadPermSources(owner), cfg, tier, PermGroupsMembersSpawn)
	assert.True(t, scoped)
	assert.NotEmpty(t, scopes, "the readable group's scope remains available for delegation")
}
