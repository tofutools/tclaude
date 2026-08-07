//go:build !linux && !darwin

package copilotfixture

import "os/exec"

func configurePTYCommand(*exec.Cmd) {}

func cleanupPTYCommand(*exec.Cmd) error { return nil }
