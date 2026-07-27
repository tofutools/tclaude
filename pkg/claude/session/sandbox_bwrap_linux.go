//go:build linux

package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"golang.org/x/sys/unix"
)

// bwrapProbeTimeout bounds the capability probe. The probe does trivial work
// (fork bwrap, stat one path, attempt one write), so anything approaching this
// means the namespace setup itself is wedged — a hung LSM, a stuck /tmp — and
// waiting longer cannot help.
//
// The deadline became load-bearing when the probe stopped being a once-per-
// launch cost: TCL-769 put the same predicate behind the dashboard's polled
// capability disclosure and the spawn boundary's refusal, so an unbounded exec
// there would hang a poll loop rather than one launch.
const bwrapProbeTimeout = 5 * time.Second

var (
	lookPathBwrap = exec.LookPath
	probeBwrap    = func(binary string, posture sandboxpolicy.NetworkPosture) error {
		args, err := tclaudeLayerProbeArgs(posture)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), bwrapProbeTimeout)
		defer cancel()
		if err := exec.CommandContext(ctx, binary, args...).Run(); err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("bubblewrap capability probe timed out after %s: %w",
					bwrapProbeTimeout, ctx.Err())
			}
			return err
		}
		return nil
	}
	probeTclaudeLayerPidfd = func() error {
		fd, err := unix.PidfdOpen(os.Getpid(), 0)
		if err != nil {
			return err
		}
		return unix.Close(fd)
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
	if err := probeTclaudeLayerPidfd(); err != nil {
		return "", fmt.Errorf("tclaude-layer requires Linux pidfd support for its terminal-resize relay: %w", err)
	}
	return binary, nil
}

func tclaudeLayerCommand(
	binary string,
	phase0WriteDirs, breakGlassPaths []string,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
) (string, error) {
	command, err := bwrapCommand(binary, phase0WriteDirs, breakGlassPaths, plan, harnessCommand)
	if err != nil {
		return "", err
	}
	relay := clcommon.DetectAbsoluteCmd("session", tclaudeLayerWinchRelayCommand)
	return relay + " -- " + command, nil
}

func tclaudeLayerLaunchOSSandbox(posture sandboxpolicy.NetworkPosture) harness.LaunchOSSandbox {
	switch posture {
	case sandboxpolicy.NetworkIsolatedWithAgentd:
		return harness.LaunchOSSandbox{
			State:  "on",
			Source: "tclaude-layer (bubblewrap; isolated network; host loopback/IDE bridge unavailable; isolated PIDs; constructed root; agentd socket allowlisted)",
		}
	default:
		return harness.LaunchOSSandbox{
			State:      "on",
			Source:     "tclaude-layer (bubblewrap; host network; ambient host Unix sockets reachable)",
			Unverified: true,
		}
	}
}
