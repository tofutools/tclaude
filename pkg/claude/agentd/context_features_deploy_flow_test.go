package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// A deployed task force is where startup-context trimming pays off most: a whole
// roster of role-shaped workers, each of which should carry only the context its
// role needs. resolveTemplateAgentLaunch resolves the trims from the profile
// tiers with no explicit per-deploy request to override them, so the tiers are
// the ONLY thing that can speak — which makes this path worth its own coverage
// rather than trusting the direct-spawn tests to stand in for it.

// TestTaskForceDeploy_CarriesContextFeaturesFromReferencedProfile: a member that
// references a lean profile deploys lean.
func TestTaskForceDeploy_CarriesContextFeaturesFromReferencedProfile(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "deploy-lean", "harness": "claude",
		"context_features": map[string]any{"bundled-skills": "off", "workflows": "off"},
	}).Code, "create deploy-lean")

	createBody := map[string]any{
		"name": "trim-team",
		"agents": []map[string]any{
			{"name": "lean-worker", "role": "dev", "spawn_profile": "deploy-lean"},
			{"name": "plain-worker", "role": "dev"}, // no profile: trims nothing
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/trim-team/deploy", map[string]any{
		"group_name": "trimmed-force",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 2, res.Spawned, "both members spawned")
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	convByName := map[string]string{}
	for _, a := range res.Agents {
		require.Emptyf(t, a.Error, "member %s spawned cleanly", a.Name)
		convByName[a.Name] = a.ConvID
	}

	lean, ok := f.World.SpawnContextFeatures(convByName["lean-worker"])
	require.Truef(t, ok, "no spawn recorded for lean-worker conv %s", convByName["lean-worker"])
	assert.Equal(t, map[string]string{"bundled-skills": "off", "workflows": "off"}, lean,
		"the referenced profile's trims must reach a deployed member's launch")

	plain, ok := f.World.SpawnContextFeatures(convByName["plain-worker"])
	require.Truef(t, ok, "no spawn recorded for plain-worker conv %s", convByName["plain-worker"])
	assert.Empty(t, plain, "a member with no profile must deploy untrimmed")
}

// TestTaskForceDeploy_CarriesContextFeaturesFromInlineProfile: the template-LOCAL
// profile path — how a task force pins per-role context without polluting the
// shared profile registry.
func TestTaskForceDeploy_CarriesContextFeaturesFromInlineProfile(t *testing.T) {
	f := newFlow(t)

	createBody := map[string]any{
		"name": "inline-trim-team",
		"agents": []map[string]any{
			{
				"name": "inline-lean", "role": "dev",
				"profile_inline": map[string]any{
					"harness":          "claude",
					"context_features": map[string]any{"artifact": "off"},
				},
			},
		},
	}
	require.Equalf(t, http.StatusCreated,
		humanReq(t, f, http.MethodPost, "/v1/templates", createBody).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/inline-trim-team/deploy", map[string]any{
		"group_name": "inline-trimmed-force",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned, "the member spawned")
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	require.Emptyf(t, res.Agents[0].Error, "member spawned cleanly")
	got, ok := f.World.SpawnContextFeatures(res.Agents[0].ConvID)
	require.Truef(t, ok, "no spawn recorded for conv %s", res.Agents[0].ConvID)
	assert.Equal(t, map[string]string{"artifact": "off"}, got,
		"a template-local profile's trims must reach the deployed member's launch")
}

// TestTemplateSave_RejectsContextFeaturesOnCodexInlineProfile: the save-time gate
// on a template-local profile, so a template can never store a trim its own
// harness cannot deliver and then silently ignore it at every deploy.
func TestTemplateSave_RejectsContextFeaturesOnCodexInlineProfile(t *testing.T) {
	f := newFlow(t)

	rec := humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "codex-trim-team",
		"agents": []map[string]any{
			{
				"name": "codex-worker", "role": "dev",
				"profile_inline": map[string]any{
					"harness":          "codex",
					"context_features": map[string]any{"bundled-skills": "off"},
				},
			},
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"a Codex template-local profile must not accept Claude Code trims; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_context_features", "body=%s", rec.Body.String())
}
