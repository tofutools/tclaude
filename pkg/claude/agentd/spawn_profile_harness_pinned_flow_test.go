package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TCL-999: model and effort are HARNESS-PINNED fields. A default spawn profile
// (the group's or the global one) may fill them only when it targets the
// harness the launch actually resolved to.
//
// The reported failure needed two individually-correct designs to compose. The
// tier resolver lets a lower tier's value participate whenever it VALIDATES
// against the resolved harness, and Copilot's ValidateModel is deliberately
// permissive — it brokers multi-vendor models with no machine-readable catalog,
// so any bounded single token passes. A Claude-targeted global default profile's
// model therefore sailed through Copilot's gate and reached the CLI, which
// answered: Model "opus[1m]" from --model flag is not available.
//
// The gate is on tier participation, not on validation: Copilot's permissiveness
// is intentional and stays. What changes is that an ambient default profile
// pinned to another vendor no longer speaks for this launch's model or effort.

// The reproduced incident: Claude-harness global default with a model set, and
// a Copilot spawn. No --model may reach the Copilot CLI, and the skip is
// disclosed rather than silent.
func TestSpawnHarnessPinned_ClaudeGlobalDefaultModelNeverReachesCopilot(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "claude-default", "harness": "claude", "model": "opus", "effort": "high",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "claude-default").Code)

	resp, out := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName,
	})
	assert.Equal(t, harness.CopilotName, resp.Resolved.Harness.Value)
	assert.Empty(t, resp.Resolved.Model.Value,
		"a Claude default profile's model must not fill a Copilot launch")
	assert.Equal(t, `global default profile "claude-default" model ignored `+
		`(profile targets claude, launch is copilot)`, resp.Resolved.Model.Note)
	assert.Empty(t, resp.Resolved.Effort.Value)
	assert.Equal(t, `global default profile "claude-default" effort ignored `+
		`(profile targets claude, launch is copilot)`, resp.Resolved.Effort.Note)
	assert.Contains(t, out, "— "+resp.Resolved.Model.Note)

	// The production spawner's argv is the load-bearing assertion: the flag the
	// Copilot CLI complained about must be absent, not merely blank in the echo.
	launch := copilotLaunchOf(t, f, resp.ConvID)
	assert.Empty(t, launch.Model, "no --model flag may be emitted")
	assert.Empty(t, launch.Effort)
}

// The same gate on the group-default tier, and the fallthrough it enables: a
// foreign group default no longer shadows a compatible global default.
func TestSpawnHarnessPinned_ForeignGroupDefaultFallsThroughToCopilotGlobal(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "claude-group", "harness": "claude", "model": "opus",
	}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "copilot-global", "harness": harness.CopilotName, "model": "claude-sonnet-4.5",
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "crew", "claude-group").Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "copilot-global").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker", Harness: harness.CopilotName,
	})
	assert.Equal(t, agent.ResolvedField{
		Value: "claude-sonnet-4.5", Source: `global default profile "copilot-global"`,
		Note: `group default profile "claude-group" model ignored ` +
			`(profile targets claude, launch is copilot)`,
	}, resp.Resolved.Model)
	assert.Equal(t, "claude-sonnet-4.5", copilotLaunchOf(t, f, resp.ConvID).Model)
}

// The gate is a HARNESS-MATCH gate, not a "default profiles never fill model"
// gate: a same-harness default still supplies both pinned fields.
func TestSpawnHarnessPinned_SameHarnessDefaultStillFillsModelAndEffort(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "copilot-default", "harness": harness.CopilotName,
		"model": "claude-sonnet-4.5", "effort": "high",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "copilot-default").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{Group: "crew", Name: "copilot-worker"})
	assert.Equal(t, harness.CopilotName, resp.Resolved.Harness.Value)
	assert.Equal(t, agent.ResolvedField{
		Value: "claude-sonnet-4.5", Source: `global default profile "copilot-default"`,
	}, resp.Resolved.Model)
	assert.Equal(t, agent.ResolvedField{
		Value: "high", Source: `global default profile "copilot-default"`,
	}, resp.Resolved.Effort)

	launch := copilotLaunchOf(t, f, resp.ConvID)
	assert.Equal(t, "claude-sonnet-4.5", launch.Model)
	assert.Equal(t, "high", launch.Effort)
}

// Scope boundary (decided with the operator): the gate covers the DEFAULT tiers
// only. A profile the operator named with -p on the same command line as
// --harness is direct intent, visible in the invocation, and keeps
// participating — the leak being fixed is ambient configuration nobody typed.
func TestSpawnHarnessPinned_ExplicitNamedProfileStillParticipates(t *testing.T) {
	f := newCopilotFlow(t)
	f.HaveGroup("crew")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "claude-kit", "harness": "claude", "model": "opus", "effort": "high",
	}).Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "crew", Name: "copilot-worker",
		Harness: harness.CopilotName, Profile: "claude-kit",
	})
	assert.Equal(t, agent.ResolvedField{
		Value: "opus", Source: `profile "claude-kit"`,
	}, resp.Resolved.Model)
	assert.Equal(t, "opus", copilotLaunchOf(t, f, resp.ConvID).Model)
}

// Non-pinned fields keep their deliberate cross-vendor participation: a foreign
// default profile's generic launch posture still applies. Only model and effort
// are gated.
func TestSpawnHarnessPinned_ForeignDefaultStillSuppliesGenericFields(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "claude-default", "harness": harness.DefaultName, "model": "opus",
		"sandbox_implementation": "harness-builtin",
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "claude-default").Code)

	resp, _ := runSpawnCLI(t, f, &agent.SpawnParams{
		Group: "alpha", Name: "worker", Harness: "codex",
	})
	assert.Empty(t, resp.Resolved.Model.Value, "the pinned field is skipped")
	impl, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "harness-builtin", impl,
		"a containment choice valid for both vendors still crosses tiers")
}
