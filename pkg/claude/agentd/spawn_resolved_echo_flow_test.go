package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// runSpawnCLI drives the production `tclaude agent spawn` path through the flow
// mux and returns the response (with its resolved-shape echo) plus captured
// stdout, so a test can assert both the wire shape and what the operator sees.
func runSpawnCLI(t *testing.T, f *testharness.Flow, p *agent.SpawnParams) (*agent.SpawnResponse, string) {
	t.Helper()
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(p, stdout, stderr, new(bytes.Buffer))
	require.Equalf(t, 0, rc, "RunSpawn stderr=%s", stderr.String())
	require.NotNil(t, resp)
	require.NotNil(t, resp.Resolved, "spawn response must carry the resolved-shape echo")
	return resp, stdout.String()
}

// A no-flag spawn with only a global default profile set inherits that
// profile's harness AND model — the TCL-304 incident. The resolved-shape echo
// must attribute all three fields to the global default profile so the silent
// vendor flip is visible instead of a surprise.
func TestSpawnResolvedEcho_GlobalDefaultProfileProvenance(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "gpt-high", "harness": "codex", "model": "gpt-5.6-sol", "effort": "high",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "gpt-high").Code)

	resp, out := runSpawnCLI(t, f, &agent.SpawnParams{Group: "alpha", Name: "worker"})

	src := `global default profile "gpt-high"`
	assert.Equal(t, agent.ResolvedField{Value: "codex", Source: src}, resp.Resolved.Harness)
	assert.Equal(t, agent.ResolvedField{Value: "gpt-5.6-sol", Source: src}, resp.Resolved.Model)
	assert.Equal(t, agent.ResolvedField{Value: "high", Source: src}, resp.Resolved.Effort)

	// The operator-facing print surfaces the same thing at a glance.
	assert.Contains(t, out, `Harness: codex (global default profile "gpt-high")`)
	assert.Contains(t, out, `Model:   gpt-5.6-sol (global default profile "gpt-high")`)
	assert.Contains(t, out, `Effort:  high (global default profile "gpt-high")`)
}

// An explicit --harness/--model wins over every profile tier and is tagged
// "explicit" even when a default profile is set.
func TestSpawnResolvedEcho_ExplicitProvenance(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "gpt-high", "harness": "codex", "model": "gpt-5.6-sol", "effort": "high",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "gpt-high").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha", Name: "worker", Harness: "codex", Model: "gpt-5.6-terra",
	})
	assert.Equal(t, agent.ResolvedField{Value: "codex", Source: agent.ProvExplicit}, resp.Resolved.Harness)
	assert.Equal(t, agent.ResolvedField{Value: "gpt-5.6-terra", Source: agent.ProvExplicit}, resp.Resolved.Model)
}

// A group default profile outranks the global default profile — per field.
func TestSpawnResolvedEcho_GroupBeatsGlobal(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "group", "harness": "codex", "model": "gpt-5.6-terra",
	}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "global", "harness": "codex", "model": "gpt-5.5",
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "group").Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "alpha", Name: "worker"})
	assert.Equal(t, agent.ResolvedField{
		Value: "gpt-5.6-terra", Source: `group default profile "group"`,
	}, resp.Resolved.Model)
}

// A CLI --profile fills fields before the daemon's group/global tiers. The
// daemon sees those folded values as "explicit"; the CLI relabels them to the
// profile tier so the printed provenance is faithful.
func TestSpawnResolvedEcho_CLIProfileRelabel(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "explicit", "harness": "codex", "model": "gpt-5.6-sol",
	}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "global", "harness": "codex", "model": "gpt-5.5",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	resp, out := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha", Name: "worker", Profile: "explicit",
	})
	src := `profile "explicit"`
	assert.Equal(t, agent.ResolvedField{Value: "codex", Source: src}, resp.Resolved.Harness)
	assert.Equal(t, agent.ResolvedField{Value: "gpt-5.6-sol", Source: src}, resp.Resolved.Model)
	assert.Contains(t, out, `Model:   gpt-5.6-sol (profile "explicit")`)
}

// With nothing pinned at any tier, harness resolves to the built-in default and
// the unset model reports the harness-default tier (and prints as such).
func TestSpawnResolvedEcho_HarnessDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp, out := runSpawnCLI(t, f, &agent.SpawnParams{Group: "alpha", Name: "worker"})
	assert.Equal(t, agent.ResolvedField{Value: "claude", Source: agent.ProvHarnessDefault}, resp.Resolved.Harness)
	assert.Equal(t, agent.ResolvedField{Value: "", Source: agent.ProvHarnessDefault}, resp.Resolved.Model)
	assert.Contains(t, out, "Harness: claude (harness default)")
	assert.Contains(t, out, "Model:   (harness default)")
}

// An explicit model no longer pins the harness; a matching default profile is
// credited for selecting Claude while the direct model remains explicit.
func TestSpawnResolvedEcho_ModelDoesNotPinHarness(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "claude-prof", "harness": "claude", "effort": "high",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "claude-prof").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "alpha", Name: "worker", Model: "sonnet"})
	assert.Equal(t, agent.ResolvedField{Value: "claude", Source: `global default profile "claude-prof"`}, resp.Resolved.Harness)
	assert.Equal(t, agent.ResolvedField{Value: "sonnet", Source: agent.ProvExplicit}, resp.Resolved.Model)
	// The profile's effort still fills the blank --effort.
	assert.Equal(t, agent.ResolvedField{
		Value: "high", Source: `global default profile "claude-prof"`,
	}, resp.Resolved.Effort)
}

// The resolved-shape echo rides the raw wire (additive), so a pure-API caller
// that never touches the CLI still receives it.
func TestSpawnResolvedEcho_OnTheWire(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "global", "harness": "codex", "model": "gpt-5.6-sol",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	var decoded agent.SpawnResponse
	require.NoError(t, json.Unmarshal(spawn.Raw, &decoded))
	require.NotNil(t, decoded.Resolved)
	assert.Equal(t, "codex", decoded.Resolved.Harness.Value)
	assert.Equal(t, `global default profile "global"`, decoded.Resolved.Harness.Source)
	assert.Equal(t, "gpt-5.6-sol", decoded.Resolved.Model.Value)
}

func TestSpawnResolution_ExplicitEffortDoesNotPinCodexDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-default", "harness": "codex", "model": "gpt-5.6-sol",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "codex-default").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "alpha", Name: "worker", Effort: "high"})
	assert.Equal(t, agent.ResolvedField{Value: "codex", Source: `global default profile "codex-default"`}, resp.Resolved.Harness)
	assert.Equal(t, agent.ResolvedField{Value: "high", Source: agent.ProvExplicit}, resp.Resolved.Effort)
}

func TestSpawnResolution_ExplicitForeignModelFailsWithoutSpawning(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-default", "harness": "codex",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "codex-default").Code)
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(&agent.SpawnParams{Group: "alpha", Name: "worker", Model: "fable"},
		new(bytes.Buffer), stderr, new(bytes.Buffer))
	assert.Nil(t, resp)
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, stderr.String(), `model "fable" is not valid for codex`)
	assert.Contains(t, stderr.String(), "pass --harness claude")
	assert.Empty(t, f.World.Tmux.Sessions(), "validation failure must not launch a pane")
}

func TestSpawnResolution_ExplicitHarnessIgnoresForeignDefaultFields(t *testing.T) {
	for _, tc := range []struct {
		name, explicitHarness, profileHarness, profileModel string
	}{
		{"claude ignores codex defaults", "claude", "codex", "gpt-5.6-sol"},
		{"codex ignores claude defaults", "codex", "claude", "opus"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
				"name": "foreign", "harness": tc.profileHarness, "model": tc.profileModel, "effort": "high",
			}).Code)
			require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "foreign").Code)

			resp, out := runSpawnCLI(t, f, &agent.SpawnParams{Group: "alpha", Name: "worker", Harness: tc.explicitHarness})
			assert.Equal(t, tc.explicitHarness, resp.Resolved.Harness.Value)
			assert.Empty(t, resp.Resolved.Model.Value)
			assert.Contains(t, resp.Resolved.Model.Note, `global default profile "foreign" model ignored`)
			assert.Equal(t, "high", resp.Resolved.Effort.Value, "generic compatible effort still applies")
			assert.Contains(t, out, "— "+resp.Resolved.Model.Note)
		})
	}
}

func TestSpawnResolution_ExplicitHarnessIgnoresForeignNamedProfileFields(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-kit", "harness": "codex", "model": "gpt-5.6-sol", "effort": "high", "sandbox": "read-only",
	}).Code)

	resp, out := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha", Name: "worker", Harness: "claude", Profile: "codex-kit",
	})
	assert.Equal(t, "claude", resp.Resolved.Harness.Value)
	assert.Empty(t, resp.Resolved.Model.Value)
	assert.Contains(t, resp.Resolved.Model.Note, `profile "codex-kit" model ignored`)
	assert.Equal(t, agent.ResolvedField{Value: "high", Source: `profile "codex-kit"`}, resp.Resolved.Effort)
	assert.Contains(t, resp.Resolved.Notes, `profile "codex-kit" sandbox ignored (not valid for claude)`)
	assert.Contains(t, out, `Note:    profile "codex-kit" sandbox ignored (not valid for claude)`)
}

func TestSpawnResolution_IgnoredProfileFieldFallsThroughToCompatibleTier(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "named-codex", "harness": "codex", "model": "gpt-5.6-sol",
	}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "global-claude", "harness": "claude", "model": "sonnet",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global-claude").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha", Name: "worker", Harness: "claude", Profile: "named-codex",
	})
	assert.Equal(t, agent.ResolvedField{
		Value: "sonnet", Source: `global default profile "global-claude"`,
		Note: `profile "named-codex" model ignored (not valid for claude)`,
	}, resp.Resolved.Model)
}

func TestSpawnResolution_AutoReviewUsesCodexDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-default", "harness": "codex",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "codex-default").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "alpha", Name: "worker", AutoReview: true})
	autoReview, ok := f.World.SpawnAutoReview(resp.ConvID)
	require.True(t, ok)
	assert.True(t, autoReview)
}

func TestSpawnResolution_CodexModelDoesNotInferHarness(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(&agent.SpawnParams{Group: "alpha", Name: "worker", Model: "gpt-5.6-sol"},
		new(bytes.Buffer), stderr, new(bytes.Buffer))
	assert.Nil(t, resp)
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, stderr.String(), `not valid for claude`)
	assert.Contains(t, stderr.String(), "pass --harness codex")
}

func TestSpawnResolvedEcho_TracksValueAppliedAtLaunch(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	restore := agentd.SetBeforeExecuteSpawnForTest(func() {
		_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "late", Harness: "claude", Model: "sonnet"})
		require.NoError(t, err)
		require.NoError(t, db.SetDashboardPref(globalDefaultProfilePrefKey, "late"))
	})
	t.Cleanup(restore)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "alpha", Name: "worker"})
	assert.Equal(t, agent.ResolvedField{Value: "sonnet", Source: agent.ProvLaunchDefault}, resp.Resolved.Model)
	launched, ok := f.World.SpawnModel(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "sonnet", launched)
}

func TestSpawnResolution_DashboardExplicitClaudeBeatsCodexDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-default", "harness": "codex", "model": "gpt-5.6-sol",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "codex-default").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "harness": "claude", "sandbox": "inherit", "remote_control": false,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "modal-shaped spawn body=%s", spawn.Raw)
	var decoded agent.SpawnResponse
	require.NoError(t, json.Unmarshal(spawn.Raw, &decoded))
	require.NotNil(t, decoded.Resolved)
	assert.Equal(t, agent.ResolvedField{Value: "claude", Source: agent.ProvExplicit}, decoded.Resolved.Harness)
}

func TestSpawnResolution_LegacyBlankHarnessProfileMeansClaude(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "legacy", "model": "sonnet",
	}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "codex-global", "harness": "codex", "model": "gpt-5.6-sol",
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "legacy").Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "codex-global").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "alpha", Name: "worker"})
	source := `group default profile "legacy"`
	assert.Equal(t, agent.ResolvedField{Value: "claude", Source: source}, resp.Resolved.Harness)
	assert.Equal(t, agent.ResolvedField{Value: "sonnet", Source: source}, resp.Resolved.Model)
}

func TestSpawnResolution_ForeignFalseBoolDoesNotShadowMatchingLowerTier(t *testing.T) {
	t.Run("remote control for claude", func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")
		require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
			"name": "foreign", "harness": "codex", "remote_control": false,
		}).Code)
		require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
			"name": "matching", "harness": "claude", "remote_control": true,
		}).Code)
		require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "foreign").Code)
		require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "matching").Code)

		spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "harness": "claude"})
		require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
		value, ok := f.World.SpawnRemoteControl(spawn.ConvID)
		require.True(t, ok)
		assert.True(t, value)
	})

	t.Run("auto review for codex", func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")
		require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
			"name": "foreign", "harness": "claude", "auto_review": false,
		}).Code)
		require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
			"name": "matching", "harness": "codex", "auto_review": true,
		}).Code)
		require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "foreign").Code)
		require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "matching").Code)

		spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "harness": "codex"})
		require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
		value, ok := f.World.SpawnAutoReview(spawn.ConvID)
		require.True(t, ok)
		assert.True(t, value)
	})
}

func TestSpawnResolution_DisclosesEveryIgnoredModelTier(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	for _, profile := range []map[string]any{
		{"name": "named", "harness": "codex", "model": "gpt-5.6-sol"},
		{"name": "group", "harness": "codex", "model": "gpt-5.6-terra"},
		{"name": "global", "harness": "claude", "model": "sonnet"},
	} {
		require.Equal(t, http.StatusCreated, createProfile(t, f, profile).Code)
	}
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "group").Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha", Name: "worker", Harness: "claude", Profile: "named",
	})
	assert.Equal(t, "sonnet", resp.Resolved.Model.Value)
	assert.Contains(t, resp.Resolved.Model.Note, `profile "named" model ignored`)
	assert.Contains(t, resp.Resolved.Model.Note, `group default profile "group" model ignored`)
}
