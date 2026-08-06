package common

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeFakeShell(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\n"), mode))
	return path
}

func withBootstrapShellResolver(t *testing.T, candidates []string, look func(string) (string, error)) {
	t.Helper()
	originalCandidates := bootstrapShellCandidates
	originalLookPath := bootstrapShellLookPath
	t.Cleanup(func() {
		bootstrapShellCandidates = originalCandidates
		bootstrapShellLookPath = originalLookPath
	})
	bootstrapShellCandidates = candidates
	bootstrapShellLookPath = look
}

func TestResolveBootstrapShellPrefersFirstExecutableCandidate(t *testing.T) {
	dir := t.TempDir()
	first := writeFakeShell(t, dir, "first-bash", 0o755)
	second := writeFakeShell(t, dir, "second-bash", 0o755)
	withBootstrapShellResolver(t, []string{first, second}, func(string) (string, error) {
		t.Fatal("PATH lookup must not run once an absolute candidate resolved")
		return "", nil
	})
	assert.Equal(t, first, resolveBootstrapShell())
}

// A non-executable file at a candidate path must not be selected: the pane
// would fail to start with an opaque exec error rather than falling through to
// a shell that works.
func TestResolveBootstrapShellSkipsNonExecutableCandidate(t *testing.T) {
	dir := t.TempDir()
	notExecutable := writeFakeShell(t, dir, "bash", 0o644)
	usable := writeFakeShell(t, dir, "real-bash", 0o755)
	withBootstrapShellResolver(t, []string{notExecutable, usable}, func(string) (string, error) {
		return "", errors.New("not on PATH")
	})
	assert.Equal(t, usable, resolveBootstrapShell())
}

func TestResolveBootstrapShellFallsBackToPathLookup(t *testing.T) {
	dir := t.TempDir()
	found := writeFakeShell(t, dir, "bash", 0o755)
	withBootstrapShellResolver(t, []string{filepath.Join(dir, "absent")}, func(name string) (string, error) {
		assert.Equal(t, "bash", name)
		return found, nil
	})
	assert.Equal(t, found, resolveBootstrapShell())
}

// With no bash anywhere, the resolver must still produce a usable interpreter
// so ordinary launches (whose bootstrap text tclaude generates entirely
// itself) keep working. Callers carrying operator-authored shell are the ones
// that must refuse, which is what BootstrapShellIsBash is for.
func TestResolveBootstrapShellFallsBackToSh(t *testing.T) {
	withBootstrapShellResolver(t, []string{filepath.Join(t.TempDir(), "absent")},
		func(string) (string, error) { return "", errors.New("not on PATH") })
	assert.Equal(t, BootstrapShellFallback, resolveBootstrapShell())
}

func TestIsBootstrapShellWord(t *testing.T) {
	// Every spelling tclaude has ever started a pane with must be recognized:
	// the bare word from a pre-bash tclaude, the fallback, and a resolved bash
	// from any of the paths the resolver can return.
	for _, word := range []string{"sh", "bash", "/bin/sh", "/bin/bash", "/usr/bin/bash", "/opt/homebrew/bin/bash"} {
		assert.True(t, IsBootstrapShellWord(word), word)
	}
	for _, word := range []string{"", "zsh", "fish", "/usr/bin/fish", "bashful", "/bin/bash-5.2"} {
		assert.False(t, IsBootstrapShellWord(word), word)
	}
}

// The pinned interpreter has to be an absolute path. A bare "bash" word would
// be resolved against the launching PATH, which is the operator's, so a bash
// earlier on it would silently become the interpreter for tclaude's bootstrap.
func TestBootstrapShellPathIsAbsolute(t *testing.T) {
	assert.True(t, filepath.IsAbs(BootstrapShellPath()), BootstrapShellPath())
	assert.True(t, IsBootstrapShellWord(BootstrapShellPath()))
}
