//go:build linux

package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/probehelper"
	"golang.org/x/sys/unix"
)

const tclaudeLayerWinchRelayCommand = "tclaude-layer-winch-relay"

type bwrapChildStatus struct {
	ChildPID int `json:"child-pid"`
}

type stackedRelayBindingOptions struct {
	ManifestPath   string
	ManifestSHA256 string
	Consume        bool
	ReadyPath      string
	FilteredPolicy string
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

func routeAuthorityMetadataPresent(binding stackedRelayBindingOptions) bool {
	return strings.TrimSpace(binding.RouteSocketPath) != "" ||
		strings.TrimSpace(binding.RouteAgentID) != "" ||
		strings.TrimSpace(binding.RouteConvID) != "" ||
		strings.TrimSpace(binding.RouteLaunchGeneration) != "" ||
		len(binding.RouteGroupIDs) > 0
}

func validateRouteAuthorityMetadata(binding stackedRelayBindingOptions, proxyActive bool) error {
	if !routeAuthorityMetadataPresent(binding) {
		return nil
	}
	if !proxyActive || binding.PreserveFDs != 1 {
		return fmt.Errorf("route authority metadata requires the filtering proxy and one preserved capability descriptor")
	}
	if strings.TrimSpace(binding.RouteSocketPath) == "" ||
		strings.TrimSpace(binding.RouteAgentID) == "" ||
		strings.TrimSpace(binding.RouteConvID) == "" ||
		strings.TrimSpace(binding.RouteLaunchGeneration) == "" || len(binding.RouteGroupIDs) == 0 {
		return fmt.Errorf("route authority metadata is incomplete")
	}
	return nil
}

func tclaudeLayerProjectsConstructedRootCLI(argv []string) bool {
	for index := 0; index+2 < len(argv); index++ {
		if argv[index] == "--ro-bind" &&
			argv[index+2] == tclaudeLayerConstructedRootTclaudePath {
			return true
		}
	}
	return false
}

func prepareTclaudeLayerShellEnv() (*os.File, error) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create constructed-root shell environment pipe: %w", err)
	}
	if _, err := io.WriteString(writeEnd, tclaudeLayerConstructedRootShellEnv); err != nil {
		_ = readEnd.Close()
		_ = writeEnd.Close()
		return nil, fmt.Errorf("stage constructed-root shell environment: %w", err)
	}
	if err := writeEnd.Close(); err != nil {
		_ = readEnd.Close()
		return nil, fmt.Errorf("finish constructed-root shell environment pipe: %w", err)
	}
	return readEnd, nil
}

func tclaudeLayerShellEnvSetupArgs(fd int) []string {
	return []string{
		"--perms", "0444",
		"--file", strconv.Itoa(fd), tclaudeLayerConstructedRootShellEnvPath,
		"--setenv", "BASH_ENV", tclaudeLayerConstructedRootShellEnvPath,
		"--setenv", "ENV", tclaudeLayerConstructedRootShellEnvPath,
	}
}

func insertTclaudeLayerShellEnvArgs(argv, shellEnvArgs []string) ([]string, error) {
	for index := 0; index+2 < len(argv); index++ {
		if argv[index] != "--ro-bind" ||
			argv[index+2] != tclaudeLayerConstructedRootTclaudePath {
			continue
		}
		// The fixed CLI bind follows the --dir operations that materialize
		// /.tclaude. Create the fragment immediately after it, before policy
		// replay remounts the constructed root read-only.
		out := make([]string, 0, len(argv)+len(shellEnvArgs))
		out = append(out, argv[:index+3]...)
		out = append(out, shellEnvArgs...)
		out = append(out, argv[index+3:]...)
		return out, nil
	}
	return nil, fmt.Errorf("constructed-root shell environment has no CLI projection")
}

// tclaudeLayerWinchRelayCmd stays outside bubblewrap and outside the terminal
// I/O path. Bubblewrap inherits stdin/stdout/stderr directly; this process only
// turns the host PTY's SIGWINCH notification into the same fixed signal for the
// disconnected sandbox process group.
func tclaudeLayerWinchRelayCmd() *cobra.Command {
	var binding stackedRelayBindingOptions
	cmd := &cobra.Command{
		Use:    tclaudeLayerWinchRelayCommand + " -- <bwrap> [args...]",
		Short:  "Relay terminal resize notifications into tclaude-layer (internal)",
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
	cmd.Flags().StringVar(
		&binding.ManifestPath,
		"stacked-binding",
		"",
		"launch-owned stacked binding manifest (internal)",
	)
	cmd.Flags().StringVar(
		&binding.ManifestSHA256,
		"stacked-binding-sha256",
		"",
		"probe-pinned stacked binding manifest digest (internal)",
	)
	cmd.Flags().BoolVar(
		&binding.Consume,
		"stacked-consume",
		false,
		"unlink stacked staging names after opening them (internal)",
	)
	cmd.Flags().StringVar(
		&binding.ReadyPath,
		"stacked-ready",
		"",
		"write after the final binding fds reach bubblewrap (internal)",
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
	binding stackedRelayBindingOptions,
) (int, error) {
	if len(argv) == 0 || argv[0] == "" {
		return 125, fmt.Errorf("missing bubblewrap command")
	}
	if strings.TrimSpace(binding.FilteredPolicy) != "" &&
		strings.TrimSpace(binding.ManifestPath) != "" {
		return 125, fmt.Errorf("stacked and filtered relay bindings cannot be combined")
	}
	if strings.TrimSpace(binding.ProxyPolicy) != "" {
		// One launch runs one engine. Refusing the combinations here means no
		// later code has to decide which policy is authoritative, which is the
		// shape of question that produces a bypass.
		if strings.TrimSpace(binding.FilteredPolicy) != "" {
			return 125, fmt.Errorf(
				"packet and proxy filtering engines cannot be combined")
		}
		if strings.TrimSpace(binding.ManifestPath) != "" {
			return 125, fmt.Errorf(
				"stacked and proxy relay bindings cannot be combined")
		}
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
	bindingArgs, bindingFiles, err := prepareStackedRelayBinding(binding)
	if err != nil {
		return 125, err
	}
	defer func() {
		for _, file := range bindingFiles {
			_ = file.Close()
		}
	}()
	filtered, err := prepareFilteredNetworkRelay(binding.FilteredPolicy)
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
	if proxy.Active() && routeAuthorityMetadataPresent(binding) {
		credential, credentialErr := readRouteHelperCredentialFD(3)
		if credentialErr != nil {
			return 125, credentialErr
		}
		proxy.RouteAuthority = newProxyRouteAuthority(proxyRouteAuthorityConfig{
			SocketPath: binding.RouteSocketPath, AgentID: binding.RouteAgentID,
			ConvID: binding.RouteConvID, LaunchGeneration: binding.RouteLaunchGeneration,
			GroupIDs: append([]int64(nil), binding.RouteGroupIDs...),
		}, credential)
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
	var shellEnvFile *os.File
	var shellEnvArgs []string
	if tclaudeLayerProjectsConstructedRootCLI(argv) {
		shellEnvFile, err = prepareTclaudeLayerShellEnv()
		if err != nil {
			return 125, err
		}
		defer func() { _ = shellEnvFile.Close() }()
		// fd 3 is the status pipe. Stacked, engine, and preserved descriptors
		// retain their existing numbers; this launch-only fragment follows them.
		shellEnvFD := tclaudeLayerRelayStatusFD + 1 + len(bindingFiles) +
			len(engineFiles) + len(preservedFiles)
		shellEnvArgs = tclaudeLayerShellEnvSetupArgs(shellEnvFD)
	}
	original := argv[1:]
	if len(shellEnvArgs) != 0 {
		original, err = insertTclaudeLayerShellEnvArgs(original, shellEnvArgs)
		if err != nil {
			return 125, err
		}
	}
	relaySetupArgs := make([]string, 0, len(bindingArgs)+len(engineSetupArgs))
	relaySetupArgs = append(relaySetupArgs, bindingArgs...)
	relaySetupArgs = append(relaySetupArgs, engineSetupArgs...)
	childArgs := make([]string, 0, len(original)+len(relaySetupArgs)+10)
	childArgs = append(childArgs, "--json-status-fd", "3")
	if len(relaySetupArgs) == 0 {
		childArgs = append(childArgs, original...)
	} else {
		commandIndex := -1
		for index, arg := range original {
			if arg == "--" {
				commandIndex = index
				break
			}
		}
		if commandIndex < 0 {
			return 125, fmt.Errorf("relay binding requires a bubblewrap command separator")
		}
		childArgs = append(childArgs, original[:commandIndex]...)
		childArgs = append(childArgs, relaySetupArgs...)
		childArgs = append(childArgs, "--")
		childArgs = append(childArgs, engineCommand...)
		childArgs = append(childArgs, original[commandIndex+1:]...)
	}
	child := exec.Command(argv[0], childArgs...)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	// ExtraFiles are installed consecutively from fd 3. Bubblewrap owns fd 3
	// for its status stream; the verified binding descriptors follow it.
	// Every --file/--ro-bind-data operation names its descriptor explicitly,
	// so bubblewrap preserves them during startup even on the minimum
	// supported 0.9.x series.
	child.ExtraFiles = append([]*os.File{statusW}, bindingFiles...)
	child.ExtraFiles = append(child.ExtraFiles, engineFiles...)
	child.ExtraFiles = append(child.ExtraFiles, preservedFiles...)
	if shellEnvFile != nil {
		child.ExtraFiles = append(child.ExtraFiles, shellEnvFile)
	}
	if err := child.Start(); err != nil {
		_ = statusW.Close()
		return 125, tclaudeLayerStartRefusal(argv[0], err)
	}
	_ = statusW.Close()
	if shellEnvFile != nil {
		_ = shellEnvFile.Close()
	}
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
	if binding.ReadyPath != "" {
		if err := writeStackedBindingReady(binding.ReadyPath); err != nil {
			return 125, err
		}
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

func prepareStackedRelayBinding(
	options stackedRelayBindingOptions,
) ([]string, []*os.File, error) {
	if strings.TrimSpace(options.ManifestPath) == "" {
		if strings.TrimSpace(options.ManifestSHA256) != "" ||
			options.Consume || strings.TrimSpace(options.ReadyPath) != "" {
			return nil, nil, fmt.Errorf("stacked binding flags require a manifest")
		}
		return nil, nil, nil
	}
	expectedDigest := strings.ToLower(strings.TrimSpace(options.ManifestSHA256))
	if decoded, decodeErr := hex.DecodeString(expectedDigest); decodeErr != nil ||
		len(decoded) != sha256.Size {
		return nil, nil, fmt.Errorf("stacked binding manifest digest is invalid")
	}
	manifestPath, err := filepath.Abs(options.ManifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("resolve stacked binding manifest: %w", err)
	}
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read stacked binding manifest: %w", err)
	}
	if stackedBindingDigest(data) != expectedDigest {
		return nil, nil, fmt.Errorf("stacked binding manifest changed after capability probe")
	}
	var manifest stackedSandboxBindingManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, nil, fmt.Errorf("parse stacked binding manifest: %w", err)
	}
	root := filepath.Clean(manifest.StageRoot)
	if manifest.Version != stackedSandboxBindingVersion ||
		!filepath.IsAbs(root) ||
		filepath.Dir(manifestPath) != root {
		return nil, nil, fmt.Errorf("stacked binding manifest has invalid authority")
	}
	if readyPath := filepath.Clean(strings.TrimSpace(options.ReadyPath)); readyPath != "." {
		if !filepath.IsAbs(readyPath) {
			return nil, nil, fmt.Errorf("stacked binding readiness path is not absolute")
		}
		relative, relErr := filepath.Rel(root, readyPath)
		if relErr != nil {
			return nil, nil, fmt.Errorf("resolve stacked binding readiness path: %w", relErr)
		}
		if relative == "." || (relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
			return nil, nil, fmt.Errorf(
				"stacked binding readiness path must be outside the staging root")
		}
	}

	files := make(
		[]*os.File,
		0,
		2+len(manifest.RuntimeFiles)+len(manifest.ManagedPolicy),
	)
	closeFiles := func() {
		for _, file := range files {
			_ = file.Close()
		}
	}
	openBinding := func(binding stackedSandboxBindingFile) (*os.File, error) {
		path := filepath.Clean(binding.StagePath)
		relative, relativeErr := filepath.Rel(root, path)
		if relativeErr != nil || relative == "." || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("stacked binding path %q escapes its launch root", path)
		}
		source, openErr := os.Open(path)
		if openErr != nil {
			return nil, openErr
		}
		info, statErr := source.Stat()
		if statErr != nil {
			_ = source.Close()
			return nil, statErr
		}
		if !info.Mode().IsRegular() || info.Size() != binding.Size {
			_ = source.Close()
			return nil, fmt.Errorf("stacked binding file %q changed shape", path)
		}
		memfd, memfdErr := unix.MemfdCreate(
			"tclaude-stacked-binding",
			unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING,
		)
		if memfdErr != nil {
			_ = source.Close()
			return nil, fmt.Errorf("create immutable stacked binding: %w", memfdErr)
		}
		file := os.NewFile(uintptr(memfd), "tclaude-stacked-binding")
		mode := os.FileMode(binding.Mode)
		if mode != 0o400 && mode != 0o500 {
			_ = source.Close()
			_ = file.Close()
			return nil, fmt.Errorf(
				"stacked binding file %q has invalid mode %04o",
				path,
				binding.Mode,
			)
		}
		if chmodErr := file.Chmod(mode); chmodErr != nil {
			_ = source.Close()
			_ = file.Close()
			return nil, fmt.Errorf("set immutable stacked binding mode: %w", chmodErr)
		}
		hash := sha256.New()
		written, copyErr := io.Copy(io.MultiWriter(file, hash), source)
		closeSourceErr := source.Close()
		if copyErr != nil {
			_ = file.Close()
			return nil, copyErr
		}
		if closeSourceErr != nil {
			_ = file.Close()
			return nil, closeSourceErr
		}
		if written != binding.Size ||
			!strings.EqualFold(
				fmt.Sprintf("%x", hash.Sum(nil)),
				strings.TrimSpace(binding.SHA256),
			) {
			_ = file.Close()
			return nil, fmt.Errorf(
				"stacked binding file %q changed after capability probe",
				path,
			)
		}
		const immutableSeals = unix.F_SEAL_SEAL |
			unix.F_SEAL_SHRINK |
			unix.F_SEAL_GROW |
			unix.F_SEAL_WRITE
		if _, sealErr := unix.FcntlInt(file.Fd(), unix.F_ADD_SEALS, immutableSeals); sealErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("seal immutable stacked binding %q: %w", path, sealErr)
		}
		if _, seekErr := file.Seek(0, io.SeekStart); seekErr != nil {
			_ = file.Close()
			return nil, seekErr
		}
		files = append(files, file)
		return file, nil
	}

	if manifest.Engine.Destination != stackedBoundExecutablePath &&
		manifest.Engine.Destination !=
			filepath.Join(stackedBoundCodexRuntimeRoot, "bin", "codex") {
		return nil, nil, fmt.Errorf("stacked binding manifest has invalid engine destination")
	}
	if manifest.Engine.Destination == stackedBoundExecutablePath &&
		len(manifest.RuntimeFiles) != 0 {
		return nil, nil, fmt.Errorf("non-Codex stacked binding carries runtime files")
	}
	if manifest.Engine.Destination == stackedBoundExecutablePath &&
		manifest.ProbeHelper != nil &&
		(manifest.ProbeHelper.Destination != probehelper.BoundPath ||
			manifest.ProbeHelper.Mode != 0o500) {
		return nil, nil, fmt.Errorf(
			"%s: Claude stacked binding has an invalid sealed Go probe helper",
			probehelper.BindingFailureMarker,
		)
	}
	if manifest.Engine.Destination != stackedBoundExecutablePath &&
		len(manifest.RuntimeFiles) == 0 {
		return nil, nil, fmt.Errorf("codex stacked binding omits its runtime closure")
	}
	if manifest.Engine.Destination != stackedBoundExecutablePath &&
		manifest.ProbeHelper != nil {
		return nil, nil, fmt.Errorf("codex stacked binding carries a probe helper")
	}
	_, err = openBinding(manifest.Engine)
	if err != nil {
		closeFiles()
		return nil, nil, fmt.Errorf("stacked engine binding: %w", err)
	}
	childFDFor := func() string {
		const firstBindingChildFD = 4 // fd 3 is bubblewrap's JSON status pipe.
		return strconv.Itoa(firstBindingChildFD + len(files) - 1)
	}
	imageRoot := stackedBoundExecutableRoot
	if manifest.Engine.Destination != stackedBoundExecutablePath {
		imageRoot = stackedBoundCodexRuntimeRoot
	}
	// --file copies each sealed descriptor to a linked path in this
	// launch-private filesystem. The final remount makes the complete runtime
	// image immutable before bubblewrap starts the harness.
	args := []string{"--tmpfs", imageRoot}
	destinationDirs := make(map[string]struct{})
	addDestinationDirs := func(destination string) {
		parent := filepath.Dir(destination)
		var parents []string
		for parent != imageRoot && parent != "/" && parent != "/tmp" && parent != "." {
			parents = append(parents, parent)
			parent = filepath.Dir(parent)
		}
		for index := len(parents) - 1; index >= 0; index-- {
			if _, exists := destinationDirs[parents[index]]; exists {
				continue
			}
			destinationDirs[parents[index]] = struct{}{}
			args = append(args, "--dir", parents[index])
		}
	}
	addDestinationDirs(manifest.Engine.Destination)
	args = append(args,
		"--perms", fmt.Sprintf("%04o", manifest.Engine.Mode),
		"--file", childFDFor(), manifest.Engine.Destination,
	)

	for _, binding := range manifest.RuntimeFiles {
		destination := filepath.Clean(binding.Destination)
		relative, relativeErr := filepath.Rel(stackedBoundCodexRuntimeRoot, destination)
		if relativeErr != nil || relative == "." || relative == ".." ||
			strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
			destination == manifest.Engine.Destination {
			closeFiles()
			return nil, nil, fmt.Errorf(
				"stacked Codex runtime destination %q is invalid",
				binding.Destination,
			)
		}
		_, openErr := openBinding(binding)
		if openErr != nil {
			closeFiles()
			return nil, nil, fmt.Errorf(
				"stacked Codex runtime binding %q: %w",
				destination,
				openErr,
			)
		}
		addDestinationDirs(destination)
		args = append(args,
			"--perms", fmt.Sprintf("%04o", binding.Mode),
			"--file", childFDFor(), destination,
		)
	}
	if manifest.ProbeHelper != nil {
		binding := *manifest.ProbeHelper
		_, openErr := openBinding(binding)
		if openErr != nil {
			closeFiles()
			return nil, nil, fmt.Errorf(
				"%s: stacked probe helper binding: %w",
				probehelper.BindingFailureMarker,
				openErr,
			)
		}
		addDestinationDirs(binding.Destination)
		args = append(args,
			"--perms", fmt.Sprintf("%04o", binding.Mode),
			"--file", childFDFor(), binding.Destination,
		)
	}
	args = append(args, "--remount-ro", imageRoot)

	if manifest.FreezeClaudeManagedPolicy {
		etcEntries, readEtcErr := os.ReadDir("/etc")
		if readEtcErr != nil {
			closeFiles()
			return nil, nil, fmt.Errorf("snapshot host /etc entries: %w", readEtcErr)
		}
		args = append(args, "--tmpfs", "/etc")
		for _, entry := range etcEntries {
			if entry.Name() == "claude-code" {
				continue
			}
			args = append(args,
				"--ro-bind",
				filepath.Join("/etc", entry.Name()),
				filepath.Join("/etc", entry.Name()),
			)
		}
		args = append(args, "--dir", "/etc/claude-code")
		for _, binding := range manifest.ManagedPolicy {
			destination := filepath.Clean(binding.Destination)
			if filepath.IsAbs(destination) || destination == "." ||
				destination == ".." ||
				strings.HasPrefix(destination, ".."+string(filepath.Separator)) ||
				(destination != "managed-settings.json" &&
					!strings.HasPrefix(destination, "managed-settings.d"+string(filepath.Separator))) {
				closeFiles()
				return nil, nil, fmt.Errorf(
					"stacked managed-policy destination %q is invalid",
					binding.Destination,
				)
			}
			_, openErr := openBinding(binding)
			if openErr != nil {
				closeFiles()
				return nil, nil, fmt.Errorf(
					"stacked managed-policy binding %q: %w",
					destination,
					openErr,
				)
			}
			if strings.HasPrefix(destination, "managed-settings.d"+string(filepath.Separator)) {
				args = append(args, "--dir", "/etc/claude-code/managed-settings.d")
			}
			args = append(args,
				"--perms",
				fmt.Sprintf("%04o", binding.Mode),
				"--ro-bind-data",
				childFDFor(),
				filepath.Join("/etc/claude-code", destination),
			)
		}
		args = append(args, "--remount-ro", "/etc")
	}
	if options.Consume {
		if err := os.RemoveAll(root); err != nil {
			closeFiles()
			return nil, nil, fmt.Errorf("consume stacked binding staging root: %w", err)
		}
	}
	return args, files, nil
}

func writeStackedBindingReady(path string) error {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "." || !filepath.IsAbs(path) {
		return fmt.Errorf("stacked binding readiness path is not absolute")
	}
	fd, err := unix.Open(
		path,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("write stacked binding readiness: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if _, err := file.WriteString("ready\n"); err != nil {
		_ = file.Close()
		return fmt.Errorf("write stacked binding readiness: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close stacked binding readiness: %w", err)
	}
	return nil
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
