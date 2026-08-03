package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

func TestPrepareClaudeSandboxLaunchDeniesOnlyTclaudeTmuxSocket(t *testing.T) {
	tmuxBase := t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	originalDenies := []string{"/opt/secret"}
	claude := MustGet(DefaultName)
	require.True(t, claude.SupportsHostControlSandbox())
	spec, err := claude.PrepareHostControlSandboxLaunch(SpawnSpec{
		SandboxMode:     ClaudeSandboxInherit,
		SandboxDenyDirs: originalDenies,
	})
	require.NoError(t, err)

	canonicalTmuxBase, err := filepath.EvalSymlinks(tmuxBase)
	require.NoError(t, err)
	socketPath := filepath.Join(
		canonicalTmuxBase, fmt.Sprintf("tmux-%d", os.Getuid()), clcommon.TmuxSocketName())
	assert.Equal(t, []string{"/opt/secret", socketPath}, spec.SandboxDenyDirs)
	assert.Equal(t, []string{"/opt/secret"}, originalDenies,
		"preparing a launch must not mutate the caller's slice")

	var settings struct {
		Sandbox struct {
			Enabled    *bool `json:"enabled"`
			Filesystem struct {
				DenyRead  []string `json:"denyRead"`
				DenyWrite []string `json:"denyWrite"`
			} `json:"filesystem"`
		} `json:"sandbox"`
	}
	require.NoError(t, json.Unmarshal([]byte(claudeSettingsJSON(spec)), &settings))
	assert.Nil(t, settings.Sandbox.Enabled,
		"inherit must add the deny without changing whether the sandbox is enabled")
	assert.True(t, slices.Contains(settings.Sandbox.Filesystem.DenyRead, socketPath))
	assert.True(t, slices.Contains(settings.Sandbox.Filesystem.DenyWrite, socketPath))
	assert.False(t, slices.Contains(settings.Sandbox.Filesystem.DenyRead, filepath.Dir(socketPath)),
		"other tmux servers in the user's socket directory remain outside this boundary")
}

func TestPrepareClaudeSandboxLaunchOffKeepsPrivateTmuxUsable(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "relative-would-fail-resolution")
	spec := SpawnSpec{SandboxMode: ClaudeSandboxOff, SandboxDenyDirs: []string{"/opt/secret"}}
	got, err := PrepareClaudeSandboxLaunch(spec)
	require.NoError(t, err)
	assert.Equal(t, spec, got)
	assert.Equal(t, `{"sandbox":{"enabled":false}}`, claudeSettingsJSON(got))
}

func TestPrepareClaudeSandboxLaunchFailsClosedWhenSocketCannotResolve(t *testing.T) {
	t.Setenv("TMUX_TMPDIR", "relative")
	_, err := PrepareClaudeSandboxLaunch(SpawnSpec{SandboxMode: ClaudeSandboxOn})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tmux socket deny path")
}

func TestPrepareClaudeSandboxLaunchDoesNotDuplicateSocketDeny(t *testing.T) {
	tmuxBase := t.TempDir()
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	socketPath, err := ClaudeTmuxSocketDenyPath()
	require.NoError(t, err)
	spec, err := PrepareClaudeSandboxLaunch(SpawnSpec{
		SandboxMode: ClaudeSandboxOn, SandboxDenyDirs: []string{socketPath},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{socketPath}, spec.SandboxDenyDirs)
}

func TestHostControlSandboxCapabilityIsDescriptorOwned(t *testing.T) {
	for _, name := range []string{CodexName, OpenCodeName} {
		target := MustGet(name)
		assert.False(t, target.SupportsHostControlSandbox(), name)
		input := SpawnSpec{SandboxDenyDirs: []string{"/operator-rule"}}
		got, err := target.PrepareHostControlSandboxLaunch(input)
		require.NoError(t, err)
		assert.Equal(t, input, got)
	}
}
