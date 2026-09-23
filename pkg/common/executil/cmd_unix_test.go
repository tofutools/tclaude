//go:build !windows

package executil

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestSetupPreservesConfiguredProcessAttributes(t *testing.T) {
	cmd := exec.Command("/bin/true")
	configured := &syscall.SysProcAttr{}
	cmd.SysProcAttr = configured
	setup(cmd)
	if cmd.SysProcAttr != configured || !cmd.SysProcAttr.Setpgid {
		t.Fatal("process-group setup replaced preconfigured process attributes")
	}
}
