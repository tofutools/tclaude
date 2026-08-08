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

// spawnWithProfile posts a spawn naming a profile, as `tclaude agent spawn
// --profile` does.
func spawnWithProfile(t *testing.T, f *testharness.Flow, caller, group, name, profile string) *httpResult {
	t.Helper()
	body := map[string]any{"name": name}
	if profile != "" {
		body["profile"] = profile
	}
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/groups/"+group+"/spawn", body)
	return &httpResult{Code: rec.Code, Body: rec.Body.String()}
}

// spawnConferring posts a spawn that mints birth-time permission overrides on
// the child — the minting surface attenuation guards.
func spawnConferring(t *testing.T, f *testharness.Flow, caller, group, name string,
	overrides map[string]any) *httpResult {
	t.Helper()
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/groups/"+group+"/spawn",
		map[string]any{"name": name, "permission_overrides": overrides})
	return &httpResult{Code: rec.Code, Body: rec.Body.String()}
}

// Scenario: the operator pins a lead's spawn capability to ONE spawn profile.
// The lead may launch workers with that profile and nothing else — not the
// other registry profile, and not an inline launch shape that names no profile
// at all.
//
// The inline case is the decision worth pinning: an inline shape is not a
// named profile, so it leaves the dimension undescribed and fails closed. The
// alternative — treating "no profile" as "any profile" — would make a
// profile-pinned grant trivially escapable by omitting the flag.
func TestAttenuation_SpawnProfileScopedGrantGatesPerProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "p1"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "p2"})
	require.NoError(t, err)

	const lead = "atten-lead-aaaa-bbbb-cccc-000000000001"
	haveSpawnCapableMember(t, f, "alpha", lead)
	grantScoped(t, f, lead, agentd.PermGroupsSpawn, map[string]any{"spawn_profile": []string{"p1"}})

	allowed := spawnWithProfile(t, f, lead, "alpha", "p1-worker", "p1")
	require.Equal(t, http.StatusOK, allowed.Code, allowed.Body)

	other := spawnWithProfile(t, f, lead, "alpha", "p2-worker", "p2")
	assert.Equal(t, http.StatusForbidden, other.Code, other.Body)

	inline := spawnWithProfile(t, f, lead, "alpha", "inline-worker", "")
	assert.Equal(t, http.StatusForbidden, inline.Code, inline.Body)
	assert.Contains(t, inline.Body, agentd.PermGroupsSpawn,
		"an unnamed profile is the ordinary not-granted path, naming the slug")
}

// The group's DEFAULT profile is a real named profile, so a spawn that names
// none still resolves to it — and a grant pinned to it admits that spawn. This
// is why the gate judges the RESOLVED profile rather than the request field:
// pinning to the raw field would refuse a spawn that genuinely uses p1.
func TestAttenuation_SpawnProfileScopeSeesTheGroupDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "groupdefault"})
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile("alpha", "groupdefault")
	require.NoError(t, err)

	const lead = "atten-lead-aaaa-bbbb-cccc-000000000002"
	haveSpawnCapableMember(t, f, "alpha", lead)
	grantScoped(t, f, lead, agentd.PermGroupsSpawn,
		map[string]any{"spawn_profile": []string{"groupdefault"}})

	allowed := spawnWithProfile(t, f, lead, "alpha", "default-worker", "")
	assert.Equal(t, http.StatusOK, allowed.Code, allowed.Body)
}

// The attenuation matrix at the spawn boundary. One lead, whose own
// groups.spawn is pinned to two groups, tries to mint that slug onto children
// at various widths.
func TestAttenuation_SpawnConferringWiderThanHeldIsRefused(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "atten-lead-aaaa-bbbb-cccc-000000000003"
	haveSpawnCapableMember(t, f, "alpha", lead)
	// Unscoped groups.spawn so the spawn itself always clears the gate — this
	// test is about what the spawn CONFERS, not whether it may spawn.
	grantScoped(t, f, lead, agentd.PermGroupsSpawn, nil)
	grantScoped(t, f, lead, agentd.PermPermissionsGrant, nil)
	// The slug under test is held NARROWLY.
	grantScoped(t, f, lead, agentd.PermRoutesPublish, map[string]any{"group": []string{"alpha", "beta"}})

	t.Run("equal scope passes", func(t *testing.T) {
		res := spawnConferring(t, f, lead, "alpha", "equal-worker", map[string]any{
			agentd.PermRoutesPublish: map[string]any{
				"effect": db.PermEffectGrant,
				"scope":  map[string]any{"group": []string{"alpha", "beta"}},
			},
		})
		assert.Equal(t, http.StatusOK, res.Code, res.Body)
	})

	t.Run("narrower scope passes", func(t *testing.T) {
		res := spawnConferring(t, f, lead, "alpha", "narrow-worker", map[string]any{
			agentd.PermRoutesPublish: map[string]any{
				"effect": db.PermEffectGrant,
				"scope":  map[string]any{"group": []string{"alpha"}},
			},
		})
		assert.Equal(t, http.StatusOK, res.Code, res.Body)
	})

	t.Run("unscoped conferral is refused", func(t *testing.T) {
		res := spawnConferring(t, f, lead, "alpha", "wide-worker", map[string]any{
			agentd.PermRoutesPublish: db.PermEffectGrant,
		})
		require.Equal(t, http.StatusForbidden, res.Code, res.Body)
		assert.Contains(t, res.Body, "scope_not_attenuated")
		assert.Contains(t, res.Body, "UNSCOPED", "the refusal says what was attempted")
	})

	t.Run("a wider value list is refused", func(t *testing.T) {
		res := spawnConferring(t, f, lead, "alpha", "wider-worker", map[string]any{
			agentd.PermRoutesPublish: map[string]any{
				"effect": db.PermEffectGrant,
				"scope":  map[string]any{"group": []string{"alpha", "gamma"}},
			},
		})
		assert.Equal(t, http.StatusForbidden, res.Code, res.Body)
	})

	t.Run("a slug the granter holds unscoped is unconstrained", func(t *testing.T) {
		// groups.spawn is held UNSCOPED above, so conferring it unscoped is
		// exactly the pre-scope behaviour and must keep working.
		res := spawnConferring(t, f, lead, "alpha", "spawner-worker", map[string]any{
			agentd.PermGroupsSpawn: db.PermEffectGrant,
		})
		assert.Equal(t, http.StatusOK, res.Code, res.Body)
	})

	// Nothing widened along the way: no refused conferral left a grant behind.
	for _, m := range f.ListGroupMembers("alpha") {
		if m.ConvID == lead {
			continue
		}
		overrides, err := db.ListAgentPermissionOverrideRowsForConv(m.ConvID)
		require.NoError(t, err)
		for _, row := range overrides {
			if row.Slug == agentd.PermRoutesPublish {
				assert.NotEqual(t, "", row.ScopeJSON,
					"every routes.publish a refusal did not stop must still be scoped")
			}
		}
	}
}

// The same rule at the post-spawn grant endpoint. Also covers the sentinel
// "default" target, which confers a slug to EVERY agent unscoped — the widest
// conferral the daemon can make, and one a scoped granter can never cover.
func TestAttenuation_PermissionsGrantEndpoint(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "atten-lead-aaaa-bbbb-cccc-000000000004"
	const worker = "atten-work-aaaa-bbbb-cccc-000000000004"
	f.HaveMember("alpha", lead)
	f.HaveMember("alpha", worker)
	grantScoped(t, f, lead, agentd.PermPermissionsGrant, nil)
	grantScoped(t, f, lead, agentd.PermRoutesPublish, map[string]any{"group": []string{"alpha"}})

	agentGrant := func(t *testing.T, body map[string]any) *httpResult {
		t.Helper()
		rec := agentReq(t, f, lead, http.MethodPost, "/v1/permissions/grant", body)
		return &httpResult{Code: rec.Code, Body: rec.Body.String()}
	}

	within := agentGrant(t, map[string]any{
		"target": worker, "slug": agentd.PermRoutesPublish,
		"scope": map[string]any{"group": []string{"alpha"}},
	})
	assert.Equal(t, http.StatusOK, within.Code, within.Body)

	wider := agentGrant(t, map[string]any{"target": worker, "slug": agentd.PermRoutesPublish})
	require.Equal(t, http.StatusForbidden, wider.Code, wider.Body)
	assert.Contains(t, wider.Body, "scope_not_attenuated")

	toDefaults := agentGrant(t, map[string]any{"target": "default", "slug": agentd.PermRoutesPublish})
	assert.Equal(t, http.StatusForbidden, toDefaults.Code, toDefaults.Body,
		"a scoped granter cannot launder its slug through the global defaults list")
}

// Capture-as-profile traces a live agent onto the spawn-profile wire shape.
// It must carry the agent's grant SCOPES: a capture that flattened them would
// hand every future spawn of that profile strictly more authority than the
// agent it was captured from.
func TestAttenuation_CaptureAsProfilePreservesScopes(t *testing.T) {
	f := newFlow(t)
	const source = "atten-src-aaaa-bbbb-cccc-000000000005"
	f.HaveConvWithTitle(source, "scoped-source")
	grantScoped(t, f, source, agentd.PermGroupsSpawn, map[string]any{"group": []string{"alpha"}})
	grantScoped(t, f, source, agentd.PermRoutesPublish, nil)

	rec := humanReq(t, f, http.MethodPost, "/v1/spawn-profiles/from-agent",
		map[string]any{"agent": source})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var seed struct {
		PermissionOverrides map[string]db.PermissionOverride `json:"permission_overrides"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &seed))
	assert.Equal(t, `{"group":["alpha"]}`, seed.PermissionOverrides[agentd.PermGroupsSpawn].Scope,
		"the captured profile keeps the narrowing the live grant had")
	assert.Equal(t, db.Grant(), seed.PermissionOverrides[agentd.PermRoutesPublish],
		"an unscoped grant still captures as the plain unscoped form")
}

// Snapshot-as-template traces a live group onto a blueprint. Like
// capture-as-profile it must carry each member's grant SCOPES: flattening them
// would make every deploy of the snapshot hand out strictly more authority
// than the group it was traced from.
func TestAttenuation_SnapshotAsTemplatePreservesScopes(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("src")
	const member = "atten-memb-aaaa-bbbb-cccc-000000000006"
	haveSpawnCapableMember(t, f, "src", member)
	grantScoped(t, f, member, agentd.PermRoutesPublish, map[string]any{"group": []string{"src"}})

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/from-group",
		map[string]any{"group": "src", "template_name": "src-tmpl"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	tmpl, err := db.GetGroupTemplate("src-tmpl")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	require.Len(t, tmpl.Agents, 1)
	inline := tmpl.Agents[0].ProfileInline
	require.NotNil(t, inline, "the traced grants land in the template-local profile")
	assert.Equal(t, `{"group":["src"]}`, inline.PermissionOverrides[agentd.PermRoutesPublish].Scope,
		"the snapshot keeps the narrowing the live grant had")
}

// A template deploy is a minting surface too: its roster confers birth-time
// grants. An agent whose own hold is scoped must not be able to deploy a
// template that hands a worker the same slug unscoped — and the refusal has to
// land BEFORE anything spawns, since the background wave runner is far too
// late to take it back.
func TestAttenuation_TemplateDeployRefusesWiderRoster(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("home")
	const lead = "atten-lead-aaaa-bbbb-cccc-000000000007"
	haveSpawnCapableMember(t, f, "home", lead)
	grantScoped(t, f, lead, agentd.PermTemplatesUse, nil)
	grantScoped(t, f, lead, agentd.PermRoutesPublish, map[string]any{"group": []string{"home"}})

	// The template hands its worker routes.publish UNSCOPED, through the legacy
	// inline grant list.
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "wide-crew",
			"agents": []map[string]any{
				{"name": "worker", "role": "worker", "permissions": []string{agentd.PermRoutesPublish}},
			},
		}).Code)

	rec := agentReq(t, f, lead, http.MethodPost, "/v1/templates/wide-crew/instantiate",
		map[string]any{"group_name": "deployed", "task": "t"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "scope_not_attenuated")

	// Refused up front: no group, so nothing to take back.
	g, err := db.GetAgentGroupByName("deployed")
	require.NoError(t, err)
	assert.Nil(t, g, "a refused deploy must not create its group")
}

// A role's and a template agent's default-grant lists carry optional scopes.
// The list's unscoped arm is still the bare slug every stored blueprint holds,
// so both arms have to survive the create → read round-trip.
func TestAttenuation_BlueprintGrantListsCarryScopes(t *testing.T) {
	f := newFlow(t)

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/roles",
		map[string]any{
			"name": "publisher",
			"permissions": []any{
				agentd.PermHumanNotify,
				map[string]any{
					"slug":  agentd.PermRoutesPublish,
					"scope": map[string]any{"group": []string{"alpha"}},
				},
			},
		}).Code)

	rec := humanReq(t, f, http.MethodGet, "/v1/roles/publisher", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var role struct {
		Permissions []db.PermissionGrant `json:"permissions"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &role))
	byslug := map[string]db.PermissionGrant{}
	for _, g := range role.Permissions {
		byslug[g.Slug] = g
	}
	assert.Equal(t, "", byslug[agentd.PermHumanNotify].Scope, "a bare slug stays unscoped")
	assert.Equal(t, `{"group":["alpha"]}`, byslug[agentd.PermRoutesPublish].Scope)

	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name": "scoped-crew",
			"agents": []map[string]any{{
				"name": "worker", "role": "worker",
				"permissions": []any{map[string]any{
					"slug":  agentd.PermRoutesPublish,
					"scope": map[string]any{"group": []string{"alpha"}},
				}},
			}},
		}).Code)

	tmpl, err := db.GetGroupTemplate("scoped-crew")
	require.NoError(t, err)
	require.Len(t, tmpl.Agents, 1)
	require.Len(t, tmpl.Agents[0].Permissions, 1)
	assert.Equal(t, `{"group":["alpha"]}`, tmpl.Agents[0].Permissions[0].Scope,
		"the stored blueprint keeps its narrowing")

	// A scope naming a dimension its slug does not declare is refused at the
	// wire boundary, exactly as a scoped per-agent grant is.
	bad := humanReq(t, f, http.MethodPost, "/v1/roles", map[string]any{
		"name": "bogus",
		"permissions": []any{map[string]any{
			"slug": agentd.PermHumanNotify, "scope": map[string]any{"group": []string{"alpha"}},
		}},
	})
	assert.Equal(t, http.StatusBadRequest, bad.Code, bad.Body.String())
}
