//go:build !linux && !darwin

package executor

import osexec "os/exec"

func configureProgramCommand(command *osexec.Cmd) {
	command.WaitDelay = programWaitDelay
}

func cleanupProgramCommand(*osexec.Cmd) {}
