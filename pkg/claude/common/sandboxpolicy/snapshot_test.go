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
	parentWrite := makeSnapshot([]FilesystemGrant{{Path: root, Access: AccessWrite}}, []EnvironmentEntry{{Name: "SAME", Value: "literal"}})
	t.Run("parent write ancestor covers child read descendant", func(t *testing.T) {
		childRead := makeSnapshot([]FilesystemGrant{{Path: childDir, Access: AccessRead}}, []EnvironmentEntry{{Name: "SAME", Value: "literal"}})
		require.NoError(t, RequireContained(parentWrite, childRead))
	})
	t.Run("parent read does not cover child write", func(t *testing.T) {
		parentRead := makeSnapshot([]FilesystemGrant{{Path: root, Access: AccessRead}}, nil)
		childWrite := makeSnapshot([]FilesystemGrant{{Path: childDir, Access: AccessWrite}}, nil)
		require.ErrorContains(t, RequireContained(parentRead, childWrite), "filesystem write grant")
	})
	t.Run("environment values must match exactly", func(t *testing.T) {
		changedEnv := makeSnapshot(nil, []EnvironmentEntry{{Name: "SAME", Value: "changed"}})
		require.ErrorContains(t, RequireContained(parentWrite, changedEnv), "new or changed")
	})
	t.Run("parent deny must be preserved", func(t *testing.T) {
		parent := makeSnapshot([]FilesystemGrant{{Path: root, Access: AccessWrite}, {Path: childDir, Access: AccessDeny}}, nil)
		withoutDeny := makeSnapshot([]FilesystemGrant{{Path: root, Access: AccessWrite}}, nil)
		require.ErrorContains(t, RequireContained(parent, withoutDeny), "not preserved")
		withDeny := makeSnapshot([]FilesystemGrant{{Path: root, Access: AccessWrite}, {Path: childDir, Access: AccessDeny}}, nil)
		require.NoError(t, RequireContained(parent, withDeny))
	})
	t.Run("deny-only policy adds no capability", func(t *testing.T) {
		denyOnly := makeSnapshot([]FilesystemGrant{{Path: childDir, Access: AccessDeny}}, nil)
		assert.False(t, HasCapabilities(denyOnly))
	})
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

func TestRevalidateSnapshotAllowsMissingPathBeforeAndAfterCreation(t *testing.T) {
	root := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	missing := filepath.Join(canonicalRoot, "future", "cache")
	effective, err := Resolve(Scopes{Global: &Profile{
		Name: "base", Filesystem: []FilesystemGrant{{Path: missing, Access: AccessWrite}},
	}})
	require.NoError(t, err)
	snapshot := NewSnapshot(effective, nil)

	validated, err := RevalidateSnapshot(snapshot)
	require.NoError(t, err)
	assert.Equal(t, missing, validated.Effective.Filesystem[0].Path)
	launchFilesystem, err := FilesystemForLaunch(validated.Effective)
	require.NoError(t, err)
	assert.Empty(t, launchFilesystem, "missing rule must be inactive for this launch")
	require.NoError(t, os.MkdirAll(missing, 0o755))
	validated, err = RevalidateSnapshot(snapshot)
	require.NoError(t, err)
	assert.Equal(t, missing, validated.Effective.Filesystem[0].Path)
	launchFilesystem, err = FilesystemForLaunch(validated.Effective)
	require.NoError(t, err)
	assert.Equal(t, validated.Effective.Filesystem, launchFilesystem, "created rule activates on a later launch")
}

func TestFilesystemForLaunchFailsClosedForMissingDeny(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	missing := filepath.Join(root, "future")
	_, err = FilesystemForLaunch(EffectiveProfile{Filesystem: []FilesystemGrant{{
		Path: missing, Access: AccessDeny,
	}}})
	require.ErrorContains(t, err, "does not exist and cannot be enforced")
}

func TestFilesystemForLaunchRejectsAncestorSymlinkSubstitution(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	target := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(target, "cache"), 0o755))
	missing := filepath.Join(root, "future", "cache")
	effective, err := Resolve(Scopes{Global: &Profile{
		Name: "base", Filesystem: []FilesystemGrant{{Path: missing, Access: AccessWrite}},
	}})
	require.NoError(t, err)
	require.NoError(t, os.Symlink(target, filepath.Join(root, "future")))

	_, err = FilesystemForLaunch(effective)
	require.ErrorContains(t, err, "changed canonical target")
}

func TestRevalidateSnapshotRejectsMissingPathMaterializedAsSymlink(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	missing := filepath.Join(canonicalRoot, "future")
	effective, err := Resolve(Scopes{Global: &Profile{
		Name: "base", Filesystem: []FilesystemGrant{{Path: missing, Access: AccessWrite}},
	}})
	require.NoError(t, err)
	snapshot := NewSnapshot(effective, nil)

	require.NoError(t, os.Symlink(target, missing))
	_, err = RevalidateSnapshot(snapshot)
	require.ErrorContains(t, err, "filesystem changed since resolution")
}
