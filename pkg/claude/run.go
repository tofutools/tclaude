package claude

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common"
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

func runCmd() *cobra.Command {
	c := boa.CmdT[runParams]{
		Use:         "run [prompt or shell command]",
		Short:       "Run one non-interactive agent turn and print its output",
		Long:        "Run a fresh, non-interactive harness turn and wait for it to finish. With --harness shell, the argument is a shell command. The child exit status is returned to the caller; a timeout exits with status 124.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *runParams, cmd *cobra.Command, args []string) {
			code, err := runOnce(*p, args, os.Stdout, os.Stderr)
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
	prompt := strings.Join(args, " ")
	if strings.TrimSpace(prompt) == "" {
		return 1, errors.New("a prompt or shell command is required")
	}
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
	var duration time.Duration
	if p.Timeout != "" {
		duration, err = time.ParseDuration(p.Timeout)
		if err != nil || duration <= 0 {
			return 1, fmt.Errorf("--timeout must be a positive duration")
		}
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

	ctx := context.Background()
	cancel := func() {}
	if duration > 0 {
		ctx, cancel = context.WithTimeout(ctx, duration)
	}
	defer cancel()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Dir = cwd
	// Force a pipe even when the caller's stdout is a terminal. OpenCode's
	// print adapter writes its clean answer to stdout only in this mode.
	command.Stdout = writerOnly{stdout}
	command.Stdin = nil
	configureRunProcess(command)
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
	if ctx.Err() == context.DeadlineExceeded {
		return 124, fmt.Errorf("run timed out after %s", duration)
	}
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, err
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
	snapshot, err := db.ResolveEffectiveSandboxSnapshot(0, profile)
	if err != nil {
		return nil, fmt.Errorf("resolve sandbox profile: %w", err)
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
	binary, _, err := session.ResolveTclaudeLayerForEngine(posture, root, engine)
	if err != nil {
		return nil, err
	}
	spec, err := session.BuildTclaudeLayerLaunchSpec(session.TclaudeLayerLaunchInput{
		HarnessName: h.Name, Cwd: cwd, Snapshot: &snapshot, NetworkEngine: engine,
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
