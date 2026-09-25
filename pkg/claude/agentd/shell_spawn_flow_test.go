package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestSpawnShellRejectsInteractiveInitialMessage(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "checks",
		"harness":         "shell",
		"initial_message": "printf hello",
	})
	require.Equalf(t, http.StatusBadRequest, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "non_interactive")
}

func TestSpawnShellRejectsProfileInitialMessage(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "shell-command", Harness: "shell", InitialMessage: "printf hello",
	})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "checks", "profile": "shell-command",
	})
	require.Equalf(t, http.StatusBadRequest, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Contains(t, string(spawn.Raw), "non_interactive")
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
