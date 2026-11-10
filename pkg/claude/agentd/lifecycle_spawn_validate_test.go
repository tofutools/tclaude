package agentd

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Trigger and template spawns bypass handleGroupSpawn, so the shell rule is
// enforced here too.
func TestNormalizeSpawnLaunchPostureRefusesAShellWithAnInitialMessage(t *testing.T) {
	resetTestDB(t)
	p := spawnParams{Harness: harness.ShellName, InitialMessage: "hello", Cwd: t.TempDir()}
	fail := normalizeSpawnLaunchPosture(nil, &p, "")
	require.NotNil(t, fail)
	assert.Equal(t, http.StatusBadRequest, fail.Status)
	assert.Equal(t, "invalid_initial_message", fail.Kind)
}

// An unnamed spawn into a group gets a name derived from the group.
func TestNormalizeSpawnLaunchPostureNamesAnUnnamedGroupSpawn(t *testing.T) {
	resetTestDB(t)
	p := spawnParams{Cwd: t.TempDir()}
	require.Nil(t, normalizeSpawnLaunchPosture(nil, &p, "crew"))
	assert.True(t, strings.HasPrefix(p.Name, "crew"), "derived name %q", p.Name)
}

// Without a dir write proof, pinned write grants and the pinned Git common dir
// are dropped rather than carried into the launch.
func TestSettleSpawnTrustDropsUnprovenPins(t *testing.T) {
	resetTestDB(t)
	p := spawnParams{
		Cwd:                        t.TempDir(),
		GitWorktreeWriteDirs:       []string{"/repo/.git"},
		GitWorktreeWriteDirsPinned: true,
		CodexGitCommonDir:          "/repo/.git",
		CodexGitCommonDirPinned:    true,
	}
	ssh, fail := settleSpawnTrust(&p)
	require.Nil(t, fail)
	assert.False(t, ssh)
	assert.Nil(t, p.GitWorktreeWriteDirs)
	assert.False(t, p.GitWorktreeWriteDirsPinned)
	assert.Empty(t, p.CodexGitCommonDir)
	assert.False(t, p.CodexGitCommonDirPinned)
}

// An agent asking to trust a directory it has not proved it owns is refused.
func TestSettleSpawnTrustRefusesAnUnprovenTrustDirFromAnAgent(t *testing.T) {
	resetTestDB(t)
	p := spawnParams{Cwd: t.TempDir(), SpawnedByConv: "conv-spawner", TrustDir: true}
	_, fail := settleSpawnTrust(&p)
	require.NotNil(t, fail)
	assert.Equal(t, http.StatusForbidden, fail.Status)
	assert.Equal(t, "trust_dir_restricted", fail.Kind)
}
