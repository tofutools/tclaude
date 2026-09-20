package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A shell brief is command text, not an agent welcome. The flow simulator
// keeps its pane alive like the other fake harnesses, which lets this exercise
// the production request resolution, launch enrollment, and SpawnArgs seam.
func TestSpawnShellPassesInitialMessageAsCommand(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	const command = "printf '%s\\n' hello && go test ./..."
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "checks",
		"harness":         "shell",
		"initial_message": command,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	f.AssertSpawnInitialPrompt(spawn.ConvID, command, 10*time.Second)
	prompt, ok := f.World.SpawnInitialPrompt(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, command, prompt, "shell command must not be wrapped in an agent welcome")
}

func TestSpawnShellWithoutInitialMessageStartsInteractive(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":    "terminal",
		"harness": "shell",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	prompt, ok := f.World.SpawnInitialPrompt(spawn.ConvID)
	assert.True(t, ok)
	assert.Empty(t, prompt)
}
