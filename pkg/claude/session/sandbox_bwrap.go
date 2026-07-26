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
	HarnessName string
	WriteDirs   []string
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
	return bwrapCommand(binary, phase0WriteDirs, plan, harnessCommand)
}

// bwrapArgs applies the launch contract, ordered MountPlan, and fixed
// bubblewrap hygiene. Policy/profile interpretation belongs before this
// boundary.
func bwrapArgs(phase0WriteDirs []string, plan sandboxpolicy.MountPlan) ([]string, error) {
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
	// Phase 0: the harness process itself runs inside this wall, unlike the
	// harness-native sandboxes which confine only tools. Reopen only its
	// launch-contract state/workspace paths here; protected children are hidden
	// on top in phase 1.
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
		}
	}
	// Phase 1: protected state is hidden after the harness root is writable.
	// EffectiveProfile deliberately excludes the protected-root baseline:
	// ordinary rules may never name these paths, while acknowledged
	// break-glass reopens arrive later in the plan. Establish the hides first
	// so normal launches cannot read private control/harness state and the
	// ordered plan can still reopen exactly what was acknowledged.
	protectedRoots, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return nil, fmt.Errorf("resolve protected sandbox roots: %w", err)
	}
	for _, root := range protectedRoots {
		args = append(args, "--tmpfs", root)
	}
	// Phase 2: replay the policy plan exactly as rendered.
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
		case sandboxpolicy.MountRW:
			exists, err := bwrapBindSourceExists(path)
			if err != nil {
				return nil, fmt.Errorf("mount plan entry %d source %q: %w", i, path, err)
			}
			if !exists {
				continue
			}
			args = append(args, "--bind", path, path)
		case sandboxpolicy.MountHide:
			args = append(args, "--tmpfs", path)
		default:
			return nil, fmt.Errorf("mount plan entry %d has invalid mode %d", i, entry.Mode)
		}
	}
	// Phase 3: host tmux control is never profile- or break-glass-reachable.
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

func bwrapCommand(binary string, phase0WriteDirs []string, plan sandboxpolicy.MountPlan, harnessCommand string) (string, error) {
	args, err := bwrapArgs(phase0WriteDirs, plan)
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
