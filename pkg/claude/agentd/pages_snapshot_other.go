//go:build !unix

package agentd

import "os/exec"

func chromeProcessGroup(*exec.Cmd) {}

func killChrome(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
