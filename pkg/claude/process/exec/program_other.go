//go:build !linux && !darwin

package processexec

import (
	osexec "os/exec"
	"time"
)

func configureProgramCommand(command *osexec.Cmd) {
	command.WaitDelay = time.Second
}
