package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
