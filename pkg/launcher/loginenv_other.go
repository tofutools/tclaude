//go:build !linux && !darwin

package launcher

import "os/exec"

func setNewSession(*exec.Cmd) {}

func killSession(*exec.Cmd) {}
