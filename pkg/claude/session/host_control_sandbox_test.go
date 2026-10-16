package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

type fakeHostControlSandbox struct {
	denyPath string
}

func TestOneShotLaunchPostureCarriesOuterPolicyIntoInnerWall(t *testing.T) {
	h, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)
	cache := testutil.CanonicalTempDir(t)
	snapshot := sandboxpolicy.NewSnapshot(sandboxpolicy.EffectiveProfile{
		Filesystem:  []sandboxpolicy.FilesystemGrant{{Path: cache, Access: sandboxpolicy.AccessWrite}},
		Environment: []sandboxpolicy.EnvironmentEntry{{Name: "GOCACHE", Value: cache}},
	}, nil)
	posture, err := OneShotLaunchPosture(testutil.CanonicalTempDir(t), h,
		harness.ClaudeSandboxOn, "default", false, &snapshot, sandboxpolicy.ImplementationStacked)
	require.NoError(t, err)
	require.Contains(t, posture.SandboxWriteDirs, cache)
	require.Equal(t, cache, posture.ShellEnvironment["GOCACHE"])
}

func (sandbox fakeHostControlSandbox) PrepareLaunch(spec harness.SpawnSpec) (harness.SpawnSpec, error) {
	spec.SandboxDenyDirs = append(spec.SandboxDenyDirs, sandbox.denyPath)
	return spec, nil
}

func TestOneShotLaunchPostureUsesDescriptorHostControlCapability(t *testing.T) {
	target := &harness.Harness{
		Name: "claude-compatible-test",
		HostControlSandbox: fakeHostControlSandbox{
			denyPath: "/daemon-prepared/host-control",
		},
	}

	posture, err := OneShotLaunchPosture(
		t.TempDir(), target, harness.ClaudeSandboxInherit, "", false, nil)
	require.NoError(t, err)
	assert.Equal(t, []string{"/daemon-prepared/host-control"}, posture.SandboxDenyDirs)
}
