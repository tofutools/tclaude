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
// A mode with no execute bit is the portable way to make access(X_OK) refuse a
// file for every user including root, which is what lets these tests cover the
// unexecutable-candidate rules on a root CI runner as well as a normal one.
func writeCodexStub(t *testing.T, dir string, mode os.FileMode, body string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, "codex")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	require.NoError(t, os.Chmod(path, mode))
	return path
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
		"#!/bin/sh\nprintf '%s\\n' '"+complaint+"' >&2\nexec /bin/sleep 1\n")

	// Comfortably shorter than the stub's lifetime, so the deadline has passed
	// by the time the stub's exit ends the read and the timeout branch is the
	// one that reports.
	previous := codexEffectiveConfigTimeout
	codexEffectiveConfigTimeout = 100 * time.Millisecond
	t.Cleanup(func() { codexEffectiveConfigTimeout = previous })

	_, err := readCodexEffectiveConfigJSON(t.TempDir(),
		[]sandboxpolicy.EnvironmentEntry{{Name: "PATH", Value: dir}}, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not answer within 100ms")
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
