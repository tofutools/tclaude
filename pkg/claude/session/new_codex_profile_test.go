package session

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/launchundo"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The managed profile file a launch writes is registered on the launch's undo
// as soon as it exists: a launch that fails later removes it, one that
// commits hands it to the pane.
func TestPrepareCodexAgentProfileRegistersItsFileOnTheLaunchUndo(t *testing.T) {
	isolateCacheAndDB(t)
	t.Setenv("CODEX_HOME", t.TempDir())
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "internet", NetworkAccess: sandboxpolicy.NetworkAccessInternet,
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)
	params := &NewParams{PermissionProfile: harness.CodexAgentProfile, GitWorktreeWriteDirsPinned: true}

	prepare := func(t *testing.T, undo *launchundo.Undo) codexAgentProfile {
		profile, err := prepareCodexAgentProfile(params, t.TempDir(), "", "spwn-codexprof",
			"1234567890abcdef", false, &snapshot, undo)
		require.NoError(t, err)
		require.NotEmpty(t, profile.name)
		_, statErr := os.Stat(profile.path)
		require.NoError(t, statErr, "the profile file exists before launch")
		assert.Empty(t, profile.appServerOverrides, "no app-server socket, no overrides")
		return profile
	}

	t.Run("failed launch removes it", func(t *testing.T) {
		var undo launchundo.Undo
		profile := prepare(t, &undo)
		undo.UnwindUnlessCommitted()
		_, statErr := os.Stat(profile.path)
		assert.True(t, os.IsNotExist(statErr), "an uncommitted launch must remove its profile file")
	})
	t.Run("committed launch keeps it", func(t *testing.T) {
		var undo launchundo.Undo
		profile := prepare(t, &undo)
		undo.Commit()
		undo.UnwindUnlessCommitted()
		_, statErr := os.Stat(profile.path)
		assert.NoError(t, statErr, "a committed launch hands the file to the pane")
	})
}
