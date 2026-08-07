//go:build !windows

package session

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GetProcessExeName must answer with the binary a process is RUNNING, which
// is the whole point of it existing next to GetProcessName: on Linux the
// latter reads /proc/<pid>/comm, the main thread's name, which a program may
// overwrite — Copilot's Node SEA does, and that made its panes
// unidentifiable to agentd (TCL-1049).
//
// The test process is its own fixture: whatever the go test binary is called,
// that is what both os.Executable and the reader must agree on. Nothing is
// asserted about comm, because a Go program is free to differ there too.
func TestGetProcessExeName_ReportsTheRunningBinary(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)
	assert.Equal(t, filepath.Base(self), GetProcessExeName(os.Getpid()))
}

// An unreadable process yields "" rather than a guess, so a walk that
// consults it degrades to the name check instead of matching something
// arbitrary. Pid 0 is never a readable process on Linux or macOS.
func TestGetProcessExeName_UnreadableProcessYieldsEmpty(t *testing.T) {
	assert.Empty(t, GetProcessExeName(0))
}

// On Linux an unreadable exe link must NOT fall back to `ps`, because there
// ps prints comm — the process-settable value this function exists to be
// stronger than. A process can reach that branch on purpose:
// prctl(PR_SET_DUMPABLE, 0) makes its own /proc/<pid>/exe unreadable. If the
// fallback ran, such a process would choose this function's answer, and the
// harness gate would be back to trusting a name.
//
// The failure is staged through the link reader rather than a real
// dumpable-cleared process: the point being pinned is what the function does
// with a failed read, and staging it is the only portable way to be sure the
// test exercises that branch at all.
func TestGetProcessExeName_LinuxDoesNotFallBackToComm(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the no-fallback rule is Linux-only; elsewhere ps reports the executable path")
	}
	prev := procExeLink
	procExeLink = func(int) (string, error) { return "", errors.New("permission denied") }
	t.Cleanup(func() { procExeLink = prev })

	assert.Empty(t, GetProcessExeName(os.Getpid()),
		"an unreadable exe link is no evidence, not weaker evidence")
	assert.False(t, IsHarnessProcessAt(os.Getpid(), "MainThread"),
		"and the harness gate above it stays closed")
}

// IsHarnessProcessAt admits a process on EITHER piece of evidence — the name
// a caller already read, or the executable underneath it — and on neither
// otherwise. The Copilot case is the second row: comm "MainThread", binary
// "copilot".
func TestIsHarnessProcessAt(t *testing.T) {
	self := os.Getpid()

	assert.True(t, IsHarnessProcessAt(self, "node"),
		"a recognised name needs no executable evidence")
	assert.False(t, IsHarnessProcessAt(self, "MainThread"),
		"neither the renamed thread nor this test binary is a harness")
	assert.False(t, IsHarnessProcessAt(self, ""),
		"an unreadable name falls through to the executable, which is this test binary")
}
