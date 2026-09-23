package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
	"golang.org/x/term"
)

// runParams describes a fresh, foreground invocation. No conversation or tmux
// session is created by tclaude; the harness owns any history it writes.
type runParams struct {
	Harness        string `long:"harness" optional:"true" help:"Harness: claude, codex, opencode, copilot, or shell (default: claude)"`
	Workdir        string `long:"workdir" optional:"true" help:"Directory in which to run the command (default: current directory)"`
	Sandbox        string `long:"sandbox" optional:"true" help:"Harness-native sandbox mode (see session new --help)"`
	SandboxImpl    string `long:"sandbox-impl" optional:"true" help:"Sandbox implementation: harness-builtin (default) or tclaude-layer"`
	SandboxProfile string `long:"sandbox-profile" optional:"true" help:"Named tclaude sandbox profile; requires --sandbox-impl tclaude-layer"`
	Timeout        string `long:"timeout" optional:"true" help:"Maximum run duration, as a Go duration (for example 10m); unset means no timeout"`
}

const maxRunPipedPromptBytes = 96 << 10

func runCmd() *cobra.Command {
	c := boa.CmdT[runParams]{
		Use:         "run [prompt or shell command]",
		Short:       "Run one non-interactive agent turn and print its output",
		Long:        "Run a fresh, non-interactive harness turn and wait for it to finish. With --harness shell, the argument is a shell command. The child exit status is returned to the caller; a timeout exits with status 124.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *runParams, cmd *cobra.Command, args []string) {
			var input io.Reader
			if !term.IsTerminal(int(os.Stdin.Fd())) {
				input = os.Stdin
			}
			code, err := runOnceInput(*p, args, input, os.Stdout, os.Stderr)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
			}
			if code != 0 {
				os.Exit(code)
			}
		},
	}.ToCobra()
	c.Args = cobra.ArbitraryArgs
	return c
}

func runOnce(p runParams, args []string, stdout, stderr io.Writer) (int, error) {
	return runOnceInput(p, args, nil, stdout, stderr)
}

func runOnceInput(p runParams, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	prompt := strings.Join(args, " ")
	var duration time.Duration
	if p.Timeout != "" {
		parsed, err := time.ParseDuration(p.Timeout)
		if err != nil || parsed <= 0 {
			return 1, fmt.Errorf("--timeout must be a positive duration")
		}
		duration = parsed
	}
	ctx := context.Background()
	cancel := func() {}
	if duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, duration)
	}
	defer cancel()
	cwd := p.Workdir
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return 1, err
		}
	}
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return 1, fmt.Errorf("resolve workdir: %w", err)
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return 1, fmt.Errorf("workdir: %w", err)
	}
	if !info.IsDir() {
		return 1, fmt.Errorf("workdir %q is not a directory", cwd)
	}

	name := p.Harness
	if name == "" {
		name = harness.DefaultName
	}
	h, err := harness.Resolve(name)
	if err != nil {
		return 1, err
	}
	if name != harness.ShellName && !h.SupportsAsk() {
		return 1, fmt.Errorf("harness %q has no non-interactive runner", name)
	}
	if name != harness.ShellName && stdin != nil {
		payload, readErr := readRunPipedPrompt(ctx, stdin)
		if readErr != nil {
			if errors.Is(readErr, context.DeadlineExceeded) {
				return 124, fmt.Errorf("run timed out after %s while reading piped input", duration)
			}
			return 1, fmt.Errorf("read piped input: %w", readErr)
		}
		data := strings.TrimRight(string(payload), "\n")
		if strings.TrimSpace(prompt) == "" {
			prompt = data
		} else if data != "" {
			prompt += "\n\n--- piped input (stdin) ---\n" + data
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return 1, errors.New("a prompt or shell command is required")
	}
	if p.SandboxImpl != "" && p.SandboxImpl != "harness-builtin" && p.SandboxImpl != "tclaude-layer" {
		return 1, fmt.Errorf("invalid --sandbox-impl %q (want harness-builtin or tclaude-layer)", p.SandboxImpl)
	}
	if p.SandboxProfile != "" && p.SandboxImpl != "tclaude-layer" {
		return 1, errors.New("--sandbox-profile requires --sandbox-impl tclaude-layer")
	}
	if p.Sandbox != "" && p.SandboxImpl == "tclaude-layer" {
		return 1, errors.New("--sandbox and --sandbox-impl tclaude-layer cannot be combined")
	}
	if p.Sandbox != "" && name != harness.DefaultName && name != harness.CodexName {
		return 1, fmt.Errorf("--sandbox is not supported for non-interactive %s runs", name)
	}
	mode, err := harness.ValidateHarnessBuiltinMode(h, p.Sandbox)
	if err != nil {
		return 1, err
	}
	var argv []string
	if name == harness.ShellName {
		shell := strings.TrimSpace(os.Getenv("SHELL"))
		if shell == "" {
			shell = "/bin/sh"
		}
		argv = []string{shell, "-c", prompt}
	} else {
		spec := harness.AskSpec{Print: true, Prompt: prompt}
		if p.SandboxImpl == "tclaude-layer" {
			// The outer layer is the selected OS wall. Askers for the two
			// supported native sandboxes can turn their inner wall off.
			if name == harness.DefaultName || name == harness.CodexName {
				spec.LaunchPosture = &harness.SpawnSpec{HarnessBuiltinMode: h.TclaudeLayerMode}
			}
		} else if mode != "" {
			spec.LaunchPosture = &harness.SpawnSpec{HarnessBuiltinMode: mode}
		}
		argv = h.Ask.BuildAskArgv(spec)
	}
	if len(argv) == 0 {
		return 1, errors.New("harness produced an empty command")
	}
	if p.SandboxImpl == "tclaude-layer" {
		argv, err = wrapRunWithLayer(h, cwd, p.SandboxProfile, argv)
		if err != nil {
			return 1, err
		}
	}

	ctx, stopSignals := context.WithCancel(ctx)
	defer stopSignals()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	signalDone := make(chan struct{})
	defer close(signalDone)
	var receivedSignal atomic.Int32
	go func() {
		select {
		case sig := <-signals:
			receivedSignal.Store(int32(sig.(syscall.Signal)))
			stopSignals()
		case <-signalDone:
		}
	}()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = cwd
	// Force a pipe even when the caller's stdout is a terminal. OpenCode's
	// print adapter writes its clean answer to stdout only in this mode.
	command.Stdout = writerOnly{stdout}
	if name == harness.ShellName {
		command.Stdin = stdin
	}
	configureRunProcess(command)
	command.WaitDelay = 2 * time.Second
	var captured bytes.Buffer
	if h.Ask != nil && h.Ask.NoisyCaptureStderr() {
		command.Stderr = &captured
	} else {
		command.Stderr = stderr
	}
	if names := h.AskEnvScrub(); len(names) > 0 {
		command.Env = scrubRunEnv(os.Environ(), names)
	}
	err = command.Run()
	if err != nil && captured.Len() > 0 {
		_, _ = io.Copy(stderr, &captured)
	}
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return 124, fmt.Errorf("run timed out after %s", duration)
	}
	if sig := receivedSignal.Load(); err != nil && sig != 0 {
		return 128 + int(sig), fmt.Errorf("run interrupted by %s", syscall.Signal(sig))
	}
	if err == nil {
		return 0, nil
	}
	if errors.Is(err, exec.ErrWaitDelay) && command.ProcessState != nil && command.ProcessState.Success() {
		// The direct child completed successfully, but a descendant kept an
		// output pipe open. End the remaining process group before returning.
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exitErr.ExitCode(), nil
	}
	return 1, err
}

func readRunPipedPrompt(ctx context.Context, stdin io.Reader) ([]byte, error) {
	type result struct {
		payload []byte
		err     error
	}
	read := func() result {
		payload, err := io.ReadAll(io.LimitReader(stdin, maxRunPipedPromptBytes+1))
		if err == nil && len(payload) > maxRunPipedPromptBytes {
			err = fmt.Errorf("piped prompt exceeds %d KiB; pass a file path instead", maxRunPipedPromptBytes>>10)
		}
		return result{payload: payload, err: err}
	}
	if ctx.Done() == nil {
		got := read()
		return got.payload, got.err
	}
	done := make(chan result, 1)
	go func() { done <- read() }()
	select {
	case got := <-done:
		return got.payload, got.err
	case <-ctx.Done():
		if closer, ok := stdin.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, ctx.Err()
	}
}

type writerOnly struct{ io.Writer }

func scrubRunEnv(env, names []string) []string {
	drop := make(map[string]bool, len(names))
	for _, name := range names {
		drop[name] = true
	}
	kept := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !drop[key] {
			kept = append(kept, entry)
		}
	}
	return kept
}

func wrapRunWithLayer(h *harness.Harness, cwd, profile string, argv []string) ([]string, error) {
	if h.Name != harness.DefaultName && h.Name != harness.CodexName && h.Name != harness.ShellName {
		return nil, fmt.Errorf("tclaude-layer is not supported for non-interactive %s runs", h.Name)
	}
	if err := session.ValidateTclaudeLayerHarness(h.Name); err != nil {
		return nil, err
	}
	resolved, err := db.ResolveEffectiveSandboxSnapshot(0, profile)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox profile: %w", err)
	}
	snapshot := &resolved
	if err := session.ValidateTclaudeLayerHarnessPosture(
		h, sandboxpolicy.EnvironmentForLaunch(snapshot), nil); err != nil {
		return nil, err
	}
	posture, err := session.TclaudeLayerNetworkPosture(snapshot.Effective)
	if err != nil {
		return nil, err
	}
	root, err := session.TclaudeLayerLaunchRootPosture(h, sandboxpolicy.ImplementationTclaudeLayer, posture, snapshot.Effective)
	if err != nil {
		return nil, err
	}
	engine, err := session.TclaudeLayerNetworkEngine(snapshot.Effective)
	if err != nil {
		return nil, err
	}
	axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(snapshot.Effective)
	if err != nil {
		return nil, err
	}
	if axes.UnixSockets.Mode != sandboxpolicy.AccessModeUnset && axes.UnixSockets.Mode != sandboxpolicy.AccessModeOpen {
		return nil, errors.New("non-interactive tclaude-layer does not support unix socket profile rules")
	}
	if posture == sandboxpolicy.NetworkFiltered && runtime.GOOS == "linux" && engine == sandboxpolicy.NetworkEnginePacket {
		probe := session.ProbeFilteredNetworkPrerequisite()
		if err := session.ValidateFilteredNetworkHarnessSupport(h, sandboxpolicy.ImplementationTclaudeLayer, axes, probe); err != nil {
			return nil, err
		}
		if !probe.Detected {
			return nil, errors.New("filtered network prerequisites are unavailable")
		}
	}
	var model harness.ResolvedModelTransport
	if posture == sandboxpolicy.NetworkFiltered && !sandboxpolicy.NetworkRulesArePrivateRoutedOpen(axes.Network) {
		model, err = session.ResolveTclaudeLayerModelTransport(h, session.ModelTransportLaunchContext{
			Cwd: cwd, Environment: sandboxpolicy.EnvironmentForLaunch(snapshot),
		})
		if err != nil {
			return nil, err
		}
	}
	if _, err := session.ValidateTclaudeLayerNetwork(h, snapshot.Effective, model); err != nil {
		return nil, err
	}
	binary, _, err := session.ResolveTclaudeLayerForEngineWithIdentity(
		posture, root, engine, posture == sandboxpolicy.NetworkFiltered && engine == sandboxpolicy.NetworkEnginePacket)
	if err != nil {
		return nil, err
	}
	spec, err := session.BuildTclaudeLayerLaunchSpec(session.TclaudeLayerLaunchInput{
		HarnessName: h.Name, Cwd: cwd, Snapshot: snapshot, NetworkEngine: engine,
	})
	if err != nil {
		return nil, err
	}
	if err := session.PrepareTclaudeLayerHarnessState(spec); err != nil {
		return nil, err
	}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = clcommon.ShellQuoteArg(arg)
	}
	wrapped, err := session.WrapTclaudeLayerSpec(binary, spec, "exec "+strings.Join(quoted, " "))
	if err != nil {
		return nil, err
	}
	return []string{"/bin/sh", "-c", wrapped}, nil
}
