package harness

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateStoreIdentityCapturesEffectiveLaunchEnvironment(t *testing.T) {
	home := t.TempDir()
	for _, tc := range []struct {
		name, harnessName, selector, selected, fallback string
	}{
		{name: "claude", harnessName: DefaultName, selector: "CLAUDE_CONFIG_DIR", selected: "claude-custom", fallback: ".claude"},
		{name: "codex", harnessName: CodexName, selector: "CODEX_HOME", selected: "codex-custom", fallback: ".codex"},
		{name: "copilot", harnessName: CopilotName, selector: CopilotHomeEnvVar, selected: "copilot-custom", fallback: ".copilot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, ok := Get(tc.harnessName)
			require.True(t, ok)
			custom := filepath.Join(home, tc.selected)
			got, err := h.StateStore.CaptureStateStoreIdentity(StateStoreLaunch{Environment: map[string]string{
				"HOME": home, tc.selector: custom,
			}})
			require.NoError(t, err)
			assert.Equal(t, custom, got.StateRoot)
			assert.Equal(t, "host-path:"+custom, got.Namespace)
			assert.Equal(t, tc.selector, got.Source)

			fallback, err := h.StateStore.CaptureStateStoreIdentity(StateStoreLaunch{
				Environment: map[string]string{"HOME": home},
			})
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(home, tc.fallback), fallback.StateRoot)
		})
	}
}

func TestStateStoreIdentityFailsClosedWithoutLaunchEvidence(t *testing.T) {
	for _, name := range []string{DefaultName, CodexName, CopilotName, OpenCodeName} {
		h, ok := Get(name)
		require.True(t, ok)
		_, err := h.StateStore.CaptureStateStoreIdentity(StateStoreLaunch{})
		require.Error(t, err, name)
	}
}

func TestOpenCodeStateStoreIdentityRequiresExplicitAllocation(t *testing.T) {
	h, ok := Get(OpenCodeName)
	require.True(t, ok)
	root := filepath.Join(t.TempDir(), "private-opencode")
	got, err := h.StateStore.CaptureStateStoreIdentity(StateStoreLaunch{
		Environment: map[string]string{"HOME": t.TempDir()}, ExplicitStateRoot: root,
	})
	require.NoError(t, err)
	assert.Equal(t, root, got.StateRoot)
	require.NoError(t, h.StateStore.ValidateStateStoreIdentity(FrozenStateStoreContract{
		HarnessName: OpenCodeName, StateRoot: root,
	}, got))
	assert.Error(t, h.StateStore.ValidateStateStoreIdentity(FrozenStateStoreContract{
		HarnessName: OpenCodeName, StateRoot: filepath.Join(t.TempDir(), "successor"),
	}, got))
}
