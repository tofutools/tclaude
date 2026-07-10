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

func TestSpawnCwdReadinessFileLivesInPrivateDataDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	path, cleanup, err := newSpawnCwdReadinessFile()
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Equal(t, filepath.Join(home, ".tclaude", "data", "spawn-readiness"), filepath.Dir(path))

	// The guard is a shell prefix executed before harnessCmd starts the
	// sandboxed harness, so it can acknowledge readiness in the denied data
	// subtree without granting that subtree to the agent.
	cmd := exec.Command("sh", "-c", guardHarnessCommandWithDirProof("true", "", path, false, nil))
	require.NoError(t, cmd.Run())
	status, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(status))
}

func TestGuardHarnessCommandWithCwdProof_ChecksMarkerThenRuns(t *testing.T) {
	dir := t.TempDir()
	proof := "valid_proof-123"
	marker := filepath.Join(dir, clcommon.SpawnDirWriteProofPrefix+proof)
	require.NoError(t, os.WriteFile(marker, nil, 0o600))
	launched := filepath.Join(dir, "launched")
	ready := filepath.Join(dir, "ready")
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))

	cmd := exec.Command("sh", "-c", guardHarnessCommandWithDirProof(
		"printf launched > "+clcommon.ShellQuoteArg(launched), proof, ready, true, nil))
	cmd.Dir = dir
	require.NoError(t, cmd.Run())

	assert.FileExists(t, launched)
	assert.FileExists(t, marker, "agentd owns marker cleanup after the pane-side check")
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
	require.NoError(t, os.WriteFile(filepath.Join(target, clcommon.SpawnDirWriteProofPrefix+proof), nil, 0o600))

	require.NoError(t, os.Rename(target, proved))
	require.NoError(t, os.Symlink(forbidden, target))
	launched := filepath.Join(forbidden, "launched")
	ready := filepath.Join(root, "ready")
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))

	cmd := exec.Command("sh", "-c", guardHarnessCommandWithDirProof(
		"printf launched > "+clcommon.ShellQuoteArg(launched), proof, ready, true, nil))
	cmd.Dir = target
	err := cmd.Run()
	require.Error(t, err)
	assert.NoFileExists(t, launched)
	status, readErr := os.ReadFile(ready)
	require.NoError(t, readErr)
	assert.Equal(t, "error:proof", string(status))
}

func TestGuardHarnessCommandWithDirProofRejectsSwappedRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "cwd")
	grant := filepath.Join(root, "grant")
	forbidden := filepath.Join(root, "forbidden")
	require.NoError(t, os.Mkdir(cwd, 0o700))
	require.NoError(t, os.Mkdir(grant, 0o700))
	require.NoError(t, os.Mkdir(forbidden, 0o700))
	proof := "valid_proof-789"
	marker := clcommon.SpawnDirWriteProofPrefix + proof
	require.NoError(t, os.WriteFile(filepath.Join(cwd, marker), nil, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(grant, marker), nil, 0o600))
	require.NoError(t, os.Rename(grant, grant+"-old"))
	require.NoError(t, os.Symlink(forbidden, grant))
	ready := filepath.Join(root, "ready")
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))

	cmd := exec.Command("sh", "-c", guardHarnessCommandWithDirProof("true", proof, ready, true, []string{grant}))
	cmd.Dir = cwd
	require.Error(t, cmd.Run())
	status, err := os.ReadFile(ready)
	require.NoError(t, err)
	assert.Equal(t, "error:repository-proof", string(status))
}

func TestGuardHarnessCommandWithDirProofChecksRepositoryWithoutCwdMarker(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cwd := filepath.Join(root, "cwd")
	grant := filepath.Join(root, "grant")
	require.NoError(t, os.Mkdir(cwd, 0o700))
	require.NoError(t, os.Mkdir(grant, 0o700))
	proof := "valid_proof-extra"
	require.NoError(t, os.WriteFile(filepath.Join(grant, clcommon.SpawnDirWriteProofPrefix+proof), nil, 0o600))
	ready := filepath.Join(root, "ready")
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))

	cmd := exec.Command("sh", "-c", guardHarnessCommandWithDirProof("true", proof, ready, false, []string{grant}))
	cmd.Dir = cwd
	require.NoError(t, cmd.Run())
	status, err := os.ReadFile(ready)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(status))
}

func TestSpawnCwdProofTokenValidation(t *testing.T) {
	assert.True(t, isValidSpawnCwdProofToken("abc_DEF-123"))
	assert.False(t, isValidSpawnCwdProofToken(""))
	assert.False(t, isValidSpawnCwdProofToken("abc/../../escape"))
	assert.False(t, isValidSpawnCwdProofToken("abc; touch nope"))
}
