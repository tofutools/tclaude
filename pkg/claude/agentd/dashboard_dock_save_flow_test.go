package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// JOH-393 — the REVERSE palette drag captures a live group / agent into an
// UNSAVED preview seed the dashboard editor then lets the human review + save.
// Both server captures are seed-only: they must return the traced blueprint
// WITHOUT persisting anything, so a cancelled drop leaves no profile/template.

// Scenario: from-group in preview mode returns the same snapshot a real
// from-group would create, but persists NOTHING — the drag-a-group-onto-the-dock
// gesture opens the template editor on this unsaved blueprint.
func TestGroupTemplate_FromGroupPreview(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("src")

	const ctx = "SRC-PREVIEW-CONTEXT"
	require.Equal(t, http.StatusOK,
		humanReq(t, f, http.MethodPatch, "/v1/groups/src",
			map[string]any{"default_context": ctx}).Code, "set group context")

	lead := f.AsHuman().Spawn("src", "lead")
	f.AsHuman().Spawn("src", "helper")
	require.Equal(t, http.StatusOK,
		humanReq(t, f, http.MethodPost, "/v1/groups/src/owners",
			map[string]any{"conv": lead.ConvID}).Code, "grant lead ownership")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/from-group",
		map[string]any{"group": "src", "template_name": "preview-tmpl", "preview": true})
	require.Equal(t, http.StatusOK, rec.Code, "preview: %s", rec.Body.String())

	// The response carries the full blueprint so the editor can pre-fill: one
	// agent per member, exactly one owner, and the group's live context.
	var res struct {
		Name           string `json:"name"`
		DefaultContext string `json:"default_context"`
		Agents         []struct {
			IsOwner bool `json:"is_owner"`
		} `json:"agents"`
	}
	testharness.DecodeJSON(t, rec, &res)
	assert.Equal(t, "preview-tmpl", res.Name, "the seed carries the suggested name")
	assert.Equal(t, ctx, res.DefaultContext, "the seed traces the group's live context")
	require.Len(t, res.Agents, 2, "one seed agent per member")
	owners := 0
	for _, a := range res.Agents {
		if a.IsOwner {
			owners++
		}
	}
	assert.Equal(t, 1, owners, "exactly one seed agent is owner")

	// The crux: a preview PERSISTS NOTHING — nothing is created until the human
	// saves from the editor.
	got, err := db.GetGroupTemplate("preview-tmpl")
	require.NoError(t, err)
	assert.Nil(t, got, "preview must not create a template")
}

// Scenario: from-agent captures a live agent's granted permissions into an
// unsaved profile seed — persisting NOTHING — the drag-an-agent-onto-the-dock
// gesture opens the profile editor on this seed. Also guards the selector: blank
// is a 400, an unknown selector is a 404.
func TestSpawnProfile_FromAgentSeed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("src")
	a := f.AsHuman().Spawn("src", "worker")

	// Give the live agent a permission grant so the capture has something
	// observable to trace into the seed's overrides.
	require.NoError(t, db.GrantAgentPermission(a.ConvID, "human.notify", "human"),
		"grant a permission to the live agent")

	// Put the agent on a NON-default, out-of-catalog full model id — the exact
	// shape the operator hit ("claude-opus-4-8[1m]"): ValidateModel accepts it
	// (fullModelIDRe), but it is NOT one of the curated alias presets. The sim's
	// SpawnNew writes the SessionRow WITHOUT a model_id, so set it directly on
	// the row the tracer reads (sessions.id == the spawn label). This exercises
	// traceMemberLaunch → the seed's Model, guarding that a non-preset model is
	// captured (the controlled Preact draft then preserves it through the Custom sentinel).
	const nonDefaultModel = "claude-opus-4-8[1m]"
	require.NoError(t, db.UpdateSessionModelID(a.TmuxSession, nonDefaultModel),
		"stamp the live agent's session row with a non-default model id")

	rec := humanReq(t, f, http.MethodPost, "/v1/spawn-profiles/from-agent",
		map[string]any{"agent": a.ConvID})
	require.Equal(t, http.StatusOK, rec.Code, "from-agent: %s", rec.Body.String())

	var seed struct {
		Name                string            `json:"name"`
		Model               string            `json:"model"`
		Approval            string            `json:"approval"`
		AutoReview          *bool             `json:"auto_review"`
		PermissionOverrides map[string]string `json:"permission_overrides"`
	}
	testharness.DecodeJSON(t, rec, &seed)
	assert.Empty(t, seed.Name, "a seed is unnamed — the editor makes the human name it")
	assert.Equal(t, nonDefaultModel, seed.Model,
		"the seed captures the agent's exact (non-preset) model id, not a blank")
	assert.Equal(t, "auto", seed.Approval, "the seed preserves the recorded approval posture")
	require.NotNil(t, seed.AutoReview, "the seed preserves an explicit resolved auto-review setting")
	assert.False(t, *seed.AutoReview)
	assert.Equal(t, "grant", seed.PermissionOverrides["human.notify"],
		"the seed captures the agent's granted permission as a grant override")

	// The crux: a from-agent capture PERSISTS NOTHING — the profile is created
	// only when the human saves from the editor.
	profiles, err := db.ListSpawnProfiles()
	require.NoError(t, err)
	assert.Empty(t, profiles, "from-agent must not create a profile")

	// Guards: a blank selector is a 400, an unknown one a 404.
	assert.Equal(t, http.StatusBadRequest,
		humanReq(t, f, http.MethodPost, "/v1/spawn-profiles/from-agent",
			map[string]any{"agent": ""}).Code, "blank selector 400s")
	assert.Equal(t, http.StatusNotFound,
		humanReq(t, f, http.MethodPost, "/v1/spawn-profiles/from-agent",
			map[string]any{"agent": "no-such-agent-xyz"}).Code, "unknown selector 404s")
}
