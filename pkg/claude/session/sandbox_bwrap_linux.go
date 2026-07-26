//go:build linux

package session

import (
	"fmt"
	"os/exec"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

var (
	lookPathBwrap = exec.LookPath
	probeBwrap    = func(binary string, posture sandboxpolicy.NetworkPosture) error {
		args := []string{
			"--die-with-parent",
			"--ro-bind", "/", "/",
			"--dev", "/dev",
			"--proc", "/proc",
			"--tmpfs", "/tmp",
		}
		switch posture {
		case sandboxpolicy.NetworkHostOpen:
		case sandboxpolicy.NetworkIsolatedWithAgentd:
			args = append(args, "--unshare-net", "--unshare-pid")
		case sandboxpolicy.NetworkFiltered:
			return fmt.Errorf("network posture %s is reserved and has no tclaude-layer probe", posture)
		default:
			return fmt.Errorf("invalid tclaude-layer network posture %d", posture)
		}
		return exec.Command(binary, append(args, "--", "true")...).Run()
	}
)

func resolveBwrapBinary(posture sandboxpolicy.NetworkPosture) (string, error) {
	binary, err := lookPathBwrap("bwrap")
	if err != nil {
		return "", fmt.Errorf("tclaude-layer requires bubblewrap (`bwrap`) on PATH: %w", err)
	}
	if err := probeBwrap(binary, posture); err != nil {
		requiredNamespaces := "mount namespace"
		if posture == sandboxpolicy.NetworkIsolatedWithAgentd {
			requiredNamespaces = "mount, network, and PID namespaces required by isolated-with-agentd"
		}
		return "", fmt.Errorf("tclaude-layer cannot create the bubblewrap %s "+
			"(unprivileged user namespaces may be unavailable): %w", requiredNamespaces, err)
	}
	return binary, nil
}
