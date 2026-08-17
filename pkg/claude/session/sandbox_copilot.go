package session

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Copilot's side of the tclaude-layer launch contract (TCL-978).
//
// This file is the ADAPTER between a launch and harness.CopilotSandboxBaseline:
// it collects the launch's own facts (its environment, its temp directory, the
// agentd endpoints it may need, the two executables it runs) and hands them to
// the catalog. It resolves no Copilot path itself. That separation is what
// makes the catalog the single authority: a second resolver here would be a
// second answer to "where does Copilot keep its state", and the two would drift
// on exactly the platform where it matters (macOS, where the package cache
// moves to ~/Library/Caches/copilot while the device-id cache stays XDG-shaped).

// ValidateTclaudeLayerHarnessPosture is the harness-native posture gate every
// tclaude-layer launch boundary runs before committing a launch: the direct
// `tclaude session new` path and the daemon spawn guard both call it, so the
// two can never disagree about whether a posture is launchable.
//
// It is the seam where "tclaude's outer layer is the enforcement boundary"
// stops being an assumption. Most harnesses have nothing to check here — their
// TclaudeLayerMode is a real launch flag that turns their own sandbox off, so
// the launch itself establishes the posture. Copilot has no such flag it can
// use — `--no-sandbox` exists but is inert without `--experimental`, which
// tclaude-layer refuses because it hands the pane the opposite lever — so the
// posture has to be VERIFIED against its configuration instead, and a launch
// that cannot verify it is refused rather than started on an unchecked claim.
//
// environment is the launch's own composed environment (pre-injection, the
// same value the model-transport gate reads), so the settings file inspected
// here is the one the launch will actually use.
func ValidateTclaudeLayerHarnessPosture(
	h *harness.Harness,
	environment []sandboxpolicy.EnvironmentEntry,
	extraArgs []string,
) error {
	if h == nil || h.Name != harness.CopilotName {
		return nil
	}
	if err := harness.CopilotTclaudeLayerExtraArgRefusal(extraArgs); err != nil {
		return err
	}
	launch := launchModelEnvironment(environment)
	home := strings.TrimSpace(launch["HOME"])
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf(
				"resolve home directory for the Copilot sandbox posture check: %w", err)
		}
		home = resolved
	}
	state, err := harness.ResolveCopilotInnerSandbox(
		func(name string) string { return launch[name] }, home)
	if err != nil {
		return err
	}
	return harness.ValidateCopilotTclaudeLayerInnerSandbox(state)
}

// copilotTclaudeLayerBaselineInput assembles the launch's facts for
// harness.CopilotSandboxBaseline.
//
// cwd is passed as the catalog's Workspace so the catalog can REFUSE a grant
// that would cover the working directory. That is not a redundant check against
// the launch contract's own cwd write grant: the contract grants the workspace
// deliberately and scoped, while a catalog row covering it would mean an
// environment variable had silently widened a Copilot state path over the
// repository.
func copilotTclaudeLayerBaselineInput(
	input TclaudeLayerLaunchInput,
	cwd string,
) (harness.CopilotBaselineInput, error) {
	environment := launchModelEnvironment(input.Environment)
	getenv := func(name string) string { return environment[name] }

	home := strings.TrimSpace(environment["HOME"])
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return harness.CopilotBaselineInput{}, fmt.Errorf(
				"resolve home directory for the Copilot sandbox baseline: %w", err)
		}
		home = resolved
	}

	sockets := make([]string, 0, 3)
	for _, socket := range sandboxpolicy.AgentdSocketFloor() {
		// Canonicalized the same way the OpenCode requirement producer
		// canonicalizes them, so
		// a symlinked home does not produce an endpoint the catalog dedupes
		// against a different spelling than the launch actually binds.
		if socket = CanonicalTclaudeLayerGeneratedPath(socket); socket != "" {
			sockets = append(sockets, socket)
		}
	}

	return harness.CopilotBaselineInput{
		Home:              home,
		Getenv:            getenv,
		TempDir:           CopilotLaunchTempDir(environment),
		AgentdSockets:     sockets,
		CopilotExecutable: copilotLaunchExecutable(environment),
		TclaudeExecutable: copilotTclaudeExecutable(),
		Workspace:         cwd,
	}, nil
}

// CopilotLaunchTempDir resolves the temp directory the LAUNCH will see, which
// is not necessarily tclaude's own: a profile that sets TMPDIR moves it, and
// granting tclaude's temp directory instead would hand the agent a path it
// never uses while withholding the one it does.
//
// An empty result omits the feature-conditional temp row entirely, which the
// catalog supports and which is the correct outcome for a launch whose
// environment names no temp directory.
//
// Exported because a SECOND consumer needs the identical answer for the
// identical reason. Copilot grants its temp directory automatically, with no
// flag, so harness.ValidateCopilotAddDirGrants has to know which directory that
// is before it can tell whether a profile's deny sits inside it. Resolving that
// independently — say, from tclaude's own os.TempDir() — would make the gate
// inspect one directory while the pane was granted another, and a deny nested
// under a profile-relocated TMPDIR would sail through it. One resolver, one
// answer, for the same reason COPILOT_HOME has one.
func CopilotLaunchTempDir(environment map[string]string) string {
	if dir := strings.TrimSpace(environment["TMPDIR"]); dir != "" {
		return filepath.Clean(dir)
	}
	return filepath.Clean(os.TempDir())
}

// copilotLaunchExecutable resolves the `copilot` binary against the LAUNCH's
// PATH rather than tclaude's, for the same reason as the temp directory: the
// two can differ, and the catalog documents that the PATH which matters is the
// launch's.
//
// A miss is not an error here. The catalog omits the row for an empty value,
// and the launch then fails at exec with Copilot's own "command not found"
// — a clearer message than a sandbox refusal that talks about grant catalogs.
func copilotLaunchExecutable(environment map[string]string) string {
	binary := "copilot"
	path := strings.TrimSpace(environment["PATH"])
	if path == "" {
		if resolved, err := exec.LookPath(binary); err == nil {
			return resolved
		}
		return ""
	}
	for _, dir := range filepath.SplitList(path) {
		if strings.TrimSpace(dir) == "" {
			continue
		}
		candidate := filepath.Join(dir, binary)
		info, err := os.Stat(candidate)
		if err != nil || info.IsDir() || info.Mode().Perm()&0o111 == 0 {
			continue
		}
		return candidate
	}
	return ""
}

// copilotTclaudeExecutable is the tclaude binary a Copilot hook callback and an
// in-agent `tclaude agent …` call execute. It is resolved from the RUNNING
// process rather than from PATH: the callback the hook installer writes names
// this binary, so a PATH lookup could grant a different tclaude than the one
// that will actually be invoked.
func copilotTclaudeExecutable() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		return resolved
	}
	return filepath.Clean(executable)
}
