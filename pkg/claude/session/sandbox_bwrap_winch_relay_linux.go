//go:build linux

package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

const tclaudeLayerWinchRelayCommand = "tclaude-layer-winch-relay"

type bwrapChildStatus struct {
	ChildPID int `json:"child-pid"`
}

// tclaudeLayerRelayOptions carries the relay's engine, descriptor and route
// authority flags.
type tclaudeLayerRelayOptions struct {
	FilteredPolicy         string
	PreserveCallerIdentity bool
	// ProxyPolicy carries the proxy engine's compiled policy. It is a separate
	// flag from FilteredPolicy rather than a mode byte on one payload so that
	// neither engine can ever be handed the other's policy by a parsing
	// accident: a supervisor started for one engine cannot silently supervise
	// the other.
	ProxyPolicy string
	PreserveFDs int
	// Route authority metadata is descriptive launch identity only. The
	// capability arrives separately on fd 3 through the existing one-shot
	// handoff and is never represented in these fields.
	RouteSocketPath       string
	RouteAgentID          string
	RouteConvID           string
	RouteLaunchGeneration string
	RouteGroupIDs         []int64
}

func routeAuthorityMetadataPresent(binding tclaudeLayerRelayOptions) bool {
	return strings.TrimSpace(binding.RouteSocketPath) != "" ||
		strings.TrimSpace(binding.RouteAgentID) != "" ||
		strings.TrimSpace(binding.RouteConvID) != "" ||
		strings.TrimSpace(binding.RouteLaunchGeneration) != "" ||
		len(binding.RouteGroupIDs) > 0
}

func validateRouteAuthorityMetadata(binding tclaudeLayerRelayOptions, proxyActive bool) error {
	if !routeAuthorityMetadataPresent(binding) {
		return nil
	}
	if !proxyActive || binding.PreserveFDs != 1 {
		return fmt.Errorf("route authority metadata requires the filtering proxy and one preserved capability descriptor")
	}
	if strings.TrimSpace(binding.RouteSocketPath) == "" ||
		strings.TrimSpace(binding.RouteAgentID) == "" ||
		strings.TrimSpace(binding.RouteLaunchGeneration) == "" || len(binding.RouteGroupIDs) == 0 {
		return fmt.Errorf("route authority metadata is incomplete")
	}
	return nil
}

// tclaudeLayerWinchRelayCmd stays outside bubblewrap and outside the terminal
// I/O path. Bubblewrap inherits stdin/stdout/stderr directly; this process only
// turns the host PTY's SIGWINCH notification into the same fixed signal for the
// disconnected sandbox process group.
func tclaudeLayerWinchRelayCmd() *cobra.Command {
	var binding tclaudeLayerRelayOptions
	cmd := &cobra.Command{
		Use:    tclaudeLayerWinchRelayCommand + " -- <bwrap> [args...]",
		Short:  "Relay terminal resize notifications into tclaude’s sandbox (internal)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		Run: func(_ *cobra.Command, args []string) {
			winch := make(chan os.Signal, 1)
			signal.Notify(winch, syscall.SIGWINCH)
			defer signal.Stop(winch)

			code, err := runTclaudeLayerWinchRelay(args, winch, binding)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tclaude: terminal resize relay: %v\n", err)
				os.Exit(125)
			}
			os.Exit(code)
		},
	}
	cmd.Flags().BoolVar(
		&binding.PreserveCallerIdentity,
		"filtered-network-preserve-caller-identity",
		false,
		"preserve caller UID/GID in the filtered sandbox (internal)",
	)
	cmd.Flags().StringVar(
		&binding.FilteredPolicy,
		"filtered-network-policy",
		"",
		"compiled filtered-network relay policy (internal)",
	)
	cmd.Flags().StringVar(
		&binding.ProxyPolicy,
		"proxy-network-policy",
		"",
		"compiled proxy-network relay policy (internal)",
	)
	cmd.Flags().IntVar(
		&binding.PreserveFDs,
		"preserve-fds",
		0,
		"consecutive inherited descriptors starting at fd 3 (internal)",
	)
	cmd.Flags().StringVar(&binding.RouteSocketPath, "route-helper-socket", "", "route authority Unix socket (internal)")
	cmd.Flags().StringVar(&binding.RouteAgentID, "route-helper-agent-id", "", "route authority agent identity (internal)")
	cmd.Flags().StringVar(&binding.RouteConvID, "route-helper-conv-id", "", "route authority conversation identity (internal)")
	cmd.Flags().StringVar(&binding.RouteLaunchGeneration, "route-helper-launch-generation", "", "route authority launch generation (internal)")
	cmd.Flags().Int64SliceVar(&binding.RouteGroupIDs, "route-helper-group-id", nil, "route authority target group (internal)")
	return cmd
}

// runTclaudeLayerWinchRelay launches one bubblewrap argv, learns the
// host-visible identity of its initial sandbox process from bubblewrap itself,
// and forwards SIGWINCH to that process group. Signaling the group rather than
// the reported process is load-bearing because bubblewrap's --new-session
// helper is the group leader while production runs the TUI beneath
// `sh -c <harness command>`.
func runTclaudeLayerWinchRelay(
	argv []string,
	winch <-chan os.Signal,
	binding tclaudeLayerRelayOptions,
) (int, error) {
	if len(argv) == 0 || argv[0] == "" {
		return 125, fmt.Errorf("missing bubblewrap command")
	}
	if strings.TrimSpace(binding.ProxyPolicy) != "" &&
		strings.TrimSpace(binding.FilteredPolicy) != "" {
		// One launch runs one engine. Refusing the combination here means no
		// later code has to decide which policy is authoritative, which is the
		// shape of question that produces a bypass.
		return 125, fmt.Errorf(
			"packet and proxy filtering engines cannot be combined")
	}
	if binding.PreserveFDs != 0 {
		// One preserved descriptor is the route-helper credential FD. Two are
		// the OpenCode launcher's listener and relay executable. A count this
		// supervisor did not render is refused rather than preserved on faith.
		if binding.PreserveFDs != 1 && binding.PreserveFDs != 2 {
			return 125, fmt.Errorf(
				"inherited descriptor preservation requires the route-helper one-fd or OpenCode two-fd contract")
		}
		// An engine policy is required for the OpenCode pair because preservation is only ever
		// rendered WITH a supervisor: a plan that deploys no engine passes the
		// launcher's descriptors straight to bubblewrap and never reaches this
		// process at all. The route-helper one-fd contract is intentionally
		// available without an engine.
		if binding.PreserveFDs == 2 && strings.TrimSpace(binding.FilteredPolicy) == "" &&
			strings.TrimSpace(binding.ProxyPolicy) == "" {
			return 125, fmt.Errorf(
				"inherited descriptor preservation requires a filtering engine policy")
		}
	}
	filtered, err := prepareFilteredNetworkRelay(
		binding.FilteredPolicy, binding.PreserveCallerIdentity)
	if err != nil {
		return 125, err
	}
	defer filtered.Close()
	proxy, err := prepareProxyNetworkRelay(binding.ProxyPolicy)
	if err != nil {
		return 125, err
	}
	defer proxy.Close()
	if err := validateRouteAuthorityMetadata(binding, proxy.Active()); err != nil {
		return 125, err
	}
	authorityConfig := proxyConsumerAuthorityConfig(proxyRouteAuthorityConfig{
		SocketPath: binding.RouteSocketPath, AgentID: binding.RouteAgentID, ConvID: binding.RouteConvID, LaunchGeneration: binding.RouteLaunchGeneration, GroupIDs: binding.RouteGroupIDs,
	})
	if proxy.Active() && routeAuthorityMetadataPresent(binding) && authorityConfig != nil {
		credential, credentialErr := readRouteHelperCredentialFD(3)
		if credentialErr != nil {
			return 125, credentialErr
		}
		proxy.RouteAuthority = newProxyRouteAuthority(*authorityConfig, credential)
	}
	preservedFiles := make([]*os.File, 0, binding.PreserveFDs)
	for index := 0; index < binding.PreserveFDs; index++ {
		fd := 3 + index
		if index == 0 && proxy.RouteAuthority != nil {
			readEnd, writeEnd, pipeErr := os.Pipe()
			if pipeErr != nil {
				return 125, fmt.Errorf("recreate route helper credential pipe: %w", pipeErr)
			}
			credential := proxy.RouteAuthority.credentialValue()
			if _, pipeErr = io.WriteString(writeEnd, credential); pipeErr != nil {
				_ = readEnd.Close()
				_ = writeEnd.Close()
				return 125, fmt.Errorf("stage route helper credential pipe: %w", pipeErr)
			}
			_ = writeEnd.Close()
			preservedFiles = append(preservedFiles, readEnd)
			continue
		}
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			for _, file := range preservedFiles {
				_ = file.Close()
			}
			return 125, fmt.Errorf("preserve inherited fd %d: %w", fd, err)
		}
		preservedFiles = append(preservedFiles,
			os.NewFile(uintptr(fd), fmt.Sprintf("tclaude-preserved-fd-%d", fd)))
	}
	defer func() {
		for _, file := range preservedFiles {
			_ = file.Close()
		}
	}()

	statusR, statusW, err := os.Pipe()
	if err != nil {
		return 125, fmt.Errorf("create bubblewrap status pipe: %w", err)
	}
	defer func() { _ = statusR.Close() }()

	engineSetupArgs := filtered.SetupArgs
	engineCommand := filtered.Command
	engineFiles := filtered.Files
	if proxy.Active() {
		engineSetupArgs = proxy.SetupArgs
		engineCommand = proxy.Command
		engineFiles = proxy.Files
	}
	childArgs := make([]string, 0, len(argv)+len(engineSetupArgs)+8)
	childArgs = append(childArgs, "--json-status-fd", "3")
	if len(engineSetupArgs) == 0 {
		childArgs = append(childArgs, argv[1:]...)
	} else {
		commandIndex := -1
		original := argv[1:]
		for index, arg := range original {
			if arg == "--" {
				commandIndex = index
				break
			}
		}
		if commandIndex < 0 {
			return 125, fmt.Errorf("relay engine setup requires a bubblewrap command separator")
		}
		childArgs = append(childArgs, original[:commandIndex]...)
		childArgs = append(childArgs, engineSetupArgs...)
		childArgs = append(childArgs, "--")
		childArgs = append(childArgs, engineCommand...)
		childArgs = append(childArgs, original[commandIndex+1:]...)
	}
	child := exec.Command(argv[0], childArgs...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	// ExtraFiles are installed consecutively from fd 3. Bubblewrap owns fd 3
	// for its status stream; the engine's sealed descriptors follow it.
	// Every --file/--ro-bind-data operation names its descriptor explicitly,
	// so bubblewrap preserves them during startup even on the minimum
	// supported 0.9.x series.
	child.ExtraFiles = append([]*os.File{statusW}, engineFiles...)
	child.ExtraFiles = append(child.ExtraFiles, preservedFiles...)
	if err := child.Start(); err != nil {
		_ = statusW.Close()
		return 125, tclaudeLayerStartRefusal(argv[0], err)
	}
	_ = statusW.Close()
	// Bubblewrap now owns duplicates of every preserved descriptor. Drop the
	// relay's copies immediately; the shell/helper topology must have exactly
	// one live credential FD path, not a parent-side duplicate.
	for _, file := range preservedFiles {
		_ = file.Close()
	}
	if len(engineFiles) > 0 {
		// The child owns duplicates of the sealed bootstrap image and policy
		// files. The parent drops the bootstrap image specifically, because it
		// is the large one; the remaining sealed inputs are small and are
		// released with the rest of the relay. It must be the descriptor list
		// the running engine actually contributed: the other engine's list is
		// empty, and indexing it would take the supervisor down.
		_ = engineFiles[0].Close()
	}

	waited := false
	waitCh := make(chan error, 1)
	go func() { waitCh <- child.Wait() }()
	defer func() {
		if waited {
			return
		}
		_ = child.Process.Kill()
		<-waitCh
	}()

	var status bwrapChildStatus
	if err := json.NewDecoder(statusR).Decode(&status); err != nil {
		if errors.Is(err, io.EOF) {
			waitErr := <-waitCh
			waited = true
			return tclaudeLayerRelayExitCode(waitErr)
		}
		_ = child.Process.Kill()
		<-waitCh
		waited = true
		return 125, fmt.Errorf("read bubblewrap child identity: %w", err)
	}
	if status.ChildPID <= 0 {
		return 125, fmt.Errorf("bubblewrap reported invalid child pid %d", status.ChildPID)
	}
	childPidfd, err := unix.PidfdOpen(status.ChildPID, 0)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			waitErr := <-waitCh
			waited = true
			return tclaudeLayerRelayExitCode(waitErr)
		}
		return 125, fmt.Errorf("pin bubblewrap child pid %d: %w", status.ChildPID, err)
	}
	defer func() { _ = unix.Close(childPidfd) }()
	if proxy.Active() {
		// The proxy is serving on the sandbox's own listener before the gate
		// below opens, so the harness never observes a namespace whose only
		// reachable endpoint answers nothing. Any error here returns without
		// releasing the gate: the harness does not start, and the namespace it
		// would have started in has no route anywhere.
		if err := proxy.waitListenerReady(status.ChildPID); err != nil {
			return 125, err
		}
		if err := proxy.releaseHarness(); err != nil {
			return 125, err
		}
	}
	if len(filtered.SetupArgs) > 0 {
		if err := filtered.waitPolicyReady(status.ChildPID); err != nil {
			return 125, err
		}
		// Install the base nft policy from here — outside bubblewrap's AppArmor
		// confinement — before the DNS broker (which mutates the sets the base
		// ruleset declares) starts and before pasta attaches.
		if err := filtered.installBasePolicy(status.ChildPID); err != nil {
			return 125, err
		}
		if err := filtered.startDNSBroker(); err != nil {
			return 125, err
		}
	}
	pasta, pastaWait, err := filtered.startPasta(status.ChildPID)
	if err != nil {
		return 125, err
	}
	pastaWaited := false
	defer func() {
		if pasta == nil || pastaWaited {
			return
		}
		_ = pasta.Process.Kill()
		<-pastaWait
	}()
	if pasta != nil {
		if err := filtered.releaseHarness(); err != nil {
			return 125, err
		}
	}

	for {
		select {
		case _, ok := <-winch:
			if !ok {
				winch = nil
				continue
			}
			if err := signalPinnedTclaudeLayerGroup(childPidfd, status.ChildPID); err != nil &&
				!errors.Is(err, syscall.ESRCH) {
				return 125, fmt.Errorf("forward SIGWINCH to sandbox process group: %w", err)
			}
		case waitErr := <-waitCh:
			waited = true
			if pasta != nil && !pastaWaited {
				_ = pasta.Process.Kill()
				<-pastaWait
				pastaWaited = true
			}
			// Sandbox exit terminates the proxy, and Close tears down both
			// halves of every carried tunnel rather than only the listener.
			proxy.Close()
			return tclaudeLayerRelayExitCode(waitErr)
		case pastaErr := <-pastaWait:
			pastaWaited = true
			select {
			case waitErr := <-waitCh:
				waited = true
				return tclaudeLayerRelayExitCode(waitErr)
			default:
			}
			_ = unix.PidfdSendSignal(childPidfd, syscall.SIGKILL, nil, 0)
			if pastaErr == nil {
				pastaErr = errors.New("gateway exited unexpectedly")
			}
			return 125, fmt.Errorf(
				"filtered-network pasta gateway exited; sandbox terminated fail-closed: %w",
				pastaErr,
			)
		case proxyErr := <-proxy.waitCh():
			// The filtering proxy is the sandbox's only exit. Its exit is
			// therefore the same class of event as a pasta exit under the
			// packet engine, and gets the identical fail-closed teardown: the
			// sandbox is killed rather than left running against a listener
			// nothing is serving.
			select {
			case waitErr := <-waitCh:
				waited = true
				return tclaudeLayerRelayExitCode(waitErr)
			default:
			}
			_ = unix.PidfdSendSignal(childPidfd, syscall.SIGKILL, nil, 0)
			if proxyErr == nil {
				proxyErr = errors.New("filtering proxy exited unexpectedly")
			}
			return 125, fmt.Errorf(
				"filtered-network filtering proxy exited; sandbox terminated fail-closed: %w",
				proxyErr,
			)
		case dnsErr := <-filtered.DNSWait:
			select {
			case waitErr := <-waitCh:
				waited = true
				return tclaudeLayerRelayExitCode(waitErr)
			default:
			}
			_ = unix.PidfdSendSignal(childPidfd, syscall.SIGKILL, nil, 0)
			if dnsErr == nil {
				dnsErr = errors.New("DNS broker exited unexpectedly")
			}
			return 125, fmt.Errorf(
				"filtered-network DNS broker exited; sandbox terminated fail-closed: %w",
				dnsErr,
			)
		}
	}
}

// signalPinnedTclaudeLayerGroup resolves the child's current process group,
// pins its leader, then verifies membership again before addressing the group.
// Bubblewrap reports the child before --new-session finishes moving it into
// the final group, so this resolution must happen when each signal arrives.
// No user-selected PID or signal reaches this sink: the child identity came
// from bubblewrap, the pgid comes from the kernel, and SIGWINCH is fixed.
func signalPinnedTclaudeLayerGroup(childPidfd, childPID int) error {
	if err := unix.PidfdSendSignal(childPidfd, 0, nil, 0); err != nil {
		return err
	}
	pgid, err := unix.Getpgid(childPID)
	if err != nil {
		return err
	}
	if pgid <= 0 {
		return fmt.Errorf("bubblewrap child pid %d has invalid process group %d", childPID, pgid)
	}
	groupLeaderPidfd, err := unix.PidfdOpen(pgid, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(groupLeaderPidfd) }()
	if err := unix.PidfdSendSignal(groupLeaderPidfd, 0, nil, 0); err != nil {
		return err
	}
	currentPGID, err := unix.Getpgid(childPID)
	if err != nil {
		return err
	}
	if currentPGID != pgid {
		return fmt.Errorf("bubblewrap child moved from process group %d to %d", pgid, currentPGID)
	}
	return unix.Kill(-pgid, syscall.SIGWINCH)
}

func tclaudeLayerRelayExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 125, fmt.Errorf("wait for bubblewrap: %w", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return 125, fmt.Errorf("inspect bubblewrap exit status: %w", err)
	}
	switch {
	case status.Exited():
		return status.ExitStatus(), nil
	case status.Signaled():
		return 128 + int(status.Signal()), nil
	default:
		return 125, fmt.Errorf("bubblewrap exited with unsupported wait status %v", status)
	}
}
