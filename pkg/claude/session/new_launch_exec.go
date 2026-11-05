package session

import (
	"fmt"
	"os/exec"
	"runtime"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/common/harnesspath"
)

// launchExecutable is the harness executable runNew resolved for this launch:
// its absolute path when one is pinned (empty means PATH lookup), the
// interpreter that runs it, and the paths the sandbox must leave readable for
// it to start.
type launchExecutable struct {
	path        string
	interpreter string
	readPaths   []string
}

// resolveLaunchExecutable is runNew's executable step. It revalidates a
// strict-Home Codex capability, pins the harness executable where tclaude's
// sandbox or the harness needs an absolute path, and runs the version gates
// (Copilot gateway, Claude subscription, the harness's own CheckVersion)
// against the binary that will actually launch.
func resolveLaunchExecutable(params *NewParams, h *harness.Harness, outerLayer bool,
	tclaudeLayerRoot sandboxpolicy.RootPosture, codexSplit *harness.CodexSplitPolicyCapability,
) (launchExecutable, error) {
	var err error
	if codexSplit != nil {
		if err := harness.RevalidateCodexHomeSplitPolicyCapability(*codexSplit); err != nil {
			return launchExecutable{}, fmt.Errorf("revalidate strict-Home Codex launch capability: %w", err)
		}
	}
	executablePath := ""
	executableInterpreter := ""
	var harnessReadPaths []string
	if params.ManagedInstructionsRoot != "" {
		harnessReadPaths = append(harnessReadPaths, params.ManagedInstructionsRoot)
	}
	var pinned TclaudeLayerExecutable
	pinnedExecutable := false
	if codexSplit == nil && outerLayer {
		if pinned, pinnedExecutable, err = ResolveTclaudeLayerExecutable(h.Name, tclaudeLayerRoot); err != nil {
			return launchExecutable{}, err
		}
	}
	if codexSplit != nil {
		executablePath = codexSplit.ExecutablePath
	} else if pinnedExecutable {
		executablePath = pinned.Path
		executableInterpreter = pinned.Interpreter
		harnessReadPaths = append(harnessReadPaths, pinned.ReadPaths...)
	} else if h.Name == harness.OpenCodeName {
		executablePath, err = harness.OpenCodeExecutable()
		if err != nil {
			return launchExecutable{}, fmt.Errorf("find OpenCode executable: %w", err)
		}
	}
	if h.Name == harness.CopilotName && params.ModelProxy != "" {
		executablePath, err = exec.LookPath("copilot")
		if err != nil {
			return launchExecutable{}, fmt.Errorf("find Copilot gateway executable: %w", err)
		}
		if err = validateCopilotModelProxyVersion(executablePath); err != nil {
			return launchExecutable{}, err
		}
	}
	if params.ClaudeSubscription {
		binary := executablePath
		if binary == "" {
			binary = harnesspath.ManagedExecutable(h.Spawn.Binary())
		}
		if err = validateClaudeSubscriptionVersion(binary); err != nil {
			return launchExecutable{}, err
		}
	}
	if h.CheckVersion != nil {
		binary := executablePath
		if binary == "" {
			if binary, err = exec.LookPath(h.Spawn.Binary()); err != nil {
				return launchExecutable{}, fmt.Errorf("find %s executable: %w", h.DisplayName, err)
			}
		}
		if err := h.CheckVersion(binary); err != nil {
			return launchExecutable{}, err
		}
	}
	return launchExecutable{path: executablePath, interpreter: executableInterpreter, readPaths: harnessReadPaths}, nil
}

// TclaudeLayerExecutable is a harness executable pinned for a launch behind
// tclaude's sandbox: its absolute path, the interpreter that runs it (empty
// for a native binary), and the paths the sandbox must leave readable for it
// to start.
type TclaudeLayerExecutable struct {
	Path        string
	Interpreter string
	ReadPaths   []string
}

// ResolveTclaudeLayerExecutable pins the harness executable a launch behind
// tclaude's sandbox starts, for the harnesses that need one on Linux: Codex
// and Claude always, Gemini and pi only under a constructed root. ok is false
// when the harness keeps resolving through PATH. `session new` and both conv
// resume paths call it, so a resumed pane starts the same binary a new one
// does.
func ResolveTclaudeLayerExecutable(harnessName string, root sandboxpolicy.RootPosture) (TclaudeLayerExecutable, bool, error) {
	if runtime.GOOS != "linux" {
		return TclaudeLayerExecutable{}, false, nil
	}
	switch {
	case harnessName == harness.CodexName:
		resolvedCodex, resolveErr := harness.ResolveCodexLaunchExecutable()
		if resolveErr != nil {
			return TclaudeLayerExecutable{}, false, fmt.Errorf("resolve Codex executable for tclaude’s sandbox: %w", resolveErr)
		}
		return TclaudeLayerExecutable{Path: resolvedCodex.Path, ReadPaths: []string{resolvedCodex.RuntimeRoot}}, true, nil
	case harnessName == harness.DefaultName:
		resolvedClaude, resolveErr := harness.ResolveClaudeLaunchExecutable()
		if resolveErr != nil {
			return TclaudeLayerExecutable{}, false, fmt.Errorf("resolve Claude executable for tclaude’s sandbox: %w", resolveErr)
		}
		return TclaudeLayerExecutable{Path: resolvedClaude.Path, ReadPaths: []string{resolvedClaude.Path}}, true, nil
	case harnessName == harness.GeminiName && root == sandboxpolicy.RootConstructed:
		// Only a constructed root needs this: a host-inherited one still sees
		// the pane's own PATH, profile environment and pre-launch script, so a
		// launch there keeps resolving `gemini` and `node` as it did before.
		resolvedGemini, resolveErr := harness.ResolveGeminiLaunchExecutable()
		if resolveErr != nil {
			return TclaudeLayerExecutable{}, false, fmt.Errorf("resolve Gemini executable for tclaude’s sandbox: %w", resolveErr)
		}
		return TclaudeLayerExecutable{Path: resolvedGemini.Path, Interpreter: resolvedGemini.Interpreter, ReadPaths: resolvedGemini.ReadPaths}, true, nil
	case harnessName == harness.PiName && root == sandboxpolicy.RootConstructed:
		// As for Gemini: pi is a Node CLI, absent from a constructed root
		// unless started by absolute path with its runtime mounted.
		resolvedPi, resolveErr := harness.ResolvePiLaunchExecutable()
		if resolveErr != nil {
			return TclaudeLayerExecutable{}, false, fmt.Errorf("resolve pi executable for tclaude’s sandbox: %w", resolveErr)
		}
		return TclaudeLayerExecutable{Path: resolvedPi.Path, Interpreter: resolvedPi.Interpreter, ReadPaths: resolvedPi.ReadPaths}, true, nil
	}
	return TclaudeLayerExecutable{}, false, nil
}

// launchDirsInput is what planLaunchDirs derives the launch's directory grants
// from.
type launchDirsInput struct {
	params                                          *NewParams
	h                                               *harness.Harness
	cwd                                             string
	launchSandbox                                   *sandboxpolicy.Snapshot
	harnessBuiltinMode, effectiveHarnessBuiltinMode string
	sandboxImplementation                           sandboxpolicy.Implementation
	outerLayer, unconfined                          bool
}

// launchDirs is the launch's directory plan: the Codex app-server runtime
// directory (nil when none), the Git write grants, and the write, read and
// deny lists the harness is told about.
type launchDirs struct {
	codexRuntime      *TclaudeLayerPrivateWriteDir
	gitWrite          []string
	write, read, deny []string
}

// planLaunchDirs is runNew's directory step. It derives the write, read and
// deny lists from the Git worktree, the sandbox snapshot, the Codex runtime
// directory and any tmpfs mounts, then re-runs the reopen-under-deny and
// Copilot add-dir gates on the rules that will actually be emitted.
func planLaunchDirs(in launchDirsInput) (launchDirs, error) {
	codexRuntimeDir, err := codexAppServerPrivateWriteDir(in.params)
	if err != nil {
		return launchDirs{}, err
	}
	launchGitWriteDirs := gitWorktreeWriteDirs(in.params, in.h.Name, in.harnessBuiltinMode, in.cwd)
	if in.outerLayer && tclaudeLayerWrapsPane(in.h.Name) {
		// Derive repository grants for the outer wall independently of the
		// harness-native mode.
		launchGitWriteDirs = gitWorktreeWriteDirs(in.params, harness.DefaultName, harness.ClaudeSandboxOn, in.cwd)
	}
	if sandboxDenyCoversPath(in.launchSandbox, in.cwd) {
		launchGitWriteDirs = denyNarrowedGitWriteDirs(in.cwd, in.params.CodexGitCommonDir, launchGitWriteDirs)
	}
	launchContractReadDirs := sandboxLaunchContractReadDirs(in.launchSandbox, append([]string{in.cwd}, launchGitWriteDirs...)...)
	launchWriteDirs := append(launchGitWriteDirs, sandboxSnapshotDirs(in.launchSandbox, sandboxpolicy.AccessWrite)...)
	if codexRuntimeDir != nil {
		// The Codex harness must be able to create the socket/pid/log; under
		// tclaude's sandbox the outer wall gets the narrower private-parent
		// hide/reopen contract below.
		launchWriteDirs = AppendUniqueDir(launchWriteDirs, codexRuntimeDir.Current)
	}
	if in.outerLayer {
		launchWriteDirs = AppendUniqueDir(launchWriteDirs, CanonicalSandboxPath(in.cwd))
	}
	// A temporary filesystem really is writable inside the namespace, so the
	// harness has to be told so. These lists become the harness's OWN view —
	// Claude Code's permission settings, Copilot's --add-dir roots — and a
	// scratch directory the operator mounted but the harness treats as
	// out-of-bounds would prompt on every write to space it was given on
	// purpose. Gated on the capability so this is a no-op on every
	// implementation ValidateTmpfsSupport has already refused.
	if sandboxpolicy.SupportsTmpfsMounts(in.sandboxImplementation, runtime.GOOS) {
		for _, mount := range sandboxSnapshotTmpfs(in.launchSandbox) {
			launchWriteDirs = AppendUniqueDir(launchWriteDirs, mount.Path)
		}
	}
	launchReadDirs := append(sandboxSnapshotDirs(in.launchSandbox, sandboxpolicy.AccessRead), launchContractReadDirs...)
	if in.params.ManagedInstructionsRoot != "" {
		launchReadDirs = AppendUniqueDir(launchReadDirs, in.params.ManagedInstructionsRoot)
	}
	launchDenyDirs := sandboxSnapshotDirs(in.launchSandbox, sandboxpolicy.AccessDeny)
	// Re-run the capability gate on the RULES THAT WILL BE EMITTED. The early
	// gate saw only the authored profile, but the launch contract above adds
	// reopens beneath a deny (workspace, Git admin paths, agent directories), so
	// an authored `deny ~` with no reopen of its own still renders as a split
	// policy and must be gated as one.
	// The LATE gate, over rendered rules rather than authored ones. It needs the
	// same unconfined arm as its early twin above: the rendered shape is no more
	// enforceable than the authored one when nothing is enforcing.
	if !in.outerLayer && !in.unconfined {
		if err := harness.ValidateSandboxReopenUnderDeny(in.h.Name, in.effectiveHarnessBuiltinMode,
			sandboxpolicy.GrantsFromDirs(launchReadDirs, launchWriteDirs, launchDenyDirs)); err != nil {
			return launchDirs{}, err
		}
	}
	// The mirror-image shape, for the one harness whose path grants have no
	// negative form: a deny nested INSIDE a granted root would be opened rather
	// than merely unrepresented. The grant set includes Copilot's automatic cwd
	// and system-temp grants, which no flag expresses and which are where this
	// shape usually appears. Only without an outer wall to enforce the deny.
	// The temp directory is resolved from the COMPOSED launch environment, not
	// from tclaude's own: a profile that sets TMPDIR moves the directory
	// Copilot grants, and a gate reading tclaude's ambient temp root would
	// inspect one directory while the pane was handed another. Same resolver
	// the Copilot sandbox baseline uses, for the same reason.
	var copilotGateEnvironment []sandboxpolicy.EnvironmentEntry
	if in.launchSandbox != nil {
		copilotGateEnvironment = sandboxpolicy.EnvironmentForLaunch(in.launchSandbox)
	}
	if err := harness.ValidateCopilotAddDirGrants(
		in.h.Name, in.cwd,
		CopilotLaunchTempDir(launchModelEnvironment(copilotGateEnvironment)),
		launchReadDirs, launchWriteDirs, launchDenyDirs, in.outerLayer); err != nil {
		return launchDirs{}, err
	}
	return launchDirs{
		codexRuntime: codexRuntimeDir, gitWrite: launchGitWriteDirs,
		write: launchWriteDirs, read: launchReadDirs, deny: launchDenyDirs,
	}, nil
}
