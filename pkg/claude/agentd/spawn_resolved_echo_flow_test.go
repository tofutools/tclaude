package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
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

// An explicit --model with no --harness defaults the harness to claude via the
// launch-field pin. A claude default profile that happens to be set must NOT be
// credited for the harness — the explicit model defaulted it, the profile only
// rode along — so the harness reports the harness-default tier, not the profile.
func TestSpawnResolvedEcho_ModelPinsHarnessDefaultNotProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "claude-prof", "harness": "claude", "effort": "high",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "claude-prof").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "alpha", Name: "worker", Model: "sonnet"})
	assert.Equal(t, agent.ResolvedField{Value: "claude", Source: agent.ProvHarnessDefault}, resp.Resolved.Harness)
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
