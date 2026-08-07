//go:build !windows

package session

import (
	"os"
	"path/filepath"
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
