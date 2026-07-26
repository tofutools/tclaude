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
		args, err := tclaudeLayerProbeArgs(posture)
		if err != nil {
			return err
		}
		return exec.Command(binary, args...).Run()
	}
)

func tclaudeLayerProbeArgs(posture sandboxpolicy.NetworkPosture) ([]string, error) {
	args := []string{
		"--die-with-parent",
		"--ro-bind", "/", "/",
	}
	switch posture {
	case sandboxpolicy.NetworkHostOpen:
	case sandboxpolicy.NetworkIsolatedWithAgentd:
		args = append(args, "--unshare-net", "--unshare-pid")
	case sandboxpolicy.NetworkFiltered:
		return nil, fmt.Errorf("network posture %s is reserved and has no tclaude-layer probe", posture)
	default:
		return nil, fmt.Errorf("invalid tclaude-layer network posture %d", posture)
	}
	const (
		probeBind  = "/tmp/.tclaude-remount-probe"
		probeWrite = "/tmp/.tclaude-remount-write"
	)
	args = append(args,
		"--dev", "/dev",
		"--proc", "/proc",
		// Exercise the required semantics rather than merely checking that
		// this bwrap version parses --remount-ro: a child bind must survive
		// the non-recursive parent remount and a new write must fail.
		"--tmpfs", "/tmp",
		"--ro-bind", "/dev/null", probeBind,
		"--remount-ro", "/tmp",
		"--", "/bin/sh", "-c",
		"test -e "+probeBind+" && ! touch "+probeWrite,
	)
	return args, nil
}

func resolveBwrapBinary(posture sandboxpolicy.NetworkPosture) (string, error) {
	binary, err := lookPathBwrap("bwrap")
	if err != nil {
		return "", fmt.Errorf("tclaude-layer requires bubblewrap (`bwrap`) on PATH: %w", err)
	}
	if err := probeBwrap(binary, posture); err != nil {
		requiredNamespaces := "mount namespace and read-only remount support"
		if posture == sandboxpolicy.NetworkIsolatedWithAgentd {
			requiredNamespaces = "mount, network, and PID namespaces plus read-only remount support required by isolated-with-agentd"
		}
		return "", fmt.Errorf("tclaude-layer cannot create the bubblewrap %s "+
			"(unprivileged user namespaces may be unavailable): %w", requiredNamespaces, err)
	}
	return binary, nil
}
