package sandboxpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotDistinguishesResolvedEmptyFromMissing(t *testing.T) {
	assert.Zero(t, Snapshot{}.Version)
	empty := EmptySnapshot()
	assert.Equal(t, SnapshotVersion, empty.Version)
	assert.Empty(t, empty.Effective.Filesystem)
	assert.Empty(t, empty.Effective.Environment)
	_, err := RevalidateSnapshot(Snapshot{})
	require.ErrorContains(t, err, "unsupported sandbox snapshot version")
}

func TestSnapshotFileRoundTripAndTamperRejection(t *testing.T) {
	snapshot := EmptySnapshot()
	snapshot.Effective.Environment = []EnvironmentEntry{{Name: "LITERAL", Value: "$(touch nope); `echo nope`\n'quoted'"}}
	path, digest, err := WriteSnapshotFile(t.TempDir(), snapshot)
	require.NoError(t, err)
	info, err := os.Lstat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	got, err := ReadSnapshotFile(path, digest)
	require.NoError(t, err)
	assert.Equal(t, snapshot.Effective.Environment, got.Effective.Environment)
	assert.False(t, strings.Contains(path, snapshot.Effective.Environment[0].Value))

	require.NoError(t, os.WriteFile(path, []byte(`{"version":1}`), 0o600))
	_, err = ReadSnapshotFile(path, digest)
	require.ErrorContains(t, err, "digest mismatch")
}

func TestRequireContainedUsesPathCoverageAccessAndExactEnvironment(t *testing.T) {
	root := t.TempDir()
	childDir := filepath.Join(root, "child")
	require.NoError(t, os.Mkdir(childDir, 0o755))
	makeSnapshot := func(grants []FilesystemGrant, env []EnvironmentEntry) Snapshot {
		effective, err := Resolve(Scopes{Global: &Profile{Name: "p", Filesystem: grants, Environment: env}})
		require.NoError(t, err)
		return NewSnapshot(effective, nil)
	}
	parent := makeSnapshot([]FilesystemGrant{{Path: root, Access: AccessWrite}}, []EnvironmentEntry{{Name: "SAME", Value: "literal"}})
	child := makeSnapshot([]FilesystemGrant{{Path: childDir, Access: AccessRead}}, []EnvironmentEntry{{Name: "SAME", Value: "literal"}})
	require.NoError(t, RequireContained(parent, child))

	stronger := makeSnapshot([]FilesystemGrant{{Path: root, Access: AccessRead}}, nil)
	require.ErrorContains(t, RequireContained(stronger, parent), "filesystem write grant")
	changedEnv := makeSnapshot(nil, []EnvironmentEntry{{Name: "SAME", Value: "changed"}})
	require.ErrorContains(t, RequireContained(parent, changedEnv), "new or changed")
}

func TestRevalidateSnapshotRejectsFilesystemRetarget(t *testing.T) {
	root := t.TempDir()
	original := filepath.Join(root, "original")
	replacement := filepath.Join(root, "replacement")
	require.NoError(t, os.Mkdir(original, 0o755))
	require.NoError(t, os.Mkdir(replacement, 0o755))
	effective, err := Resolve(Scopes{Global: &Profile{
		Name: "base", Filesystem: []FilesystemGrant{{Path: original, Access: AccessRead}},
	}})
	require.NoError(t, err)
	snapshot := NewSnapshot(effective, nil)

	require.NoError(t, os.Remove(original))
	require.NoError(t, os.Symlink(replacement, original))
	_, err = RevalidateSnapshot(snapshot)
	require.ErrorContains(t, err, "filesystem changed since resolution")
}
