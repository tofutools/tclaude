//go:build !linux && !darwin

package agentd

import "os/exec"

func configureOpenCodeProcessGroup(*exec.Cmd) {}
func killOpenCodeProcessGroup(*exec.Cmd)      {}
