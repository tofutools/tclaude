package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A sandbox-off spawn omits every sandbox-profile tier, but the launch
// environment (group, spawn-profile and explicit entries) is not sandbox
// policy: it must still reach the pane. Regression: the spawn executor rebuilt
// the omitted snapshot from scratch and dropped it, so e.g. a Gemini pane lost
// its GOOGLE_CLOUD_PROJECT and fell back to the auth dialog.
func TestSpawn_SandboxOffKeepsTheLaunchEnvironment(t *testing.T) {
	for _, name := range []string{harness.DefaultName, harness.CodexName, harness.GeminiName} {
		t.Run(name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")

			resp := f.AsHuman().SpawnWith("crew", map[string]any{
				"name":                   name + "-off-env",
				"harness":                name,
				"sandbox_implementation": string(sandboxpolicy.ImplementationOff),
				"environment": []map[string]string{
					{"name": "GOOGLE_CLOUD_PROJECT", "value": "proj-1"},
				},
			})
			require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)

			snapshot, ok := f.World.SpawnSandboxPolicy(resp.ConvID)
			require.True(t, ok)
			require.NotNil(t, snapshot)
			assert.True(t, snapshot.ProfilesOmitted)
			assert.Contains(t, sandboxpolicy.EnvironmentForLaunch(snapshot),
				sandboxpolicy.EnvironmentEntry{Name: "GOOGLE_CLOUD_PROJECT", Value: "proj-1"})
		})
	}
}
