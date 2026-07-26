//go:build linux

package session

import (
	"fmt"
	"os/exec"
)

var (
	lookPathBwrap = exec.LookPath
	probeBwrap    = func(binary string) error {
		return exec.Command(binary,
			"--die-with-parent",
			"--ro-bind", "/", "/",
			"--dev", "/dev",
			"--proc", "/proc",
			"--tmpfs", "/tmp",
			"--", "true",
		).Run()
	}
)

func resolveBwrapBinary() (string, error) {
	binary, err := lookPathBwrap("bwrap")
	if err != nil {
		return "", fmt.Errorf("tclaude-layer requires bubblewrap (`bwrap`) on PATH: %w", err)
	}
	if err := probeBwrap(binary); err != nil {
		return "", fmt.Errorf("tclaude-layer cannot create a bubblewrap mount namespace "+
			"(unprivileged user namespaces may be unavailable): %w", err)
	}
	return binary, nil
}
