//go:build linux || darwin

package copilotfixture

import (
	"slices"
	"syscall"
	"testing"
	"time"
)

func TestCleanupPTYProcessGroupWaitsForExit(t *testing.T) {
	var signals []syscall.Signal
	checks := 0
	err := cleanupPTYProcessGroup(1234, time.Second,
		func(pid int, signal syscall.Signal) error {
			if pid != -1234 {
				t.Fatalf("signal target = %d, want private process group -1234", pid)
			}
			signals = append(signals, signal)
			if signal == syscall.SIGKILL {
				return nil
			}
			checks++
			if checks < 3 {
				return nil
			}
			return syscall.ESRCH
		})
	if err != nil {
		t.Fatalf("cleanupPTYProcessGroup: %v", err)
	}
	want := []syscall.Signal{syscall.SIGKILL, 0, 0, 0}
	if !slices.Equal(signals, want) {
		t.Fatalf("signals = %v, want %v", signals, want)
	}
}
