package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestPaneBootstrapLaunchCompositionPreservesHarnessStatus(t *testing.T) {
	binDir := t.TempDir()
	writeExitHarness := func(name string, status int) {
		t.Helper()
		path := filepath.Join(binDir, name)
		require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit "+strconv.Itoa(status)+"\n"), 0o700))
	}
	writeExitHarness("claude", 23)
	writeExitHarness("codex", 37)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	gatePath := filepath.Join(t.TempDir(), "launch-gate")
	require.NoError(t, os.WriteFile(gatePath, []byte("go"), 0o600))
	guard := &exitLaunchGuard{enabled: true, barrierPath: gatePath}
	claudeCmd := harness.MustGet(harness.DefaultName).Spawn.BuildCommand(harness.SpawnSpec{})
	assert.NotContains(t, claudeCmd, "exec claude", "the shipping pane-bootstrap contract must not silently become direct harness observation")
	err := exec.Command("sh", "-c", guard.wrap(claudeCmd)).Run()
	var exitErr *exec.ExitError
	require.True(t, errors.As(err, &exitErr))
	assert.Equal(t, 23, exitErr.ExitCode(), "the pane bootstrap exits with Claude's status")

	// A managed Codex launch adds post-harness profile cleanup. Even a cleanup
	// failure must not replace the saved harness status observed on the pane.
	codexCmd := harness.MustGet(harness.CodexName).Spawn.BuildCommand(harness.SpawnSpec{
		PermissionProfile: harness.CodexAgentProfile,
	})
	assert.NotContains(t, codexCmd, "exec codex", "the shipping observer remains the pane bootstrap")
	err = exec.Command("sh", "-c", commandWithFileCleanupCommand(codexCmd, "false")).Run()
	require.True(t, errors.As(err, &exitErr))
	assert.Equal(t, 37, exitErr.ExitCode(), "managed cleanup failure cannot mask the saved Codex status")
}
