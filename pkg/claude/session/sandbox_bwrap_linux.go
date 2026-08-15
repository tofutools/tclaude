//go:build linux

package session

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
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
	probeBwrap    = func(
		binary string,
		posture sandboxpolicy.NetworkPosture,
		root sandboxpolicy.RootPosture,
	) error {
		args, err := tclaudeLayerProbeArgs(posture, root)
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

func tclaudeLayerProbeArgs(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) ([]string, error) {
	args := []string{
		"--die-with-parent",
		"--ro-bind", "/", "/",
	}
	switch posture {
	case sandboxpolicy.NetworkHostOpen:
		if root == sandboxpolicy.RootConstructed {
			// A host-open constructed root keeps the network namespace but
			// still needs the PID namespace that closes the /proc/<pid>/root
			// route back to the host mount namespace. Probe exactly that, and
			// not the network namespace the launch will not create.
			args = append(args, "--unshare-pid")
		}
	case sandboxpolicy.NetworkIsolatedWithAgentd:
		args = append(args, "--unshare-net", "--unshare-pid")
	case sandboxpolicy.NetworkFiltered:
		args = append(args,
			"--unshare-user",
			"--uid", "0",
			"--gid", "0",
			"--unshare-net",
			"--unshare-pid",
		)
		args = append(args, filteredNetworkBootstrapCapabilityArgs()...)
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
		"test -e "+probeBind+" && ! touch "+probeWrite+
			filteredNetworkProbeCapabilityCheck(posture),
	)
	return args, nil
}

func filteredNetworkProbeCapabilityCheck(posture sandboxpolicy.NetworkPosture) string {
	if posture != sandboxpolicy.NetworkFiltered {
		return ""
	}
	// CAP_NET_ADMIN is bit 12: the fourth hex digit from the right must be odd.
	// This stays within POSIX shell vocabulary and needs no probe helper.
	return ` && cap_eff=$(sed -n 's/^CapEff:[[:space:]]*//p' /proc/self/status)` +
		` && case "$cap_eff" in *[13579bBdDfF]???) true ;; *) false ;; esac`
}

func resolveBwrapBinary(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) (string, error) {
	binary, err := resolveBwrapServerBinary(posture, root)
	if err != nil {
		return "", err
	}
	if err := probeTclaudeLayerPidfd(); err != nil {
		return "", fmt.Errorf("tclaude-layer requires Linux pidfd support for its terminal-resize relay: %w", err)
	}
	return binary, nil
}

// tclaudeLayerToolingPresence is the fork-free half of resolveBwrapBinary: it
// answers "is bubblewrap installed" with a PATH lookup and, for the
// interactive boundary, the pidfd capability the terminal relay needs (two
// syscalls, no child process). It deliberately does NOT run probeBwrap — that
// exec is what makes the availability predicate too expensive for a polled
// disclosure surface.
func tclaudeLayerToolingPresence(interactive bool) error {
	if _, err := lookPathBwrap("bwrap"); err != nil {
		return fmt.Errorf("tclaude-layer requires bubblewrap (`bwrap`) on PATH: %w", err)
	}
	if interactive {
		if err := probeTclaudeLayerPidfd(); err != nil {
			return fmt.Errorf("tclaude-layer requires Linux pidfd support for its terminal-resize relay: %w", err)
		}
	}
	return nil
}

func resolveBwrapServerBinary(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) (string, error) {
	binary, err := lookPathBwrap("bwrap")
	if err != nil {
		return "", fmt.Errorf("tclaude-layer requires bubblewrap (`bwrap`) on PATH: %w", err)
	}
	if err := probeBwrap(binary, posture, root); err != nil {
		requiredNamespaces := "mount namespace and read-only remount support"
		switch posture {
		case sandboxpolicy.NetworkIsolatedWithAgentd:
			requiredNamespaces = "mount, network, and PID namespaces plus read-only remount support required by isolated-with-agentd"
		case sandboxpolicy.NetworkFiltered:
			requiredNamespaces = "mount, network, and PID namespaces plus read-only remount support required by filtered network"
		default:
			if root == sandboxpolicy.RootConstructed {
				requiredNamespaces = "mount and PID namespaces plus read-only remount support required by a constructed root under host networking"
			}
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
	engine, err := tclaudeLayerEnginePrefix(plan)
	if err != nil {
		return "", err
	}
	return relay + engine + " -- " + command, nil
}

func tclaudeLayerCommandWithLoopbackBind(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	_ int,
	harnessCommand string,
) (string, error) {
	return tclaudeLayerCommand(binary, phase0WriteDirs, privateWriteDirs,
		finalHideDirs, readOnlyBinds, socketPaths, plan, harnessCommand)
}

func tclaudeLayerCommandWithRouteSlots(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	routeSlots []int,
	_ *DarwinRouteSlotReservation,
	_ *TclaudeLayerRouteHelper,
	harnessCommand string,
) (string, error) {
	if len(routeSlots) != 0 {
		return "", fmt.Errorf("darwin route slots are unsupported on Linux")
	}
	return tclaudeLayerCommand(
		binary, phase0WriteDirs, privateWriteDirs, finalHideDirs,
		readOnlyBinds, socketPaths, plan, harnessCommand)
}

func tclaudeLayerCommandWithRouteSlotsAndLoopbackBind(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	routeSlots []int,
	preReservation *DarwinRouteSlotReservation,
	routeHelper *TclaudeLayerRouteHelper,
	_ int,
	harnessCommand string,
) (string, error) {
	return tclaudeLayerCommandWithRouteSlots(binary, phase0WriteDirs, privateWriteDirs,
		finalHideDirs, readOnlyBinds, socketPaths, plan, routeSlots,
		preReservation, routeHelper, harnessCommand)
}

func tclaudeLayerCommandWithRouteHelper(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	helper TclaudeLayerRouteHelper,
	harnessCommand string,
) (string, error) {
	credentialFD := tclaudeLayerRouteHelperGuestFD(plan)
	helper.CredentialFD = credentialFD
	wrapper, err := wrapTclaudeLayerRouteHelper(binary, helper, harnessCommand)
	if err != nil {
		return "", err
	}
	command, err := bwrapCommand(binary, phase0WriteDirs, privateWriteDirs,
		finalHideDirs, readOnlyBinds, socketPaths, plan, wrapper)
	if err != nil {
		return "", err
	}
	command = tclaudeLayerPreserveRouteHelperFD(command)
	engine, err := tclaudeLayerEnginePrefix(plan)
	if err != nil {
		return "", err
	}
	routeFlags := tclaudeLayerRouteAuthorityRelayFlags(plan, helper)
	return tclaudeLayerRouteHelperBootstrapPrefix(helper) +
		tclaudeLayerRelayPrefix() + " --preserve-fds 1" + engine + routeFlags + " -- " + command, nil
}

// tclaudeLayerEnginePrefix contributes the supervisor flag for whichever
// filtering engine this plan deploys, and nothing when it deploys none.
//
// The two engines are mutually exclusive by construction here — the plan names
// exactly one — rather than by a check further down, so no launch can be
// rendered with both supervisors attached.
func tclaudeLayerEnginePrefix(plan sandboxpolicy.MountPlan) (string, error) {
	if tclaudeLayerPlanDeploysProxy(plan) {
		return proxyNetworkRelayPrefix(plan)
	}
	return filteredNetworkRelayPrefix(plan)
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

func tclaudeLayerStackedCommandWithRouteHelper(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	helper TclaudeLayerRouteHelper,
	manifestPath, manifestSHA256, readyPath string,
	consume bool,
	harnessCommand string,
) (string, error) {
	if plan.NetworkPosture == sandboxpolicy.NetworkFiltered {
		return "", fmt.Errorf("stacked filtered-network launches are not enabled in M2b")
	}
	helper.CredentialFD = tclaudeLayerRouteHelperGuestFD(plan)
	wrapper, err := wrapTclaudeLayerRouteHelper(binary, helper, harnessCommand)
	if err != nil {
		return "", err
	}
	command, err := bwrapCommand(binary, phase0WriteDirs, privateWriteDirs,
		finalHideDirs, readOnlyBinds, socketPaths, plan, wrapper)
	if err != nil {
		return "", err
	}
	command = tclaudeLayerPreserveRouteHelperFD(command)
	relay := tclaudeLayerRelayPrefix() + " --preserve-fds 1"
	relay += " --stacked-binding " + clcommon.ShellQuoteArg(manifestPath)
	relay += " --stacked-binding-sha256 " + clcommon.ShellQuoteArg(manifestSHA256)
	if consume {
		relay += " --stacked-consume"
	}
	if readyPath != "" {
		relay += " --stacked-ready " + clcommon.ShellQuoteArg(readyPath)
	}
	return tclaudeLayerRouteHelperBootstrapPrefix(helper) + relay + " -- " + command, nil
}

func tclaudeLayerRouteHelperGuestFD(plan sandboxpolicy.MountPlan) int {
	engineDescriptors, _ := tclaudeLayerRelayEngineDescriptors(plan)
	return tclaudeLayerRelayStatusFD + 1 + engineDescriptors
}

func tclaudeLayerRouteAuthorityRelayFlags(plan sandboxpolicy.MountPlan, helper TclaudeLayerRouteHelper) string {
	if !tclaudeLayerPlanDeploysProxy(plan) || helper.ProxyOnly {
		return ""
	}
	flags := " --route-helper-socket " + clcommon.ShellQuoteArg(helper.SocketPath) +
		" --route-helper-agent-id " + clcommon.ShellQuoteArg(helper.AgentID) +
		" --route-helper-conv-id " + clcommon.ShellQuoteArg(helper.ConvID) +
		" --route-helper-launch-generation " + clcommon.ShellQuoteArg(helper.LaunchGeneration)
	for _, groupID := range helper.GroupIDs {
		flags += " --route-helper-group-id " + strconv.FormatInt(groupID, 10)
	}
	return flags
}

func tclaudeLayerPreserveRouteHelperFD(command string) string {
	prefix := sandboxExecShellPrefix()
	return strings.Replace(command, prefix, " --preserve-fds 1"+prefix, 1)
}

func tclaudeLayerRouteHelperBootstrapPrefix(helper TclaudeLayerRouteHelper) string {
	cli := tclaudeLayerRouteHelperCLI
	if strings.TrimSpace(helper.BinaryPath) != "" {
		cli = helper.BinaryPath
	}
	return clcommon.ShellQuoteArg(cli) + " session " +
		tclaudeLayerRouteHelperBootstrapCommand + " --handoff-socket " +
		clcommon.ShellQuoteArg(helper.HandoffSocketPath) + " -- "
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
	return tclaudeLayerServerCommandWithLoopbackBind(
		binary, phase0WriteDirs, privateWriteDirs, finalHideDirs,
		readOnlyBinds, socketPaths, plan, 0, serverCommand)
}

func tclaudeLayerServerCommandWithLoopbackBind(
	binary string,
	phase0WriteDirs []string,
	privateWriteDirs []TclaudeLayerPrivateWriteDir,
	finalHideDirs []string,
	readOnlyBinds []TclaudeLayerReadOnlyBind,
	socketPaths []string,
	plan sandboxpolicy.MountPlan,
	loopbackBindPort int,
	serverCommand string,
) (string, error) {
	if loopbackBindPort != 0 {
		return "", fmt.Errorf("loopback server bind exceptions are Darwin-only")
	}
	command, err := bwrapCommand(
		binary,
		phase0WriteDirs,
		privateWriteDirs,
		finalHideDirs,
		readOnlyBinds,
		socketPaths,
		plan,
		serverCommand,
	)
	if err != nil {
		return "", err
	}
	engine, err := tclaudeLayerEnginePrefix(plan)
	if err != nil {
		return "", err
	}
	if engine == "" {
		return command, nil
	}
	return tclaudeLayerRelayPrefix() + engine + " -- " + command, nil
}

func tclaudeLayerUnixRelayServerCommandArgs(
	spec TclaudeLayerLaunchSpec,
	bwrapArgv []string,
) ([]string, error) {
	_, _, _, _, _, plan, err := tclaudeLayerSpecRenderInput(spec)
	if err != nil {
		return nil, err
	}
	if plan.NetworkPosture != sandboxpolicy.NetworkFiltered {
		return bwrapArgv, nil
	}
	// WHICH FLAG carries the payload is how the supervisor learns which engine
	// it runs — never a mode byte on a shared one — so a supervisor started for
	// one engine cannot be handed the other's policy by a parsing accident.
	// Each policy is also encoded by its own engine's encoder, which validates
	// through that engine's acceptance test before a namespace exists.
	policyFlag := "--filtered-network-policy"
	encode := encodeFilteredNetworkRelayPolicy
	if tclaudeLayerPlanDeploysProxy(plan) {
		policyFlag = "--proxy-network-policy"
		encode = encodeProxyNetworkRelayPolicy
	}
	encoded, err := encode(plan)
	if err != nil {
		return nil, err
	}
	argv := []string{
		// The LAUNCHER's fd space, not the sandbox's, and it is the same for
		// both engines: this process was exec'd by the OpenCode launcher with
		// the bound listener at fd 3 and the tclaude executable at fd 4. What
		// differs per engine is only what the descriptors become on the far
		// side of bubblewrap — see TclaudeLayerUnixRelayServerFDs.
		"/proc/self/fd/4",
		"session", tclaudeLayerWinchRelayCommand,
		"--preserve-fds", "2",
		policyFlag, encoded,
		"--",
	}
	return append(argv, bwrapArgv...), nil
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

func tclaudeLayerLaunchOSSandbox(
	posture sandboxpolicy.NetworkPosture,
	root sandboxpolicy.RootPosture,
) harness.LaunchOSSandbox {
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
		if root == sandboxpolicy.RootConstructed {
			// TCL-798. The ambient-socket caveat that keeps the plain host-open
			// row unverified does not apply: ambient FILESYSTEM sockets are gone
			// here. What remains is narrower and is named rather than implied,
			// because the shared network namespace is exactly what makes it
			// possible.
			return harness.LaunchOSSandbox{
				State: "on",
				Source: "tclaude-layer (bubblewrap; host network; isolated PIDs; " +
					"constructed root; agentd socket allowlisted; " +
					"abstract-namespace Unix sockets remain reachable through the shared network namespace)",
			}
		}
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
