package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TclaudeLayerLaunchContract carries writable paths required by the launched
// harness itself rather than granted by the operator's sandbox profile.
type TclaudeLayerLaunchContract struct {
	HarnessName       string
	WriteDirs         []string
	ProfileFilesystem []sandboxpolicy.FilesystemGrant
}

// ResolveTclaudeLayer verifies the host capability before a launch is
// committed. Callers record the returned verdict even when verification fails.
func ResolveTclaudeLayer() (string, harness.LaunchOSSandbox, error) {
	binary, err := resolveBwrapBinary()
	if err != nil {
		return "", harness.LaunchOSSandbox{
			State:  "off",
			Source: "tclaude-layer unavailable",
		}, err
	}
	return binary, TclaudeLayerLaunchOSSandbox(), nil
}

// TclaudeLayerLaunchOSSandbox records the layer's current partial fidelity:
// filesystem mounts are enforced, while ambient host Unix sockets remain
// connectable until TCL-752 adds the allowlisted network/socket posture.
func TclaudeLayerLaunchOSSandbox() harness.LaunchOSSandbox {
	return harness.LaunchOSSandbox{
		State:      "on",
		Source:     "tclaude-layer (bubblewrap; ambient host Unix sockets reachable)",
		Unverified: true,
	}
}

// WrapTclaudeLayer renders one effective profile and applies the resulting
// mount plan around the complete harness command.
func WrapTclaudeLayer(
	binary string,
	effective sandboxpolicy.EffectiveProfile,
	contract TclaudeLayerLaunchContract,
	harnessCommand string,
) (string, error) {
	plan, err := sandboxpolicy.RenderMountPlan(effective)
	if err != nil {
		return "", fmt.Errorf("render mount plan: %w", err)
	}
	phase0WriteDirs, err := tclaudeLayerPhase0WriteDirs(contract, effective)
	if err != nil {
		return "", err
	}
	if err := validateTclaudeLayerHarnessStateRules(
		phase0WriteDirs[0],
		contract.ProfileFilesystem,
	); err != nil {
		return "", err
	}
	breakGlassPaths := make([]string, 0, len(effective.BreakGlassFilesystem))
	for _, grant := range effective.BreakGlassFilesystem {
		breakGlassPaths = append(breakGlassPaths, grant.Path)
	}
	return bwrapCommand(binary, phase0WriteDirs, breakGlassPaths, plan, harnessCommand)
}

// bwrapArgs applies the launch contract, ordered MountPlan, and fixed
// bubblewrap hygiene. Policy/profile interpretation belongs before this
// boundary.
func bwrapArgs(
	phase0WriteDirs, breakGlassPaths []string,
	plan sandboxpolicy.MountPlan,
) ([]string, error) {
	args := []string{
		"--die-with-parent",
		// Give the wrapped process a new terminal session so a copied tmux
		// client or TIOCSTI-capable process cannot inject input into the host
		// pane shell outside this namespace.
		"--new-session",
		// Start from a readable, non-writable host view. Explicit plan entries
		// follow and shadow this baseline.
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		// Never share the host's scratch directory by default.
		"--tmpfs", "/tmp",
	}
	// Class 1 baseline: the harness process itself runs inside this wall,
	// unlike the harness-native sandboxes which confine only tools. Reopen only its
	// launch-contract state/workspace paths here; protected children are hidden
	// on top next. An ordinary ancestor hide triggers a narrower repair during
	// plan replay.
	existingPhase0WriteDirs := make([]string, 0, len(phase0WriteDirs))
	for i, path := range phase0WriteDirs {
		path = filepath.Clean(path)
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("launch-contract write entry %d has non-absolute path %q", i, path)
		}
		exists, err := bwrapBindSourceExists(path)
		if err != nil {
			return nil, fmt.Errorf("launch-contract write entry %d source %q: %w", i, path, err)
		}
		if exists {
			args = append(args, "--bind", path, path)
			existingPhase0WriteDirs = append(existingPhase0WriteDirs, path)
		}
	}
	// Class 3 baseline: protected state is hidden after the harness root is
	// writable.
	// EffectiveProfile deliberately excludes the protected-root baseline:
	// ordinary rules may never name these paths, while acknowledged
	// break-glass reopens arrive later in the plan. Establish the hides first
	// so normal launches cannot read private control/harness state and the
	// ordered plan can still reopen exactly what was acknowledged.
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return nil, fmt.Errorf("resolve protected sandbox roots: %w", err)
	}
	for _, writeDir := range phase0WriteDirs {
		for _, protected := range protectedRoots {
			if sandboxpolicy.PathContainsOrEqual(protected, writeDir) {
				return nil, fmt.Errorf(
					"tclaude-layer launch-contract path %q is at or below protected root %q",
					writeDir,
					protected,
				)
			}
		}
	}
	for _, root := range protectedRoots {
		args = append(args, "--tmpfs", root)
	}
	breakGlass := make(map[string]bool, len(breakGlassPaths))
	for _, path := range breakGlassPaths {
		breakGlass[filepath.Clean(path)] = true
	}
	// Class 2: replay the policy plan exactly as rendered. Repair mounts do not
	// reorder or deduplicate plan entries; they preserve the higher-precedence
	// launch contract after an ordinary ancestor hide.
	for i, entry := range plan.Entries {
		path := filepath.Clean(entry.Path)
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("mount plan entry %d has non-absolute path %q", i, entry.Path)
		}
		switch entry.Mode {
		case sandboxpolicy.MountRO:
			exists, err := bwrapBindSourceExists(path)
			if err != nil {
				return nil, fmt.Errorf("mount plan entry %d source %q: %w", i, path, err)
			}
			if !exists {
				continue
			}
			args = append(args, "--ro-bind", path, path)
			if !breakGlass[path] {
				args = appendTclaudeLayerProtectedRehides(args, path, protectedRoots)
			}
		case sandboxpolicy.MountRW:
			exists, err := bwrapBindSourceExists(path)
			if err != nil {
				return nil, fmt.Errorf("mount plan entry %d source %q: %w", i, path, err)
			}
			if !exists {
				continue
			}
			args = append(args, "--bind", path, path)
			if !breakGlass[path] {
				args = appendTclaudeLayerProtectedRehides(args, path, protectedRoots)
			}
		case sandboxpolicy.MountHide:
			args = append(args, "--tmpfs", path)
			args = appendTclaudeLayerContractRepairs(
				args,
				path,
				existingPhase0WriteDirs,
				protectedRoots,
			)
		default:
			return nil, fmt.Errorf("mount plan entry %d has invalid mode %d", i, entry.Mode)
		}
	}
	// Class 4: host tmux control is never profile- or break-glass-reachable.
	// This hide must be last: unlike ProtectedPaths, an ordinary profile may
	// grant a parent of the socket directory, and a later bind would otherwise
	// reopen the host tmux server.
	tmuxSocketDir, err := clcommon.TclaudeTmuxSocketDir()
	if err != nil {
		return nil, fmt.Errorf("resolve tclaude tmux socket directory: %w", err)
	}
	args = append(args, "--tmpfs", tmuxSocketDir)
	return args, nil
}

func appendTclaudeLayerProtectedRehides(
	args []string,
	mountedPath string,
	protectedRoots []string,
) []string {
	for _, protected := range protectedRoots {
		if sandboxpolicy.PathContainsOrEqual(mountedPath, protected) ||
			sandboxpolicy.PathContainsOrEqual(protected, mountedPath) {
			args = append(args, "--tmpfs", protected)
		}
	}
	return args
}

func appendTclaudeLayerContractRepairs(
	args []string,
	hide string,
	existingWriteDirs, protectedRoots []string,
) []string {
	repaired := make([]string, 0, len(existingWriteDirs))
	for _, writeDir := range existingWriteDirs {
		// An exact or narrower policy row wins. The launch seam refuses an
		// ordinary profile row at/below the harness state root separately;
		// workspace and agent-directory rows retain normal plan precedence.
		if hide != writeDir && sandboxpolicy.PathContainsOrEqual(hide, writeDir) {
			args = append(args, "--bind", writeDir, writeDir)
			repaired = append(repaired, writeDir)
		}
	}
	// A repaired parent bind would cover an earlier protected child hide.
	// Restore every overlapping protected root after all repairs so protected
	// state still outranks launch-contract authority. A later break-glass plan
	// entry remains able to reopen its acknowledged path.
	for _, protected := range protectedRoots {
		for _, writeDir := range repaired {
			if sandboxpolicy.PathContainsOrEqual(writeDir, protected) ||
				sandboxpolicy.PathContainsOrEqual(protected, writeDir) {
				args = append(args, "--tmpfs", protected)
				break
			}
		}
	}
	return args
}

func validateTclaudeLayerHarnessStateRules(
	stateRoot string,
	profileFilesystem []sandboxpolicy.FilesystemGrant,
) error {
	for _, grant := range profileFilesystem {
		if sandboxpolicy.PathContainsOrEqual(stateRoot, grant.Path) {
			return fmt.Errorf(
				"tclaude-layer profile filesystem rule %q (%s) is at or below harness state root %q; refusing a launch that cannot persist harness state",
				grant.Path,
				grant.Access,
				stateRoot,
			)
		}
	}
	return nil
}

func tclaudeLayerPhase0WriteDirs(
	contract TclaudeLayerLaunchContract,
	effective sandboxpolicy.EffectiveProfile,
) ([]string, error) {
	stateRoot, err := tclaudeLayerHarnessStateRoot(contract.HarnessName)
	if err != nil {
		return nil, err
	}
	candidates := append([]string{stateRoot}, contract.WriteDirs...)
	agentDirectoryNames := make(map[string]bool, len(effective.AgentDirectories))
	for _, name := range effective.AgentDirectories {
		agentDirectoryNames[name] = true
	}
	for _, entry := range effective.Environment {
		if agentDirectoryNames[entry.Name] {
			candidates = append(candidates, entry.Value)
		}
	}
	out := make([]string, 0, len(candidates))
	seen := make(map[string]bool, len(candidates))
	for _, path := range candidates {
		path = filepath.Clean(strings.TrimSpace(path))
		if path == "." || !filepath.IsAbs(path) {
			return nil, fmt.Errorf("tclaude-layer launch-contract path %q is not absolute", path)
		}
		if resolved, resolveErr := filepath.EvalSymlinks(path); resolveErr == nil {
			path = filepath.Clean(resolved)
		}
		if !seen[path] {
			seen[path] = true
			out = append(out, path)
		}
	}
	return out, nil
}

func tclaudeLayerHarnessStateRoot(harnessName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory for harness state: %w", err)
	}
	switch harnessName {
	case harness.DefaultName:
		return filepath.Join(home, ".claude"), nil
	case harness.CodexName:
		if root := strings.TrimSpace(os.Getenv("CODEX_HOME")); root != "" {
			return root, nil
		}
		return filepath.Join(home, ".codex"), nil
	default:
		return "", fmt.Errorf("tclaude-layer has no launch-contract state root for harness %q", harnessName)
	}
}

// ValidateTclaudeLayerHarness refuses topologies where the wrapped pane does
// not contain the process that executes tools.
func ValidateTclaudeLayerHarness(harnessName string) error {
	if harnessName == harness.OpenCodeName {
		return fmt.Errorf("tclaude-layer does not yet support OpenCode: its tool-executing server runs outside the wrapped pane")
	}
	return nil
}

func bwrapBindSourceExists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		// Policy authoring intentionally permits future paths. Never create one
		// on the host merely to make a bind possible.
		return false, nil
	default:
		return false, err
	}
}

func bwrapCommand(
	binary string,
	phase0WriteDirs, breakGlassPaths []string,
	plan sandboxpolicy.MountPlan,
	harnessCommand string,
) (string, error) {
	args, err := bwrapArgs(phase0WriteDirs, breakGlassPaths, plan)
	if err != nil {
		return "", err
	}
	command := clcommon.ShellQuoteArg(binary)
	for _, arg := range args {
		command += " " + clcommon.ShellQuoteArg(arg)
	}
	// Harness spawners return a safe shell command rather than argv. Keep that
	// contract intact inside the namespace instead of reparsing it here.
	command += " -- sh -c " + clcommon.ShellQuoteArg(harnessCommand)
	return command, nil
}
