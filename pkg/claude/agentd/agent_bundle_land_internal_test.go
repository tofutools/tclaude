package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Native history is budgeted by effective context window, not model name.
func TestLandsOnSmallerWindow(t *testing.T) {
	claude, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)
	var std string
	for _, m := range claude.Models.Models() {
		if m != "" {
			std = m
			break
		}
	}
	require.NotEmpty(t, std)
	origin := func(m string) *agentbundle.Origin { return &agentbundle.Origin{Model: m} }
	assert.True(t, landsOnSmallerWindow(claude, origin(std+"[1m]"), std), "1M onto 200k is the same model with a smaller window")
	assert.False(t, landsOnSmallerWindow(claude, origin(std), std+"[1m]"), "a larger window keeps native fidelity")
	assert.False(t, landsOnSmallerWindow(claude, origin(std), std), "an equal window keeps native fidelity")
	assert.False(t, landsOnSmallerWindow(claude, origin("claude-unlisted"), "claude-unlisted"), "the same model with an unknown window keeps native fidelity")
	assert.True(t, landsOnSmallerWindow(claude, origin("claude-unlisted"), std), "a different model from an unknown window is budgeted")
}

// A relaunch profile records a blank implementation as harness-builtin, which
// OpenCode refuses. That must not cost the agent its stricter recorded mode.
func TestReceiverSandboxProfile_OpenCodeKeepsATighterModeFromARelaunchProfile(t *testing.T) {
	setupTestDB(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "landing", Harness: harness.OpenCodeName, Sandbox: harness.OpenCodeSandboxOff})
	require.NoError(t, err)
	opencode, err := harness.Resolve(harness.OpenCodeName)
	require.NoError(t, err)

	got := receiverSandboxProfile(spawnProfileJSON{
		Sandbox: harness.OpenCodeSandboxAccessControl, SandboxImplementation: "harness-builtin",
	}, opencode, "", "landing")

	assert.Equal(t, harness.OpenCodeSandboxAccessControl, got.Sandbox)
	assert.Empty(t, got.SandboxImplementation)
}
