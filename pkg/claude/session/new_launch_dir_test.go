package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testutil"
)

// A writable-cwd sandbox rooted at $HOME would make the state dirs writable,
// so it is refused; the managed tclaude-agent profile is guarded exactly like
// workspace-write. A project subdirectory, or a mode that writes nothing
// outside cwd, launches.
func TestRefuseStateDirCwdUnderWorkspaceWrite(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	project := filepath.Join(home, "project")
	require.NoError(t, os.MkdirAll(project, 0o755))
	codex := harness.MustGet(harness.CodexName)

	err := refuseStateDirCwdUnderWorkspaceWrite(codex, &NewParams{}, harness.SandboxWorkspaceWrite, home)
	require.ErrorContains(t, err, "under workspace-write containment")
	err = refuseStateDirCwdUnderWorkspaceWrite(codex, &NewParams{PermissionProfile: harness.CodexAgentProfile}, "", home)
	require.ErrorContains(t, err, "under workspace-write containment", "the managed profile extends :workspace")

	require.NoError(t, refuseStateDirCwdUnderWorkspaceWrite(codex, &NewParams{}, harness.SandboxWorkspaceWrite, project))
	require.NoError(t, refuseStateDirCwdUnderWorkspaceWrite(codex, &NewParams{}, harness.SandboxReadOnly, home))
}

// --trust-dir seeds the store the launch will read. A constructed-root
// Claude launch relocates its config through the launch-owned
// CLAUDE_CONFIG_DIR, so the entry must land there, not in the operator's
// ~/.claude.json.
func TestPrepareLaunchDirTrustSeedsTheLaunchesOwnClaudeConfig(t *testing.T) {
	home := testutil.CanonicalTempDir(t)
	t.Setenv("HOME", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	configDir := filepath.Join(home, "state-root", "claude")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	cwd := testutil.CanonicalTempDir(t)
	claude := harness.MustGet(harness.DefaultName)

	err := prepareLaunchDirTrust(claude, &NewParams{TrustDir: true}, nil,
		map[string]string{"CLAUDE_CONFIG_DIR": configDir}, cwd)
	require.NoError(t, err)

	seeded, readErr := os.ReadFile(filepath.Join(configDir, ".claude.json"))
	require.NoError(t, readErr, "the trust entry must land in the launch's own config dir")
	assert.Contains(t, string(seeded), cwd)
	_, statErr := os.Stat(filepath.Join(home, ".claude.json"))
	assert.True(t, os.IsNotExist(statErr), "the operator's own config must be left alone")
}
