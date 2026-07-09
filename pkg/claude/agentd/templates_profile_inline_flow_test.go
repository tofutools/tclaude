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

// Template-LOCAL spawn profiles (profile_inline): a roster agent can embed a
// full spawn-profile-shaped launch config inside the template instead of
// referencing the registry. These flow tests assert the config lands at the
// real surfaces — the Spawner boundary for the launch fields (including the
// remote-control + ask-timeout fields the template path previously ignored)
// and group ownership / per-conv overrides for the birth-time access bits.

// Scenario: an agent whose ONLY launch config is a template-local profile
// spawns with every field of it applied — launch fields, remote control,
// ask-timeout, owner, and both permission override effects.
func TestGroupTemplate_ProfileInline_AppliesAtDeploy(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "crew",
		"agents": []map[string]any{
			{"name": "lead", "profile_inline": map[string]any{
				"model":                     "haiku",
				"effort":                    "high",
				"remote_control":            true,
				"ask_user_question_timeout": "60s",
				"is_owner":                  true,
				"permission_overrides": map[string]any{
					agentd.PermGroupsSpawn:   "grant",
					agentd.PermMessageDirect: "deny",
				},
			}},
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

	model, ok := f.World.SpawnModel(leadConv)
	require.True(t, ok)
	assert.Equal(t, "haiku", model, "inline profile's model applies")
	effort, _ := f.World.SpawnEffort(leadConv)
	assert.Equal(t, "high", effort, "inline profile's effort applies")
	rc, _ := f.World.SpawnRemoteControl(leadConv)
	assert.True(t, rc, "inline profile's remote_control reaches the spawn")
	askTimeout, _ := f.World.SpawnAskTimeout(leadConv)
	assert.Equal(t, "60s", askTimeout, "inline profile's ask-timeout reaches the spawn")

	g, err := db.GetAgentGroupByName("voyage")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.True(t, ownsGroup(t, g.ID, leadConv), "inline profile's is_owner made the agent a group owner")

	overrides, err := db.ListAgentPermissionOverridesForConv(leadConv)
	require.NoError(t, err)
	assert.Equal(t, "grant", overrides[agentd.PermGroupsSpawn], "inline profile grant applied")
	assert.Equal(t, "deny", overrides[agentd.PermMessageDirect], "inline profile deny applied")
}

// Scenario: precedence. The template-local profile sits between the legacy
// inline fields (highest) and the registry reference: with all three present
// the legacy model wins; without it, the inline profile's model beats the
// referenced profile's.
func TestGroupTemplate_ProfileInline_Precedence(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "cheap", "model": "haiku"}).Code, "create profile")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "team",
		"agents": []map[string]any{
			// Inline profile (opus) over registry ref (haiku) — inline wins.
			{"name": "a", "spawn_profile": "cheap", "profile_inline": map[string]any{"model": "opus"}},
			// Legacy inline field (sonnet) over BOTH profiles — legacy wins.
			{"name": "b", "model": "sonnet", "spawn_profile": "cheap",
				"profile_inline": map[string]any{"model": "opus"}},
		},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/instantiate",
		map[string]any{"group_name": "g1"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 2, res.Spawned)
	agentd.WaitForBackgroundForTest()

	convByName := map[string]string{}
	for _, a := range res.Agents {
		convByName[a.Name] = a.ConvID
	}
	aModel, ok := f.World.SpawnModel(convByName["a"])
	require.True(t, ok)
	assert.Equal(t, "opus", aModel, "template-local profile beats the registry reference")
	bModel, ok := f.World.SpawnModel(convByName["b"])
	require.True(t, ok)
	assert.Equal(t, "sonnet", bModel, "legacy inline field beats both profiles")
}

// Scenario: validation. A template-local profile is held to the registry
// profiles' own field validation, plus a "no identity / dialog-only fields"
// rule — and a rejected template stores nothing.
func TestGroupTemplate_ProfileInline_Validation(t *testing.T) {
	f := newFlow(t)

	// An identity field inside profile_inline is a 400 — those live on the
	// template agent itself.
	rec := humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "t",
		"agents": []map[string]any{
			{"name": "lead", "profile_inline": map[string]any{"agent_name": "sneaky"}},
		},
	})
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"identity field in profile_inline should 400; body=%s", rec.Body.String())

	// A bad model is a 400 through the shared profile validation.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "t",
		"agents": []map[string]any{
			{"name": "lead", "profile_inline": map[string]any{"model": "not-a-model"}},
		},
	})
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"invalid model in profile_inline should 400; body=%s", rec.Body.String())

	tmpl, err := db.GetGroupTemplate("t")
	require.NoError(t, err)
	assert.Nil(t, tmpl, "rejected template must not be stored")
}

// Scenario: the wire round-trips. A stored template's profile_inline comes back
// field-for-field on GET — the editor re-opens exactly what was saved.
func TestGroupTemplate_ProfileInline_WireRoundTrip(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "crew",
		"agents": []map[string]any{
			{"name": "lead", "profile_inline": map[string]any{
				"model": "haiku", "effort": "low", "remote_control": false,
				"permission_overrides": map[string]any{agentd.PermGroupsSpawn: "grant"},
			}},
		},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodGet, "/v1/templates/crew", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var got struct {
		Agents []struct {
			Name          string `json:"name"`
			ProfileInline *struct {
				Model               string            `json:"model"`
				Effort              string            `json:"effort"`
				RemoteControl       *bool             `json:"remote_control"`
				PermissionOverrides map[string]string `json:"permission_overrides"`
			} `json:"profile_inline"`
		} `json:"agents"`
	}
	testharness.DecodeJSON(t, rec, &got)
	require.Len(t, got.Agents, 1)
	p := got.Agents[0].ProfileInline
	require.NotNil(t, p, "profile_inline round-trips")
	assert.Equal(t, "haiku", p.Model)
	assert.Equal(t, "low", p.Effort)
	require.NotNil(t, p.RemoteControl, "an explicit false round-trips as false, not unset")
	assert.False(t, *p.RemoteControl)
	assert.Equal(t, map[string]string{agentd.PermGroupsSpawn: "grant"}, p.PermissionOverrides)
}

// Scenario: harness-composition validation at SAVE. A blank-harness
// profile_inline is validated against the default (Claude) catalog on its
// own, but its fields overlay ABOVE the referenced profile at instantiate —
// so over a Codex spawn_profile ref they'd ride onto a Codex launch and the
// template would be saveable yet never instantiable. The save must reject
// the composed shape with a message naming profile_inline; the same fields
// explicitly tagged with the ref's harness (and a model that harness takes)
// stay saveable.
func TestGroupTemplate_ProfileInline_HarnessMismatchWithRefRejectedAtSave(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "cx-base", "harness": "codex", "model": "gpt-5-codex"}).Code,
		"create codex profile")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "mixed",
		"agents": []map[string]any{
			{"name": "a", "spawn_profile": "cx-base",
				"profile_inline": map[string]any{"model": "opus"}},
		},
	})
	require.Equalf(t, http.StatusBadRequest, rec.Code,
		"blank-harness inline Claude model over a codex ref must fail at save, not deploy: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "profile_inline", "error names the offending tier")

	// Positive control: same shape, inline profile explicitly codex-tagged
	// with a model that harness accepts — saves fine.
	rec = humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "mixed-ok",
		"agents": []map[string]any{
			{"name": "a", "spawn_profile": "cx-base",
				"profile_inline": map[string]any{"harness": "codex", "model": "gpt-5"}},
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "codex-tagged inline over codex ref saves: %s", rec.Body.String())
}

// Scenario: tri-state owner. The referenced registry profile defaults
// is_owner=true; the template-local custom config explicitly turns it off.
// More specific wins — the deployed agent must NOT be a group owner. (The
// resolver previously honoured only true, so the registry default silently
// re-granted ownership past an explicit local "off" — exactly what the
// dashboard's custom-config editor lets a human express.)
func TestGroupTemplate_ProfileInline_ExplicitNotOwnerOverridesRefProfile(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "owner-kit", "is_owner": true}).Code,
		"create owner profile")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "crew-tri",
		"agents": []map[string]any{
			{"name": "lead", "spawn_profile": "owner-kit",
				"profile_inline": map[string]any{"is_owner": false}},
		},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/crew-tri/instantiate",
		map[string]any{"group_name": "flotilla"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 1, res.Spawned)
	require.Equal(t, 0, res.Failed, "no spawn failures: %+v", res.Agents)
	agentd.WaitForBackgroundForTest()

	g, err := db.GetAgentGroupByName("flotilla")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.False(t, ownsGroup(t, g.ID, res.Agents[0].ConvID),
		"explicit profile_inline.is_owner=false beats the referenced profile's owner default")

	// The legacy per-agent flag stays a pure raise: with it set, the explicit
	// inline false is overridden (legacy inline is the highest tier).
	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "crew-legacy",
		"agents": []map[string]any{
			{"name": "lead", "is_owner": true, "spawn_profile": "owner-kit",
				"profile_inline": map[string]any{"is_owner": false}},
		},
	}).Code, "create legacy-flag template")
	rec = humanReq(t, f, http.MethodPost, "/v1/templates/crew-legacy/instantiate",
		map[string]any{"group_name": "armada"})
	require.Equalf(t, http.StatusCreated, rec.Code, "instantiate: %s", rec.Body.String())
	testharness.DecodeJSON(t, rec, &res)
	require.Equal(t, 0, res.Failed)
	agentd.WaitForBackgroundForTest()
	g2, err := db.GetAgentGroupByName("armada")
	require.NoError(t, err)
	require.NotNil(t, g2)
	assert.True(t, ownsGroup(t, g2.ID, res.Agents[0].ConvID),
		"the legacy per-agent owner flag still raises past an inline false")
}
