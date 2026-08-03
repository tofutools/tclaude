package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario (TCL-978): a Copilot agent runs inside tclaude's outer OS sandbox,
// with Copilot's own command sandbox asserted off, so the launch has exactly
// one claimed enforcement boundary.
//
// These flows exist because the interesting property is not "does the spawn
// carry the flag" — it is that the assert-off gate runs on EVERY path that can
// start a Copilot pane. Copilot's own sandbox is configured out of band, in a
// file an operator can edit at any moment between one launch and the next, so
// a gate that ran only at first spawn would let a resume, a clone or a daemon
// relaunch quietly start the very posture the first spawn refused. Each test
// below therefore changes the CONFIG between two launches rather than changing
// the request.

// copilotSettings writes settings.json into the flow's temp HOME, at the exact
// path a real Copilot launch would read (COPILOT_HOME defaults to ~/.copilot).
func copilotSettings(t *testing.T, f *testharness.Flow, body string) {
	t.Helper()
	dir := filepath.Join(f.World.HomeDir, ".copilot")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(dir, harness.CopilotSettingsFileName), []byte(body), 0o600))
}

// TestSpawn_CopilotTclaudeLayerLaunchesWithInnerSandboxOff is the positive
// case: the ordinary Copilot posture (no settings file, or one that leaves the
// command sandbox alone) reaches the launch with the outer layer selected.
func TestSpawn_CopilotTclaudeLayerLaunchesWithInnerSandboxOff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
	}{
		{name: "no settings file at all"},
		{name: "settings that say nothing about the sandbox", settings: `{"model":"gpt-5.4"}`},
		{name: "an explicitly disabled sandbox", settings: `{"sandbox":{"enabled":false}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			if tc.settings != "" {
				copilotSettings(t, f, tc.settings)
			}

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				"name":                   "copilot-layered",
				"harness":                harness.CopilotName,
				"sandbox_implementation": "tclaude-layer",
			})
			require.Equalf(t, http.StatusOK, resp.Code,
				"a Copilot launch whose own sandbox is off must be accepted; body=%s", resp.Raw)

			got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
			require.True(t, ok, "the spawn should have been observed by the sim spawner")
			assert.Equal(t, "tclaude-layer", got)
			// Copilot's pane is wrapped directly, like Claude's and Codex's —
			// not through OpenCode's server boundary.
			assertSandboxLayerCalls(t, f, testharness.SandboxLayerInteractive)
		})
	}
}

// TestSpawn_CopilotTclaudeLayerRefusesEnabledInnerSandbox is the core refusal.
// Launching here would stack Copilot's own MXC policy inside tclaude's, so the
// operator's effective confinement would be the unreviewed intersection of two
// policies while the recorded posture named only one.
func TestSpawn_CopilotTclaudeLayerRefusesEnabledInnerSandbox(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	copilotSettings(t, f, `{"sandbox":{"enabled":true}}`)

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "copilot-stacked",
		"harness":                harness.CopilotName,
		"sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusUnprocessableEntity, resp.Code,
		"an enabled inner sandbox must refuse rather than stack; body=%s", resp.Raw)
	failure := decodeFailure(t, resp.Raw)
	assert.Equal(t, harness.SandboxCapabilityCopilotInnerSandbox, failure.Code,
		"the refusal kind must name the Copilot-side conflict, so a client renders the "+
			"remedy for it rather than the generic sandbox-profile one")
	assert.Contains(t, failure.Error, "sandbox.enabled",
		"the refusal must name the key an operator has to change")
	assert.Contains(t, failure.Error, filepath.Join(f.World.HomeDir, ".copilot"),
		"the refusal must name the file, since Copilot's home is relocatable")
}

// TestSpawn_CopilotTclaudeLayerRefusesAmbiguousSettings: an unreadable posture
// is refused on the same terms as a hostile one. The shapes below all decode to
// the zero value under a typed struct — which for a field named `enabled` reads
// as "off" — so accepting them would mean launching on a claim nobody checked.
func TestSpawn_CopilotTclaudeLayerRefusesAmbiguousSettings(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
	}{
		{name: "unparsable settings", settings: `{"sandbox":`},
		{name: "a sandbox key that is not an object", settings: `{"sandbox":true}`},
		{name: "a stringly-typed enabled value", settings: `{"sandbox":{"enabled":"true"}}`},
		{name: "experimental features register the in-pane /sandbox command",
			settings: `{"experimental":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			copilotSettings(t, f, tc.settings)

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				"name":                   "copilot-ambiguous",
				"harness":                harness.CopilotName,
				"sandbox_implementation": "tclaude-layer",
			})
			require.Equalf(t, http.StatusUnprocessableEntity, resp.Code,
				"an unverifiable inner-sandbox posture must refuse; body=%s", resp.Raw)
			failure := decodeFailure(t, resp.Raw)
			assert.Equal(t, harness.SandboxCapabilityCopilotInnerSandbox, failure.Code)
		})
	}
}

// TestSpawn_CopilotHarnessBuiltinIsUnaffectedByInnerSandboxSettings keeps the
// gate scoped to the launches it is about. A spawn that is NOT using tclaude's
// layer makes no single-boundary claim, so Copilot's own sandbox setting is the
// operator's business and must not block anything.
func TestSpawn_CopilotHarnessBuiltinIsUnaffectedByInnerSandboxSettings(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	copilotSettings(t, f, `{"sandbox":{"enabled":true}}`)

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":    "copilot-own-wall",
		"harness": harness.CopilotName,
	})
	require.Equalf(t, http.StatusOK, resp.Code,
		"a spawn that does not select tclaude-layer must be unaffected by Copilot's own "+
			"sandbox setting; body=%s", resp.Raw)
	assertSandboxLayerCalls(t, f)
}

// TestResume_CopilotTclaudeLayerRechecksInnerSandbox is the reason the gate
// lives at the launch boundary rather than at the spawn request.
//
// The agent was enrolled under a verified posture. Its settings then change —
// which an operator can do at any time, and which tclaude neither controls nor
// is notified of — and the resume must re-verify rather than replay the
// recorded posture as if it were still true.
func TestResume_CopilotTclaudeLayerRechecksInnerSandbox(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "copilot-resumable",
		"harness":                harness.CopilotName,
		"sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "initial spawn; body=%s", spawn.Raw)

	f.MarkOffline(spawn.TmuxSession)
	copilotSettings(t, f, `{"sandbox":{"enabled":true}}`)

	// The relaunch path reports a refused launch in its OWN vocabulary — a 200
	// carrying action "error" and a detail — rather than as an HTTP status, the
	// same way it already reports sandbox_profile_changed. Asserting that shape
	// keeps this test about the re-verification rather than quietly proposing a
	// second error convention for one harness.
	resume := f.AsHuman().ResumeTolerating(spawn.ConvID)
	assert.Equal(t, "error", resume.Action,
		"a resume must re-verify Copilot's posture rather than replaying the recorded "+
			"one; body=%s", resume.Raw)
	assert.Contains(t, resume.Detail, "sandbox_posture_changed",
		"the refusal must be attributed to the posture, not to a profile change")
	assert.Contains(t, resume.Detail, "sandbox.enabled",
		"the refusal must name the key an operator has to change")
}

// TestClone_CopilotTclaudeLayerRechecksInnerSandbox: a clone inherits the
// source agent's launch posture, so it is exactly the path where a stale
// verification would be invisible — the operator never restates the sandbox
// choice, and the clone would carry a single-boundary claim made about a
// configuration that no longer exists.
func TestClone_CopilotTclaudeLayerRechecksInnerSandbox(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "copilot-clone-source",
		"harness":                harness.CopilotName,
		"sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "source spawn; body=%s", spawn.Raw)

	// A clone taken while the posture is still clean inherits the layer.
	clone := f.AsHuman().CloneWith(spawn.ConvID, map[string]any{"no_copy_conv": true})
	require.Equalf(t, http.StatusOK, clone.Code, "clean clone; body=%s", clone.Raw)
	got, ok := f.World.SpawnSandboxImplementation(clone.NewConv)
	require.True(t, ok, "the clone should have been observed by the sim spawner")
	assert.Equal(t, "tclaude-layer", got,
		"a clone must inherit the source's outer-layer posture")

	// The same clone, after the operator enables Copilot's own sandbox, is
	// refused: inheritance carries the CHOICE, never the verification.
	copilotSettings(t, f, `{"sandbox":{"enabled":true}}`)
	stale := f.AsHuman().CloneWith(spawn.ConvID, map[string]any{"no_copy_conv": true})
	require.Equalf(t, http.StatusUnprocessableEntity, stale.Code,
		"a clone must re-verify the inherited posture; body=%s", stale.Raw)
	failure := decodeFailure(t, stale.Raw)
	assert.Equal(t, harness.SandboxCapabilityCopilotInnerSandbox, failure.Code)
}

// TestReincarnate_CopilotTclaudeLayerRechecksInnerSandbox closes the last
// pane-starting path. Reincarnation replaces a running agent with a successor
// that inherits its identity and posture, and it is initiated by the agent
// itself — so nobody is looking at a spawn dialog when it happens, which is
// precisely why it must not be the path that skips the check.
func TestReincarnate_CopilotTclaudeLayerRechecksInnerSandbox(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "copilot-reincarnating",
		"harness":                harness.CopilotName,
		"sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "source spawn; body=%s", spawn.Raw)

	copilotSettings(t, f, `{"sandbox":{"enabled":true}}`)
	resp := f.AsHuman().ReincarnateWith(spawn.ConvID, map[string]any{"follow_up": "keep going"})
	require.Equalf(t, http.StatusUnprocessableEntity, resp.Code,
		"reincarnation must re-verify the inherited posture; body=%s", resp.Raw)
	failure := decodeFailure(t, resp.Raw)
	assert.Equal(t, harness.SandboxCapabilityCopilotInnerSandbox, failure.Code)
}
