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

var tclaudeLayerRelayPrefix = func() string {
	return clcommon.DetectAbsoluteCmd("session", tclaudeLayerWinchRelayCommand)
}

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
	case sandboxpolicy.NetworkIsolatedWithAgentd, sandboxpolicy.NetworkFiltered:
		args = append(args, "--unshare-net", "--unshare-pid")
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
	binary, err := resolveBwrapServerBinary(posture)
	if err != nil {
		return "", err
	}
	if err := probeTclaudeLayerPidfd(); err != nil {
		return "", fmt.Errorf("tclaude-layer requires Linux pidfd support for its terminal-resize relay: %w", err)
	}
	return binary, nil
}

func resolveBwrapServerBinary(posture sandboxpolicy.NetworkPosture) (string, error) {
	binary, err := lookPathBwrap("bwrap")
	if err != nil {
		return "", fmt.Errorf("tclaude-layer requires bubblewrap (`bwrap`) on PATH: %w", err)
	}
	if err := probeBwrap(binary, posture); err != nil {
		requiredNamespaces := "mount namespace and read-only remount support"
		if posture == sandboxpolicy.NetworkIsolatedWithAgentd ||
			posture == sandboxpolicy.NetworkFiltered {
			requiredNamespaces = "mount, network, and PID namespaces plus read-only remount support required by isolated-with-agentd"
		}
		return "", fmt.Errorf("tclaude-layer cannot create the bubblewrap %s "+
			"(unprivileged user namespaces may be unavailable): %w", requiredNamespaces, err)
	}
	if posture == sandboxpolicy.NetworkFiltered {
		if _, err := resolveFilteredNetworkExecutables(); err != nil {
			return "", fmt.Errorf("tclaude-layer filtered network prerequisite: %w", err)
		}
	}
	return binary, nil
}

func tclaudeLayerCommand(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
) (string, error) {
	command, err := bwrapCommand(
		binary,
		phase0WriteDirs,
		privateWriteDirs,
		finalHideDirs,
		readOnlyBinds,
		socketPaths,
		plan,
		harnessCommand,
	)
	if err != nil {
		return "", err
	}
	relay := tclaudeLayerRelayPrefix()
	filtered, err := filteredNetworkRelayPrefix(plan)
	if err != nil {
		return "", err
	}
	return relay + filtered + " -- " + command, nil
}

func tclaudeLayerStackedCommand(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	manifestPath, manifestSHA256, readyPath string,
	consume bool,
	harnessCommand string,
) (string, error) {
	if plan.NetworkPosture == sandboxpolicy.NetworkFiltered {
		return "", fmt.Errorf("stacked filtered-network launches are not enabled in M2b")
	}
	command, err := bwrapCommand(
		binary,
		phase0WriteDirs,
		privateWriteDirs,
		finalHideDirs,
		readOnlyBinds,
		socketPaths,
		plan,
		harnessCommand,
	)
	if err != nil {
		return "", err
	}
	relay := tclaudeLayerRelayPrefix()
	relay += " --stacked-binding " + clcommon.ShellQuoteArg(manifestPath)
	relay += " --stacked-binding-sha256 " + clcommon.ShellQuoteArg(manifestSHA256)
	if consume {
		relay += " --stacked-consume"
	}
	if readyPath != "" {
		relay += " --stacked-ready " + clcommon.ShellQuoteArg(readyPath)
	}
	return relay + " -- " + command, nil
}

func tclaudeLayerServerCommand(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	serverCommand string,
) (string, error) {
	if plan.NetworkPosture == sandboxpolicy.NetworkFiltered {
		return "", fmt.Errorf("filtered-network server boundaries remain disabled until M3")
	}
	return bwrapCommand(
		binary,
		phase0WriteDirs,
		privateWriteDirs,
		finalHideDirs,
		readOnlyBinds,
		socketPaths,
		plan,
		serverCommand,
	)
}

func tclaudeLayerOpenCodeLaunchOSSandbox() harness.LaunchOSSandbox {
	return harness.LaunchOSSandbox{
		State: "on",
		// The pane, control-plane and networking caveats live in the badge's
		// partial-fidelity sentence rather than here, so each is stated once
		// (TCL-790).
		Source:     "tclaude-layer (bubblewrap; OpenCode tool-executing server confined)",
		Unverified: true,
	}
}

func tclaudeLayerLaunchOSSandbox(posture sandboxpolicy.NetworkPosture) harness.LaunchOSSandbox {
	switch posture {
	case sandboxpolicy.NetworkIsolatedWithAgentd:
		return harness.LaunchOSSandbox{
			State:  "on",
			Source: "tclaude-layer (bubblewrap; isolated network; host loopback/IDE bridge unavailable; isolated PIDs; constructed root; agentd socket allowlisted)",
		}
	case sandboxpolicy.NetworkFiltered:
		return harness.LaunchOSSandbox{
			State:           "on",
			Source:          "tclaude-layer (bubblewrap; filtered network via supervised rootless pasta + atomic nftables; isolated PIDs; constructed root; agentd socket allowlisted)",
			FilteredNetwork: true,
		}
	default:
		return harness.LaunchOSSandbox{
			State: "on",
			// Source names the mechanism and posture that decided; the badge's
			// own partial-fidelity sentence is the single home of the ambient
			// host Unix socket caveat, so repeating it here would print the same
			// warning twice in one tooltip (TCL-790).
			Source:     "tclaude-layer (bubblewrap; host network)",
			Unverified: true,
		}
	}
}

func validateTclaudeLayerHarness(string) error {
	return nil
}
