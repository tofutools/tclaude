//go:build !windows

package executil

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestSetupPreservesConfiguredProcessAttributes(t *testing.T) {
	cmd := exec.Command("/bin/true")
	configured := &syscall.SysProcAttr{Pgid: 1234}
	cmd.SysProcAttr = configured
	setup(cmd)
	if cmd.SysProcAttr != configured || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pgid != 0 {
		t.Fatal("process-group setup replaced preconfigured process attributes")
	}
}
