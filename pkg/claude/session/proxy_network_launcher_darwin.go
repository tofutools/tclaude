//go:build darwin

package session

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
	"golang.org/x/sys/unix"
)

const (
	tclaudeLayerDarwinProxyLauncherCommand = "tclaude-layer-darwin-proxy-launcher"
	darwinProxyLaunchSpecEncodingLimit     = 64 << 10
)

var darwinProxyLauncherPrefix = func() string {
	return clcommon.DetectAbsoluteCmd(
		"session", tclaudeLayerDarwinProxyLauncherCommand)
}

var serveDarwinFilteringProxy = func(
	server *sandboxproxy.Server,
	listener net.Listener,
) error {
	return server.Serve(listener)
}

var (
	darwinProxyTerminalForegroundGroup = func(fd int) (int, error) {
		return unix.IoctlGetInt(fd, unix.TIOCGPGRP)
	}
	darwinProxySetTerminalForegroundGroup = func(fd, pgid int) error {
		return unix.IoctlSetPointerInt(fd, unix.TIOCSPGRP, pgid)
	}
)

// darwinProxyLaunchSpec is the input that has to cross the rendered shell
// command. Seatbelt cannot be rendered until the launcher owns the real
// ephemeral port, so the same inputs the ordinary Darwin renderer consumes are
// carried to that launch-time boundary instead.
type darwinProxyLaunchSpec struct {
	Binary           string                        `json:"binary"`
	Phase0WriteDirs  []string                      `json:"phase0_write_dirs,omitempty"`
	PrivateWriteDirs []TclaudeLayerPrivateWriteDir `json:"private_write_dirs,omitempty"`
	FinalHideDirs    []string                      `json:"final_hide_dirs,omitempty"`
	ReadOnlyBinds    []TclaudeLayerReadOnlyBind    `json:"read_only_binds,omitempty"`
	SocketPaths      []string                      `json:"socket_paths,omitempty"`
	Plan             sandboxpolicy.MountPlan       `json:"plan"`
	HarnessCommand   string                        `json:"harness_command"`
	PreserveFDs      int                           `json:"preserve_fds,omitempty"`
	LoopbackBindPort int                           `json:"loopback_bind_port,omitempty"`
}

func darwinProxyLauncherCommand(spec darwinProxyLaunchSpec) (string, error) {
	if !tclaudeLayerPlanDeploysProxy(spec.Plan) || spec.Plan.FilteredNetwork == nil {
		return "", fmt.Errorf("Darwin proxy launcher requires a compiled proxy network plan")
	}
	if _, err := sandboxproxy.NewEvaluatorFromRuleSet(*spec.Plan.FilteredNetwork); err != nil {
		return "", fmt.Errorf("validate Darwin filtering proxy policy: %w", err)
	}
	if spec.PreserveFDs != 0 && spec.PreserveFDs != 2 {
		return "", fmt.Errorf("Darwin proxy launcher can preserve only the server boundary's two descriptors")
	}
	if spec.LoopbackBindPort < 0 || spec.LoopbackBindPort > 65535 {
		return "", fmt.Errorf("Darwin proxy launcher has invalid loopback bind port %d", spec.LoopbackBindPort)
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("encode Darwin proxy launch: %w", err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(data)
	if len(encoded) > darwinProxyLaunchSpecEncodingLimit {
		return "", fmt.Errorf("Darwin proxy launch exceeds the encoding limit")
	}
	return darwinProxyLauncherPrefix() + " --launch " +
		clcommon.ShellQuoteArg(encoded), nil
}

func decodeDarwinProxyLaunchSpec(encoded string) (darwinProxyLaunchSpec, error) {
	if len(encoded) > darwinProxyLaunchSpecEncodingLimit {
		return darwinProxyLaunchSpec{}, fmt.Errorf("Darwin proxy launch exceeds the encoding limit")
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return darwinProxyLaunchSpec{}, fmt.Errorf("decode Darwin proxy launch: %w", err)
	}
	var spec darwinProxyLaunchSpec
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&spec); err != nil {
		return darwinProxyLaunchSpec{}, fmt.Errorf("parse Darwin proxy launch: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return darwinProxyLaunchSpec{}, fmt.Errorf("parse Darwin proxy launch: trailing JSON value")
		}
		return darwinProxyLaunchSpec{}, fmt.Errorf("parse Darwin proxy launch: %w", err)
	}
	if !tclaudeLayerPlanDeploysProxy(spec.Plan) || spec.Plan.FilteredNetwork == nil {
		return darwinProxyLaunchSpec{}, fmt.Errorf("Darwin proxy launcher requires a compiled proxy network plan")
	}
	if _, err := sandboxproxy.NewEvaluatorFromRuleSet(*spec.Plan.FilteredNetwork); err != nil {
		return darwinProxyLaunchSpec{}, fmt.Errorf("validate Darwin filtering proxy policy: %w", err)
	}
	if spec.PreserveFDs != 0 && spec.PreserveFDs != 2 {
		return darwinProxyLaunchSpec{}, fmt.Errorf("Darwin proxy launcher can preserve only the server boundary's two descriptors")
	}
	if spec.LoopbackBindPort < 0 || spec.LoopbackBindPort > 65535 {
		return darwinProxyLaunchSpec{}, fmt.Errorf("Darwin proxy launcher has invalid loopback bind port %d", spec.LoopbackBindPort)
	}
	return spec, nil
}

func tclaudeLayerDarwinProxyLauncherCmd() *cobra.Command {
	var encoded string
	cmd := &cobra.Command{
		Use:    tclaudeLayerDarwinProxyLauncherCommand,
		Short:  "Supervise the Darwin Seatbelt filtering proxy (internal)",
		Hidden: true,
		Args:   cobra.NoArgs,
		Run: func(*cobra.Command, []string) {
			spec, err := decodeDarwinProxyLaunchSpec(encoded)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tclaude: Darwin filtering proxy launcher: %v\n", err)
				os.Exit(125)
			}
			code, err := runDarwinProxyLauncher(spec)
			if err != nil {
				fmt.Fprintf(os.Stderr, "tclaude: Darwin filtering proxy launcher: %v\n", err)
			}
			os.Exit(code)
		},
	}
	cmd.Flags().StringVar(&encoded, "launch", "", "encoded Darwin proxy launch (internal)")
	_ = cmd.MarkFlagRequired("launch")
	return cmd
}

func runDarwinProxyLauncher(spec darwinProxyLaunchSpec) (int, error) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{
		IP: net.IPv4(127, 0, 0, 1), Port: 0,
	})
	if err != nil {
		return 125, fmt.Errorf("bind Darwin filtering proxy: %w", err)
	}
	endpoint := listener.Addr().(*net.TCPAddr).AddrPort()
	if err := validateSeatbeltProxyEndpoint(endpoint, true); err != nil {
		_ = listener.Close()
		return 125, err
	}

	server, err := sandboxproxy.NewFromRuleSet(
		*spec.Plan.FilteredNetwork,
		sandboxproxy.Config{
			OnDecision: logProxyNetworkDecision,
			OnError:    logProxyNetworkError,
		},
	)
	if err != nil {
		_ = listener.Close()
		return 125, fmt.Errorf("build Darwin filtering proxy: %w", err)
	}
	defer func() { _ = server.Close() }()

	seatbeltCommand, err := renderDarwinSeatbeltCommand(
		spec.Binary,
		spec.Phase0WriteDirs,
		spec.PrivateWriteDirs,
		spec.FinalHideDirs,
		spec.ReadOnlyBinds,
		spec.SocketPaths,
		spec.Plan,
		spec.HarnessCommand,
		endpoint,
		spec.LoopbackBindPort,
	)
	if err != nil {
		_ = listener.Close()
		return 125, err
	}

	proxyDone := make(chan error, 1)
	go func() { proxyDone <- serveDarwinFilteringProxy(server, listener) }()
	select {
	case proxyErr := <-proxyDone:
		if proxyErr == nil {
			proxyErr = fmt.Errorf("filtering proxy exited during launch")
		}
		return 125, fmt.Errorf("start Darwin filtering proxy: %w", proxyErr)
	default:
	}

	child := exec.Command("/bin/sh", "-c", seatbeltCommand)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	child.Env = proxyNetworkSandboxEnv(os.Environ(), int(endpoint.Port()))
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	for fd := 3; fd < 3+spec.PreserveFDs; fd++ {
		if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); err != nil {
			return 125, fmt.Errorf("preserve inherited fd %d: %w", fd, err)
		}
		child.ExtraFiles = append(child.ExtraFiles,
			os.NewFile(uintptr(fd), fmt.Sprintf("tclaude-preserved-fd-%d", fd)))
	}
	if err := child.Start(); err != nil {
		return 125, fmt.Errorf("start Darwin Seatbelt sandbox: %w", err)
	}

	restoreForeground, err := darwinProxyGiveTerminalTo(child.Process.Pid)
	if err != nil {
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		_ = child.Wait()
		return 125, err
	}
	defer restoreForeground()

	childDone := make(chan error, 1)
	go func() { childDone <- child.Wait() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)

	select {
	case waitErr := <-childDone:
		_ = server.Close()
		<-proxyDone
		return darwinProxyExitCode(waitErr)
	case proxyErr := <-proxyDone:
		_ = syscall.Kill(-child.Process.Pid, syscall.SIGKILL)
		<-childDone
		if proxyErr == nil {
			proxyErr = fmt.Errorf("filtering proxy exited while the sandbox was running")
		}
		return 125, fmt.Errorf("Darwin filtering proxy failed: %w", proxyErr)
	case received := <-signals:
		sig, ok := received.(syscall.Signal)
		if !ok {
			sig = syscall.SIGTERM
		}
		_ = syscall.Kill(-child.Process.Pid, sig)
		waitErr := <-childDone
		_ = server.Close()
		<-proxyDone
		return darwinProxyExitCode(waitErr)
	}
}

// The supervised sandbox gets a private process group so a proxy failure can
// tear down all ordinary descendants. For an interactive pane that group must
// also become the terminal foreground group; otherwise its first read would be
// stopped by SIGTTIN. Non-terminal server launches simply skip this handoff.
func darwinProxyGiveTerminalTo(pgid int) (func(), error) {
	old, err := darwinProxyTerminalForegroundGroup(int(os.Stdin.Fd()))
	if err != nil {
		// Darwin reports ENODEV rather than ENOTTY for some non-terminal
		// descriptors, including the CI runner's stdin. Both mean there is no
		// foreground process group to hand off; a real terminal error remains
		// fatal so an interactive child cannot be left stopped in the background.
		if errors.Is(err, syscall.ENOTTY) || errors.Is(err, syscall.ENODEV) {
			return func() {}, nil
		}
		return nil, fmt.Errorf("inspect Darwin launcher terminal foreground group: %w", err)
	}
	signal.Ignore(syscall.SIGTTOU)
	if err := darwinProxySetTerminalForegroundGroup(
		int(os.Stdin.Fd()), pgid); err != nil {
		signal.Reset(syscall.SIGTTOU)
		return nil, fmt.Errorf("give Darwin sandbox the terminal foreground: %w", err)
	}
	return func() {
		_ = darwinProxySetTerminalForegroundGroup(int(os.Stdin.Fd()), old)
		signal.Reset(syscall.SIGTTOU)
	}, nil
}

func darwinProxyExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 125, fmt.Errorf("wait for Darwin Seatbelt sandbox: %w", err)
	}
	status, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return 125, fmt.Errorf("inspect Darwin Seatbelt exit status: %w", err)
	}
	if status.Exited() {
		return status.ExitStatus(), nil
	}
	if status.Signaled() {
		return 128 + int(status.Signal()), nil
	}
	return 125, fmt.Errorf("Darwin Seatbelt sandbox exited with unsupported wait status %v", status)
}
