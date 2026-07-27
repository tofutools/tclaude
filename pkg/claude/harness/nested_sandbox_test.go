package harness

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNestedSandboxContractsPrepareRealInnerEngines(t *testing.T) {
	claude := MustGet(DefaultName)
	require.True(t, claude.SupportsNestedSandbox())
	claudeSpec := claude.NestedSandbox.PrepareLaunch(SpawnSpec{
		SandboxMode: ClaudeSandboxOff,
	})
	assert.Equal(t, ClaudeSandboxOn, claudeSpec.SandboxMode)
	assert.True(t, claudeSpec.StrongNestedSandbox)
	var settings map[string]any
	require.NoError(t, json.Unmarshal([]byte(claudeSettingsJSON(claudeSpec)), &settings))
	block := settings["sandbox"].(map[string]any)
	assert.Equal(t, false, block["enableWeakerNestedSandbox"])
	assert.Equal(t, true, block["enabled"])

	codex := MustGet(CodexName)
	require.True(t, codex.SupportsNestedSandbox())
	codexSpec := codex.NestedSandbox.PrepareLaunch(SpawnSpec{
		SandboxMode: SandboxDangerFull,
	})
	assert.Empty(t, codexSpec.SandboxMode)
	assert.Equal(t, CodexAgentProfile, codexSpec.PermissionProfile)
	assert.True(t, codexSpec.StrongNestedSandbox)
	command := codex.Spawn.BuildCommand(codexSpec)
	assert.Contains(t, command, "-p "+CodexAgentProfile)
	assert.Contains(t, command, "features.use_legacy_landlock=false")
}

func TestNestedSandboxProbeCommandsAreModelFreeAndPolicyShaped(t *testing.T) {
	executable := NestedSandboxExecutable{Path: "/usr/bin/engine"}
	for _, tc := range []struct {
		name     string
		contract NestedSandboxContract
		contains []string
	}{
		{
			name: "claude", contract: claudeNestedSandbox{},
			contains: []string{"--settings", "enableWeakerNestedSandbox", "socket.AF_UNIX"},
		},
		{
			name: "codex", contract: codexNestedSandbox{},
			contains: []string{"sandbox -P stacked-probe", "use_legacy_landlock = false", "CODEX_HOME="},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probe, err := tc.contract.PrepareProbe(t.TempDir(), executable)
			require.NoError(t, err)
			t.Cleanup(probe.Cleanup)
			joined := probe.Command
			for _, path := range probe.KnownPaths {
				if strings.HasSuffix(path, "settings.json") || strings.HasSuffix(path, "config.toml") {
					content, err := os.ReadFile(path)
					require.NoError(t, err)
					joined += string(content)
				}
			}
			for _, expected := range tc.contains {
				assert.Contains(t, joined, expected)
			}
			assert.Contains(t, joined, string(os.PathSeparator)+"workspace"+string(os.PathSeparator)+"allowed")
			assert.Contains(t, joined, string(os.PathSeparator)+"private"+string(os.PathSeparator)+"denied")
			assert.NotContains(t, joined, "prompt")
		})
	}
}
