package agentd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestMaterializeAgentDirectoriesCreatesPrivateFrozenBindings(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE", "GOLANGCI_LINT_CACHE"},
	}})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, nil)

	got, cleanup, err := materializeAgentDirectories(snapshot, "spwn-test123")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Equal(t, []string{"GOCACHE", "GOLANGCI_LINT_CACHE"}, got.Effective.AgentDirectories)
	require.Len(t, got.Effective.Filesystem, 2)
	require.Len(t, got.Effective.Environment, 2)
	for i, entry := range got.Effective.Environment {
		assert.Equal(t, entry.Value, got.Effective.Filesystem[i].Path)
		assert.Equal(t, sandboxpolicy.AccessWrite, got.Effective.Filesystem[i].Access)
		info, statErr := os.Stat(entry.Value)
		require.NoError(t, statErr)
		assert.True(t, info.IsDir())
		assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
		assert.Equal(t, entry.Name, filepath.Base(entry.Value))
	}
	_, err = sandboxpolicy.RevalidateSnapshot(got)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Dir(got.Effective.Environment[0].Value)))
	recreated, err := ensureAgentDirectoriesForRelaunch(got)
	require.NoError(t, err)
	for _, entry := range recreated.Effective.Environment {
		_, statErr := os.Stat(entry.Value)
		require.NoError(t, statErr)
	}

	clone, cloneCleanup, err := materializeAgentDirectories(got, "spwn-clone456")
	require.NoError(t, err)
	t.Cleanup(cloneCleanup)
	require.Len(t, clone.Effective.Environment, 2)
	for i := range clone.Effective.Environment {
		assert.NotEqual(t, got.Effective.Environment[i].Value, clone.Effective.Environment[i].Value)
	}
}

func TestMaterializeAgentDirectoriesRejectsUnsafeLaunchKey(t *testing.T) {
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	_, _, err = materializeAgentDirectories(sandboxpolicy.NewSnapshot(effective, nil), "../escape")
	require.ErrorContains(t, err, "invalid agent-directory launch key")
}

func TestMaterializeAgentDirectoriesCanonicalizesSymlinkedCachePrefix(t *testing.T) {
	realCache := t.TempDir()
	linkParent := t.TempDir()
	cacheLink := filepath.Join(linkParent, "cache")
	require.NoError(t, os.Symlink(realCache, cacheLink))
	t.Setenv("XDG_CACHE_HOME", cacheLink)
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	got, cleanup, err := materializeAgentDirectories(sandboxpolicy.NewSnapshot(effective, nil), "symlink-prefix")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	require.Len(t, got.Effective.Environment, 1)
	assert.True(t, strings.HasPrefix(got.Effective.Environment[0].Value, realCache+string(filepath.Separator)))
}

func TestEnsureAgentDirectoriesRejectsFrozenBindingOutsideCacheRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	effective.Environment = []sandboxpolicy.EnvironmentEntry{{Name: "GOCACHE", Value: filepath.Join(t.TempDir(), "GOCACHE")}}
	_, err = ensureAgentDirectoriesForRelaunch(sandboxpolicy.NewSnapshot(effective, nil))
	require.ErrorContains(t, err, "escapes tclaude's cache root")
}
