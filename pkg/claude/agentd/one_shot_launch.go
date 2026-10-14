package agentd

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/uuid"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// AskLaunchInput describes a foreground ask using the same preparation that
// precedes the daemon's one-shot helper. It creates no conversation or identity.
type AskLaunchInput struct {
	Cwd                string
	Harness            *harness.Harness
	Ask                harness.AskSpec
	Snapshot           *sandboxpolicy.Snapshot
	Implementation     string
	HarnessBuiltinMode string
	ApprovalPolicy     string
}

// PreparedOneShotLaunch owns launch resources; execution remains with either
// the daemon's existing tmux helper or ask's foreground/TTY runner.
type PreparedOneShotLaunch struct {
	Argv              []string
	Env               []string
	ResourceLimits    sandboxpolicy.ResourceLimits
	ResourceCgroupDir string
	directory         *oneShotDirectoryRun
	cleanup           []func()
	closed            bool
	notes             []string
}

func PrepareAskLaunch(input AskLaunchInput) (*PreparedOneShotLaunch, error) {
	if input.Harness == nil {
		return nil, fmt.Errorf("ask sandbox: missing harness")
	}
	// These askers have no reviewed posture-replay contract. Pi's walled state
	// is currently tied to tracked conversation lifetime, which a direct ask
	// does not have. Refuse rather than silently lose policy or live state.
	if !input.Harness.CanReplayOneShotLaunchPosture() || input.Harness.Name == harness.PiName {
		return nil, fmt.Errorf("ask sandbox: harness %q cannot replay a foreground sandbox policy; set ask.sandbox=off (Config → Ask — sandbox → Off), pick ask.profile with Claude, Codex, or Gemini, or use a tracked interactive worker", input.Harness.Name)
	}
	p := spawnParams{Cwd: input.Cwd, Harness: input.Harness.Name, Model: input.Ask.Model, Effort: input.Ask.Effort, EffectiveSandbox: input.Snapshot, SandboxImplementation: input.Implementation, HarnessBuiltinMode: input.HarnessBuiltinMode, ApprovalPolicy: input.ApprovalPolicy}
	if _, err := validateSandboxImplementationForHarness(input.Harness, input.Implementation); err != nil {
		return nil, fmt.Errorf("ask sandbox: %w", err)
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(input.Implementation)
	if err != nil {
		return nil, fmt.Errorf("ask sandbox: %w", err)
	}
	if implementation == sandboxpolicy.ImplementationHarnessBuiltin {
		confined := false
		switch input.Harness.Name {
		case harness.DefaultName:
			verdict := harness.ResolveLaunchOSSandbox(input.Harness, input.HarnessBuiltinMode, "", input.Cwd)
			confined = verdict.State == harness.ClaudeSandboxStateOn.String() && !verdict.Unverified
		case harness.CodexName:
			confined = input.HarnessBuiltinMode == harness.SandboxManagedProfile || input.HarnessBuiltinMode == harness.SandboxReadOnly || input.HarnessBuiltinMode == harness.SandboxWorkspaceWrite
		}
		if !confined {
			return nil, fmt.Errorf("ask sandbox: native mode %q cannot establish confinement for %s; choose an ask profile with sandbox_implementation=tclaude-layer or a confined native mode", input.HarnessBuiltinMode, input.Harness.Name)
		}
	}
	if fail := sandboxProfileCapabilityFailure(p.Harness, p.HarnessBuiltinMode, p.EffectiveSandbox, p.SandboxImplementation); fail != nil {
		return nil, fmt.Errorf("ask sandbox: %s", fail.Msg)
	}
	if _, fail := planSandboxProfileAccessForLaunch(p.Harness, p.HarnessBuiltinMode, p.EffectiveSandbox, p.SandboxImplementation,
		session.ModelTransportLaunchContext{Model: input.Ask.Model, Cwd: input.Cwd}, false); fail != nil {
		return nil, fmt.Errorf("ask sandbox: %s", fail.Msg)
	}
	if fail := sandboxImplementationHostFailure(p.Harness, p.SandboxImplementation); fail != nil {
		return nil, fmt.Errorf("ask sandbox: %s", fail.Msg)
	}
	prepared, fail := prepareOneShotLaunch(p, input.Harness, input.Ask, true)
	if fail != nil {
		return nil, fmt.Errorf("ask sandbox: %s", fail.Msg)
	}
	return prepared, nil
}

func (prepared *PreparedOneShotLaunch) MarkLaunchAttempt() error {
	if prepared.directory != nil {
		return prepared.directory.launched(prepared.ResourceCgroupDir, prepared.ResourceLimits)
	}
	return nil
}

func (prepared *PreparedOneShotLaunch) LaunchFailed() {
	if prepared.directory != nil {
		prepared.directory.Started = false
		if err := prepared.directory.save(); err != nil {
			slog.Warn("one-shot directory launch failure journal", "error", err)
		}
	}
}

func (prepared *PreparedOneShotLaunch) ConfigureProcess(cmd *exec.Cmd) (func(), error) {
	if prepared.ResourceCgroupDir == "" {
		return func() {}, nil
	}
	if err := validateNonInteractivePreparedResourceCgroup(prepared.ResourceCgroupDir, prepared.ResourceLimits); err != nil {
		return func() {}, err
	}
	return configureNonInteractiveResourceCgroup(cmd, prepared.ResourceCgroupDir)
}

func (prepared *PreparedOneShotLaunch) Close() []string {
	if prepared.closed {
		return prepared.notes
	}
	prepared.closed = true
	for i := len(prepared.cleanup) - 1; i >= 0; i-- {
		prepared.cleanup[i]()
	}
	if prepared.directory != nil {
		prepared.directory.finish()
		if _, err := os.Lstat(filepath.Join(prepared.directory.journalRoot(), prepared.directory.Key+".json")); err == nil {
			prepared.notes = append(prepared.notes, fmt.Sprintf("One-shot directories retained at %s until termination can be proven; without a cgroup the automatic sweep waits for a reboot. Remove them manually only after all descendants have stopped.", filepath.Join(prepared.directory.Base, prepared.directory.Key)))
		}
	}
	return prepared.notes
}

func prepareOneShotLaunch(p spawnParams, h *harness.Harness, askSpec harness.AskSpec, foreground bool) (result *PreparedOneShotLaunch, failure *spawnFailure) {
	prepared := &PreparedOneShotLaunch{}
	defer func() {
		if failure != nil {
			for _, note := range prepared.Close() {
				failure.Msg += "; " + note
			}
		}
	}()
	bad := func(kind, message string) (*PreparedOneShotLaunch, *spawnFailure) {
		return nil, &spawnFailure{Status: 400, Kind: kind, Msg: message}
	}
	hostFailure := func(kind, message string) (*PreparedOneShotLaunch, *spawnFailure) {
		return nil, &spawnFailure{Status: 502, Kind: kind, Msg: message}
	}
	layer := p.SandboxImplementation == "tclaude-layer" || p.SandboxImplementation == "stacked"
	if p.EffectiveSandbox != nil && len(p.EffectiveSandbox.Effective.AgentDirectories) > 0 {
		run, err := newOneShotDirectoryRun()
		if err != nil {
			return hostFailure("sandbox_init", err.Error())
		}
		prepared.directory = run
		if foreground {
			if err := run.setForegroundOwner(); err != nil {
				return hostFailure("sandbox_init", err.Error())
			}
		}
		materialized, _, err := prepareCodexSSHWorkaroundForNewLaunch(*p.EffectiveSandbox, run.Key, p.SSHWorkaround && codexSSHWorkaroundApplies(p.Harness, p.HarnessBuiltinMode, p.SandboxImplementation, p.EffectiveSandbox))
		if err != nil {
			return hostFailure("sandbox_init", err.Error())
		}
		p.EffectiveSandbox = &materialized
	}
	prompt := askSpec.Prompt
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
	if strings.TrimSpace(prompt) == "" && askSpec.Print {
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
			prepared.cleanup = append(prepared.cleanup, func() { _ = os.Remove(path) })
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
		askSpec.Prompt, askSpec.LaunchPosture = prompt, &posture
		if remover, ok := h.Ask.(harness.EphemeralSessionRemover); ok && askSpec.Ephemeral && h.PreMintsAskConvID() {
			// The harness persists the turn anyway; pin its id so the
			// conversation it creates is removed once the run is over.
			askSpec.SessionID = uuid.NewString()
			prepared.cleanup = append(prepared.cleanup, func() { remover.RemoveEphemeralSession(askSpec.SessionID) })
		}
		argv = h.Ask.BuildAskArgv(askSpec)
	}
	if len(argv) == 0 {
		return bad("unsupported_harness", "harness returned an empty command")
	}
	if layer {
		wrapped, cleanup, wrapErr := prepareNonInteractiveWithTclaudeLayer(p, h, argv, preLaunch)
		prepared.cleanup = append(prepared.cleanup, cleanup)
		if wrapErr != nil {
			return bad("unsupported_sandbox", oneShotPolicyFailure(p.EffectiveSandbox, wrapErr))
		}
		argv = []string{"/bin/sh", "-c", wrapped}
	} else if preLaunch != "" {
		argv = append(clcommon.BootstrapShellArgv(), "-c", preLaunch+"exec "+quoteOneShotArgv(argv))
	}

	prepared.Argv = argv
	prepared.Env = append(seanceProcessEnv(posture.ShellEnvironment), "TCLAUDE_AGENT_HINT=1", "TCLAUDE_IGNORE_HOOKS=1")
	if p.EffectiveSandbox != nil {
		prepared.ResourceLimits = p.EffectiveSandbox.Effective.ResourceLimits
	}
	required := prepared.ResourceLimits.Enabled() || p.SandboxImplementation == "resource-only"
	if required || (prepared.directory != nil && runtime.GOOS == "linux") {
		if err := sandboxpolicy.ValidateResourceLimitTarget(prepared.ResourceLimits, implementation, runtime.GOOS); err != nil {
			return bad("unsupported_sandbox", err.Error())
		}
		dir, cleanup, err := prepareNonInteractiveResourceCgroup(session.GenerateSessionID(), prepared.ResourceLimits)
		if err != nil {
			if required {
				return hostFailure("resource_limit_init", err.Error())
			}
		} else {
			prepared.ResourceCgroupDir = dir
			prepared.cleanup = append(prepared.cleanup, func() {
				if err := removeNonInteractiveResourceCgroup(dir); err != nil {
					slog.Warn("one-shot resource cgroup cleanup failed", "dir", dir, "error", err)
				}
				cleanup()
			})
		}
	}
	return prepared, nil
}
