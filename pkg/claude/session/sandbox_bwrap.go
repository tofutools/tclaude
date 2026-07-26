package session

import (
	"fmt"
	"os"
	"path/filepath"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

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
	return binary, harness.LaunchOSSandbox{
		State:  "on",
		Source: "tclaude-layer (bubblewrap)",
	}, nil
}

// WrapTclaudeLayer renders one effective profile through the temporary
// profile→MountPlan seam and applies that plan around the complete harness
// command. Keeping the renderer call here gives TCL-751 one replacement site.
func WrapTclaudeLayer(binary string, effective sandboxpolicy.EffectiveProfile, harnessCommand string) (string, error) {
	plan, err := renderMountPlanInterim(effective)
	if err != nil {
		return "", fmt.Errorf("render mount plan: %w", err)
	}
	return bwrapCommand(binary, plan, harnessCommand)
}

// bwrapArgs renders only the ordered MountPlan plus fixed bubblewrap hygiene.
// Policy/profile interpretation belongs before this boundary.
func bwrapArgs(plan sandboxpolicy.MountPlan) ([]string, error) {
	args := []string{
		"--die-with-parent",
		// Start from a readable, non-writable host view. Explicit plan entries
		// follow and shadow this baseline.
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		// Never share the host's scratch directory by default.
		"--tmpfs", "/tmp",
	}
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
	return args, nil
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

func bwrapCommand(binary string, plan sandboxpolicy.MountPlan, harnessCommand string) (string, error) {
	args, err := bwrapArgs(plan)
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
