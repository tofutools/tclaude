package agentd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func openCodePermissionJSONForLaunch(
	cwd, harnessBuiltinMode, approvalPolicy, toolGovernance string,
	snapshot *sandboxpolicy.Snapshot,
) (string, error) {
	var err error
	cwd, err = resolveOpenCodeLaunchCwd(cwd)
	if err != nil {
		return "", err
	}
	spec := harness.OpenCodePermissionSpec{
		Cwd:                cwd,
		Worktree:           harness.OpenCodeWorktree(cwd),
		HarnessBuiltinMode: harnessBuiltinMode,
		ApprovalPolicy:     approvalPolicy,
		ToolGovernance:     toolGovernance,
	}
	if snapshot != nil {
		filesystem, err := sandboxpolicy.FilesystemForLaunch(snapshot.Effective)
		if err != nil {
			return "", err
		}
		for _, grant := range filesystem {
			// OpenCode's soft tool governance names paths its tools will see,
			// so a remapped grant contributes its sandbox path. Unremapped rules
			// are unaffected: their guest path is the host path.
			switch grant.Access {
			case sandboxpolicy.AccessRead:
				spec.ReadDirs = append(spec.ReadDirs, grant.GuestPath())
			case sandboxpolicy.AccessWrite:
				spec.WriteDirs = append(spec.WriteDirs, grant.GuestPath())
			case sandboxpolicy.AccessDeny:
				spec.DenyDirs = append(spec.DenyDirs, grant.Path)
			}
		}
		spec.NetworkAccess = snapshot.Effective.NetworkAccess
	}
	rules, err := harness.BuildOpenCodePermissionRules(spec)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(rules)
	if err != nil {
		return "", fmt.Errorf("encode OpenCode permission rules: %w", err)
	}
	return string(encoded), nil
}

func resolveOpenCodeLaunchCwd(cwd string) (string, error) {
	if strings.TrimSpace(cwd) != "" {
		return cwd, nil
	}
	// A blank spawn cwd means "inherit agentd's cwd" throughout the existing
	// launch path. Make it explicit before compiling rules and addressing the
	// server API so every OpenCode component uses the same directory identity.
	resolved, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve inherited OpenCode working directory: %w", err)
	}
	return resolved, nil
}
