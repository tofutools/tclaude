//go:build darwin

package copilotfixture

import (
	"syscall"
	"testing"
)

func TestDarwinNoSignalableProcessGroupIsGone(t *testing.T) {
	if !ptyProcessGroupGone(syscall.EPERM) {
		t.Fatal("Darwin EPERM after cleanup group SIGKILL must end the wait")
	}
}
