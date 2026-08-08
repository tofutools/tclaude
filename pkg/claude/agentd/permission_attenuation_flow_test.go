package agentd_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

// A selector names a set relative to the agent that receives it. Structural
// equality is therefore sufficient only when the conferee is the granter or
// already lies in the granter's tree: only then is the conferee's descendant
// set guaranteed to be a subset of the granter's.
func TestAttenuation_SelectorConferralRequiresConfereeInDescendants(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "atten-selector-lead-aaaa-bbbb-cccc-00000001"
	const child = "atten-selector-child-aaaa-bbbb-cccc-0000001"
	const unrelated = "atten-selector-peer-aaaa-bbbb-cccc-00000001"
	f.HaveMember("alpha", lead)
	f.HaveMember("alpha", child)
	f.HaveMember("alpha", unrelated)

	leadID, err := db.AgentIDForConv(lead)
	require.NoError(t, err)
	childID, err := db.AgentIDForConv(child)
	require.NoError(t, err)
	require.NoError(t, db.RecordAgentLineage(childID, leadID, time.Now()))

	grantScoped(t, f, lead, agentd.PermPermissionsGrant, nil)
	grantScoped(t, f, lead, agentd.PermAgentRetire,
		map[string]any{"target_agent": []string{"@descendants"}})

	agentGrant := func(target, selector string) *httpResult {
		rec := agentReq(t, f, lead, http.MethodPost, "/v1/permissions/grant", map[string]any{
			"target": target,
			"slug":   agentd.PermAgentRetire,
			"scope":  map[string]any{"target_agent": []string{selector}},
		})
		return &httpResult{Code: rec.Code, Body: rec.Body.String()}
	}

	descendant := agentGrant(child, "@descendants")
	assert.Equal(t, http.StatusOK, descendant.Code, descendant.Body)

	self := agentGrant(lead, "@descendants")
	assert.Equal(t, http.StatusOK, self.Code, self.Body)

	outside := agentGrant(unrelated, "@descendants")
	require.Equal(t, http.StatusForbidden, outside.Code, outside.Body)
	assert.Contains(t, outside.Body, "scope_not_attenuated")
	assert.Contains(t, outside.Body, "outside", "the refusal names the lineage reason")

	// @self-spawned has a smaller relative set but the same containment
	// requirement when it is moved onto an out-of-tree conferee.
	grantScoped(t, f, lead, agentd.PermAgentRetire,
		map[string]any{"target_agent": []string{"@self-spawned"}})
	selfSpawnedOutside := agentGrant(unrelated, "@self-spawned")
	require.Equal(t, http.StatusForbidden, selfSpawnedOutside.Code, selfSpawnedOutside.Body)
	assert.Contains(t, selfSpawnedOutside.Body, "scope_not_attenuated")

	// An unscoped hold still covers every conferred shape, exactly as before.
	grantScoped(t, f, lead, agentd.PermAgentRetire, nil)
	unscoped := agentGrant(unrelated, "@descendants")
	assert.Equal(t, http.StatusOK, unscoped.Code, unscoped.Body)

	// The operator is not an attenuation subject and may place the selector on
	// any agent directly.
	human := humanReq(t, f, http.MethodPost, "/v1/permissions/grant", map[string]any{
		"target": unrelated,
		"slug":   agentd.PermAgentRetire,
		"scope":  map[string]any{"target_agent": []string{"@self-spawned"}},
	})
	assert.Equal(t, http.StatusOK, human.Code, human.Body.String())
}

// Spawn attenuation runs before the child actor and lineage edge exist. The
// path remains sound because enrollment writes that edge before applying the
// authorized birth-time overrides and fails if it cannot do so.
func TestAttenuation_SpawnConfersSelectorToDescendantByConstruction(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "atten-selector-spawn-lead-aaaa-bbbb-cccc-0001"
	haveSpawnCapableMember(t, f, "alpha", lead)
	grantScoped(t, f, lead, agentd.PermGroupsSpawn, nil)
	grantScoped(t, f, lead, agentd.PermPermissionsGrant, nil)
	grantScoped(t, f, lead, agentd.PermAgentRetire,
		map[string]any{"target_agent": []string{"@descendants"}})

	res := spawnConferring(t, f, lead, "alpha", "selector-worker", map[string]any{
		agentd.PermAgentRetire: map[string]any{
			"effect": db.PermEffectGrant,
			"scope":  map[string]any{"target_agent": []string{"@descendants"}},
		},
	})
	require.Equal(t, http.StatusOK, res.Code, res.Body)

	leadID, err := db.AgentIDForConv(lead)
	require.NoError(t, err)
	var childConv string
	for _, member := range f.ListGroupMembers("alpha") {
		if member.Title == "selector-worker" {
			childConv = member.ConvID
			break
		}
	}
	require.NotEmpty(t, childConv, "spawned child is enrolled")
	childID, err := db.AgentIDForConv(childConv)
	require.NoError(t, err)
	descendant, err := db.IsAgentDescendant(leadID, childID)
	require.NoError(t, err)
	assert.True(t, descendant, "the promised lineage edge exists before the spawn completes")

	rows, err := db.ListAgentPermissionOverrideRowsForConv(childConv)
	require.NoError(t, err)
	found := false
	for _, row := range rows {
		if row.Slug == agentd.PermAgentRetire {
			found = true
			assert.Equal(t, `{"target_agent":["@descendants"]}`, row.ScopeJSON)
		}
	}
	assert.True(t, found, "the selector-bearing birth-time grant was applied")
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

// The attenuation rule answers "unconstrained" when the granter holds no scope
// for a slug, because permissions.grant is recursive by design. That is only
// safe if a granter cannot MANUFACTURE the unheld state for itself — otherwise
// every scoped agent shreds its own narrowing and re-grants wide, and the rule
// has no force at all.
//
// Two shedding routes, both closed:
//   - deny yourself the slug, then re-grant it unscoped;
//   - revoke your own scoped row, then re-grant it unscoped.
func TestAttenuation_GranterCannotShedItsOwnScope(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	const lead = "atten-lead-aaaa-bbbb-cccc-000000000008"
	f.HaveMember("alpha", lead)
	grantScoped(t, f, lead, agentd.PermPermissionsGrant, nil)
	grantScoped(t, f, lead, agentd.PermPermissionsRevoke, nil)
	grantScoped(t, f, lead, agentd.PermRoutesPublish, map[string]any{"group": []string{"alpha"}})

	agentPost := func(t *testing.T, verb string, body map[string]any) *httpResult {
		t.Helper()
		rec := agentReq(t, f, lead, http.MethodPost, "/v1/permissions/"+verb, body)
		return &httpResult{Code: rec.Code, Body: rec.Body.String()}
	}

	// Route 1: self-deny is allowed (it only reduces authority) — but it does
	// NOT unlock a wider re-grant, because a deny confers nothing.
	denied := agentPost(t, "deny", map[string]any{"target": lead, "slug": agentd.PermRoutesPublish})
	require.Equal(t, http.StatusOK, denied.Code, denied.Body)

	regrant := agentPost(t, "grant", map[string]any{"target": lead, "slug": agentd.PermRoutesPublish})
	require.Equal(t, http.StatusForbidden, regrant.Code, regrant.Body)
	assert.Contains(t, regrant.Body, "scope_not_attenuated")

	// Route 2: revoking your own SCOPED grant is refused outright.
	require.Equal(t, http.StatusOK,
		postPermissionScope(t, f, "revoke", map[string]any{"target": lead, "slug": agentd.PermRoutesPublish}).Code,
		"operator clears the deny")
	grantScoped(t, f, lead, agentd.PermRoutesPublish, map[string]any{"group": []string{"alpha"}})

	shed := agentPost(t, "revoke", map[string]any{"target": lead, "slug": agentd.PermRoutesPublish})
	require.Equal(t, http.StatusForbidden, shed.Code, shed.Body)
	assert.Contains(t, shed.Body, "scope_not_attenuated")

	// The narrowing is still in place after both attempts.
	rows, err := db.ListAgentPermissionOverrideRowsForConv(lead)
	require.NoError(t, err)
	found := false
	for _, row := range rows {
		if row.Slug == agentd.PermRoutesPublish {
			found = true
			assert.Equal(t, db.PermEffectGrant, row.Effect)
			assert.Equal(t, `{"group":["alpha"]}`, row.ScopeJSON)
		}
	}
	assert.True(t, found, "the scoped grant survived both shedding attempts")

	// Revoking a slug held UNSCOPED is untouched — this guard is about scopes,
	// not about self-service permission management in general.
	ok := agentPost(t, "revoke", map[string]any{"target": lead, "slug": agentd.PermPermissionsRevoke})
	assert.Equal(t, http.StatusOK, ok.Code, ok.Body)
}

// A group grant is conferred on every member and is written UNSCOPED, so it is
// a minting surface: an agent whose own hold is scoped must not be able to add
// that slug to a group it belongs to and resolve unscoped on the next request
// (the group tier unions its rows, and one unscoped row absorbs the tier).
//
// The endpoint gates on groups.rename, which is NOT in the permission registry
// today — so no API grant can hand it to an agent and the path is effectively
// human-only right now. This grants it at the storage layer, which is what the
// daemon would resolve if the slug were ever registered, so the check is under
// test before that day rather than after it.
func TestAttenuation_GroupPermissionsPatchIsAttenuated(t *testing.T) {
	f := newFlow(t)
	group := f.HaveGroup("alpha")
	const lead = "atten-lead-aaaa-bbbb-cccc-000000000009"
	f.HaveMember("alpha", lead)
	require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsRename, "<test>"))
	grantScoped(t, f, lead, agentd.PermPermissionsGrant, nil)
	grantScoped(t, f, lead, agentd.PermPermissionsRevoke, nil)
	grantScoped(t, f, lead, agentd.PermRoutesPublish, map[string]any{"group": []string{"alpha"}})

	rec := agentReq(t, f, lead, http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"permissions": []string{agentd.PermRoutesPublish}})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "scope_not_attenuated")

	grants, err := db.ListAgentGroupPermissionRows(group.ID)
	require.NoError(t, err)
	assert.Empty(t, grants, "a refused PATCH writes no group grant")
}

// agent_profiles pins a registry profile onto roster members that carry no
// launch config of their own, and that profile's permission_overrides confer
// grants the stored template never mentioned. The deploy check has to judge
// the composed roster, not the template as stored.
func TestAttenuation_TemplateDeployChecksDeployTimeProfileOverrides(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("home")
	const lead = "atten-lead-aaaa-bbbb-cccc-000000000010"
	haveSpawnCapableMember(t, f, "home", lead)
	grantScoped(t, f, lead, agentd.PermTemplatesUse, nil)
	grantScoped(t, f, lead, agentd.PermRoutesPublish, map[string]any{"group": []string{"home"}})

	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name:                "wide",
		PermissionOverrides: db.UnscopedOverrides(map[string]string{agentd.PermRoutesPublish: db.PermEffectGrant}),
	})
	require.NoError(t, err)

	// The stored template grants nothing at all.
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates",
		map[string]any{
			"name":   "bare-crew",
			"agents": []map[string]any{{"name": "worker", "role": "worker"}},
		}).Code)

	rec := agentReq(t, f, lead, http.MethodPost, "/v1/templates/bare-crew/instantiate",
		map[string]any{
			"group_name":     "deployed-wide",
			"task":           "t",
			"agent_profiles": map[string]any{"worker": "wide"},
		})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "scope_not_attenuated")

	g, err := db.GetAgentGroupByName("deployed-wide")
	require.NoError(t, err)
	assert.Nil(t, g, "a refused deploy must not create its group")
}

// Scenario: birth-time overrides now ride the profile tier stack, so a profile
// the caller never mentioned can confer grants. Attenuation runs on the
// RESOLVED map, and splits the same way the permissions.grant gate beside it
// does: direct intent is refused loudly, an ambient DEFAULT profile is dropped
// and disclosed.
//
// The asymmetry is the point. Refusing on a group default would mean an
// operator's house profile silently breaks every spawn its scoped agents make,
// for a conferral nobody typed at that launch. Dropping it only ever narrows
// what the child is born with, so the fail-closed direction is preserved.
func TestAttenuation_SpawnProfileTierConferralIsAttenuated(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("alpha")
	const lead = "atten-lead-aaaa-bbbb-cccc-000000000009"
	haveSpawnCapableMember(t, f, "alpha", lead)
	grantScoped(t, f, lead, agentd.PermGroupsSpawn, nil)
	grantScoped(t, f, lead, agentd.PermPermissionsGrant, nil)
	// Held narrowly; every profile below tries to confer it UNSCOPED.
	grantScoped(t, f, lead, agentd.PermRoutesPublish, map[string]any{"group": []string{"alpha"}})

	wide := map[string]db.PermissionOverride{agentd.PermRoutesPublish: db.Grant()}
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "wide-named", PermissionOverrides: wide})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "wide-default", PermissionOverrides: wide})
	require.NoError(t, err)

	// A profile the caller NAMES is direct intent, exactly like an explicit
	// permission_overrides field, and is refused.
	t.Run("a named profile that over-confers is refused", func(t *testing.T) {
		rec := agentReq(t, f, lead, http.MethodPost, "/v1/groups/alpha/spawn",
			map[string]any{"name": "named-worker", "profile": "wide-named"})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "scope_not_attenuated")
	})

	// The same profile as the group's default is ambient configuration. The
	// spawn proceeds; the over-wide conferral is what gets dropped.
	t.Run("a group default that over-confers is dropped, not refused", func(t *testing.T) {
		require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "wide-default").Code,
			"set default_profile")
		rec := agentReq(t, f, lead, http.MethodPost, "/v1/groups/alpha/spawn",
			map[string]any{"name": "default-worker"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// The child exists and did NOT come up holding the unscoped grant.
		var child string
		for _, m := range f.ListGroupMembers(g.Name) {
			if m.Title == "default-worker" {
				child = m.ConvID
			}
		}
		require.NotEmpty(t, child, "the spawn produced a member")
		rows, err := db.ListAgentPermissionOverrideRowsForConv(child)
		require.NoError(t, err)
		for _, row := range rows {
			assert.NotEqualf(t, agentd.PermRoutesPublish, row.Slug,
				"the default profile's unscoped %s must not have been conferred",
				agentd.PermRoutesPublish)
		}
	})
}

// Scenario: the spawn body is decoded BEFORE the groups.spawn gate, because
// the gate has to judge the profile the request asks for. That decode must not
// infer "no body" from Content-Length: a chunked request carries -1, and
// reading that as empty would hand the handler a zero-valued SpawnRequest —
// spawning with defaults while silently discarding the caller's name, profile
// and permission_overrides, and leaving the spawn_profile dimension to be
// judged on a profile the request never got to state.
//
// httptest.NewRequest sets a real Content-Length, so the chunked case is
// reproduced the way the handler sees it: ContentLength -1 with the body still
// readable.
func TestAttenuation_SpawnBodySurvivesUnknownContentLength(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "chunked-p1"})
	require.NoError(t, err)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "chunked-p2"})
	require.NoError(t, err)

	const lead = "atten-lead-aaaa-bbbb-cccc-000000000010"
	haveSpawnCapableMember(t, f, "alpha", lead)
	// groups.spawn pinned to ONE profile — the dimension the decode feeds.
	grantScoped(t, f, lead, agentd.PermGroupsSpawn,
		map[string]any{"spawn_profile": []string{"chunked-p1"}})

	chunkedSpawn := func(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		r := agentd.AsAgentPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/groups/alpha/spawn", body), lead)
		// What net/http hands a handler for a chunked request.
		r.ContentLength = -1
		return testharness.Serve(f.Mux, r)
	}

	t.Run("the stated profile is still what the gate judges", func(t *testing.T) {
		// Refused because the body SAID chunked-p2 — not silently accepted as a
		// profile-less spawn, and not refused for the wrong reason.
		rec := chunkedSpawn(t, map[string]any{"name": "chunk-denied", "profile": "chunked-p2"})
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})

	t.Run("a chunked spawn keeps the fields it sent", func(t *testing.T) {
		rec := chunkedSpawn(t, map[string]any{"name": "chunk-worker", "profile": "chunked-p1"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var named bool
		for _, m := range f.ListGroupMembers("alpha") {
			if m.Title == "chunk-worker" {
				named = true
			}
		}
		assert.True(t, named,
			"the name in the chunked body reached the spawn instead of being dropped")
	})
}
