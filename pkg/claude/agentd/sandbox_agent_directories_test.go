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

// withAgentDirsMountParent isolates the config home for the test and writes the
// experimental features.agent_dirs_mount_parent flag. Passing false only
// isolates HOME (flag absent = off), keeping grant-shape assertions independent
// of the developer's real ~/.tclaude/data/config.json.
func withAgentDirsMountParent(t *testing.T, enabled bool) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	if !enabled {
		return
	}
	dataDir := filepath.Join(home, ".tclaude", "data")
	require.NoError(t, os.MkdirAll(dataDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "config.json"),
		[]byte(`{"features":{"agent_dirs_mount_parent":true}}`), 0o600))
}

// agentDirWriteGrants returns the write grants from a materialized snapshot.
func agentDirWriteGrants(snapshot sandboxpolicy.Snapshot) []sandboxpolicy.FilesystemGrant {
	var out []sandboxpolicy.FilesystemGrant
	for _, grant := range snapshot.Effective.Filesystem {
		if grant.Access == sandboxpolicy.AccessWrite {
			out = append(out, grant)
		}
	}
	return out
}

func TestMaterializeAgentDirectoriesCreatesPrivateFrozenBindings(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	withAgentDirsMountParent(t, false)
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

func TestReconcileAgentDirectoriesForResumeRetainsExistingAndAddsStableBinding(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	withAgentDirsMountParent(t, false)
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

func TestMaterializeAgentDirectoriesMountsParentRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	withAgentDirsMountParent(t, true)
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE", "GOLANGCI_LINT_CACHE"},
	}})
	require.NoError(t, err)

	got, cleanup, err := materializeAgentDirectories(sandboxpolicy.NewSnapshot(effective, nil), "spwn-mountparent")
	require.NoError(t, err)
	t.Cleanup(cleanup)

	// Env vars still point at each per-name subdir, and each subdir exists.
	require.Len(t, got.Effective.Environment, 2)
	for _, entry := range got.Effective.Environment {
		assert.Equal(t, entry.Name, filepath.Base(entry.Value))
		assert.DirExists(t, entry.Value)
	}
	// But exactly one write grant is issued: the shared parent root, so the
	// agent can create, rewrite, and delete its own env-var'd directories.
	writeGrants := agentDirWriteGrants(got)
	require.Len(t, writeGrants, 1)
	parent := filepath.Dir(got.Effective.Environment[0].Value)
	assert.Equal(t, parent, writeGrants[0].Path)
	for _, entry := range got.Effective.Environment {
		assert.Equal(t, parent, filepath.Dir(entry.Value))
	}
	_, err = sandboxpolicy.RevalidateSnapshot(got)
	require.NoError(t, err)

	// Relaunch recreates the subdirs (and thus the parent) at their frozen paths.
	require.NoError(t, os.RemoveAll(parent))
	recreated, err := ensureAgentDirectoriesForRelaunch(got)
	require.NoError(t, err)
	for _, entry := range recreated.Effective.Environment {
		assert.DirExists(t, entry.Value)
	}
}

func TestMaterializeAgentDirectoriesCloneDropsSourceParentGrant(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	withAgentDirsMountParent(t, true)
	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "cache", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	source, cleanup, err := materializeAgentDirectories(sandboxpolicy.NewSnapshot(effective, nil), "spwn-source")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	sourceParent := filepath.Dir(source.Effective.Environment[0].Value)

	// A clone carries the source's materialized snapshot; the source's parent-root
	// grant must be stripped and replaced by the clone's own root — never leaked.
	clone, cloneCleanup, err := materializeAgentDirectories(source, "spwn-clone")
	require.NoError(t, err)
	t.Cleanup(cloneCleanup)
	cloneParent := filepath.Dir(clone.Effective.Environment[0].Value)
	assert.NotEqual(t, sourceParent, cloneParent)
	for _, grant := range clone.Effective.Filesystem {
		assert.NotEqual(t, sourceParent, grant.Path, "source parent-root grant leaked into clone")
	}
	writeGrants := agentDirWriteGrants(clone)
	require.Len(t, writeGrants, 1)
	assert.Equal(t, cloneParent, writeGrants[0].Path)
}

func TestReconcileAgentDirectoriesForResumeMountsParentPerRoot(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	withAgentDirsMountParent(t, true)
	oldEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "old", AgentDirectories: []string{"GOCACHE"},
	}})
	require.NoError(t, err)
	previous, cleanup, err := materializeAgentDirectories(sandboxpolicy.NewSnapshot(oldEffective, nil), "spwn-original")
	require.NoError(t, err)
	t.Cleanup(cleanup)
	oldPath := previous.Effective.Environment[0].Value

	currentEffective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{Global: &sandboxpolicy.Profile{
		Name: "current", AgentDirectories: []string{"GOCACHE", "GOLANGCI_LINT_CACHE"},
	}})
	require.NoError(t, err)
	resumed, err := reconcileAgentDirectoriesForResume(sandboxpolicy.NewSnapshot(currentEffective, nil), previous, "agt_resume_mount")
	require.NoError(t, err)

	bindings := map[string]string{}
	for _, entry := range resumed.Effective.Environment {
		bindings[entry.Name] = entry.Value
	}
	// The retained binding sits under the original root; the new binding under the
	// resumed root. Mount-parent grants each distinct parent once (deduped).
	assert.Equal(t, oldPath, bindings["GOCACHE"])
	wantParents := map[string]bool{
		filepath.Dir(bindings["GOCACHE"]):             true,
		filepath.Dir(bindings["GOLANGCI_LINT_CACHE"]): true,
	}
	gotParents := map[string]bool{}
	for _, grant := range agentDirWriteGrants(resumed) {
		gotParents[grant.Path] = true
	}
	assert.Equal(t, wantParents, gotParents)
	assert.Len(t, wantParents, 2, "existing and new bindings should live under different roots")
	_, err = sandboxpolicy.RevalidateSnapshot(resumed)
	require.NoError(t, err)
}
