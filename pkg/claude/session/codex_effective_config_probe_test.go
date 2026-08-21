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
// permission checks these cases depend on: root traverses any directory, and
// its access(X_OK) succeeds whenever any execute bit is set, so no mode can
// express "not executable by me" for it.
func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the permission checks under test")
	}
}

const codexStubBody = "#!/bin/sh\nexit 0\n"

// The distinction the walk turns on: a candidate whose mode bits look
// executable but which THIS process may not run. Without this case the suite
// passes against the old mode-bit predicate, and a regression back to it would
// ship green.
func TestCodexEffectiveConfigLookPathSkipsCandidateItMayNotExecute(t *testing.T) {
	requireUnprivileged(t)
	root := t.TempDir()
	shadowed := filepath.Join(root, "shadowed")
	usable := filepath.Join(root, "usable")
	blocked := writeCodexStub(t, shadowed, modeExecutableByOthersOnly, codexStubBody)
	want := writeCodexStub(t, usable, 0o755, codexStubBody)

	// Guard the fixture itself: if this file ever stops satisfying "execute bit
	// set, still not executable by me", the test would silently stop covering
	// the regression it exists for.
	info, err := os.Stat(blocked)
	require.NoError(t, err)
	require.NotZero(t, info.Mode().Perm()&0o111,
		"fixture must carry an execute bit the old predicate accepted")
	require.Error(t, codexExecutableAccess(blocked),
		"fixture must still be unexecutable by this process")

	resolved, err := codexEffectiveConfigLookPath("",
		strings.Join([]string{shadowed, usable}, string(os.PathListSeparator)))
	require.NoError(t, err)
	assert.Equal(t, want, resolved)
}

// A file that is not an executable at all is skipped the same way, and the walk
// still reaches a working codex behind it. This one needs no privilege gate.
func TestCodexEffectiveConfigLookPathSkipsNonExecutableCandidate(t *testing.T) {
	root := t.TempDir()
	shadowed := filepath.Join(root, "shadowed")
	usable := filepath.Join(root, "usable")
	writeCodexStub(t, shadowed, 0o644, codexStubBody)
	want := writeCodexStub(t, usable, 0o755, codexStubBody)

	resolved, err := codexEffectiveConfigLookPath("",
		strings.Join([]string{shadowed, usable}, string(os.PathListSeparator)))
	require.NoError(t, err)
	assert.Equal(t, want, resolved)
}

// When the only codex in PATH is one this process cannot run, the refusal has
// to say so and name it. "Not found" would send an operator to install a binary
// they already have, which is the wrong half of the problem.
func TestCodexEffectiveConfigLookPathNamesCandidateItCannotExecute(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bin")
	blocked := writeCodexStub(t, dir, 0o644, codexStubBody)

	_, err := codexEffectiveConfigLookPath("", dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), blocked)
	assert.Contains(t, err.Error(), "cannot be executed by this process")
	assert.NotContains(t, err.Error(), "not found in the launch PATH")
}

// A candidate the process cannot even stat — a directory on the way to it that
// it may not traverse — is a permission problem too, and reporting it as "not
// found" would be the same misdirection in a different disguise.
func TestCodexEffectiveConfigLookPathNamesCandidateItCannotExamine(t *testing.T) {
	requireUnprivileged(t)
	dir := filepath.Join(t.TempDir(), "bin")
	candidate := writeCodexStub(t, dir, 0o755, codexStubBody)
	require.NoError(t, os.Chmod(dir, 0o000))
	// Restore before TempDir's own cleanup, which cannot remove what it cannot
	// traverse either.
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, err := codexEffectiveConfigLookPath("", dir)
	require.Error(t, err)
	assert.Contains(t, err.Error(), candidate)
	assert.Contains(t, err.Error(), "cannot be executed by this process")
	assert.NotContains(t, err.Error(), "not found in the launch PATH")
}

// A relative PATH entry means "relative to the process that uses it", and that
// process is the launch, not this one. Resolving it against the parent's cwd
// would inspect one codex and let the launch run a different one.
func TestCodexEffectiveConfigLookPathResolvesRelativeEntryFromLaunchCwd(t *testing.T) {
	parent := t.TempDir()
	launch := t.TempDir()
	decoy := writeCodexStub(t, filepath.Join(parent, "bin"), 0o755, codexStubBody)
	want := writeCodexStub(t, filepath.Join(launch, "bin"), 0o755, codexStubBody)
	t.Chdir(parent)

	resolved, err := codexEffectiveConfigLookPath(launch, "bin")
	require.NoError(t, err)
	assert.Equal(t, want, resolved)
	assert.NotEqual(t, decoy, resolved,
		"a relative PATH entry must not resolve against the parent process cwd")
}

// Candidates come back absolute. An empty PATH element must not collapse to the
// bare name "codex", which exec would resolve against the PARENT process PATH.
func TestCodexEffectiveConfigLookPathReturnsAbsoluteCandidate(t *testing.T) {
	launch := t.TempDir()
	want := writeCodexStub(t, launch, 0o755, codexStubBody)
	t.Chdir(t.TempDir())

	// A lone separator is two EMPTY entries, which is the case that used to
	// collapse to the bare name; a wholly empty PATH is a different branch that
	// defers to exec.LookPath.
	for _, entry := range []string{string(os.PathListSeparator), "."} {
		resolved, err := codexEffectiveConfigLookPath(launch, entry)
		require.NoErrorf(t, err, "PATH entry %q", entry)
		assert.Truef(t, filepath.IsAbs(resolved),
			"PATH entry %q resolved to non-absolute %q", entry, resolved)
		assert.Equalf(t, want, resolved, "PATH entry %q", entry)
	}
}

// A PATH with no codex at all keeps the plain not-found refusal: the two
// diagnoses point at different fixes and must stay distinguishable.
func TestCodexEffectiveConfigLookPathReportsAbsentCodex(t *testing.T) {
	_, err := codexEffectiveConfigLookPath("", t.TempDir())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in the launch PATH")
}

// A directory named codex is not a candidate, and must not be reported as an
// unexecutable one — a directory carries execute bits meaning "traversable".
func TestCodexEffectiveConfigLookPathIgnoresDirectoryNamedCodex(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "codex"), 0o755))

	_, err := codexEffectiveConfigLookPath("", dir)
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
		"#!/bin/sh\nprintf '%s\\n' '"+complaint+"' >&2\nexit 2\n")

	_, err := readCodexEffectiveConfigJSON(t.TempDir(),
		[]sandboxpolicy.EnvironmentEntry{{Name: "PATH", Value: dir}}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "produced no config/read result")
	assert.Contains(t, err.Error(), "Codex reported: "+complaint)
}
