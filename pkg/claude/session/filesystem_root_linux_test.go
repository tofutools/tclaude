//go:build linux

package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestExplicitFilesystemRootLaunchPostureUsesProvenTargetMatrix(t *testing.T) {
	effective := sandboxpolicy.EffectiveProfile{
		FilesystemRoot: sandboxpolicy.FilesystemRootSeparate,
		Network:        &sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen},
	}
	root, err := TclaudeLayerLaunchRootPosture(
		harness.MustGet(harness.CodexName),
		sandboxpolicy.ImplementationTclaudeLayer,
		sandboxpolicy.NetworkHostOpen,
		effective,
	)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.RootConstructed, root)

	_, err = TclaudeLayerLaunchRootPosture(
		harness.MustGet(harness.CodexName),
		sandboxpolicy.ImplementationStacked,
		sandboxpolicy.NetworkHostOpen,
		effective,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "filesystem_root")
}

func TestRunNewRefusesRootOnlySnapshotOnUnconfinedImplementation(t *testing.T) {
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{
		FilesystemRoot: sandboxpolicy.FilesystemRootSeparate,
	}, nil)
	path, digest, err := sandboxpolicy.WriteSnapshotFile(t.TempDir(), snapshot)
	require.NoError(t, err)

	err = runNew(&NewParams{
		Harness:               harness.CodexName,
		Sandbox:               harness.SandboxDangerFull,
		SandboxImpl:           string(sandboxpolicy.ImplementationOff),
		SandboxSnapshotPath:   path,
		SandboxSnapshotDigest: digest,
		Dir:                   t.TempDir(),
		Detached:              true,
	})
	require.Error(t, err)
	var capabilityErr *harness.SandboxCapabilityError
	require.ErrorAs(t, err, &capabilityErr)
	assert.Equal(t, harness.SandboxCapabilityFilesystemRoot, capabilityErr.Kind)
}
