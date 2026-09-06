package session

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestCaptureLaunchStateStoreIdentityUsesComposedCopilotEnvironment(t *testing.T) {
	t.Setenv("HOME", filepath.Join(t.TempDir(), "ambient-home"))
	t.Setenv(harness.CopilotHomeEnvVar, filepath.Join(t.TempDir(), "ambient-copilot"))
	h, err := harness.Resolve(harness.CopilotName)
	require.NoError(t, err)
	launchHome := filepath.Join(t.TempDir(), "launch-home")
	explicit := filepath.Join(t.TempDir(), "launch-copilot")

	identity, err := captureLaunchStateStoreIdentity(h, map[string]string{
		"HOME": launchHome, harness.CopilotHomeEnvVar: explicit,
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, identity)
	assert.Equal(t, explicit, identity.StateRoot)
	assert.Equal(t, harness.CopilotHomeEnvVar, identity.Source)

	identity, err = captureLaunchStateStoreIdentity(h, map[string]string{
		"HOME": launchHome, harness.CopilotHomeEnvVar: "",
	}, nil)
	require.NoError(t, err)
	require.NotNil(t, identity)
	assert.Equal(t, filepath.Join(launchHome, ".copilot"), identity.StateRoot)
	assert.Equal(t, "launch HOME", identity.Source)
}

func TestCaptureLaunchStateStoreIdentityKeepsPreLaunchMutationUnknown(t *testing.T) {
	h, err := harness.Resolve(harness.CopilotName)
	require.NoError(t, err)
	identity, err := captureLaunchStateStoreIdentity(h, map[string]string{
		"HOME": t.TempDir(),
	}, []sandboxpolicy.PreLaunchBlock{{Name: "dynamic", Script: "export COPILOT_HOME=/dynamic"}})
	require.NoError(t, err)
	assert.Nil(t, identity)
}
