package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-350 / JOH-354: a template agent's launch config becomes "pick a stored
// spawn profile", and — the heart of JOH-354's close — the referenced profile's
// birth-time PERMISSIONS + owner default now actually ride onto the spawned
// agent at deploy, not just its launch fields. These flow tests assert that at
// the real access surfaces (group ownership + per-conv permission overrides),
// plus the composition precedence and the legacy-inline compatibility path.

// Scenario: a profile carries is_owner + a grant AND a deny override; a template
// agent references it and nothing else. Instantiating must make the agent a
// group owner and apply BOTH overrides to the spawned conv — the profile is the
// single unit of launch config + access.
func TestGroupTemplate_ProfileCarriesPermissionsAndOwnerAtDeploy(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name":     "lead-kit",
		"model":    "haiku",
		"is_owner": true,
		"permission_overrides": map[string]any{
			agentd.PermGroupsSpawn:   "grant",
			agentd.PermMessageDirect: "deny",
		},
	}).Code, "create profile")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "crew",
		"agents": []map[string]any{
			{"name": "lead", "spawn_profile": "lead-kit"},
		},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/crew/instantiate",
		map[string]any{"group_name": "voyage"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	leadConv := res.Agents[0].ConvID
	require.NotEmpty(t, leadConv)
	// The per-agent result reports the owner + the granted slug (the deny is not
	// a "grant", so it is not listed there).
	assert.True(t, res.Agents[0].Owner, "result reports the profile's owner default")
	assert.Contains(t, res.Agents[0].Granted, agentd.PermGroupsSpawn, "result reports the profile grant")

	// Real surfaces: the profile launched the agent (its model), owns the group,
	// and both overrides landed on the conv.
	model, ok := f.World.SpawnModel(leadConv)
	require.True(t, ok)
	assert.Equal(t, "haiku", model, "the profile's launch fields still apply")

	g, err := db.GetAgentGroupByName("voyage")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.True(t, ownsGroup(t, g.ID, leadConv), "the profile's is_owner made the agent a group owner")

	overrides, err := db.ListAgentPermissionOverridesForConv(leadConv)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermGroupsSpawn], "profile grant applied at deploy")
	assert.Equal(t, "deny", overrides[agentd.PermMessageDirect], "profile deny applied at deploy")
}

// Scenario: composition precedence. A profile DENIES a slug; the agent's own
// inline permission list GRANTS the same slug. The inline grant is the highest
// tier, so it wins — the spawned conv ends up granted, not denied.
func TestGroupTemplate_InlineGrantWinsOverProfileDeny(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "locked",
		"permission_overrides": map[string]any{
			agentd.PermGroupsSpawn: "deny",
		},
	}).Code, "create profile")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "team",
		"agents": []map[string]any{
			// Inline grant of the slug the profile denies — inline wins.
			{"name": "lead", "spawn_profile": "locked", "permissions": []string{agentd.PermGroupsSpawn}},
		},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/instantiate",
		map[string]any{"group_name": "g1"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	agentd.WaitForBackgroundForTest()

	overrides, err := db.ListAgentPermissionOverridesForConv(res.Agents[0].ConvID)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermGroupsSpawn],
		"the agent's inline grant overrides the profile's deny")
}

// Scenario: composition precedence, the other direction. A ROLE grants a slug
// (its default), the agent's profile DENIES it, and there is no inline grant.
// The profile override is a higher tier than the role default, so the spawned
// conv ends up DENIED — the behaviour-changing path of the new tri-state
// composition (pre-cutover a role grant could never be turned into a deny).
func TestGroupTemplate_ProfileDenyOverridesRoleGrant(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createRole(t, f, map[string]any{
		"name":        "spawner",
		"permissions": []string{agentd.PermGroupsSpawn},
	}).Code, "create role")
	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "no-spawn",
		"permission_overrides": map[string]any{
			agentd.PermGroupsSpawn: "deny",
		},
	}).Code, "create profile")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "team",
		"agents": []map[string]any{
			// role_ref grants groups.spawn, profile denies it, no inline override.
			{"name": "lead", "role_ref": "spawner", "spawn_profile": "no-spawn"},
		},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/instantiate",
		map[string]any{"group_name": "g1"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	overrides, err := db.ListAgentPermissionOverridesForConv(res.Agents[0].ConvID)
	require.NoError(t, err)
	assert.Equal(t, "deny", overrides[agentd.PermGroupsSpawn],
		"the profile's deny overrides the role's default grant")
	// And the deny is not reported as a grant on the per-agent result.
	assert.NotContains(t, res.Agents[0].Granted, agentd.PermGroupsSpawn,
		"a denied slug is not listed as granted")
}

// Scenario: LEGACY compatibility (no migration). A template authored before the
// profile-picker cutover carries inline permissions + is_owner and NO profile.
// Those must still apply at deploy — the backend honours the inline fields so a
// pre-cutover template (or a bundled starter that still lists inline grants)
// keeps deploying its escalated leads correctly.
func TestGroupTemplate_LegacyInlinePermsAndOwnerStillApplyAtDeploy(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "legacy",
		"agents": []map[string]any{
			{"name": "lead", "is_owner": true, "permissions": []string{agentd.PermGroupsSpawn}},
		},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/legacy/instantiate",
		map[string]any{"group_name": "old"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	agentd.WaitForBackgroundForTest()

	leadConv := res.Agents[0].ConvID
	g, err := db.GetAgentGroupByName("old")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.True(t, ownsGroup(t, g.ID, leadConv), "inline is_owner still makes the agent a group owner")
	overrides, err := db.ListAgentPermissionOverridesForConv(leadConv)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermGroupsSpawn], "inline permission grant still applies")
}

// exportEnvelopeRaw decodes an export as a generic map so a test can re-POST the
// WHOLE envelope (including the embedded profiles/roles arrays) to import,
// faithfully round-tripping every field the daemon emitted.
func exportRaw(t *testing.T, f *testharness.Flow, name string) map[string]any {
	t.Helper()
	rec := humanReq(t, f, http.MethodGet, "/v1/templates/"+name+"/export", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "export: %s", rec.Body.String())
	var raw map[string]any
	testharness.DecodeJSON(t, rec, &raw)
	return raw
}

// Scenario (JOH-350 portability): a template references a spawn profile. Export
// must embed the full profile definition; importing on a machine where that
// profile is MISSING must materialize it by name (so the imported template is
// instantiable), and importing where a same-named profile already EXISTS must
// keep the local one and warn (sacred edits) — mirroring the roles embed rule.
func TestTemplateExportImport_EmbedsAndMaterializesProfile(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name":     "shipped-kit",
		"model":    "haiku",
		"is_owner": true,
		"permission_overrides": map[string]any{
			agentd.PermGroupsSpawn: "grant",
		},
	}).Code, "create profile")
	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "shipper",
		"agents": []map[string]any{{"name": "lead", "spawn_profile": "shipped-kit"}},
	}).Code, "create template")

	env := exportRaw(t, f, "shipper")
	profiles, _ := env["profiles"].([]any)
	require.Len(t, profiles, 1, "export embeds the referenced profile")
	p0, _ := profiles[0].(map[string]any)
	assert.Equal(t, "shipped-kit", p0["name"], "embedded profile carries its definition")
	assert.Equal(t, true, p0["is_owner"], "embedded profile carries its owner default")

	// Simulate importing on a machine that LACKS the profile: delete it here,
	// then re-import under a fresh name. The importer must materialize it.
	_, err := db.DeleteSpawnProfile("shipped-kit")
	require.NoError(t, err, "delete local profile to simulate a fresh machine")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/import?as=shipper2", env)
	require.Equalf(t, http.StatusCreated, rec.Code, "import (missing profile): %s", rec.Body.String())
	var ir tmplImportResult
	testharness.DecodeJSON(t, rec, &ir)
	assert.Contains(t, ir.Warnings, `imported spawn profile "shipped-kit"`, "materialized the missing profile")

	got, err := db.GetSpawnProfile("shipped-kit")
	require.NoError(t, err)
	require.NotNil(t, got, "profile materialized on import")
	require.NotNil(t, got.IsOwner)
	assert.True(t, *got.IsOwner, "materialized profile keeps its owner default")
	assert.Equal(t, "grant", got.PermissionOverrides[agentd.PermGroupsSpawn], "materialized profile keeps its overrides")

	// The imported template's agent still references the profile by name (the
	// materialized one), so it is instantiable.
	tmpl, err := db.GetGroupTemplate("shipper2")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	require.Len(t, tmpl.Agents, 1)
	assert.Equal(t, "shipped-kit", tmpl.Agents[0].SpawnProfile, "profile reference preserved on import")

	// Now re-import AGAIN (profile now exists locally, possibly user-edited): the
	// importer must KEEP the local profile and warn, never overwrite.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/import?as=shipper3", env)
	require.Equalf(t, http.StatusCreated, rec.Code, "import (existing profile): %s", rec.Body.String())
	testharness.DecodeJSON(t, rec, &ir)
	assert.Contains(t, ir.Warnings, `spawn profile "shipped-kit" already exists here — kept the local version (import never overwrites a profile)`,
		"existing profile kept local + warned")
}
