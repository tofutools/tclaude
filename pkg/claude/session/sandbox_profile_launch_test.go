package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestSandboxSnapshotDirsOmitsMissingRuleUntilLaterLaunch(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	missing := filepath.Join(root, "future", "cache")
	snapshot := &sandboxpolicy.Snapshot{
		Version: sandboxpolicy.SnapshotVersion,
		Effective: sandboxpolicy.EffectiveProfile{Filesystem: []sandboxpolicy.FilesystemGrant{{
			Path: missing, Access: sandboxpolicy.AccessWrite,
		}}},
	}

	launch, err := sandboxSnapshotForLaunch(snapshot)
	require.NoError(t, err)
	assert.Empty(t, sandboxSnapshotDirs(launch, sandboxpolicy.AccessWrite),
		"a missing rule must not reach the harness on this launch")
	require.NoError(t, os.MkdirAll(missing, 0o755))
	launch, err = sandboxSnapshotForLaunch(snapshot)
	require.NoError(t, err)
	assert.Equal(t, []string{missing}, sandboxSnapshotDirs(launch, sandboxpolicy.AccessWrite),
		"the same frozen rule becomes active on a later launch")
}

func TestSandboxSnapshotProofDirsExcludesMaterializedAgentDirectory(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	cwd := filepath.Join(root, "cwd")
	customWriteDir := filepath.Join(root, "custom")
	agentWriteDir := filepath.Join(root, "agent-dirs", "spwn-test", "GOCACHE")
	for _, dir := range []string{cwd, customWriteDir, agentWriteDir} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	snapshot := &sandboxpolicy.Snapshot{
		Version: sandboxpolicy.SnapshotVersion,
		Effective: sandboxpolicy.EffectiveProfile{
			AgentDirectories: []string{"GOCACHE"},
			Environment:      []sandboxpolicy.EnvironmentEntry{{Name: "GOCACHE", Value: agentWriteDir}},
			Filesystem: []sandboxpolicy.FilesystemGrant{
				{Path: customWriteDir, Access: sandboxpolicy.AccessWrite},
				{Path: agentWriteDir, Access: sandboxpolicy.AccessWrite},
			},
		},
	}

	assert.Equal(t, []string{customWriteDir, agentWriteDir},
		sandboxSnapshotDirs(snapshot, sandboxpolicy.AccessWrite),
		"the generated directory must remain writable by the child")
	proofDirs, generatedDirs := sandboxSnapshotProofDirs(snapshot, sandboxpolicy.AccessWrite)
	assert.Equal(t, []string{customWriteDir}, proofDirs,
		"only caller-controlled roots should require the caller's marker")
	assert.Equal(t, []string{agentWriteDir}, generatedDirs,
		"the generated root should retain a path-substitution check")

	proof := "proof-agent-directory"
	marker := clcommon.SpawnDirWriteProofPrefix + proof
	for _, dir := range []string{cwd, customWriteDir} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, marker), nil, 0o600))
	}
	ready := filepath.Join(root, "ready")
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))

	cmd := exec.Command("sh", "-c", guardHarnessCommandWithDirProof(
		"true", proof, ready, true, proofDirs, generatedDirs))
	cmd.Dir = cwd
	require.NoError(t, cmd.Run(),
		"a daemon-materialized directory created after the challenge must not need a caller marker")
	status, err := os.ReadFile(ready)
	require.NoError(t, err)
	assert.Equal(t, "ok", string(status))

	// The generated directory needs no caller marker, but it must not be
	// replaceable with a symlink between daemon materialization and launch.
	forbidden := filepath.Join(root, "forbidden")
	require.NoError(t, os.Mkdir(forbidden, 0o700))
	require.NoError(t, os.Rename(agentWriteDir, agentWriteDir+"-old"))
	require.NoError(t, os.Symlink(forbidden, agentWriteDir))
	require.NoError(t, os.WriteFile(ready, []byte("pending"), 0o600))
	cmd = exec.Command("sh", "-c", guardHarnessCommandWithDirProof(
		"true", proof, ready, true, proofDirs, generatedDirs))
	cmd.Dir = cwd
	require.Error(t, cmd.Run())
	status, err = os.ReadFile(ready)
	require.NoError(t, err)
	assert.Equal(t, "error:repository-proof", string(status))
}
