package agentd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/common/executil"
	"github.com/tofutools/tclaude/pkg/federation/jobstream"
)

const maxNonInteractiveOutputBytes = 4 << 20

var prepareNonInteractiveResourceCgroup = session.PrepareResourceCgroup
var configureNonInteractiveResourceCgroup = session.ConfigureProcessResourceCgroup
var validateNonInteractivePreparedResourceCgroup = session.ValidatePreparedResourceCgroup
var removeNonInteractiveResourceCgroup = session.RemoveResourceCgroup
var killNonInteractiveResourceCgroupMembers = session.KillResourceCgroupMembers
var runNonInteractiveTmuxCommand = runNonInteractiveThroughTmux

type nonInteractiveSpawnResult struct {
	Notes       []string `json:"notes,omitempty"`
	StdoutBytes []byte   `json:"stdout_bytes,omitempty"`
	StderrBytes []byte   `json:"stderr_bytes,omitempty"`
	Stdout      string   `json:"stdout"`
	Stderr      string   `json:"stderr"`
	ExitCode    int      `json:"exit_code"`
}

// runNonInteractiveSpawn uses the ordinary spawn boundary's resolved fields,
// but does not allocate a conversation or group membership.
func runNonInteractiveSpawn(parent context.Context, p spawnParams, seconds int64) (out nonInteractiveSpawnResult, failure *spawnFailure) {
	if p.CleanupDirWriteProof {
		defer cleanupDirWriteProofMarkers(p.DirWriteProofToken, p.DirWriteProofDirs)
	}
	bad := func(kind, message string) (nonInteractiveSpawnResult, *spawnFailure) {
		return nonInteractiveSpawnResult{}, &spawnFailure{Status: 400, Kind: kind, Msg: message}
	}
	if p.ModelProxy != "" && p.ModelProxy != "off" {
		return bad("unsupported_model_proxy", fmt.Sprintf("one-shot runs cannot use model gateway %q without a gateway caller identity; use an interactive worker or --model-proxy off", p.ModelProxy))
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
	layer := p.SandboxImplementation == "tclaude-layer" || p.SandboxImplementation == "stacked"
	if p.SandboxImplementation != "" && p.SandboxImplementation != "harness-builtin" &&
		p.SandboxImplementation != "off" && p.SandboxImplementation != "resource-only" && !layer {
		return bad("unsupported_sandbox", "non-interactive runs cannot replay this sandbox implementation")
	}
	if snapshot := p.EffectiveSandbox; snapshot != nil {
		effective := snapshot.Effective
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
	var directoryRun *oneShotDirectoryRun
	if p.EffectiveSandbox != nil && len(p.EffectiveSandbox.Effective.AgentDirectories) > 0 {
		var prepareErr error
		directoryRun, prepareErr = newOneShotDirectoryRun()
		if prepareErr != nil {
			return hostFailure("sandbox_init", prepareErr.Error())
		}
		defer func() {
			directoryRun.finish()
			if _, err := os.Lstat(filepath.Join(oneShotDirectoryJournalRoot(), directoryRun.Key+".json")); err == nil {
				note := fmt.Sprintf("One-shot directories retained at %s until termination can be proven; without a cgroup the automatic sweep waits for a reboot. Remove them manually only after all descendants have stopped.", filepath.Join(directoryRun.Base, directoryRun.Key))
				if failure != nil {
					failure.Msg += "; " + note
				} else {
					out.Notes = append(out.Notes, note)
				}
			}
		}()
		materialized, _, prepareErr := prepareCodexSSHWorkaroundForNewLaunch(
			*p.EffectiveSandbox, directoryRun.Key, p.SSHWorkaround && codexSSHWorkaroundApplies(
				p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation, p.EffectiveSandbox))
		if prepareErr != nil {
			return hostFailure("sandbox_init", prepareErr.Error())
		}
		p.EffectiveSandbox = &materialized
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
	if p.SandboxImplementation == "stacked" {
		if err := session.ValidateStackedSandboxHarness(h); err != nil {
			return bad("unsupported_sandbox", err.Error())
		}
		// As runNew does, force the reviewed inner wall before generating a
		// managed profile; the requested native mode must not skip that step.
		if h.Name == harness.CodexName {
			mode = harness.SandboxManagedProfile
		} else {
			mode = harness.ClaudeSandboxOn
		}
	}
	postureSnapshot := p.EffectiveSandbox
	if layer {
		if p.SandboxImplementation != "stacked" {
			mode = h.TclaudeLayerMode
		}
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(p.SandboxImplementation)
	if err != nil {
		return bad("unsupported_sandbox", err.Error())
	}
	posture, err := session.OneShotLaunchPosture(p.Cwd, h, mode,
		p.ApprovalPolicy, false, postureSnapshot, implementation)
	if err != nil {
		return bad("unsupported_sandbox", err.Error())
	}
	posture.PeerMessaging = p.PeerMessaging
	if posture.ShellEnvironment == nil {
		posture.ShellEnvironment = make(map[string]string)
	}
	preLaunch, err := session.RenderPreLaunchScript(p.EffectiveSandbox)
	if err != nil {
		return bad("unsupported_sandbox", oneShotPolicyFailure(p.EffectiveSandbox, err))
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
		if p.SandboxImplementation == "stacked" {
			if err := session.ValidateStackedSandboxHarness(h); err != nil {
				return bad("unsupported_sandbox", err.Error())
			}
			posture = h.NestedSandbox.PrepareLaunch(posture)
		}
		askSpec := harness.AskSpec{Prompt: prompt, Print: true, Ephemeral: true,
			Model: p.Model, Effort: p.Effort, LaunchPosture: &posture}
		if remover, ok := h.Ask.(harness.EphemeralSessionRemover); ok && h.PreMintsAskConvID() {
			// The harness persists the turn anyway; pin its id so the
			// conversation it creates is removed once the run is over.
			askSpec.SessionID = uuid.NewString()
			defer remover.RemoveEphemeralSession(askSpec.SessionID)
		}
		argv = h.Ask.BuildAskArgv(askSpec)
	}
	if len(argv) == 0 {
		return bad("unsupported_harness", "harness returned an empty command")
	}
	if layer {
		wrapped, cleanup, wrapErr := prepareNonInteractiveWithTclaudeLayer(p, h, argv, preLaunch)
		defer cleanup()
		if wrapErr != nil {
			return bad("unsupported_sandbox", oneShotPolicyFailure(p.EffectiveSandbox, wrapErr))
		}
		argv = []string{"/bin/sh", "-c", wrapped}
	} else if preLaunch != "" {
		argv = append(clcommon.BootstrapShellArgv(), "-c", preLaunch+"exec "+quoteOneShotArgv(argv))
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
		TmuxSessionName:       p.OneShotName,
		RemoteJob:             p.RemoteJob,
	}
	if p.RemoteJob != nil {
		command.JobLiveOutput = p.RemoteJob.LivePath
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
		if cfg, err := config.Load(); err == nil && cfg.HTTPProxyConfigured() {
			result.Notes = append(result.Notes, "Named HTTP proxy URLs are omitted from one-shot runs because they have no gateway caller identity; use an interactive spawn for named proxies.")
		}
		if failure == nil && result.ExitCode == 0 && h.Name != harness.ShellName {
			// Harness CLIs print diagnostics and session metadata to stderr even
			// when the requested answer succeeded. The one-shot's answer is stdout.
			result.Stderr = ""
		}
		return result, failure
	}
	if runtime.GOOS == "linux" || p.RemoteJob != nil {
		if command.ResourceLimits.Enabled() || p.SandboxImplementation == "resource-only" || directoryRun != nil {
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
				if command.ResourceLimits.Enabled() || p.SandboxImplementation == "resource-only" {
					return hostFailure("resource_limit_init", prepErr.Error())
				}
				// As with ordinary layer launches, a cgroup without authored
				// ceilings is opportunistic. Directory cleanup stays conservative.
				dir, cleanup = "", func() {}
			}
			command.ResourceCgroupDir = dir
			defer func() {
				if err := removeNonInteractiveResourceCgroup(dir); err != nil {
					slog.Warn("one-shot resource cgroup cleanup failed", "dir", dir, "error", err)
				}
				cleanup()
			}()
		}
		if directoryRun != nil {
			if err := directoryRun.launched(command.ResourceCgroupDir, command.ResourceLimits); err != nil {
				return hostFailure("sandbox_init", err.Error())
			}
		}
		return finish(runNonInteractiveTmuxCommand(ctx, command))
	}
	if directoryRun != nil {
		if err := directoryRun.launched(""); err != nil {
			return hostFailure("sandbox_init", err.Error())
		}
	}
	return finish(executeNonInteractiveCommand(ctx, command))
}

type nonInteractiveCommand struct {
	JobLiveOutput         string                       `json:"job_live_output,omitempty"`
	Argv                  []string                     `json:"argv"`
	Cwd                   string                       `json:"cwd"`
	Env                   []string                     `json:"env"`
	SandboxImplementation string                       `json:"sandbox_implementation"`
	ResourceLimits        sandboxpolicy.ResourceLimits `json:"resource_limits"`
	ResourceCgroupDir     string                       `json:"resource_cgroup_dir,omitempty"`
	TimeoutSeconds        int64                        `json:"timeout_seconds"`
	ObservePane           bool                         `json:"-"`
	// TmuxSessionName names the Linux one-shot pane's tmux session. Empty
	// picks a generated one-shot-<id>. Daemon-side only.
	TmuxSessionName string               `json:"-"`
	RemoteJob       *federationJobLaunch `json:"-"`
}

func executeNonInteractiveCommand(ctx context.Context, command nonInteractiveCommand) (out nonInteractiveSpawnResult, failure *spawnFailure) {
	defer func() {
		if command.JobLiveOutput != "" {
			out.StdoutBytes = []byte(out.Stdout)
			out.StderrBytes = []byte(out.Stderr)
		}
	}()
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
	if command.JobLiveOutput != "" {
		file, e := openFederationJobLiveWriter(command.JobLiveOutput)
		if e != nil {
			return hostFailure("live_output", e.Error())
		}
		defer func() { _ = file.Close() }()
		frames := jobstream.NewEncoder(file)
		cmd.Stdout = io.MultiWriter(cmd.Stdout, frames.Channel(jobstream.Stdout))
		cmd.Stderr = io.MultiWriter(cmd.Stderr, frames.Channel(jobstream.Stderr))
	}
	cmd.Env = command.Env
	if command.ResourceLimits.Enabled() || command.ResourceCgroupDir != "" {
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

type oneShotLayerOptions struct {
	preLaunch string
	stacked   bool
	cleanup   func()
}

func prepareNonInteractiveWithTclaudeLayer(p spawnParams, h *harness.Harness, argv []string, preLaunch string) (string, func(), error) {
	opts := &oneShotLayerOptions{preLaunch: preLaunch, stacked: p.SandboxImplementation == "stacked", cleanup: func() {}}
	wrapped, err := wrapOneShotLayer(h, p.Cwd, p.EffectiveSandbox, p.GitWorktreeWriteDirs, argv, opts)
	return wrapped, opts.cleanup, err
}

// wrapOneShotWithTclaudeLayer renders a one-shot argv as a shell command run
// inside tclaude's built-in sandbox for cwd and snapshot. Shared by
// non-interactive spawns and séances replaying a tclaude-layer generation.
func wrapOneShotWithTclaudeLayer(
	h *harness.Harness, cwd string, snapshot *sandboxpolicy.Snapshot, gitWriteDirs, argv []string,
) (string, error) {
	return wrapOneShotLayer(h, cwd, snapshot, gitWriteDirs, argv, nil)
}

func wrapOneShotLayer(
	h *harness.Harness, cwd string, snapshot *sandboxpolicy.Snapshot, gitWriteDirs, argv []string, opts *oneShotLayerOptions,
) (string, error) {
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
	input := session.TclaudeLayerLaunchInput{
		HarnessName: h.Name, Cwd: cwd, Snapshot: snapshot,
		GitWriteDirs:     append([]string(nil), gitWriteDirs...),
		HarnessReadPaths: harnessReadPaths,
	}
	var piEnv []string
	if h.Name == harness.PiName {
		// As a pane launch: pi runs against a walled agent directory of its
		// own, never the operator's. A one-shot is keyed by an id of its
		// own, so it never shares one with a running agent, and the daemon
		// removes it once nothing has a session under that id.
		walled, err := harness.PreparePiWalledAgentDir(uuid.NewString())
		if err != nil {
			return "", err
		}
		input.StateRoot = walled
		piEnv = append(piEnv, harness.PiAgentDirEnvVar+"="+walled)
	}
	spec, err := session.BuildTclaudeLayerLaunchSpec(input)
	if err != nil {
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
	if h.Name == harness.PiName {
		axes, err := sandboxpolicy.PlannedEffectiveAccessAxes(spec.Effective)
		if err != nil {
			return "", err
		}
		// Under a filtered network pi runs offline, as the pane launch does
		// (piFilteredNetworkPrefix).
		if posture == sandboxpolicy.NetworkFiltered && !sandboxpolicy.NetworkRulesArePrivateRoutedOpen(axes.Network) {
			for name, value := range harness.PiFilteredNetworkEnv() {
				piEnv = append(piEnv, name+"="+value)
			}
			sort.Strings(piEnv[1:])
		}
		if runtime.GOOS == "linux" && root == sandboxpolicy.RootConstructed {
			// pi is a Node CLI, absent from a constructed root unless started
			// by absolute path with its runtime mounted.
			resolved, err := harness.ResolvePiLaunchExecutable()
			if err != nil {
				return "", fmt.Errorf("resolve pi executable for tclaude’s sandbox: %w", err)
			}
			if argv, err = resolved.SpliceBinary(argv, "pi"); err != nil {
				return "", err
			}
			input.HarnessReadPaths = append(input.HarnessReadPaths, resolved.ReadPaths...)
			if spec, err = session.BuildTclaudeLayerLaunchSpec(input); err != nil {
				return "", err
			}
		}
		argv = slices.Concat([]string{"env"}, piEnv, argv)
	}
	if h.Name == harness.GeminiName && runtime.GOOS == "linux" && root == sandboxpolicy.RootConstructed {
		// Only a constructed root needs this: a host-inherited one still sees
		// the pane's own PATH, so a launch there keeps resolving `gemini` and
		// `node` exactly as it did before.
		resolved, err := harness.ResolveGeminiLaunchExecutable()
		if err != nil {
			return "", fmt.Errorf("resolve Gemini executable for tclaude’s sandbox: %w", err)
		}
		argv, err = resolved.SpliceArgv(argv)
		if err != nil {
			return "", err
		}
		input.HarnessReadPaths = append(input.HarnessReadPaths, resolved.ReadPaths...)
		if spec, err = session.BuildTclaudeLayerLaunchSpec(input); err != nil {
			return "", err
		}
	}
	if err := session.PrepareTclaudeLayerHarnessState(spec); err != nil {
		return "", err
	}
	binary, _, err := session.ResolveTclaudeLayerServerForEngine(posture, root, spec.Contract.NetworkEngine)
	if err != nil {
		return "", err
	}
	if opts != nil && opts.stacked {
		proof, err := session.ProbeStackedSandbox(binary, spec, h, cwd)
		if err != nil {
			return "", err
		}
		opts.cleanup = proof.Cleanup
		argv[0] = proof.Executable.Path
		command := "exec " + quoteOneShotArgv(argv)
		if opts.preLaunch != "" {
			command = opts.preLaunch + command
			command = "exec " + quoteOneShotArgv(append(clcommon.BootstrapShellArgv(), "-c", command))
		}
		return session.WrapTclaudeLayerStackedSpec(binary, spec,
			proof.ManifestPath, proof.ManifestSHA256, proof.ReadyPath, true, command)
	}
	command := "exec " + quoteOneShotArgv(argv)
	if opts != nil && opts.preLaunch != "" {
		command = opts.preLaunch + command
		// The ordinary pane bootstrap uses Bash too. Run the fragments in
		// that shell inside the outer wall, not in the host /bin/sh wrapper.
		command = "exec " + quoteOneShotArgv(append(clcommon.BootstrapShellArgv(), "-c", command))
	}
	return session.WrapTclaudeLayerServerSpec(binary, spec, command)
}

func quoteOneShotArgv(argv []string) string {
	quoted := make([]string, len(argv))
	for i, arg := range argv {
		quoted[i] = clcommon.ShellQuoteArg(arg)
	}
	return strings.Join(quoted, " ")
}

func oneShotPolicyFailure(snapshot *sandboxpolicy.Snapshot, err error) string {
	if snapshot == nil {
		return err.Error()
	}
	var profiles, blocks []string
	for _, source := range snapshot.Effective.Provenance.Applied {
		profiles = append(profiles, fmt.Sprintf("%s %q", source.Scope, source.Profile))
	}
	for _, block := range snapshot.Effective.PreLaunch {
		blocks = append(blocks, block.Name)
	}
	return fmt.Sprintf("%s (sandbox profiles: %s; agent_directories: %s; pre_launch blocks: %s). An operator can enable the missing capability or adjust the supplying profile; another --sandbox-profile cannot remove inherited global/group settings.",
		err, strings.Join(profiles, ", "), strings.Join(snapshot.Effective.AgentDirectories, ", "), strings.Join(blocks, ", "))
}
