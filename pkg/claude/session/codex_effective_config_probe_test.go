//go:build unix

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// writeCodexStub writes a `codex` entry with mode into dir and returns its path.
func writeCodexStub(t *testing.T, dir string, mode os.FileMode, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "codex")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

// modeExecutableByOthersOnly carries an execute bit — so the mode-bit test this
// walk used to apply accepts it — while denying execute to its owner. It is the
// mode that separates "has an x bit somewhere" from "this process may run it".
const modeExecutableByOthersOnly os.FileMode = 0o601

// requireUnprivileged skips when the test runs as root, which bypasses the
// permission check the case under test depends on: root's access(X_OK) succeeds
// whenever any execute bit is set, so no mode can express "not executable by
// me" for it.
func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the execute-permission check under test")
	}
}

// The distinction the walk turns on: a candidate whose mode bits look
// executable but which THIS process may not run. Without this case the suite
// passes against the old mode-bit predicate, and a regression back to it would
// ship green.
func TestCodexEffectiveConfigLookPathSkipsCandidateItMayNotExecute(t *testing.T) {
	requireUnprivileged(t)
	root := t.TempDir()
	shadowed := filepath.Join(root, "shadowed")
	usable := filepath.Join(root, "usable")
	blocked := writeCodexStub(t, shadowed, modeExecutableByOthersOnly, "#!/bin/sh\nexit 0\n")
	want := writeCodexStub(t, usable, 0o755, "#!/bin/sh\nexit 0\n")

	// Guard the fixture itself: if this file ever stops satisfying "execute bit
	// set, still not executable by me", the test would silently stop covering
	// the regression it exists for.
	info, err := os.Stat(blocked)
	require.NoError(t, err)
	require.NotZero(t, info.Mode().Perm()&0o111,
		"fixture must carry an execute bit the old predicate accepted")
	require.Error(t, codexExecutableAccess(blocked),
		"fixture must still be unexecutable by this process")

	resolved, err := codexEffectiveConfigLookPath(
		strings.Join([]string{shadowed, usable}, string(os.PathListSeparator)))
	require.NoError(t, err)
	assert.Equal(t, want, resolved)
}

// An entry the process may not execute is a permission problem and is named;
// one that simply is not an executable file is not a candidate at all.
func TestCodexEffectiveConfigLookPathNamesCandidateItMayNotExecute(t *testing.T) {
	requireUnprivileged(t)
	dir := filepath.Join(t.TempDir(), "bin")
	blocked := writeCodexStub(t, dir, modeExecutableByOthersOnly, "#!/bin/sh\nexit 0\n")

	_, err := codexEffectiveConfigLookPath(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), blocked)
	assert.Contains(t, err.Error(), "not executable by this process")
}

// PATH entries are resolved to absolute paths: the caller hands the result to a
// command whose Dir is the launch cwd, so a relative entry must not come back
// relative, and an empty entry must not collapse to the bare name "codex" —
// that would send exec back to the parent process PATH.
func TestCodexEffectiveConfigLookPathReturnsAbsoluteCandidate(t *testing.T) {
	dir := t.TempDir()
	want := writeCodexStub(t, dir, 0o755, "#!/bin/sh\nexit 0\n")
	t.Chdir(dir)

	// A lone separator is two EMPTY entries, which is the case that used to
	// collapse to the bare name; a wholly empty PATH is a different branch that
	// defers to exec.LookPath.
	for _, entry := range []string{string(os.PathListSeparator), "."} {
		resolved, err := codexEffectiveConfigLookPath(entry)
		require.NoErrorf(t, err, "PATH entry %q", entry)
		assert.Truef(t, filepath.IsAbs(resolved),
			"PATH entry %q resolved to non-absolute %q", entry, resolved)
		assert.Equalf(t, want, resolved, "PATH entry %q", entry)
	}
}

// The walk must treat a candidate this process cannot execute the way
// exec.LookPath does — as no match — and keep looking. Stopping there hid a
// working Codex later in PATH behind an EACCES raised much later by execve.
func TestCodexEffectiveConfigLookPathSkipsUnexecutableCandidate(t *testing.T) {
	root := t.TempDir()
	shadowed := filepath.Join(root, "shadowed")
	usable := filepath.Join(root, "usable")
	writeCodexStub(t, shadowed, 0o644, "#!/bin/sh\nexit 0\n")
	want := writeCodexStub(t, usable, 0o755, "#!/bin/sh\nexit 0\n")

	resolved, err := codexEffectiveConfigLookPath(
		strings.Join([]string{shadowed, usable}, string(os.PathListSeparator)))
	require.NoError(t, err)
	assert.Equal(t, want, resolved)
}

// When the only codex in PATH is one this process may not execute, the refusal
// has to say so and name it. "Not found" would send an operator to install a
// binary they already have, which is the wrong half of the problem.
func TestCodexEffectiveConfigLookPathNamesUnexecutableCandidate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	blocked := writeCodexStub(t, dir, 0o644, "#!/bin/sh\nexit 0\n")

	_, err := codexEffectiveConfigLookPath(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), blocked)
	assert.Contains(t, err.Error(), "not executable by this process")
	assert.NotContains(t, err.Error(), "not found in the launch PATH")
}

// A PATH with no codex at all keeps the plain not-found refusal: the two
// diagnoses point at different fixes and must stay distinguishable.
func TestCodexEffectiveConfigLookPathReportsAbsentCodex(t *testing.T) {
	_, err := codexEffectiveConfigLookPath(t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in the launch PATH")
}

// A directory named codex is not a candidate, and must not be reported as an
// unexecutable one — a directory carries execute bits meaning "traversable".
func TestCodexEffectiveConfigLookPathIgnoresDirectoryNamedCodex(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "codex"), 0o755))

	_, err := codexEffectiveConfigLookPath(dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in the launch PATH")
}

// The timeout branch is the one that matters most for a Codex blocked from its
// own state root: such a Codex prints the reason and then hangs rather than
// exiting, so dropping its stderr left the operator a bare "did not answer".
//
// Run under -race this also covers the read itself: os/exec copies a non-file
// Stderr on a goroutine joined only by Wait(), which this path does not call.
func TestCodexEffectiveConfigTimeoutKeepsCodexDiagnostics(t *testing.T) {
	const complaint = "codex: cannot write /home/u/.codex/state_5.sqlite: permission denied"
	dir := filepath.Join(t.TempDir(), "bin")
	// The stub complains and then outlives the deadline before exiting on its
	// own, rather than parking until something kills it: a `go test`-hosted
	// binary does not reliably reap a parked child, so a stub that waits to be
	// killed hangs the run instead of failing it. sleep is spelled absolutely
	// because the probe runs the stub with the launch PATH, which is this
	// directory alone.
	writeCodexStub(t, dir, 0o755,
		"#!/bin/sh\nprintf '%s\\n' '"+complaint+"' >&2\nexec /bin/sleep 3\n")

	// The deadline has to be long enough that forking /bin/sh and running its
	// printf beats it even on a loaded runner — otherwise the stub is killed
	// before it complains and the tail under test never exists — and short
	// enough that it passes well before the stub exits on its own, so the
	// timeout branch is the one that reports.
	setCodexEffectiveConfigTimeoutForTest(t, 750*time.Millisecond)

	_, err := readCodexEffectiveConfigJSON(t.TempDir(),
		[]sandboxpolicy.EnvironmentEntry{{Name: "PATH", Value: dir}}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not answer within 750ms")
	assert.Contains(t, err.Error(), "Codex reported: "+complaint)
}

// A Codex that exits without answering already reported its stderr; keep that
// covered alongside the timeout so the two paths cannot drift apart again.
func TestCodexEffectiveConfigExitKeepsCodexDiagnostics(t *testing.T) {
	const complaint = "codex: unknown subcommand `app-server`"
	dir := filepath.Join(t.TempDir(), "bin")
	writeCodexStub(t, dir, 0o755,
		"#!/bin/sh\nprintf '%s\\n' "+"'"+complaint+"'"+" >&2\nexit 2\n")

	_, err := readCodexEffectiveConfigJSON(t.TempDir(),
		[]sandboxpolicy.EnvironmentEntry{{Name: "PATH", Value: dir}}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "produced no config/read result")
	assert.Contains(t, err.Error(), "Codex reported: "+complaint)
}
