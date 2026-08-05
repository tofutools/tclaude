//go:build unix

package agentd

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The escalation ladder's signal step must refuse group spellings that are not
// a pane's group.
//
// kill(2) reads a negative pid as a process group, and two spellings are not
// group sends at all: -1 is every process the caller may signal, and -0 is the
// caller's own group. Neither is ever what "end this pane's process group"
// means, so both are refusals rather than trade-offs — there is no legitimate
// call they block.
//
// Signal 0 throughout: it performs the permission and existence checks without
// delivering anything, so a run where a guard has been REMOVED probes rather
// than kills. A test for a guard against catastrophic signals must not be the
// thing that sends one. Do not "simplify" this to SIGTERM — it passes either
// way, and the run where that matters is the run where the guard is gone.
//
// Calls signalProcessGroup, the named production implementation, NOT the
// signalLifecycleProcessGroup var: TestMain swaps that var binary-wide for a
// no-op stub, so going through it would assert against the stub and pass for
// no reason at all.
func TestSignalLifecycleProcessGroup_RefusesNonPaneGroups(t *testing.T) {
	t.Run("the daemon's own group", func(t *testing.T) {
		// This process's pid resolves to this process's group, which is what
		// the first guard names.
		err := signalProcessGroup(os.Getpid(), syscall.Signal(0))
		require.Error(t, err, "signalling our own process group must be refused, not delivered")
		assert.Contains(t, err.Error(), "daemon's own process group")
	})

	t.Run("pgid 1", func(t *testing.T) {
		pgid, err := syscall.Getpgid(1)
		if err != nil || pgid != 1 || syscall.Getpgrp() == 1 {
			// Not every environment lets us resolve pid 1's group, and in a
			// pid namespace it need not be 1. The Getpgrp check covers a test
			// binary that is ITSELF in pgid 1 (a container where `go test` is
			// PID 1): there the FIRST guard fires and refuses with a different
			// message, which would fail the assertion below for a reason that
			// has nothing to do with the guard under test.
			//
			// Skipping is honest: the guard is unconditional in the code, this
			// case just cannot be staged here.
			t.Skipf("cannot stage pgid 1 in this environment (pgid=%d, pgrp=%d, err=%v)",
				pgid, syscall.Getpgrp(), err)
		}
		err = signalProcessGroup(1, syscall.Signal(0))
		require.Error(t, err, "kill(-1, ...) is every process the caller may signal; it must be refused")
		assert.Contains(t, err.Error(), "not a pane's group")
	})
}
