// Package testutil provides small, dependency-free helpers shared by tests.
package testutil

import (
	"path/filepath"
	"testing"
)

// CanonicalTempDir creates a directory managed by t and returns its absolute,
// symlink-resolved path. Use it when a fixture needs filesystem identity, such
// as a Git path comparison or the input to a no-follow filesystem operation.
// For tests of user-supplied path spellings, keep explicit symlink inputs:
// canonicalizing those inputs would hide the behavior being tested.
func CanonicalTempDir(t testing.TB) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		t.Fatalf("make temporary directory absolute: %v", err)
	}
	return path
}
