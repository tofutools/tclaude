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
	cmd := exec.Command("sh", "-c", guardHarnessCommandWithCwdProof(
		"printf launched > "+clcommon.ShellQuoteArg(launched), proof))
	cmd.Dir = dir
	require.NoError(t, cmd.Run())
	assert.FileExists(t, launched)
	_, err := os.Lstat(marker)
	assert.True(t, os.IsNotExist(err), "proof marker should be consumed; err=%v", err)
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
	cmd := exec.Command("sh", "-c", guardHarnessCommandWithCwdProof(
		"printf launched > "+clcommon.ShellQuoteArg(launched), proof))
	cmd.Dir = target
	err := cmd.Run()
	require.Error(t, err)
	assert.NoFileExists(t, launched)
}

func TestSpawnCwdProofTokenValidation(t *testing.T) {
	assert.True(t, isValidSpawnCwdProofToken("abc_DEF-123"))
	assert.False(t, isValidSpawnCwdProofToken(""))
	assert.False(t, isValidSpawnCwdProofToken("abc/../../escape"))
	assert.False(t, isValidSpawnCwdProofToken("abc; touch nope"))
}
