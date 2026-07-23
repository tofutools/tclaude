package harness

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// OpenCodePermissionRule is OpenCode v1.18.4's stored per-session permission
// rule. OpenCode flattens the agent and session rules and applies the last
// matching rule, so order is part of the security contract.
type OpenCodePermissionRule struct {
	Permission string `json:"permission"`
	Pattern    string `json:"pattern"`
	Action     string `json:"action"`
}

// OpenCodePermissionSpec is the validated launch posture rendered into one
// session ruleset. Paths must already be canonical absolute directories.
type OpenCodePermissionSpec struct {
	Cwd                 string
	Worktree            string
	SandboxMode         string
	ApprovalPolicy      string
	ReadDirs            []string
	WriteDirs           []string
	DenyDirs            []string
	ReadBaseline        string
	BreakGlassReadDirs  []string
	BreakGlassWriteDirs []string
	NetworkAccess       sandboxpolicy.NetworkAccess
}

const (
	openCodeActionAllow = "allow"
	openCodeActionAsk   = "ask"
	openCodeActionDeny  = "deny"
)

type openCodePathAccess uint8

const (
	openCodePathRead openCodePathAccess = iota + 1
	openCodePathWrite
	openCodePathDeny
)

// OpenCodeWorktree returns the root OpenCode v1.18.4 uses when forming
// read/edit permission patterns: the git worktree root, or "/" outside git.
func OpenCodeWorktree(cwd string) string {
	out, err := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return string(filepath.Separator)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return string(filepath.Separator)
	}
	if resolved, err := filepath.Abs(root); err == nil {
		root = resolved
	}
	return filepath.Clean(root)
}

// BuildOpenCodePermissionRules renders the complete, ordered session policy.
// It is intentionally allowlist-shaped: an unknown or newly added OpenCode
// tool remains caught by the leading deny-all rule until it is audited.
func BuildOpenCodePermissionRules(spec OpenCodePermissionSpec) ([]OpenCodePermissionRule, error) {
	sandboxMode, err := (openCodeSandbox{}).ValidateMode(spec.SandboxMode)
	if err != nil || sandboxMode == "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("opencode sandbox mode is required")
	}
	approval, err := (openCodeApproval{}).ValidatePolicy(spec.ApprovalPolicy)
	if err != nil || approval == "" {
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("opencode approval policy is required")
	}
	if strings.TrimSpace(spec.ReadBaseline) != "" {
		return nil, fmt.Errorf("opencode access control cannot accept legacy read baseline %q", spec.ReadBaseline)
	}
	network, err := sandboxpolicy.NormalizeNetworkAccess(spec.NetworkAccess)
	if err != nil {
		return nil, err
	}
	cwd, err := validateOpenCodePolicyRoot("cwd", spec.Cwd)
	if err != nil {
		return nil, err
	}
	worktree := spec.Worktree
	if strings.TrimSpace(worktree) == "" {
		worktree = OpenCodeWorktree(cwd)
	}
	worktree, err = validateOpenCodePolicyRoot("worktree", worktree)
	if err != nil {
		return nil, err
	}

	rules := []OpenCodePermissionRule{{
		Permission: "*", Pattern: "*", Action: openCodeActionDeny,
	}}
	if sandboxMode == OpenCodeSandboxOff {
		rules = appendOpenCodeOffRules(rules, approval, network)
		return appendOpenCodeEnvReadRules(rules, approval, "."), nil
	}

	ordinary := map[string]openCodePathAccess{cwd: openCodePathWrite}
	if err := mergeOpenCodeRoots(ordinary, spec.ReadDirs, openCodePathRead); err != nil {
		return nil, err
	}
	if err := mergeOpenCodeRoots(ordinary, spec.WriteDirs, openCodePathWrite); err != nil {
		return nil, err
	}
	if err := mergeOpenCodeRoots(ordinary, spec.DenyDirs, openCodePathDeny); err != nil {
		return nil, err
	}
	// OpenCode applies the last matching rule, not the most-specific matching
	// rule. Render ordinary allows and denies together from broadest to
	// narrowest so a specific read/write grant can reopen beneath a broader
	// deny. Reassert the environment-file restrictions after every allow:
	// later narrow denies still dominate them, while a narrow reopen does not
	// accidentally grant unrestricted access to environment files.
	rules, err = appendOpenCodeRootRules(rules, worktree, ordinary, approval, true)
	if err != nil {
		return nil, err
	}

	protected, err := sandboxpolicy.ProtectedPaths()
	if err != nil {
		return nil, err
	}
	protectedRoots := make(map[string]openCodePathAccess, len(protected))
	if err := mergeOpenCodeRoots(protectedRoots, protected, openCodePathDeny); err != nil {
		return nil, err
	}
	rules, err = appendOpenCodeRootRules(rules, worktree, protectedRoots, approval, false)
	if err != nil {
		return nil, err
	}

	breakGlass := map[string]openCodePathAccess{}
	if err := mergeOpenCodeRoots(breakGlass, spec.BreakGlassReadDirs, openCodePathRead); err != nil {
		return nil, err
	}
	if err := mergeOpenCodeRoots(breakGlass, spec.BreakGlassWriteDirs, openCodePathWrite); err != nil {
		return nil, err
	}
	rules, err = appendOpenCodeRootRules(rules, worktree, breakGlass, approval, false)
	if err != nil {
		return nil, err
	}

	// These tools cannot express a path boundary that matches the authored
	// filesystem policy. The catch-all already denies them; explicit terminal
	// rules make the no-shell contract reviewable and resilient to refactors.
	for _, permission := range []string{"bash", "glob", "grep", "lsp", "task", "skill"} {
		rules = append(rules, OpenCodePermissionRule{
			Permission: permission, Pattern: "*", Action: openCodeActionDeny,
		})
	}
	rules = appendOpenCodeWebRules(rules, approval, network)
	return rules, nil
}

func appendOpenCodeOffRules(rules []OpenCodePermissionRule, approval string, network sandboxpolicy.NetworkAccess) []OpenCodePermissionRule {
	for _, permission := range []string{"read", "glob", "grep"} {
		rules = append(rules, OpenCodePermissionRule{
			Permission: permission, Pattern: "*", Action: openCodeActionAllow,
		})
	}
	action := openCodeApprovalAction(approval)
	rules = append(rules,
		OpenCodePermissionRule{Permission: "edit", Pattern: "*", Action: action},
		OpenCodePermissionRule{Permission: "external_directory", Pattern: "*", Action: action},
	)
	bashAction := openCodeActionDeny
	if approval != OpenCodeApprovalDeny {
		bashAction = openCodeActionAsk
	}
	rules = append(rules, OpenCodePermissionRule{
		Permission: "bash", Pattern: "*", Action: bashAction,
	})
	return appendOpenCodeWebRules(rules, approval, network)
}

func appendOpenCodeWebRules(rules []OpenCodePermissionRule, approval string, network sandboxpolicy.NetworkAccess) []OpenCodePermissionRule {
	action := openCodeActionDeny
	switch network {
	case sandboxpolicy.NetworkAccessInternet:
		action = openCodeApprovalAction(approval)
	case sandboxpolicy.NetworkAccessInherit:
		if approval != OpenCodeApprovalDeny {
			action = openCodeActionAsk
		}
	case sandboxpolicy.NetworkAccessNone:
	}
	for _, permission := range []string{"webfetch", "websearch"} {
		rules = append(rules, OpenCodePermissionRule{
			Permission: permission, Pattern: "*", Action: action,
		})
	}
	return rules
}

func appendOpenCodeEnvReadRules(
	rules []OpenCodePermissionRule,
	approval, relativeRoot string,
) []OpenCodePermissionRule {
	action := openCodeActionDeny
	if approval != OpenCodeApprovalDeny {
		action = openCodeActionAsk
	}
	prefix := ""
	if relativeRoot != "." {
		prefix = strings.TrimSuffix(relativeRoot, "/") + "/"
	}
	return append(rules,
		OpenCodePermissionRule{Permission: "read", Pattern: prefix + "*.env", Action: action},
		OpenCodePermissionRule{Permission: "read", Pattern: prefix + "*.env.*", Action: action},
		OpenCodePermissionRule{Permission: "read", Pattern: prefix + "*.env.example", Action: openCodeActionAllow},
	)
}

func openCodeApprovalAction(approval string) string {
	switch approval {
	case OpenCodeApprovalAsk:
		return openCodeActionAsk
	case OpenCodeApprovalAllowTools:
		return openCodeActionAllow
	default:
		return openCodeActionDeny
	}
}

func mergeOpenCodeRoots(dst map[string]openCodePathAccess, roots []string, access openCodePathAccess) error {
	for _, root := range roots {
		canonical, err := validateOpenCodePolicyRoot("permission root", root)
		if err != nil {
			return err
		}
		current := dst[canonical]
		switch {
		case access == openCodePathDeny:
			dst[canonical] = access
		case current == openCodePathDeny:
		case access == openCodePathWrite || current == 0:
			dst[canonical] = access
		}
	}
	return nil
}

func validateOpenCodePolicyRoot(label, root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" || !filepath.IsAbs(root) {
		return "", fmt.Errorf("opencode %s must be an absolute path, got %q", label, root)
	}
	root = filepath.Clean(root)
	// Permission roots are embedded in OpenCode wildcard patterns. Refuse the
	// complete reserved glob-syntax set rather than trying to escape it: a
	// literal operator-authored deny that is reinterpreted as a pattern would
	// silently fail open. Backslash is included because OpenCode normalizes it
	// to slash before matching.
	if strings.ContainsAny(filepath.ToSlash(root), `*?[]{}!\`) {
		return "", fmt.Errorf("opencode %s %q contains an unrepresentable wildcard metacharacter", label, root)
	}
	return root, nil
}

func appendOpenCodeRootRules(
	rules []OpenCodePermissionRule,
	worktree string,
	roots map[string]openCodePathAccess,
	approval string,
	constrainEnv bool,
) ([]OpenCodePermissionRule, error) {
	paths := make([]string, 0, len(roots))
	for root := range roots {
		paths = append(paths, root)
	}
	sort.Slice(paths, func(i, j int) bool {
		leftDepth := openCodePathDepth(paths[i])
		rightDepth := openCodePathDepth(paths[j])
		if leftDepth != rightDepth {
			return leftDepth < rightDepth
		}
		return paths[i] < paths[j]
	})
	for _, root := range paths {
		relative, err := filepath.Rel(worktree, root)
		if err != nil {
			return nil, fmt.Errorf("make opencode permission path relative to worktree: %w", err)
		}
		relative = filepath.ToSlash(relative)
		readAction := openCodeActionAllow
		editAction := openCodeActionDeny
		externalAction := openCodeActionAllow
		switch roots[root] {
		case openCodePathWrite:
			editAction = openCodeApprovalAction(approval)
		case openCodePathDeny:
			readAction = openCodeActionDeny
			externalAction = openCodeActionDeny
		}
		for _, pattern := range openCodeRelativePatterns(relative) {
			rules = append(rules,
				OpenCodePermissionRule{Permission: "read", Pattern: pattern, Action: readAction},
				OpenCodePermissionRule{Permission: "edit", Pattern: pattern, Action: editAction},
			)
		}
		rules = append(rules, OpenCodePermissionRule{
			Permission: "external_directory",
			Pattern:    openCodeAbsoluteDescendantPattern(root),
			Action:     externalAction,
		})
		if constrainEnv && roots[root] != openCodePathDeny {
			rules = appendOpenCodeEnvReadRules(rules, approval, relative)
		}
	}
	return rules, nil
}

func openCodeRelativePatterns(relative string) []string {
	if relative == "." {
		return []string{".", "*"}
	}
	return []string{relative, strings.TrimSuffix(relative, "/") + "/*"}
}

func openCodeAbsoluteDescendantPattern(root string) string {
	root = filepath.ToSlash(filepath.Clean(root))
	if root == "/" {
		return "/*"
	}
	return strings.TrimSuffix(root, "/") + "/*"
}

func openCodePathDepth(path string) int {
	path = strings.Trim(filepath.ToSlash(filepath.Clean(path)), "/")
	if path == "" {
		return 0
	}
	return strings.Count(path, "/") + 1
}
