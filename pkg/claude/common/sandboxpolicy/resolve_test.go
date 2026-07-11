package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveComposesScopesWithProvenance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := filepath.Join(home, "a")
	b := filepath.Join(home, "b")
	c := filepath.Join(home, "c")
	for _, path := range []string{a, b, c} {
		require.NoError(t, os.Mkdir(path, 0o755))
	}
	canonicalA, err := filepath.EvalSymlinks(a)
	require.NoError(t, err)
	canonicalB, err := filepath.EvalSymlinks(b)
	require.NoError(t, err)
	canonicalC, err := filepath.EvalSymlinks(c)
	require.NoError(t, err)

	global := &Profile{
		Name: " global ",
		Filesystem: []FilesystemGrant{
			{Path: a, Access: AccessRead},
			{Path: b, Access: AccessWrite},
		},
		Environment: []EnvironmentEntry{{Name: "SHARED", Value: "global"}, {Name: "GLOBAL_ONLY", Value: "yes"}},
	}
	group := &Profile{
		Name: "group",
		Filesystem: []FilesystemGrant{
			{Path: a, Access: AccessWrite},
			{Path: c, Access: AccessRead},
		},
		Environment: []EnvironmentEntry{{Name: "SHARED", Value: "group"}, {Name: "GROUP_ONLY", Value: "yes"}},
	}
	explicit := &Profile{
		Name:        "explicit",
		Filesystem:  []FilesystemGrant{{Path: b, Access: AccessRead}},
		Environment: []EnvironmentEntry{{Name: "SHARED", Value: "explicit"}},
	}

	got, err := Resolve(Scopes{Global: global, Group: group, Explicit: explicit})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{
		{Path: canonicalA, Access: AccessWrite},
		{Path: canonicalB, Access: AccessWrite},
		{Path: canonicalC, Access: AccessRead},
	}, got.Filesystem)
	assert.Equal(t, []EnvironmentEntry{
		{Name: "GLOBAL_ONLY", Value: "yes"},
		{Name: "GROUP_ONLY", Value: "yes"},
		{Name: "SHARED", Value: "explicit"},
	}, got.Environment)
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeGlobal, Profile: "global"},
		{Scope: ScopeGroup, Profile: "group"},
		{Scope: ScopeExplicit, Profile: "explicit"},
	}, got.Provenance.Applied)
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeGlobal, Profile: "global"},
		{Scope: ScopeGroup, Profile: "group"},
	}, got.Provenance.Filesystem[canonicalA])
	assert.Equal(t, []ProfileSource{
		{Scope: ScopeGlobal, Profile: "global"},
		{Scope: ScopeExplicit, Profile: "explicit"},
	}, got.Provenance.Filesystem[canonicalB], "later read does not weaken an earlier write")
	assert.Equal(t, ProfileSource{Scope: ScopeExplicit, Profile: "explicit"}, got.Provenance.Environment["SHARED"])
	assert.Equal(t, ProfileSource{Scope: ScopeGlobal, Profile: "global"}, got.Provenance.Environment["GLOBAL_ONLY"])
	assert.Equal(t, " global ", global.Name, "resolution does not mutate inputs")
}

func TestResolveEmptyScopesReturnsNonNilCollections(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := Resolve(Scopes{})
	require.NoError(t, err)
	assert.NotNil(t, got.Filesystem)
	assert.NotNil(t, got.Environment)
	assert.NotNil(t, got.Provenance.Applied)
	assert.NotNil(t, got.Provenance.Filesystem)
	assert.NotNil(t, got.Provenance.Environment)
}

func TestResolveRevalidatesPersistedCanonicalPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mount := filepath.Join(home, "mount")
	require.NoError(t, os.Mkdir(mount, 0o755))

	// Simulate the canonical profile value persisted when mount was a safe
	// directory. Replace that directory with a symlink into protected state
	// before resolution; Normalize must run again and fail closed.
	persisted, err := Normalize(Profile{Name: "saved", Filesystem: []FilesystemGrant{{Path: mount, Access: AccessWrite}}})
	require.NoError(t, err)
	require.NoError(t, os.Rename(mount, filepath.Join(home, "old-mount")))
	protected := filepath.Join(home, ".codex")
	require.NoError(t, os.Mkdir(protected, 0o755))
	require.NoError(t, os.Symlink(protected, mount))

	_, err = Resolve(Scopes{Global: &persisted})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "at resolution time")
	assert.Contains(t, err.Error(), "intersects protected")
}

func TestResolveRecanonicalizesPathChangedSincePersistence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mount := filepath.Join(home, "mount")
	oldTarget := filepath.Join(home, "old-target")
	newTarget := filepath.Join(home, "new-target")
	require.NoError(t, os.Mkdir(mount, 0o755))
	require.NoError(t, os.Mkdir(newTarget, 0o755))
	persisted, err := Normalize(Profile{Name: "saved", Filesystem: []FilesystemGrant{{Path: mount, Access: AccessRead}}})
	require.NoError(t, err)
	require.NoError(t, os.Rename(mount, oldTarget))
	require.NoError(t, os.Symlink(newTarget, mount))
	canonicalNew, err := filepath.EvalSymlinks(newTarget)
	require.NoError(t, err)

	got, err := Resolve(Scopes{Global: &persisted})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{{Path: canonicalNew, Access: AccessRead}}, got.Filesystem)
	assert.Equal(t, []ProfileSource{{Scope: ScopeGlobal, Profile: "saved"}}, got.Provenance.Filesystem[canonicalNew])
}

func TestResolveEnforcesAggregateEnvironmentLimits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entries := func(prefix string, count int) []EnvironmentEntry {
		out := make([]EnvironmentEntry, count)
		for i := range out {
			out[i] = EnvironmentEntry{Name: fmt.Sprintf("%s_%03d", prefix, i), Value: "x"}
		}
		return out
	}
	global := &Profile{Name: "global", Environment: entries("GLOBAL", 65)}
	group := &Profile{Name: "group", Environment: entries("GROUP", 64)}
	_, err := Resolve(Scopes{Global: global, Group: group})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "effective environment")
	assert.Contains(t, err.Error(), "too many entries")
}

func TestResolveWrapsScopeValidationErrors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	bad := &Profile{Name: "bad", Environment: []EnvironmentEntry{{Name: "PATH", Value: "nope"}}}
	_, err := Resolve(Scopes{Group: bad})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `normalize group sandbox profile "bad" at resolution time`)
	assert.Contains(t, err.Error(), "reserved")
}
