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
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common"
	"golang.org/x/term"
)

// runParams describes a fresh, foreground invocation. No conversation or tmux
// session is created by tclaude; the harness owns any history it writes.
type runParams struct {
	Harness string `long:"harness" optional:"true" help:"Harness: claude, codex, opencode, copilot, or shell (default: claude)"`
	Workdir string `long:"workdir" optional:"true" help:"Directory in which to run the command (default: current directory)"`
	Sandbox string `long:"sandbox" optional:"true" help:"Harness-native sandbox mode (see session new --help)"`
	Timeout string `long:"timeout" optional:"true" help:"Maximum run duration, as a Go duration (for example 10m); unset means no timeout"`
	Cgroup  bool   `long:"cgroup" optional:"true" help:"Run in a fresh cgroup created by tclaude agentd (Linux only); implied by --cpu, --memory and --pids"`
	CPU     string `long:"cpu" optional:"true" help:"CPU limit in cores for the run cgroup, for example 1.5"`
	Memory  string `long:"memory" optional:"true" help:"Memory limit for the run cgroup, for example 512MiB or 4GB"`
	PIDs    int    `long:"pids" optional:"true" help:"Maximum processes and threads in the run cgroup"`
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
	limits, err := runCgroupLimits(p)
	if err != nil {
		return 1, err
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
	cwd, err = filepath.Abs(cwd)
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
		if mode != "" {
			spec.LaunchPosture = &harness.SpawnSpec{HarnessBuiltinMode: mode}
		}
		argv = h.Ask.BuildAskArgv(spec)
	}
	if len(argv) == 0 {
		return 1, errors.New("harness produced an empty command")
	}
	if limits != nil {
		hold, err := joinRunCgroup(*limits, stderr)
		if err != nil {
			return 1, err
		}
		// Closing the connection early would let agentd reap the run while
		// it is still reporting its status.
		defer runtime.KeepAlive(hold)
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

// runCgroupLimits turns the cgroup flags into limits, or nil when no cgroup
// was asked for.
func runCgroupLimits(p runParams) (*sandboxpolicy.ResourceLimits, error) {
	if !p.Cgroup && p.CPU == "" && p.Memory == "" && p.PIDs == 0 {
		return nil, nil
	}
	if runtime.GOOS != "linux" {
		return nil, errors.New("--cgroup, --cpu, --memory and --pids are Linux only")
	}
	var limits sandboxpolicy.ResourceLimits
	limits.Memory = p.Memory
	if p.CPU != "" {
		cores, err := strconv.ParseFloat(strings.TrimSpace(p.CPU), 64)
		if err != nil {
			return nil, fmt.Errorf("--cpu must be a number of cores: %w", err)
		}
		limits.CPU = &cores
	}
	if p.PIDs < 0 {
		return nil, errors.New("--pids must be positive")
	}
	if p.PIDs > 0 {
		pids := uint64(p.PIDs)
		limits.PIDs = &pids
	}
	normalized, err := sandboxpolicy.NormalizeResourceLimits(limits)
	if err != nil {
		return nil, err
	}
	return &normalized, nil
}

// joinRunCgroup moves this process into an agentd-owned run cgroup. The
// returned hold owns the connection that keeps the cgroup alive; it is never
// released explicitly, because the process exiting closes the connection and
// agentd then removes the cgroup. A seam for tests, which have no agentd.
var joinRunCgroup = func(limits sandboxpolicy.ResourceLimits, stderr io.Writer) (*agent.RunCgroupHold, error) {
	hold, resp, err := agent.JoinRunCgroup(limits)
	if err != nil {
		return nil, err
	}
	if !sameRunLimits(resp.Limits, limits) {
		fmt.Fprintf(stderr, "tclaude run: cgroup limits clamped to the caller's own ceilings: %s\n",
			describeRunLimits(resp.Limits))
	}
	return hold, nil
}

func sameRunLimits(a, b sandboxpolicy.ResourceLimits) bool {
	sameCPU := (a.CPU == nil) == (b.CPU == nil) && (a.CPU == nil || *a.CPU == *b.CPU)
	samePIDs := (a.PIDs == nil) == (b.PIDs == nil) && (a.PIDs == nil || *a.PIDs == *b.PIDs)
	return a.MemoryBytes == b.MemoryBytes && sameCPU && samePIDs
}

func describeRunLimits(limits sandboxpolicy.ResourceLimits) string {
	parts := []string{}
	if limits.CPU != nil {
		parts = append(parts, "cpu="+strconv.FormatFloat(*limits.CPU, 'f', -1, 64))
	}
	if limits.Memory != "" {
		parts = append(parts, "memory="+limits.Memory)
	}
	if limits.PIDs != nil {
		parts = append(parts, "pids="+strconv.FormatUint(*limits.PIDs, 10))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}
