package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

func TestGuardHarnessCommandWithCwdProof_ConsumesMarkerThenRuns(t *testing.T) {
	dir := t.TempDir()
	proof := "valid_proof-123"
	marker := filepath.Join(dir, clcommon.SpawnCwdProofPrefix+proof)
	require.NoError(t, os.WriteFile(marker, nil, 0o600))
	launched := filepath.Join(dir, "launched")
	ready := filepath.Join(dir, "ready")
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))
	cmd := exec.Command("sh", "-c", guardHarnessCommandWithCwdProof(
		"printf launched > "+clcommon.ShellQuoteArg(launched), proof, "", ready))
	cmd.Dir = dir
	require.NoError(t, cmd.Run())
	assert.FileExists(t, launched)
	_, err := os.Lstat(marker)
	assert.True(t, os.IsNotExist(err), "proof marker should be consumed; err=%v", err)
	status, err := os.ReadFile(ready)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(status))
}

func TestGuardHarnessCommandWithCwdProof_RejectsPathSwap(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	proved := filepath.Join(root, "proved")
	forbidden := filepath.Join(root, "forbidden")
	require.NoError(t, os.Mkdir(target, 0o700))
	require.NoError(t, os.Mkdir(forbidden, 0o700))
	proof := "valid_proof-456"
	require.NoError(t, os.WriteFile(
		filepath.Join(target, clcommon.SpawnCwdProofPrefix+proof), nil, 0o600))

	// Swap the already-proved pathname to another directory before the pane's
	// shell starts. The shell chdirs into forbidden, finds no marker there, and
	// must refuse to run the harness command.
	require.NoError(t, os.Rename(target, proved))
	require.NoError(t, os.Symlink(forbidden, target))
	launched := filepath.Join(forbidden, "launched")
	mutated := filepath.Join(forbidden, "privileged-setup-ran")
	ready := filepath.Join(root, "ready")
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))
	prepare := "printf mutated > " + clcommon.ShellQuoteArg(mutated)
	cmd := exec.Command("sh", "-c", guardHarnessCommandWithCwdProof(
		"printf launched > "+clcommon.ShellQuoteArg(launched), proof, prepare, ready))
	cmd.Dir = target
	err := cmd.Run()
	require.Error(t, err)
	assert.NoFileExists(t, launched)
	assert.NoFileExists(t, mutated, "cwd-dependent privileged setup must not run after a path swap")
	status, readErr := os.ReadFile(ready)
	require.NoError(t, readErr)
	assert.Equal(t, "error:proof", string(status))
}

func TestSpawnCwdProofTokenValidation(t *testing.T) {
	assert.True(t, isValidSpawnCwdProofToken("abc_DEF-123"))
	assert.False(t, isValidSpawnCwdProofToken(""))
	assert.False(t, isValidSpawnCwdProofToken("abc/../../escape"))
	assert.False(t, isValidSpawnCwdProofToken("abc; touch nope"))
}

func TestWaitForSpawnCwdReadiness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ready")
	require.NoError(t, os.WriteFile(path, []byte("ok"), 0o600))
	require.NoError(t, waitForSpawnCwdReadiness(path))

	require.NoError(t, os.WriteFile(path, []byte("error:proof"), 0o600))
	err := waitForSpawnCwdReadiness(path)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "proof")
}
