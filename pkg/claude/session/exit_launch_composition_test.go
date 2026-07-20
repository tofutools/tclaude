package session

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestPaneBootstrapLaunchGateBlocksHarnessUntilReleased(t *testing.T) {
	binDir := t.TempDir()
	harnessPath := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(harnessPath, []byte("#!/bin/sh\nprintf started > \"$TCL573_STARTED\"\n"), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(t.TempDir(), "harness-started")
	t.Setenv("TCL573_STARTED", marker)

	gatePath := filepath.Join(t.TempDir(), "launch-gate")
	require.NoError(t, os.WriteFile(gatePath, []byte("pending"), 0o600))
	guard := &exitLaunchGuard{enabled: true, barrierPath: gatePath}
	cmd := exec.Command("sh", "-c", guard.wrap("claude"))
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	})
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		require.Failf(t, "harness crossed pending launch gate", "wrapped command exited early: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	_, err := os.Stat(marker)
	require.ErrorIs(t, err, os.ErrNotExist, "the harness must not start while the launch gate is pending")

	require.NoError(t, os.WriteFile(gatePath, []byte("go"), 0o600))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.Fail(t, "harness did not start after launch gate release")
	}
	require.FileExists(t, marker)
}

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

// The gate release must flip "pending" → "go" as ONE observable step. An
// in-place truncate+write publishes an intermediate EMPTY gate, and the
// pane's poll loop treats any non-pending read as terminal — a reader
// catching the intermediate state turns a correct release into a spurious
// exit-125 launch failure. Whole-file replacement makes intermediate
// states unobservable: a descriptor opened before publication keeps
// reading the complete old content, while fresh opens read the complete
// new content.
func TestWriteExistingExitLaunchGatePublishesAtomically(t *testing.T) {
	dir := t.TempDir()
	gatePath := filepath.Join(dir, exitLaunchArtifactPrefix+"gate")
	require.NoError(t, os.WriteFile(gatePath, []byte("pending"), 0o600))

	before, err := os.Open(gatePath)
	require.NoError(t, err)
	defer func() { _ = before.Close() }()

	require.NoError(t, writeExistingExitLaunchGate(gatePath, "go"))

	old, err := io.ReadAll(before)
	require.NoError(t, err)
	assert.Equal(t, "pending", string(old),
		"a reader that opened the gate before publication must observe the complete old content, never a truncated intermediate")
	fresh, err := os.ReadFile(gatePath)
	require.NoError(t, err)
	assert.Equal(t, "go", string(fresh), "fresh opens read the complete new content")
}

// "go" is shorter than "pending": any publication path that skips
// truncation would leave "gonding" behind and permanently wedge the
// pane's release check. Locks the shorter-replacement semantics, and that
// publication leaves no temporary sibling behind.
func TestWriteExistingExitLaunchGateReplacesLongerContent(t *testing.T) {
	dir := t.TempDir()
	gatePath := filepath.Join(dir, exitLaunchArtifactPrefix+"gate")
	require.NoError(t, os.WriteFile(gatePath, []byte("pending"), 0o600))
	require.NoError(t, writeExistingExitLaunchGate(gatePath, "go"))
	raw, err := os.ReadFile(gatePath)
	require.NoError(t, err)
	assert.Equal(t, "go", string(raw))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "publication must not leak its temporary sibling")
}

// A gate the pane already consumed or removed must never be recreated by
// a delayed publication (see also
// TestExitLaunchGuard_LateReleaseAfterExecutedTimeoutDoesNotResurrectGate).
func TestWriteExistingExitLaunchGateRefusesMissingGate(t *testing.T) {
	gatePath := filepath.Join(t.TempDir(), "launch-gate")
	require.ErrorIs(t, writeExistingExitLaunchGate(gatePath, "go"), os.ErrNotExist)
	require.NoFileExists(t, gatePath)
}
