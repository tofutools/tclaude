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

// JOH-239: per-role launch profiles in group templates. A template can give
// each role a distinct launch shape — inline overrides and/or a referenced
// spawn profile — instead of every spawned agent inheriting one default. These
// flow tests assert the resolved launch fields at real surfaces: the Spawner
// boundary (SpawnModel/SpawnEffort, where the --model/--effort flags are built)
// for instantiate, and the stored template for the from-group round-trip.

// Scenario: a 2-role template — a lead with INLINE model+effort overrides and a
// tester that REFERENCES a cheap spawn profile — instantiates so each agent is
// spawned with its own distinct resolved model/effort. This is the core win:
// the whole task force no longer runs on one model.
func TestGroupTemplate_PerRoleLaunchProfiles_DistinctModels(t *testing.T) {
	f := newFlow(t)

	// A cheap profile the tester role points at by name.
	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "cheap", "model": "haiku"}).Code,
		"create profile")

	createBody := map[string]any{
		"name": "feature-team",
		"agents": []map[string]any{
			// Lead: inline overrides win, no profile reference.
			{"name": "lead", "role": "lead", "model": "opus", "effort": "high"},
			// Tester: inherits the referenced profile's model, no inline override.
			{"name": "tester", "role": "qa", "spawn_profile": "cheap"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/feature-team/instantiate",
		map[string]any{"group_name": "phoenix"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 2, res.Spawned, "both roles spawned")
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	// Drain the queued brief/welcome deliveries before any later DB read.
	agentd.WaitForBackgroundForTest()

	convByName := map[string]string{}
	for _, a := range res.Agents {
		require.Emptyf(t, a.Error, "agent %s spawned cleanly", a.Name)
		convByName[a.Name] = a.ConvID
	}
	require.NotEmpty(t, convByName["lead"], "lead conv-id")
	require.NotEmpty(t, convByName["tester"], "tester conv-id")

	// The lead spawned on its inline overrides.
	leadModel, ok := f.World.SpawnModel(convByName["lead"])
	require.Truef(t, ok, "no spawn recorded for lead conv %s", convByName["lead"])
	assert.Equal(t, "opus", leadModel, "lead spawns on its inline model")
	leadEffort, _ := f.World.SpawnEffort(convByName["lead"])
	assert.Equal(t, "high", leadEffort, "lead spawns on its inline effort")

	// The tester spawned on the referenced profile's model — and, crucially, a
	// DIFFERENT model than the lead.
	testerModel, ok := f.World.SpawnModel(convByName["tester"])
	require.Truef(t, ok, "no spawn recorded for tester conv %s", convByName["tester"])
	assert.Equal(t, "haiku", testerModel, "tester inherits the referenced profile's model")
	assert.NotEqual(t, leadModel, testerModel, "the two roles resolved to distinct models")
	testerEffort, _ := f.World.SpawnEffort(convByName["tester"])
	assert.Equal(t, "", testerEffort, "tester's profile sets no effort, so none is threaded")
}

// Scenario: an inline model override wins over the referenced profile — the
// per-agent field is the highest-precedence tier.
func TestGroupTemplate_PerRoleLaunchProfiles_InlineOverridesProfile(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "base", "model": "haiku"}).Code, "create profile")

	createBody := map[string]any{
		"name": "team",
		"agents": []map[string]any{
			// References "base" (haiku) but pins an inline model — inline wins.
			{"name": "lead", "spawn_profile": "base", "model": "opus"},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/instantiate",
		map[string]any{"group_name": "g1"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	agentd.WaitForBackgroundForTest()

	got, ok := f.World.SpawnModel(res.Agents[0].ConvID)
	require.True(t, ok)
	assert.Equal(t, "opus", got, "the inline override wins over the referenced profile")
}

// Scenario (TCL-311): template instantiation follows direct spawn's
// least-surprise doctrine. The explicit harness resolves first, then each
// foreign-profile field participates independently: a compatible effort is
// inherited while an incompatible model is skipped and disclosed.
func TestGroupTemplate_LaunchResolution_ForeignProfileValidateOrSkip(t *testing.T) {
	f := newFlow(t)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-kit", "harness": "codex", "model": "gpt-5", "effort": "high",
	}).Code)
	require.Equal(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "mixed-team",
		"agents": []map[string]any{{
			"name": "worker", "harness": "claude", "spawn_profile": "codex-kit",
		}},
	}).Code)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/mixed-team/instantiate",
		map[string]any{"group_name": "mixed"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	require.Len(t, res.Agents, 1)

	conv := res.Agents[0].ConvID
	modelName, ok := f.World.SpawnModel(conv)
	require.True(t, ok)
	assert.Empty(t, modelName, "foreign Codex model is invalid for Claude and must be skipped")
	effort, ok := f.World.SpawnEffort(conv)
	require.True(t, ok)
	assert.Equal(t, "high", effort, "foreign-tier effort is valid for Claude and must participate")
	assert.Contains(t, res.Agents[0].Notes,
		`profile "codex-kit" model ignored (not valid for claude)`,
		"the instantiate response discloses the skipped foreign-tier field")
}

// Scenario: a template agent referencing a non-existent profile is a 400 at
// save — the existence check lives at the wire boundary (no DB-level FK), and
// nothing is stored.
func TestGroupTemplate_PerRoleLaunchProfiles_RejectsUnknownProfile(t *testing.T) {
	f := newFlow(t)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "t",
		"agents": []map[string]any{{"name": "lead", "spawn_profile": "ghost"}},
	})
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"unknown profile reference should 400; body=%s", rec.Body.String())

	tmpl, err := db.GetGroupTemplate("t")
	require.NoError(t, err)
	assert.Nil(t, tmpl, "rejected template must not be stored")
}

// Scenario: an inline model that isn't valid for the resolved harness is a 400
// at save — inline overrides are validated against the same catalog the spawn
// will use.
func TestGroupTemplate_PerRoleLaunchProfiles_RejectsBadModel(t *testing.T) {
	f := newFlow(t)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name":   "t",
		"agents": []map[string]any{{"name": "lead", "model": "not-a-model"}},
	})
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"invalid inline model should 400; body=%s", rec.Body.String())
}

// Scenario: from-group round-trip. A member's OBSERVABLE launch fields (the
// model/effort it is actually running, per its session row) re-trace into the
// template agent, while the curated spawn-profile REFERENCE is preserved by
// name-match — the blueprint-vs-observable split (JOH-239). This proves a
// round-trip preserves per-role launch shape.
func TestGroupTemplate_FromGroupRetracesLaunchAndKeepsProfileRef(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "cheap", "model": "haiku"}).Code, "create profile")

	createBody := map[string]any{
		"name": "crew",
		"agents": []map[string]any{
			{"name": "lead", "role": "lead", "spawn_profile": "cheap", "initial_message": "Lead the crew."},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/crew/instantiate",
		map[string]any{"group_name": "voyage"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	agentd.WaitForBackgroundForTest()
	leadConv := res.Agents[0].ConvID
	require.NotEmpty(t, leadConv)

	// Simulate a statusline tick: the live agent reports it is actually running
	// a different model/effort than the profile it launched from. These are the
	// OBSERVABLE fields a from-group snapshot re-traces.
	sess, err := db.FindSessionByConvID(leadConv)
	require.NoError(t, err)
	require.NotNil(t, sess, "lead session row")
	require.NoError(t, db.UpdateSessionModelID(sess.ID, "claude-fable-5"))
	require.NoError(t, db.UpdateSessionEffort(sess.ID, "high"))

	// Re-snapshot the group in place.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/from-group",
		map[string]any{"group": "voyage", "template_name": "crew", "update": true})
	require.Equalf(t, http.StatusOK, rec.Code, "update from-group: %s", rec.Body.String())

	tmpl, err := db.GetGroupTemplate("crew")
	require.NoError(t, err)
	require.NotNil(t, tmpl)
	require.Len(t, tmpl.Agents, 1)
	lead := tmpl.Agents[0]
	assert.Equal(t, "lead", lead.Name, "member title round-trips to its template-agent name")
	// Observable launch fields re-trace into the template-LOCAL profile
	// (profile_inline) — first-class in the editor, unlike the read-only legacy
	// inline fields, which a snapshot no longer writes.
	require.NotNil(t, lead.ProfileInline, "re-traced launch fields land in the template-local profile")
	assert.Equal(t, "claude-fable-5", lead.ProfileInline.Model, "the model the agent is actually running re-traces")
	assert.Equal(t, "high", lead.ProfileInline.Effort, "the running effort re-traces")
	assert.Empty(t, lead.Model, "the legacy inline model stays unwritten")
	assert.Empty(t, lead.Effort, "the legacy inline effort stays unwritten")
	// The curated profile reference is preserved by name-match (blueprint
	// curation, not observable) — like the brief.
	assert.Equal(t, "cheap", lead.SpawnProfile, "curated profile reference survives the re-snapshot")
	assert.Equal(t, "Lead the crew.", lead.InitialMessage, "curated brief survives too")
}

// Scenario: the save-time validation harness follows the ROLE tiers when the
// template-side chain (agent inline / profile_inline / spawn_profile) is all
// blank. An agent whose only launch config is a Claude-only model over a role
// whose profile is Codex used to save fine (validated against the Claude
// catalog) and then fail every deploy; it must be a 400 at save. Conversely a
// Codex model in the same shape used to be FALSELY rejected at save (validated
// against Claude) even though the deploy resolves to Codex and works — it must
// save and spawn.
func TestGroupTemplate_RoleProfileHarnessFeedsSaveValidation(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "cx-kit", "harness": "codex", "model": "gpt-5-codex"}).Code,
		"create codex profile")
	require.Equalf(t, http.StatusCreated,
		createRole(t, f, map[string]any{"name": "cx-dev", "spawn_profile": "cx-kit"}).Code,
		"create role referencing it")

	// Claude-only model over the role's codex profile: rejected at save.
	rec := humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "doomed",
		"agents": []map[string]any{
			{"name": "a", "model": "opus", "role_ref": "cx-dev"},
		},
	})
	require.Equalf(t, http.StatusBadRequest, rec.Code,
		"claude model over a codex role profile must fail at save, not deploy: %s", rec.Body.String())

	// The role's DIRECT harness field feeds the chain too (one tier above its
	// profile): a role that says harness=codex itself rejects the same shape.
	require.Equalf(t, http.StatusCreated,
		createRole(t, f, map[string]any{"name": "cx-direct", "harness": "codex"}).Code,
		"create role with a direct harness")
	rec = humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "doomed-direct",
		"agents": []map[string]any{
			{"name": "a", "model": "opus", "role_ref": "cx-direct"},
		},
	})
	require.Equalf(t, http.StatusBadRequest, rec.Code,
		"claude model over a role's DIRECT codex harness must fail at save: %s", rec.Body.String())

	// The profile_inline revalidation composes with the role tiers as well: a
	// blank-harness custom config whose model is Claude-only over the
	// role-resolved codex harness is rejected at save, naming the offending
	// tier.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "doomed-inline",
		"agents": []map[string]any{
			{"name": "a", "role_ref": "cx-dev",
				"profile_inline": map[string]any{"model": "opus"}},
		},
	})
	require.Equalf(t, http.StatusBadRequest, rec.Code,
		"blank-harness profile_inline over a role-resolved codex harness must fail at save: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "profile_inline", "error names the offending tier")

	// Codex model in the same shape: saves (no more false rejection) and the
	// deploy spawns with it.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "cx-team",
		"agents": []map[string]any{
			{"name": "a", "model": "gpt-5", "role_ref": "cx-dev"},
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "codex model validates against the role-resolved harness: %s", rec.Body.String())

	rec = humanReq(t, f, http.MethodPost, "/v1/templates/cx-team/instantiate",
		map[string]any{"group_name": "cx-crew"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	model, ok := f.World.SpawnModel(res.Agents[0].ConvID)
	require.True(t, ok)
	assert.Equal(t, "gpt-5", model, "the agent's codex model reaches the spawn")
}
