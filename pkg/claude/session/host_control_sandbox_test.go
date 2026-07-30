package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

type fakeHostControlSandbox struct {
	denyPath string
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
