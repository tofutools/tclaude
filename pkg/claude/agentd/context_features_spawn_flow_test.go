package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: Claude Code loads a fixed body of startup context — bundled skills,
// tool schemas for capabilities a given agent will never touch, system-prompt
// blocks — sized for a general-purpose assistant. A tclaude worker is usually
// spawned for one narrow job, so most of that is context it reads past before
// reaching its brief. tclaude lets the operator trim it per spawn and per spawn
// profile (TCL-597).
//
// These pin the daemon's resolution at the Spawner boundary
// (World.SpawnContextFeatures — the same surface the auto-memory / remote-control
// spawn flow tests assert). The env-var and settings.json rendering is
// unit-tested in harness.ContextFeatureEnv / ContextFeatureSettings.

// TestClaudeSpawn_ContextFeaturesDefaultToNothingTrimmed: the safe default. An
// operator who never touches the control gets exactly today's behaviour, so this
// feature can never silently take a capability away from an existing workflow.
func TestClaudeSpawn_ContextFeaturesDefaultToNothingTrimmed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	spawn := f.AsHuman().SpawnHarness("cc-crew", "plain-worker", "claude")

	got, ok := f.World.SpawnContextFeatures(spawn.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Empty(t, got, "an untouched spawn must trim nothing")
}

// TestClaudeSpawn_ContextFeaturesExplicitTrims: the headline path — a per-spawn
// selection reaches the launch.
func TestClaudeSpawn_ContextFeaturesExplicitTrims(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "focused-worker",
		"context_features": map[string]any{
			"bundled-skills": "off",
			"workflows":      "off",
			"artifact":       "on",
		},
	})
	require.Equal(t, 200, resp.Code,
		"a context_features selection on a Claude Code spawn must be accepted; body=%s", resp.Raw)

	got, ok := f.World.SpawnContextFeatures(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Equal(t, map[string]string{
		"bundled-skills": "off",
		"workflows":      "off",
		"artifact":       "on",
	}, got, "the per-spawn selection must reach the launch verbatim")
}

// TestClaudeSpawn_ContextFeaturesDropsDefaultStates: a row the operator set and
// then reverted must not persist as a stored "default" — the resolved map holds
// only real intent, which is what keeps a profile readable.
func TestClaudeSpawn_ContextFeaturesDropsDefaultStates(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "mostly-default",
		"context_features": map[string]any{
			"bundled-skills": "off",
			"artifact":       "default",
			"cron":           "",
		},
	})
	require.Equal(t, 200, resp.Code, "spawn body=%s", resp.Raw)

	got, ok := f.World.SpawnContextFeatures(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, map[string]string{"bundled-skills": "off"}, got)
}

// TestClaudeSpawn_ContextFeaturesFromProfile: a profile's trims fill a spawn that
// said nothing — the tier behaviour every other launch field uses.
func TestClaudeSpawn_ContextFeaturesFromProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "lean-worker", "harness": "claude",
		"context_features": map[string]any{"bundled-skills": "off", "artifact": "off"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "inherits-lean", "profile": "lean-worker",
	})
	require.Equal(t, 200, resp.Code, "spawn with profile; body=%s", resp.Raw)

	got, ok := f.World.SpawnContextFeatures(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, map[string]string{"bundled-skills": "off", "artifact": "off"}, got,
		"a profile's trims must reach the launch")
}

// TestClaudeSpawn_ExplicitContextFeaturesReplaceProfileWholesale: the tiers do
// NOT merge. An explicit per-spawn map replaces the profile's outright, so the
// effective startup context is readable from one place rather than being the
// union of every profile in the lineage.
func TestClaudeSpawn_ExplicitContextFeaturesReplaceProfileWholesale(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "lean-worker-2", "harness": "claude",
		"context_features": map[string]any{"bundled-skills": "off", "artifact": "off"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "overrides-lean", "profile": "lean-worker-2",
		"context_features": map[string]any{"cron": "off"},
	})
	require.Equal(t, 200, resp.Code, "spawn body=%s", resp.Raw)

	got, ok := f.World.SpawnContextFeatures(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, map[string]string{"cron": "off"}, got,
		"the per-spawn map replaces the profile's rather than merging with it")
}

// TestClaudeSpawn_ExplicitEmptyContextFeaturesOverridesProfile: an operator who
// clears the trims in the spawn form must actually get an untrimmed agent. An
// explicitly EMPTY map is a decision, not an absent field.
func TestClaudeSpawn_ExplicitEmptyContextFeaturesOverridesProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "lean-worker-3", "harness": "claude",
		"context_features": map[string]any{"bundled-skills": "off"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "opts-back-in", "profile": "lean-worker-3",
		"context_features": map[string]any{},
	})
	require.Equal(t, 200, resp.Code, "spawn body=%s", resp.Raw)

	got, ok := f.World.SpawnContextFeatures(resp.ConvID)
	require.True(t, ok)
	assert.Empty(t, got, "an explicit empty selection must beat the profile's trims")
}

// TestClaudeSpawn_RejectsUnknownContextFeature: a typo'd slug is a 400 rather
// than a silently ignored setting, so an operator never believes they trimmed
// something they did not.
func TestClaudeSpawn_RejectsUnknownContextFeature(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name":             "typo-worker",
		"context_features": map[string]any{"bundled-skillz": "off"},
	})
	require.Equal(t, 400, resp.Code, "an unknown feature must be refused; body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "invalid_context_features",
		"the refusal should name the context-features gate; body=%s", resp.Raw)
}

// TestClaudeSpawn_RejectsBadContextFeatureState: likewise for a state outside
// on/off/default.
func TestClaudeSpawn_RejectsBadContextFeatureState(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name":             "bad-state-worker",
		"context_features": map[string]any{"artifact": "sometimes"},
	})
	require.Equal(t, 400, resp.Code, "a bad state must be refused; body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "invalid_context_features", "body=%s", resp.Raw)
}

// TestCodexSpawn_RejectsContextFeatures: Codex exposes no startup-context
// switches, so a trim is a 400 at the boundary rather than a setting that
// silently does nothing.
func TestCodexSpawn_RejectsContextFeatures(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name":             "no-trims",
		"harness":          "codex",
		"context_features": map[string]any{"bundled-skills": "off"},
	})
	require.Equal(t, 400, resp.Code,
		"context_features on a Codex spawn must be refused with a 400; body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "invalid_context_features", "body=%s", resp.Raw)
}

// TestCodexSpawn_EmptyContextFeaturesIsFine: an all-default map is valid for
// every harness — it asks for nothing, so the Codex gate must not trip on the
// dashboard sending the field for a harness that ignores it.
func TestCodexSpawn_EmptyContextFeaturesIsFine(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("codex-crew")

	resp := f.AsHuman().SpawnWith("codex-crew", map[string]any{
		"name": "codex-plain", "harness": "codex",
		"context_features": map[string]any{"artifact": "default"},
	})
	require.Equal(t, 200, resp.Code,
		"an all-default context_features map must be accepted for Codex; body=%s", resp.Raw)

	got, ok := f.World.SpawnContextFeatures(resp.ConvID)
	require.True(t, ok)
	assert.Empty(t, got)
}

// TestProfileSave_RejectsContextFeaturesForCodexProfile: the same gate at the
// profile boundary, so a profile can never persist a trim its own harness cannot
// deliver and then quietly ignore it at every deploy.
func TestProfileSave_RejectsContextFeaturesForCodexProfile(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name": "codex-lean", "harness": "codex",
		"context_features": map[string]any{"bundled-skills": "off"},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"a Codex profile must not accept Claude Code trims; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid_context_features", "body=%s", rec.Body.String())
}

// TestProfileContextFeaturesRoundTrip: the editor posts the profile's complete
// desired state and reloads it, so the trims must survive the save/read cycle
// byte-for-byte or the dialog would silently drift from the stored row.
func TestProfileContextFeaturesRoundTrip(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name": "round-trip-lean", "harness": "claude",
		"context_features": map[string]any{
			"bundled-skills": "off",
			"artifact":       "on",
			"cron":           "default", // dropped: only real intent is stored
		},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	got := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/spawn-profiles/round-trip-lean", nil)))
	require.Equalf(t, http.StatusOK, got.Code, "fetch profile body=%s", got.Body.String())

	var profile struct {
		ContextFeatures map[string]string `json:"context_features"`
	}
	require.NoError(t, json.Unmarshal(got.Body.Bytes(), &profile))
	assert.Equal(t, map[string]string{
		"bundled-skills": "off",
		"artifact":       "on",
	}, profile.ContextFeatures,
		"the stored map must hold exactly the steered features, normalized")
}

// TestClaudeSpawn_ContextFeaturesFromGroupDefaultProfile: the tier the DASHBOARD
// actually exercises. The named-profile tests above cover the explicit
// `"profile": X` path, but a dashboard spawn usually names no profile and lets
// the group's default speak — so a regression here would be invisible to them.
func TestClaudeSpawn_ContextFeaturesFromGroupDefaultProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "group-lean", "harness": "claude",
		"context_features": map[string]any{"bundled-skills": "off", "cron": "off"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "cc-crew", "group-lean").Code,
		"set the group's default profile")

	// No "profile" key: the group default is the only tier that can speak.
	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{"name": "inherits-group-lean"})
	require.Equal(t, 200, resp.Code, "spawn body=%s", resp.Raw)

	got, ok := f.World.SpawnContextFeatures(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, map[string]string{"bundled-skills": "off", "cron": "off"}, got,
		"the group default profile's trims must reach the launch")
}

// TestClaudeSpawn_ExplicitContextFeaturesBeatGroupDefaultProfile: the same
// whole-tier replacement, proven against the group tier rather than a named one —
// this is what makes "clear the rows in the dialog and get an untrimmed agent"
// true for a group that has a lean default.
func TestClaudeSpawn_ExplicitContextFeaturesBeatGroupDefaultProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("cc-crew")

	rec := createProfile(t, f, map[string]any{
		"name": "group-lean-2", "harness": "claude",
		"context_features": map[string]any{"bundled-skills": "off"},
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "cc-crew", "group-lean-2").Code,
		"set the group's default profile")

	resp := f.AsHuman().SpawnWith("cc-crew", map[string]any{
		"name": "clears-group-lean", "context_features": map[string]any{},
	})
	require.Equal(t, 200, resp.Code, "spawn body=%s", resp.Raw)

	got, ok := f.World.SpawnContextFeatures(resp.ConvID)
	require.True(t, ok)
	assert.Empty(t, got, "an explicit empty selection must beat the GROUP default's trims too")
}
