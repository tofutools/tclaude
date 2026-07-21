package sandboxpolicy

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeFilesystemCanonicalizesFoldsAndSorts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	a := filepath.Join(home, "a")
	b := filepath.Join(home, "b")
	require.NoError(t, os.MkdirAll(a, 0o755))
	require.NoError(t, os.MkdirAll(b, 0o755))
	canonicalA, err := filepath.EvalSymlinks(a)
	require.NoError(t, err)
	canonicalB, err := filepath.EvalSymlinks(b)
	require.NoError(t, err)
	alias := filepath.Join(home, "alias")
	require.NoError(t, os.Symlink(a, alias))

	in := Profile{Name: " caches ", Filesystem: []FilesystemGrant{
		{Path: b + string(filepath.Separator), Access: AccessRead},
		{Path: alias, Access: AccessRead},
		{Path: a, Access: AccessWrite},
	}}
	got, err := Normalize(in)
	require.NoError(t, err)
	assert.Equal(t, "caches", got.Name)
	assert.Equal(t, []FilesystemGrant{
		{Path: canonicalA, Access: AccessWrite},
		{Path: canonicalB, Access: AccessRead},
	}, got.Filesystem)
	assert.Equal(t, alias, in.Filesystem[1].Path, "caller input must not be mutated")
}

func TestNormalizeNetworkAccess(t *testing.T) {
	for _, access := range []NetworkAccess{NetworkAccessInherit, NetworkAccessInternet, NetworkAccessNone} {
		got, err := Normalize(Profile{Name: "p", NetworkAccess: access})
		require.NoError(t, err)
		assert.Equal(t, access, got.NetworkAccess)
	}
	_, err := Normalize(Profile{Name: "p", NetworkAccess: "local-only"})
	require.ErrorContains(t, err, "network_access")
}

func TestNormalizeFilesystemWriteWinsInEitherOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "cache")
	require.NoError(t, os.Mkdir(dir, 0o755))
	canonicalDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	for _, grants := range [][]FilesystemGrant{
		{{Path: dir, Access: AccessRead}, {Path: dir, Access: AccessWrite}},
		{{Path: dir, Access: AccessWrite}, {Path: dir, Access: AccessRead}},
	} {
		got, err := Normalize(Profile{Name: "p", Filesystem: grants})
		require.NoError(t, err)
		assert.Equal(t, []FilesystemGrant{{Path: canonicalDir, Access: AccessWrite}}, got.Filesystem)
	}
}

func TestNormalizeFilesystemDenyWinsAndMayCoverProtectedPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "cache")
	protected := filepath.Join(home, ".claude", "sessions")
	require.NoError(t, os.Mkdir(dir, 0o755))
	require.NoError(t, os.Mkdir(protected, 0o755))
	canonicalDir, err := filepath.EvalSymlinks(dir)
	require.NoError(t, err)
	canonicalProtected, err := filepath.EvalSymlinks(protected)
	require.NoError(t, err)

	got, err := Normalize(Profile{Name: "p", Filesystem: []FilesystemGrant{
		{Path: dir, Access: AccessDeny},
		{Path: dir, Access: AccessWrite},
		{Path: protected, Access: AccessDeny},
	}})
	require.NoError(t, err)
	assert.Contains(t, got.Filesystem, FilesystemGrant{Path: canonicalDir, Access: AccessDeny})
	assert.Contains(t, got.Filesystem, FilesystemGrant{Path: canonicalProtected, Access: AccessDeny})
}

func TestNormalizeFilesystemRejectsInvalidAndProtectedPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	safe := filepath.Join(home, "safe")
	require.NoError(t, os.MkdirAll(safe, 0o755))
	protected := []string{
		filepath.Join(home, ".tclaude", "data"),
		filepath.Join(home, ".claude", "sessions"),
	}
	for _, path := range protected {
		require.NoError(t, os.MkdirAll(filepath.Join(path, "child"), 0o755))
	}

	tests := []struct {
		name, path, want string
		access           Access
	}{
		{"relative", "cache", "not absolute", AccessRead},
		{"missing", filepath.Join(home, "missing"), "resolve symlinks", AccessRead},
		{"regular file", filepath.Join(safe, "file"), "not a directory", AccessRead},
		{"bad access", safe, "access", Access("execute")},
		{"protected exact", protected[0], "intersects protected", AccessRead},
		{"protected child", filepath.Join(protected[1], "child"), "intersects protected", AccessWrite},
		{"protected ancestor", home, "intersects protected", AccessRead},
	}
	require.NoError(t, os.WriteFile(filepath.Join(safe, "file"), []byte("x"), 0o644))
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Normalize(Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: tt.path, Access: tt.access}}})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}

	lookalike := filepath.Join(home, ".claude", "sessions-cache")
	require.NoError(t, os.Mkdir(lookalike, 0o755))
	_, err := Normalize(Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: lookalike, Access: AccessWrite}}})
	require.NoError(t, err, "shared string prefixes are not path ancestry")
}

func TestNormalizeFilesystemExpandsTilde(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cache := filepath.Join(home, "cache")
	require.NoError(t, os.MkdirAll(cache, 0o755))
	canonicalCache, err := filepath.EvalSymlinks(cache)
	require.NoError(t, err)

	// "~/cache" resolves to the daemon home's cache directory, identically to
	// passing the absolute path.
	got, err := Normalize(Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: "~/cache", Access: AccessWrite}}})
	require.NoError(t, err)
	assert.Equal(t, []FilesystemGrant{{Path: canonicalCache, Access: AccessWrite}}, got.Filesystem)

	// A "~otheruser/..." form is not a home alias — the literal "~" survives and
	// the path is rejected as not absolute rather than guessing another account.
	_, err = Normalize(Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: "~someone/cache", Access: AccessRead}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not absolute")
}

func TestNormalizeFilesystemRejectsSymlinkIntoProtectedTree(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	protected := filepath.Join(home, ".tclaude", "data")
	require.NoError(t, os.MkdirAll(protected, 0o755))
	alias := filepath.Join(home, "looks-safe")
	require.NoError(t, os.Symlink(protected, alias))
	_, err := Normalize(Profile{Name: "p", Filesystem: []FilesystemGrant{{Path: alias, Access: AccessRead}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "intersects protected")
}

func TestNormalizeForImportRetainsMissingPathsWithWarnings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	realParent := filepath.Join(home, "shared")
	require.NoError(t, os.MkdirAll(realParent, 0o755))
	alias := filepath.Join(home, "alias")
	require.NoError(t, os.Symlink(realParent, alias))
	missing := filepath.Join(alias, "recipient", "cache")

	_, err := Normalize(Profile{Name: "portable", Filesystem: []FilesystemGrant{{Path: missing, Access: AccessWrite}}})
	require.Error(t, err, "ordinary validation still requires a usable local directory")

	got, warnings, err := NormalizeForImport(Profile{Name: "portable", Filesystem: []FilesystemGrant{
		{Path: missing, Access: AccessRead},
		{Path: missing + string(filepath.Separator), Access: AccessWrite},
	}})
	require.NoError(t, err)
	canonicalParent, err := filepath.EvalSymlinks(realParent)
	require.NoError(t, err)
	want := filepath.Join(canonicalParent, "recipient", "cache")
	assert.Equal(t, []FilesystemGrant{{Path: want, Access: AccessWrite}}, got.Filesystem)
	assert.Equal(t, []string{want}, warnings)
}

func TestNormalizeForImportRejectsDanglingSymlinkInMissingPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dangling := filepath.Join(home, "dangling")
	require.NoError(t, os.Symlink(filepath.Join(home, "absent-target"), dangling))

	_, _, err := NormalizeForImport(Profile{Name: "portable", Filesystem: []FilesystemGrant{{
		Path: filepath.Join(dangling, "cache"), Access: AccessWrite,
	}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "resolve symlinks")
}

func TestNormalizeForImportStillRejectsUnsafeOrMalformedMissingPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, path := range []string{
		"relative/missing",
		filepath.Join(home, ".claude", "sessions", "missing"),
		filepath.Join(home, ".tclaude", "data", "missing"),
	} {
		_, _, err := NormalizeForImport(Profile{Name: "portable", Filesystem: []FilesystemGrant{{Path: path, Access: AccessRead}}})
		require.Error(t, err, "path %q", path)
	}
}

func TestNormalizeEnvironmentCanonicalAndConflicts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	in := Profile{Name: "p", Environment: []EnvironmentEntry{
		{Name: "ZED", Value: "line one\n'$HOME`"},
		{Name: "ALPHA", Value: "a"},
		{Name: "ALPHA", Value: "a"},
	}}
	got, err := Normalize(in)
	require.NoError(t, err)
	assert.Equal(t, []EnvironmentEntry{{Name: "ALPHA", Value: "a"}, {Name: "ZED", Value: "line one\n'$HOME`"}}, got.Environment)
	assert.Len(t, in.Environment, 3, "caller input must not be mutated")

	_, err = Normalize(Profile{Name: "p", Environment: []EnvironmentEntry{{Name: "A", Value: "1"}, {Name: "A", Value: "2"}}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "conflicting values")
}

func TestNormalizeEnvironmentRejectsInvalidReservedAndOversize(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	tests := []EnvironmentEntry{
		{Name: "", Value: "x"},
		{Name: "9BAD", Value: "x"},
		{Name: "WITH-DASH", Value: "x"},
		{Name: "TCLAUDE_SESSION_ID", Value: "x"},
		{Name: "CODEX_HOME", Value: "x"},
		{Name: "CLAUDE_CODE_FOO", Value: "x"},
		{Name: "PATH", Value: "x"},
		{Name: "LD_PRELOAD", Value: "x"},
		{Name: "OK", Value: "x\x00y"},
		{Name: strings.Repeat("A", MaxEnvironmentName+1), Value: "x"},
		{Name: "OK", Value: strings.Repeat("x", MaxEnvironmentValue+1)},
	}
	for _, entry := range tests {
		_, err := Normalize(Profile{Name: "p", Environment: []EnvironmentEntry{entry}})
		require.Error(t, err, "entry %#v", entry)
	}
}

func TestNormalizeEnvironmentLimits(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	entries := make([]EnvironmentEntry, MaxEnvironmentCount+1)
	for i := range entries {
		entries[i] = EnvironmentEntry{Name: "V" + strings.Repeat("X", i), Value: "x"}
	}
	_, err := Normalize(Profile{Name: "p", Environment: entries})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "too many entries")
}

func TestNormalizeAgentDirectoriesCanonicalAndRejectsConflicts(t *testing.T) {
	got, err := Normalize(Profile{Name: "p", AgentDirectories: []string{
		"GOLANGCI_LINT_CACHE", "GOCACHE", "GOCACHE",
	}})
	require.NoError(t, err)
	assert.Equal(t, []string{"GOCACHE", "GOLANGCI_LINT_CACHE"}, got.AgentDirectories)

	_, err = Normalize(Profile{Name: "p", AgentDirectories: []string{"NOT-AN-ENV"}})
	require.ErrorContains(t, err, "ASCII environment-variable name")
	_, err = Normalize(Profile{Name: "p", AgentDirectories: []string{"HOME"}})
	require.ErrorContains(t, err, "reserved")
	_, err = Normalize(Profile{
		Name: "p", Environment: []EnvironmentEntry{{Name: "GOCACHE", Value: "/literal"}},
		AgentDirectories: []string{"GOCACHE"},
	})
	require.ErrorContains(t, err, "also has a literal environment value")

	environment := make([]EnvironmentEntry, MaxEnvironmentCount)
	for i := range environment {
		environment[i] = EnvironmentEntry{Name: fmt.Sprintf("ENV_%d", i), Value: "x"}
	}
	_, err = Normalize(Profile{Name: "p", Environment: environment, AgentDirectories: []string{"ONE_MORE"}})
	require.ErrorContains(t, err, "too many entries combined")
}

func TestNormalizeProfileName(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, name := range []string{"", "a/b", "a\\b", "bad\nname", strings.Repeat("x", MaxProfileNameBytes+1)} {
		_, err := Normalize(Profile{Name: name})
		require.Error(t, err, "name %q", name)
	}
}

func TestNormalizeProfileNameRejectsTransferRouteNames(t *testing.T) {
	for _, name := range []string{"export", "IMPORT", " Export "} {
		_, err := Normalize(Profile{Name: name})
		require.ErrorContains(t, err, "reserved for profile transfer routes")
	}
}
