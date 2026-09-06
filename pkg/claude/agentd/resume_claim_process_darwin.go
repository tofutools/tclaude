//go:build darwin

package agentd

import (
	"fmt"
	"os"
	"syscall"
)

func resumeClaimProcessCurrent(pid int, expected string) bool {
	if pid <= 1 || expected != fmt.Sprintf("pid:%d", pid) {
		return false
	}
	p, err := os.FindProcess(pid)
	return err == nil && p.Signal(syscall.Signal(0)) == nil
}
