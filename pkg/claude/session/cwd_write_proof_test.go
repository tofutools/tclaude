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

func TestGuardHarnessCommandWithCwdProof_ChecksMarkerThenRuns(t *testing.T) {
	dir := t.TempDir()
	proof := "valid_proof-123"
	marker := filepath.Join(dir, clcommon.SpawnDirWriteProofPrefix+proof)
	require.NoError(t, os.WriteFile(marker, nil, 0o600))
	launched := filepath.Join(dir, "launched")
	ready := filepath.Join(dir, "ready")
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))

	cmd := exec.Command("sh", "-c", guardHarnessCommandWithCwdProof(
		"printf launched > "+clcommon.ShellQuoteArg(launched), proof, ready))
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

	cmd := exec.Command("sh", "-c", guardHarnessCommandWithCwdProof(
		"printf launched > "+clcommon.ShellQuoteArg(launched), proof, ready))
	cmd.Dir = target
	err := cmd.Run()
	require.Error(t, err)
	assert.NoFileExists(t, launched)
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
