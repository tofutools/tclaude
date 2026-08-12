package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// haveSpawnCapableOwner enrolls convID as an OWNER of group with the launch
// posture the spawn path requires, and NO permission grant. The owner-implied
// bypass is what admits it — which is exactly what these tests narrow.
func haveSpawnCapableOwner(t *testing.T, f *testharness.Flow, group, convID string) {
	t.Helper()
	haveSpawnCapableMember(t, f, group, convID)
	g, err := db.GetAgentGroupByName(group)
	require.NoError(t, err)
	require.NotNil(t, g)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, convID, "<test>"))
}

// narrowOwnerBypass writes a group's owner-scope map through the production
// endpoint, as the human operator would.
func narrowOwnerBypass(t *testing.T, f *testharness.Flow, group string, scopes map[string]any) {
	t.Helper()
	rec := humanReq(t, f, http.MethodPatch, "/v1/groups/"+group,
		map[string]any{"owner_scopes": scopes})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// THE headline scenario (TCL-1071). Group g1 pins its owner-implied
// groups.members.spawn to one profile. An owner of g1 holding NO grant of its own may
// spawn into g1 with that profile — the bypass still fills the gap — and is
// refused with any other profile, and with an inline shape that names none.
//
// The inline case is the same fail-closed decision the grant path makes: an
// unnamed profile leaves the dimension undescribed, and a narrowing that
// treated "no profile" as "any profile" would be escapable by omitting a flag.
func TestOwnerScopes_NarrowedBypassGatesSpawnPerProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("g1")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "p1"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "p2"})
	require.NoError(t, err)

	const owner = "ownerscope-lead-aaaa-bbbb-cccc-00000000001"
	haveSpawnCapableOwner(t, f, "g1", owner)
	narrowOwnerBypass(t, f, "g1", map[string]any{
		agentd.PermGroupsMembersSpawn: map[string]any{"spawn_profile": []string{"p1"}},
	})

	allowed := spawnWithProfile(t, f, owner, "g1", "p1-worker", "p1")
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body)

	other := spawnWithProfile(t, f, owner, "g1", "p2-worker", "p2")
	assert.Equal(t, http.StatusForbidden, other.Code, other.Body)
	assert.Contains(t, other.Body, agentd.PermGroupsMembersSpawn,
		"a narrowed-away bypass lands on the ordinary not-granted path, naming the slug")

	inline := spawnWithProfile(t, f, owner, "g1", "inline-worker", "")
	assert.Equal(t, http.StatusForbidden, inline.Code, inline.Body)

	// Only the permitted spawn enrolled anyone.
	names := map[string]bool{}
	for _, m := range f.ListGroupMembers("g1") {
		names[m.ConvID] = true
	}
	assert.Len(t, names, 2, "the owner plus exactly one spawned worker")
}

// An owner holding an EXPLICIT unscoped grant is UNAFFECTED: the grant
// resolves first, under the ordinary precedence, and the narrowing speaks only
// to the structural bypass. Settled design decision — an operator who wants
// that grant narrowed edits the grant's own scope.
func TestOwnerScopes_ExplicitGrantIsUnaffectedByNarrowing(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("g1")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "p1"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "p2"})
	require.NoError(t, err)

	const owner = "ownerscope-lead-aaaa-bbbb-cccc-00000000002"
	haveSpawnCapableOwner(t, f, "g1", owner)
	narrowOwnerBypass(t, f, "g1", map[string]any{
		agentd.PermGroupsMembersSpawn: map[string]any{"spawn_profile": []string{"p1"}},
	})
	grantScoped(t, f, owner, agentd.PermGroupsMembersSpawn, nil)

	allowed := spawnWithProfile(t, f, owner, "g1", "granted-worker", "p2")
	assert.Equal(t, http.StatusOK, allowed.Code, allowed.Body,
		"an explicit unscoped grant wins over the group's owner-bypass narrowing")
}

// Narrowing is PER GROUP. An owner of a narrowed g1 and an unnarrowed g2 is
// confined only when acting on g1 — a site that consulted "some owned group's
// map" instead of the TARGET group's would get this backwards in one direction
// or the other.
func TestOwnerScopes_NarrowingIsPerGroup(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("g1")
	f.HaveGroup("g2")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "p1"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "p2"})
	require.NoError(t, err)

	const owner = "ownerscope-lead-aaaa-bbbb-cccc-00000000003"
	haveSpawnCapableOwner(t, f, "g1", owner)
	haveSpawnCapableOwner(t, f, "g2", owner)
	narrowOwnerBypass(t, f, "g1", map[string]any{
		agentd.PermGroupsMembersSpawn: map[string]any{"spawn_profile": []string{"p1"}},
	})

	refused := spawnWithProfile(t, f, owner, "g1", "g1-p2-worker", "p2")
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body,
		"g1's own narrowing binds a spawn into g1")

	allowed := spawnWithProfile(t, f, owner, "g2", "g2-p2-worker", "p2")
	assert.Equal(t, http.StatusOK, allowed.Code, allowed.Body,
		"g2 declares no narrowing, so acting on g2 is untouched by g1's")
}

// A narrowing takes reach away, never adds any: a DENY still suppresses the
// bypass entirely, exactly as before. Guards against a refactor that made the
// owner-scope map an independent allow-source.
func TestOwnerScopes_DenyStillSuppressesTheNarrowedBypass(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("g1")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "p1"})
	require.NoError(t, err)

	const owner = "ownerscope-lead-aaaa-bbbb-cccc-00000000004"
	haveSpawnCapableOwner(t, f, "g1", owner)
	narrowOwnerBypass(t, f, "g1", map[string]any{
		agentd.PermGroupsMembersSpawn: map[string]any{"spawn_profile": []string{"p1"}},
	})
	require.NoError(t, db.SetAgentPermissionOverride(owner, agentd.PermGroupsMembersSpawn, db.PermEffectDeny, "test"))

	refused := spawnWithProfile(t, f, owner, "g1", "denied-worker", "p1")
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body,
		"an explicit deny is authoritative even for a spawn the narrowing would permit")
}

// Listing == gate. An owner whose bypass is pinned must not be told it holds
// the fleet-wide slug: the effective view renders the narrowing the gate will
// enforce, in the same bracketed form a scoped grant uses.
func TestOwnerScopes_ListingRendersTheNarrowing(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("g1")
	const owner = "ownerscope-lead-aaaa-bbbb-cccc-00000000005"
	haveSpawnCapableOwner(t, f, "g1", owner)
	narrowOwnerBypass(t, f, "g1", map[string]any{
		agentd.PermGroupsMembersSpawn: map[string]any{"spawn_profile": []string{"p1"}},
	})

	view := getPermissionsTarget(t, f, owner, owner)
	require.Equal(t, http.StatusOK, view.Code, view.Body)
	var effective struct {
		Provenance map[string]string `json:"provenance"`
	}
	require.NoError(t, json.Unmarshal([]byte(view.Body), &effective))
	assert.Equal(t, "owner:group [spawn_profile=p1]", effective.Provenance[agentd.PermGroupsMembersSpawn],
		"the owner tier states where its reach ends")
	assert.Equal(t, "owner:group", effective.Provenance[agentd.PermGroupsMembersStop],
		"a slug the map does not mention keeps the unrestricted rendering")
}

// Attenuation through the BYPASS. An owner whose only authority for
// groups.members.spawn is a narrowed bypass must not be able to mint a child a wider
// one and act through it — the same escalation the grant path already refuses,
// arriving via ownership instead.
func TestOwnerScopes_NarrowedOwnerCannotConferWiderSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("g1")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "p1"})
	require.NoError(t, err)

	const owner = "ownerscope-lead-aaaa-bbbb-cccc-00000000006"
	haveSpawnCapableOwner(t, f, "g1", owner)
	narrowOwnerBypass(t, f, "g1", map[string]any{
		agentd.PermGroupsMembersSpawn: map[string]any{"spawn_profile": []string{"p1"}},
	})
	// permissions.grant is what lets it mint at all; it is deliberately
	// unscoped so the refusal below can only come from the groups.members.spawn shape.
	grantScoped(t, f, owner, agentd.PermPermissionsGrant, nil)

	rec := agentReq(t, f, owner, http.MethodPost, "/v1/groups/g1/spawn", map[string]any{
		"name": "wide-child", "profile": "p1",
		"permission_overrides": map[string]any{
			agentd.PermGroupsMembersSpawn: db.PermEffectGrant,
		},
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "scope_not_attenuated")
	assert.Contains(t, rec.Body.String(), "UNSCOPED", "the refusal says what was attempted")

	// Within its own narrowing it may still confer.
	ok := agentReq(t, f, owner, http.MethodPost, "/v1/groups/g1/spawn", map[string]any{
		"name": "narrow-child", "profile": "p1",
		"permission_overrides": map[string]any{
			agentd.PermGroupsMembersSpawn: map[string]any{
				"effect": db.PermEffectGrant,
				"scope":  map[string]any{"spawn_profile": []string{"p1"}},
			},
		},
	})
	assert.Equal(t, http.StatusOK, ok.Code, ok.Body.String())
}

// A bypass site with NO target group in context fails closed for a narrowed
// group's contribution. /v1/worktrees/prepare is gated on groups.members.spawn through
// the owns-any-group bypass and describes neither a group nor a profile, so an
// owner whose ONLY group narrows groups.members.spawn cannot reach it — while an owner
// of an unnarrowed group still can.
func TestOwnerScopes_NoTargetGroupInContextFailsClosed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("g1")
	f.HaveGroup("g2")
	const narrowed = "ownerscope-lead-aaaa-bbbb-cccc-00000000007"
	const wide = "ownerscope-lead-aaaa-bbbb-cccc-00000000008"
	haveSpawnCapableOwner(t, f, "g1", narrowed)
	haveSpawnCapableOwner(t, f, "g2", wide)
	narrowOwnerBypass(t, f, "g1", map[string]any{
		agentd.PermGroupsMembersSpawn: map[string]any{"spawn_profile": []string{"p1"}},
	})

	// A malformed body would be rejected before the gate, so send a real one
	// and read only the AUTHORIZATION outcome: 403 vs anything else.
	body := map[string]any{"repo": f.World.HomeDir, "branch": "ownerscope-wt"}
	refused := agentReq(t, f, narrowed, http.MethodPost, "/v1/worktrees/prepare", body)
	assert.Equal(t, http.StatusForbidden, refused.Code, refused.Body.String(),
		"a site that names no group cannot satisfy a narrowing, so it must refuse")

	permitted := agentReq(t, f, wide, http.MethodPost, "/v1/worktrees/prepare", body)
	assert.NotEqual(t, http.StatusForbidden, permitted.Code, permitted.Body.String(),
		"an owner of an unnarrowed group still passes the gate")
}

// The write surface: PATCH /v1/groups/{name} validates the map through the
// same registry that validates grant scopes, and refuses the whole request
// rather than storing something the gate would later fail closed on.
func TestOwnerScopes_PatchValidatesTheMap(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("g1")

	bad := humanReq(t, f, http.MethodPatch, "/v1/groups/g1", map[string]any{
		"owner_scopes": map[string]any{
			agentd.PermGroupsMembersSpawn: map[string]any{"remote": []string{"github.com"}},
		},
	})
	require.Equal(t, http.StatusBadRequest, bad.Code, bad.Body.String())
	assert.Contains(t, bad.Body.String(), "does not declare scope dimension")

	notOwnerImplied := humanReq(t, f, http.MethodPatch, "/v1/groups/g1", map[string]any{
		"owner_scopes": map[string]any{
			agentd.PermPermissionsGrant: map[string]any{"group": []string{"g1"}},
		},
	})
	require.Equal(t, http.StatusBadRequest, notOwnerImplied.Code, notOwnerImplied.Body.String())
	assert.Contains(t, notOwnerImplied.Body.String(), "no owner-implied bypass to narrow")

	// A rejected map leaves nothing behind.
	g, err := db.GetAgentGroupByName("g1")
	require.NoError(t, err)
	assert.Empty(t, g.OwnerScopesJSON)

	// And the accepted form round-trips, canonicalized, then clears.
	narrowOwnerBypass(t, f, "g1", map[string]any{
		agentd.PermGroupsMembersSpawn: map[string]any{"spawn_profile": []string{"p2", "p1"}},
	})
	g, err = db.GetAgentGroupByName("g1")
	require.NoError(t, err)
	assert.Equal(t, `{"groups.members.spawn":{"spawn_profile":["p1","p2"]}}`, g.OwnerScopesJSON)

	narrowOwnerBypass(t, f, "g1", map[string]any{})
	g, err = db.GetAgentGroupByName("g1")
	require.NoError(t, err)
	assert.Empty(t, g.OwnerScopesJSON, "an empty map clears the narrowing")
}
