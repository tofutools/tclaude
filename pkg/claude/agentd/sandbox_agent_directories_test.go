package agentd

import (
	"os"
	"path/filepath"
	"runtime"
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
	canonicalRealCache, err := filepath.EvalSymlinks(realCache)
	require.NoError(t, err)
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
	assert.True(t, strings.HasPrefix(got.Effective.Environment[0].Value, canonicalRealCache+string(filepath.Separator)))
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

func TestRemoveMaterializedAgentDirectoriesDeletesEveryLaunchRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	oldEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "old", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	previous, cleanup, err := materializeAgentDirectories(sandboxpolicy.NewSnapshot(oldEffective, nil), "spwn-original")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	currentEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "current", AgentDirectories: []string{"GOCACHE", "GOLANGCI_LINT_CACHE"},
	}})
	require.NoError(t, err)
	current, err := reconcileAgentDirectoriesForResume(
		sandboxpolicy.NewSnapshot(currentEffective, nil), previous, "agt_resume_test")
	require.NoError(t, err)

	roots := map[string]struct{}{}
	for _, entry := range current.Effective.Environment {
		root := filepath.Dir(entry.Value)
		roots[root] = struct{}{}
		require.NoError(t, os.WriteFile(filepath.Join(entry.Value, "cache-entry"), []byte("cached"), 0o600))
	}
	require.Len(t, roots, 2, "resume should preserve the original root and add an actor-keyed root")

	removed, err := removeMaterializedAgentDirectories(current)
	require.NoError(t, err)
	assert.Equal(t, 2, removed)
	for root := range roots {
		assert.NoDirExists(t, root)
	}
	removed, err = removeMaterializedAgentDirectories(current)
	require.NoError(t, err)
	assert.Zero(t, removed, "already-missing roots must not be counted as removed")
}

func TestRemoveMaterializedAgentDirectoriesRejectsBindingOutsideCacheRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	outside := filepath.Join(t.TempDir(), "GOCACHE")
	require.NoError(t, os.MkdirAll(outside, 0o700))
	marker := filepath.Join(outside, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0o600))
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	effective.Environment = []sandboxpolicy.EnvironmentEntry{{Name: "GOCACHE", Value: outside}}

	removed, err := removeMaterializedAgentDirectories(sandboxpolicy.NewSnapshot(effective, nil))
	assert.Zero(t, removed)
	require.ErrorContains(t, err, "escapes its launch root")
	assert.FileExists(t, marker)
}

func TestRemoveMaterializedAgentDirectoriesDoesNotFollowReplacedBase(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	snapshot, cleanup, err := materializeAgentDirectories(
		sandboxpolicy.NewSnapshot(effective, nil), "spwn-symlink-race")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	base := filepath.Dir(filepath.Dir(snapshot.Effective.Environment[0].Value))
	movedBase := base + "-original"
	require.NoError(t, os.Rename(base, movedBase))
	outside := t.TempDir()
	outsideRoot := filepath.Join(outside, "spwn-symlink-race")
	require.NoError(t, os.MkdirAll(outsideRoot, 0o700))
	marker := filepath.Join(outsideRoot, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(outside, base))

	_, err = removeMaterializedAgentDirectories(snapshot)
	require.Error(t, err)
	assert.FileExists(t, marker, "descriptor-relative deletion must not follow a replaced agent-dirs base")
	assert.DirExists(t, filepath.Join(movedBase, "spwn-symlink-race"), "the original root was moved out of the frozen path")
}

func TestRemoveMaterializedAgentDirectoriesDoesNotFollowNestedSymlink(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	snapshot, cleanup, err := materializeAgentDirectories(
		sandboxpolicy.NewSnapshot(effective, nil), "spwn-nested-symlink")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	binding := snapshot.Effective.Environment[0].Value
	require.NoError(t, os.Remove(binding))
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0o600))
	require.NoError(t, os.Symlink(outside, binding))

	_, err = removeMaterializedAgentDirectories(snapshot)
	require.NoError(t, err)
	assert.FileExists(t, marker, "recursive deletion must unlink rather than follow nested symlinks")
	assert.NoDirExists(t, filepath.Dir(binding))
}

func TestRemoveMaterializedAgentDirectoriesTraversesSearchOnlyAncestorOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux O_PATH behavior")
	}
	parent := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(parent, "cache"))
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	snapshot, cleanup, err := materializeAgentDirectories(
		sandboxpolicy.NewSnapshot(effective, nil), "spwn-search-only")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	root := filepath.Dir(snapshot.Effective.Environment[0].Value)
	require.NoError(t, os.Chmod(parent, 0o111))
	t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })

	removed, err := removeMaterializedAgentDirectories(snapshot)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.NoDirExists(t, root)
}

func TestRemoveSupersededMaterializedAgentDirectoriesDeletesReplacedRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	oldEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "old", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	previous, cleanup, err := materializeAgentDirectories(
		sandboxpolicy.NewSnapshot(oldEffective, nil), "spwn-original")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	oldRoot := filepath.Dir(previous.Effective.Environment[0].Value)

	newEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "new", AgentDirectories: []string{"GOLANGCI_LINT_CACHE"},
	}})
	require.NoError(t, err)
	current, err := reconcileAgentDirectoriesForResume(
		sandboxpolicy.NewSnapshot(newEffective, nil), previous, "agt_resume_test")
	require.NoError(t, err)
	newRoot := filepath.Dir(current.Effective.Environment[0].Value)

	removed, err := removeSupersededMaterializedAgentDirectories(previous, current)
	require.NoError(t, err)
	assert.Equal(t, 1, removed)
	assert.NoDirExists(t, oldRoot)
	assert.DirExists(t, newRoot)
}

func TestReconcileAgentDirectoriesForResumeRetainsExistingAndAddsStableBinding(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	oldEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "old", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	previous, cleanup, err := materializeAgentDirectories(sandboxpolicy.NewSnapshot(oldEffective, nil), "spwn-original")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	oldPath := previous.Effective.Environment[0].Value

	currentEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name:             "current",
		Environment:      []sandboxpolicy.EnvironmentEntry{{Name: "PROFILE_VERSION", Value: "v2"}},
		AgentDirectories: []string{"GOCACHE", "GOLANGCI_LINT_CACHE"},
	}})
	require.NoError(t, err)
	current := sandboxpolicy.NewSnapshot(currentEffective, nil)
	resumed, err := reconcileAgentDirectoriesForResume(current, previous, "agt_resume_test")
	require.NoError(t, err)

	bindings := map[string]string{}
	for _, entry := range resumed.Effective.Environment {
		bindings[entry.Name] = entry.Value
	}
	assert.Equal(t, "v2", bindings["PROFILE_VERSION"])
	assert.Equal(t, oldPath, bindings["GOCACHE"])
	assert.Contains(t, bindings["GOLANGCI_LINT_CACHE"], filepath.Join("agent-dirs", "agt_resume_test"))
	assert.DirExists(t, bindings["GOLANGCI_LINT_CACHE"])
}
