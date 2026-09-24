package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common/executil"
)

const maxNonInteractiveOutputBytes = 4 << 20

var prepareNonInteractiveResourceCgroup = session.PrepareResourceCgroup
var configureNonInteractiveResourceCgroup = session.ConfigureProcessResourceCgroup
var validateNonInteractivePreparedResourceCgroup = session.ValidatePreparedResourceCgroup
var removeNonInteractiveResourceCgroup = session.RemoveResourceCgroup
var killNonInteractiveResourceCgroupMembers = session.KillResourceCgroupMembers
var runNonInteractiveTmuxCommand = runNonInteractiveThroughTmux

type nonInteractiveSpawnResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

// runNonInteractiveSpawn uses the ordinary spawn boundary's resolved fields,
// but does not allocate a conversation or group membership.
func runNonInteractiveSpawn(parent context.Context, p spawnParams, seconds int64) (nonInteractiveSpawnResult, *spawnFailure) {
	if p.CleanupDirWriteProof {
		defer cleanupDirWriteProofMarkers(p.DirWriteProofToken, p.DirWriteProofDirs)
	}
	bad := func(kind, message string) (nonInteractiveSpawnResult, *spawnFailure) {
		return nonInteractiveSpawnResult{}, &spawnFailure{Status: 400, Kind: kind, Msg: message}
	}
	hostFailure := func(kind, message string) (nonInteractiveSpawnResult, *spawnFailure) {
		return nonInteractiveSpawnResult{}, &spawnFailure{Status: 502, Kind: kind, Msg: message}
	}
	if seconds == 0 {
		seconds = 3600
	}
	if seconds < 1 || seconds > 24*3600 {
		return bad("invalid_timeout", "non-interactive timeout must be between 1 second and 24 hours")
	}
	layer := p.SandboxImplementation == "tclaude-layer"
	if p.SandboxImplementation != "" && p.SandboxImplementation != "harness-builtin" &&
		p.SandboxImplementation != "off" && !layer {
		return bad("unsupported_sandbox", "non-interactive runs cannot replay this sandbox implementation")
	}
	if snapshot := p.EffectiveSandbox; snapshot != nil {
		effective := snapshot.Effective
		if len(effective.PreLaunch) > 0 || len(effective.AgentDirectories) > 0 {
			return bad("unsupported_sandbox", "one-shot runs cannot replay pre-launch scripts or agent-owned directories")
		}
		if !layer && (effective.NetworkAccess == sandboxpolicy.NetworkAccessNone || effective.Network != nil ||
			effective.UnixSockets != nil || len(effective.Tmpfs) > 0 ||
			effective.FilesystemRoot == sandboxpolicy.FilesystemRootSeparate) {
			return bad("unsupported_sandbox", "the resolved sandbox has restrictions that a one-shot run cannot enforce")
		}
		if !layer && p.Harness == harness.ShellName && len(effective.Filesystem) > 0 {
			return bad("unsupported_sandbox", "a one-shot shell cannot enforce filesystem sandbox rules")
		}
	}
	h, err := harness.Resolve(p.Harness)
	if err != nil {
		return bad("invalid_harness", err.Error())
	}
	if h.Name != harness.ShellName && !h.CanReplayOneShotLaunchPosture() {
		return bad("unsupported_harness", fmt.Sprintf("harness %q cannot run a one-shot launch with its resolved posture", h.Name))
	}
	prompt := p.InitialMessage
	if h.Name != harness.ShellName {
		if p.WorktreePath != "" {
			prompt = fmt.Sprintf("Worktree: %s (branch %s)\n\n%s", p.WorktreePath, p.WorktreeBranch, prompt)
		}
		for _, context := range []string{p.ProfileContext, p.GroupContext} {
			if strings.TrimSpace(context) != "" {
				prompt = context + "\n\n" + prompt
			}
		}
	}
	if strings.TrimSpace(prompt) == "" {
		return bad("invalid_prompt", "a non-interactive prompt is required")
	}
	mode := p.HarnessBuiltinMode
	postureSnapshot := p.EffectiveSandbox
	if layer {
		mode = h.TclaudeLayerMode
		postureSnapshot = nil // the outer layer owns the filesystem grants
	}
	posture, err := session.OneShotLaunchPosture(p.Cwd, h, mode,
		p.ApprovalPolicy, false, postureSnapshot)
	if err != nil {
		return bad("unsupported_sandbox", err.Error())
	}
	posture.PeerMessaging = p.PeerMessaging
	if posture.ShellEnvironment == nil {
		posture.ShellEnvironment = make(map[string]string)
	}
	// Match ordinary Claude panes: its account and project state lives in the
	// writable harness state root, not the top-level file in $HOME. Without
	// this, a one-shot under a constructed root looks for ~/.claude.json and
	// reports a missing config even when normal tclaude sessions are set up.
	if err := session.ApplyClaudeConfigDirEnv(h.Name, posture.ShellEnvironment); err != nil {
		return hostFailure("sandbox_init", err.Error())
	}
	session.ApplyAutoMemoryEnv(h, p.AutoMemory, posture.ShellEnvironment)
	var argv []string
	if h.Name == harness.ShellName {
		shell := strings.TrimSpace(os.Getenv("SHELL"))
		if shell == "" {
			shell = "/bin/sh"
		}
		argv = []string{shell, "-c", prompt}
	} else {
		if h.NeedsManagedProfileForOneShot(posture.HarnessBuiltinMode) {
			name, path, capability, profileErr := EnsureSeanceCodexProfile(p.Cwd, session.GenerateSessionID(), p.EffectiveSandbox)
			if profileErr != nil {
				return hostFailure("sandbox_init", profileErr.Error())
			}
			defer func() { _ = os.Remove(path) }()
			if capability != nil {
				if err := RevalidateSeanceCodexCapability(*capability); err != nil {
					return hostFailure("sandbox_init", err.Error())
				}
			}
			posture.HarnessBuiltinMode = ""
			posture.PermissionProfile = name
		}
		argv = h.Ask.BuildAskArgv(harness.AskSpec{Prompt: prompt, Print: true, Ephemeral: true,
			Model: p.Model, Effort: p.Effort, LaunchPosture: &posture})
	}
	if len(argv) == 0 {
		return bad("unsupported_harness", "harness returned an empty command")
	}
	if layer {
		wrapped, wrapErr := wrapNonInteractiveWithTclaudeLayer(p, h, argv)
		if wrapErr != nil {
			return bad("unsupported_sandbox", wrapErr.Error())
		}
		argv = []string{"/bin/sh", "-c", wrapped}
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(seconds)*time.Second)
	defer cancel()
	command := nonInteractiveCommand{
		Argv: argv, Cwd: p.Cwd,
		// A one-shot has no session row or group member. Installed harness
		// callbacks cannot be attributed to it, so suppress those callbacks
		// before they reach the broker and raise an unplaceable-caller alert.
		Env: append(seanceProcessEnv(posture.ShellEnvironment),
			"TCLAUDE_AGENT_HINT=1", "TCLAUDE_IGNORE_HOOKS=1"),
		SandboxImplementation: p.SandboxImplementation,
		TimeoutSeconds:        seconds,
	}
	if p.EffectiveSandbox != nil {
		command.ResourceLimits = p.EffectiveSandbox.Effective.ResourceLimits
	}
	if fail := reassertDirWriteProof(p.DirWriteProofDirs); fail != nil {
		return nonInteractiveSpawnResult{}, fail
	}
	if p.CleanupDirWriteProof {
		cleanupDirWriteProofMarkers(p.DirWriteProofToken, p.DirWriteProofDirs)
	}
	finish := func(result nonInteractiveSpawnResult, failure *spawnFailure) (nonInteractiveSpawnResult, *spawnFailure) {
		if failure == nil && result.ExitCode == 0 && h.Name != harness.ShellName {
			// Harness CLIs print diagnostics and session metadata to stderr even
			// when the requested answer succeeded. The one-shot's answer is stdout.
			result.Stderr = ""
		}
		return result, failure
	}
	if runtime.GOOS == "linux" {
		if command.ResourceLimits.Enabled() {
			implementation, implErr := sandboxpolicy.NormalizeImplementation(command.SandboxImplementation)
			if implErr != nil {
				return bad("unsupported_sandbox", implErr.Error())
			}
			if err := sandboxpolicy.ValidateResourceLimitTarget(command.ResourceLimits, implementation, runtime.GOOS); err != nil {
				return bad("unsupported_sandbox", err.Error())
			}
			// Ordinary pane launches prepare their resource boundary in agentd
			// and hand it to the tmux child. The pane's own cgroup may not have
			// permission to create or configure a sibling boundary.
			dir, cleanup, prepErr := prepareNonInteractiveResourceCgroup(session.GenerateSessionID(), command.ResourceLimits)
			if prepErr != nil {
				return hostFailure("resource_limit_init", prepErr.Error())
			}
			command.ResourceCgroupDir = dir
			defer func() {
				if err := removeNonInteractiveResourceCgroup(dir); err != nil {
					slog.Warn("one-shot resource cgroup cleanup failed", "dir", dir, "error", err)
				}
				cleanup()
			}()
		}
		return finish(runNonInteractiveTmuxCommand(ctx, command))
	}
	return finish(executeNonInteractiveCommand(ctx, command))
}

type nonInteractiveCommand struct {
	Argv                  []string                     `json:"argv"`
	Cwd                   string                       `json:"cwd"`
	Env                   []string                     `json:"env"`
	SandboxImplementation string                       `json:"sandbox_implementation"`
	ResourceLimits        sandboxpolicy.ResourceLimits `json:"resource_limits"`
	ResourceCgroupDir     string                       `json:"resource_cgroup_dir,omitempty"`
	TimeoutSeconds        int64                        `json:"timeout_seconds"`
	ObservePane           bool                         `json:"-"`
}

func executeNonInteractiveCommand(ctx context.Context, command nonInteractiveCommand) (nonInteractiveSpawnResult, *spawnFailure) {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	hostFailure := func(kind, message string) (nonInteractiveSpawnResult, *spawnFailure) {
		return nonInteractiveSpawnResult{}, &spawnFailure{Status: 502, Kind: kind, Msg: message}
	}
	bad := func(kind, message string) (nonInteractiveSpawnResult, *spawnFailure) {
		return nonInteractiveSpawnResult{}, &spawnFailure{Status: 400, Kind: kind, Msg: message}
	}
	stdout := &boundedHeadBuffer{max: maxNonInteractiveOutputBytes, onLimit: cancel}
	stderr := &boundedHeadBuffer{max: maxNonInteractiveOutputBytes, onLimit: cancel}
	cmd := executil.CommandContextWithGrace(runCtx, 0, command.Argv[0], command.Argv[1:]...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = command.Cwd
	cmd.Stdin = nil
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if command.ObservePane {
		cmd.Stdout = io.MultiWriter(stdout, bestEffortPaneWriter{os.Stdout})
		cmd.Stderr = io.MultiWriter(stderr, bestEffortPaneWriter{os.Stderr})
	}
	cmd.Env = command.Env
	if command.ResourceLimits.Enabled() {
		limits := command.ResourceLimits
		implementation, implErr := sandboxpolicy.NormalizeImplementation(command.SandboxImplementation)
		if implErr != nil {
			return bad("unsupported_sandbox", implErr.Error())
		}
		if err := sandboxpolicy.ValidateResourceLimitTarget(limits, implementation, runtime.GOOS); err != nil {
			return bad("unsupported_sandbox", err.Error())
		}
		cgroupDir := command.ResourceCgroupDir
		if cgroupDir == "" {
			return hostFailure("resource_limit_init", "one-shot resource limits require a daemon-prepared cgroup")
		}
		if err := validateNonInteractivePreparedResourceCgroup(cgroupDir, limits); err != nil {
			return hostFailure("resource_limit_init", err.Error())
		}
		closeFD, configureErr := configureNonInteractiveResourceCgroup(cmd.Cmd, cgroupDir)
		if configureErr != nil {
			return hostFailure("resource_limit_init", configureErr.Error())
		}
		defer closeFD()
		stopCgroupKill := context.AfterFunc(runCtx, func() {
			if err := killNonInteractiveResourceCgroupMembers(cgroupDir); err != nil {
				slog.Warn("one-shot resource cgroup cancellation failed", "dir", cgroupDir, "error", err)
			}
		})
		defer stopCgroupKill()
	}
	err := cmd.Run()
	// The leader may have exited while children in its process group still hold
	// stdout or stderr open. WaitDelay bounds pipe draining; reap that group
	// before returning. A resource cgroup additionally catches descendants that
	// created a different process group or session.
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	if stdout.truncated || stderr.truncated {
		return nonInteractiveSpawnResult{Stdout: stdout.String(), Stderr: stderr.String() + "\nnon-interactive output exceeded 4 MiB\n", ExitCode: 125}, nil
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nonInteractiveSpawnResult{Stdout: stdout.String(), Stderr: stderr.String() + fmt.Sprintf("run timed out after %s\n", time.Duration(command.TimeoutSeconds)*time.Second), ExitCode: 124}, nil
	}
	result := nonInteractiveSpawnResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.ExitCode = 128 + int(status.Signal())
			return result, nil
		}
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		result.ExitCode = 125
		result.Stderr += "\nrun ended with output pipes still open\n"
		return result, nil
	}
	return nonInteractiveSpawnResult{}, &spawnFailure{Status: 502, Kind: "run_failed", Msg: err.Error()}
}

// A detached pane may close while the command is still winding down. Keep
// capturing its output even if the terminal can no longer accept writes.
type bestEffortPaneWriter struct{ io.Writer }

func (w bestEffortPaneWriter) Write(p []byte) (int, error) {
	_, _ = w.Writer.Write(p)
	return len(p), nil
}

func wrapNonInteractiveWithTclaudeLayer(p spawnParams, h *harness.Harness, argv []string) (string, error) {
	var harnessReadPaths []string
	if runtime.GOOS == "linux" {
		switch h.Name {
		case harness.DefaultName:
			resolved, err := harness.ResolveClaudeLaunchExecutable()
			if err != nil {
				return "", fmt.Errorf("resolve Claude executable for tclaude’s sandbox: %w", err)
			}
			argv[0] = resolved.Path
			harnessReadPaths = append(harnessReadPaths, resolved.Path)
		case harness.CodexName:
			resolved, err := harness.ResolveCodexLaunchExecutable()
			if err != nil {
				return "", fmt.Errorf("resolve Codex executable for tclaude’s sandbox: %w", err)
			}
			argv[0] = resolved.Path
			harnessReadPaths = append(harnessReadPaths, resolved.RuntimeRoot)
		}
	}
	spec, err := session.BuildTclaudeLayerLaunchSpec(session.TclaudeLayerLaunchInput{
		HarnessName: h.Name, Cwd: p.Cwd, Snapshot: p.EffectiveSandbox,
		GitWriteDirs:     append([]string(nil), p.GitWorktreeWriteDirs...),
		HarnessReadPaths: harnessReadPaths,
	})
	if err != nil {
		return "", err
	}
	if err := session.PrepareTclaudeLayerHarnessState(spec); err != nil {
		return "", err
	}
	posture, err := session.TclaudeLayerNetworkPosture(spec.Effective)
	if err != nil {
		return "", err
	}
	root, err := session.TclaudeLayerRootPosture(posture, spec.Effective)
	if err != nil {
		return "", err
	}
	binary, _, err := session.ResolveTclaudeLayerServerForEngine(posture, root, spec.Contract.NetworkEngine)
	if err != nil {
		return "", err
	}
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = clcommon.ShellQuoteArg(arg)
	}
	return session.WrapTclaudeLayerServerSpec(binary, spec, "exec "+strings.Join(quoted, " "))
}
