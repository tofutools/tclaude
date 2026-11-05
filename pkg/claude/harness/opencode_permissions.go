package harness

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/common/skillroots"
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
	Cwd                string
	Worktree           string
	HarnessBuiltinMode string
	ApprovalPolicy     string
	ToolGovernance     string
	ReadDirs           []string
	WriteDirs          []string
	DenyDirs           []string
	ReadBaseline       string
	NetworkAccess      sandboxpolicy.NetworkAccess
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
//
// The policy always ends by denying OpenCode's task tool, so sub-agents are
// off under tclaude. See openCodeSubagentDenyRule for why.
//
// The exception is `--tools all` (openCodeAllToolsRules): no deny-all and no
// task deny, so every tool runs and sub-agents work.
func BuildOpenCodePermissionRules(spec OpenCodePermissionSpec) ([]OpenCodePermissionRule, error) {
	rules, err := buildOpenCodePermissionRules(spec)
	if err != nil {
		return nil, err
	}
	if policy, _ := ParseOpenCodeToolGovernance(spec.ToolGovernance); policy.AllTools() {
		return rules, nil
	}
	return append(rules, openCodeSubagentDenyRule), nil
}

// openCodeAllToolsRules is the `--tools all` policy. It has no deny-all
// baseline and no tool allowlist: whatever OpenCode would run, runs. What it
// does carry is only what the operator chose — approval for edits and
// leaving the worktree, a closed network for the web tools, and the per-tool
// entries — and every one of those is an allow or a deny, never an ask
// (ValidateOpenCodeToolsCombination refuses approval ask). That matters
// because OpenCode copies a parent session's deny rules, and only those, into
// a sub-agent's session: the denies reach every helper, and what is left
// falls to OpenCode's own defaults, which allow it, exactly as for the agent.
func openCodeAllToolsRules(
	approval string,
	network sandboxpolicy.NetworkAccess,
	policy OpenCodeToolsPolicy,
) []OpenCodePermissionRule {
	action := openCodeApprovalAction(approval)
	rules := []OpenCodePermissionRule{
		{Permission: "edit", Pattern: "*", Action: action},
		{Permission: "external_directory", Pattern: "*", Action: action},
	}
	web := openCodeActionAllow
	if network == sandboxpolicy.NetworkAccessNone {
		web = openCodeActionDeny
	}
	for _, permission := range []string{"webfetch", "websearch"} {
		rules = append(rules, OpenCodePermissionRule{Permission: permission, Pattern: "*", Action: web})
	}
	if policy.DenyQuestion {
		rules = append(rules, OpenCodePermissionRule{Permission: "question", Pattern: "*", Action: openCodeActionDeny})
	}
	if policy.DenyPlan {
		for _, permission := range []string{"plan_enter", "plan_exit"} {
			rules = append(rules, OpenCodePermissionRule{Permission: permission, Pattern: "*", Action: openCodeActionDeny})
		}
	}
	if policy.AllowEnv {
		// Allows are not inherited, so a sub-agent's .env read falls to its
		// own OpenCode default: explore reads, general asks. Never wider than
		// the agent, which is what the operator chose here.
		return append(rules,
			OpenCodePermissionRule{Permission: "read", Pattern: "*.env", Action: openCodeActionAllow},
			OpenCodePermissionRule{Permission: "read", Pattern: "*.env.*", Action: openCodeActionAllow},
		)
	}
	return append(rules,
		OpenCodePermissionRule{Permission: "read", Pattern: "*.env", Action: openCodeActionDeny},
		OpenCodePermissionRule{Permission: "read", Pattern: "*.env.*", Action: openCodeActionDeny},
		OpenCodePermissionRule{Permission: "read", Pattern: "*.env.example", Action: openCodeActionAllow},
	)
}

// openCodeSubagentDenyRule turns OpenCode's task tool (its sub-agents) off.
//
// tclaude's policy is a per-session ruleset, deliberately: the session rules
// are evaluated after every config layer (global, project, remote org, and
// per-agent config), so nothing the agent or a repo writes can widen them.
// But when the task tool creates a sub-agent session, OpenCode 1.18.6 copies
// only the parent session's deny and external_directory rules into it
// (deriveSubagentSessionPermission). The child inherits the leading deny-all
// rule and none of the allows, so it could not read, search, or run anything,
// and the parent saw "permission denied" for work it had delegated.
//
// Neither repair gives the child exactly the parent's authority. Moving the
// policy into config would let a colliding project or remote key reorder it
// and widen it; patching the child after creation races OpenCode's prompt
// loop, which snapshots the session permission when the child starts. A
// trailing deny with pattern "*" makes OpenCode hide the tool altogether
// (Permission.disabled), so the primary agent does that work itself.
//
// It is appended after tool governance on purpose: no governance setting can
// produce a working sub-agent, so letting one re-enable task would only bring
// the broken delegation back. Lift this once OpenCode can hand a child the
// parent's full session ruleset.
var openCodeSubagentDenyRule = OpenCodePermissionRule{
	Permission: "task", Pattern: "*", Action: openCodeActionDeny,
}

// OpenCodeSubagentsOffNote is the launch note every daemon OpenCode spawn (and its preview) carries.
const OpenCodeSubagentsOffNote = "OpenCode sub-agents (its task tool) are off under tclaude: " +
	"OpenCode gives a sub-agent only the deny rules of tclaude's permission policy, " +
	"so it could not read or search files. The main agent does that work itself. " +
	"--tools all turns every OpenCode tool on, sub-agents included."

// OpenCodeAllToolsNote is the launch note an OpenCode spawn with --tools all
// carries instead of OpenCodeSubagentsOffNote.
const OpenCodeAllToolsNote = "OpenCode --tools all: every OpenCode tool runs, including MCP and plugin tools, " +
	"ones tclaude has not audited, and sub-agents; only your approval, network, and per-tool choices are applied."

func buildOpenCodePermissionRules(spec OpenCodePermissionSpec) ([]OpenCodePermissionRule, error) {
	harnessBuiltinMode, err := (openCodeSandbox{}).ValidateMode(spec.HarnessBuiltinMode)
	if err != nil || harnessBuiltinMode == "" {
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
	toolGovernance := strings.TrimSpace(spec.ToolGovernance)
	if toolGovernance == "" {
		toolGovernance = (openCodeToolGovernance{}).DefaultPolicy()
	}
	toolGovernance, err = (openCodeToolGovernance{}).ValidatePolicy(toolGovernance)
	if err != nil {
		return nil, err
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

	if policy, _ := ParseOpenCodeToolGovernance(toolGovernance); policy.AllTools() {
		if err := ValidateOpenCodeToolsCombination(harnessBuiltinMode, approval, toolGovernance); err != nil {
			return nil, err
		}
		return openCodeAllToolsRules(approval, network, policy), nil
	}

	rules := []OpenCodePermissionRule{{
		Permission: "*", Pattern: "*", Action: openCodeActionDeny,
	}}
	if harnessBuiltinMode == OpenCodeSandboxOff {
		rules = appendOpenCodeOffRules(rules, approval, toolGovernance, network)
		return appendOpenCodeEnvReadRules(rules, approval, "."), nil
	}
	if harnessBuiltinMode == OpenCodeSandboxTclaudeLayer {
		rules = appendOpenCodeTclaudeLayerRules(
			rules, approval, toolGovernance, network,
		)
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

	skills, err := skillroots.All()
	if err != nil {
		return nil, err
	}
	skillRoots := make(map[string]openCodePathAccess, len(skills))
	if err := mergeOpenCodeRoots(skillRoots, skills, openCodePathRead); err != nil {
		return nil, err
	}
	// Skill discovery is part of the OpenCode access-control baseline, not an
	// operator-authored filesystem grant. Render it after ordinary roots so it
	// stays readable in every approval mode, while the read access type emits
	// explicit edit denies. Protected state is re-denied immediately below.
	rules, err = appendOpenCodeRootRules(rules, worktree, skillRoots, approval, false)
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

	// These tool permissions are separate from read/edit/external_directory
	// and cannot express the authored lexical filesystem boundary. Keep the
	// tools available despite that soft-sandbox limitation by default; the
	// independent tool-governance axis can instead make the whole block ask or
	// deny. Explicit rules are required after the leading catch-all deny.
	rules = appendOpenCodeToolRules(rules, toolGovernance)
	rules = appendOpenCodeWebRules(rules, approval, network)
	return rules, nil
}

func appendOpenCodeTclaudeLayerRules(
	rules []OpenCodePermissionRule,
	approval, toolGovernance string,
	network sandboxpolicy.NetworkAccess,
) []OpenCodePermissionRule {
	// The OS wall is authoritative in this mode, so do not compile the sandbox
	// profile's path grants into OpenCode's bypassable command filter. Keep the
	// rule set allowlist-shaped: explicitly enable the audited permission keys
	// while the leading catch-all continues to deny an unknown future tool.
	rules = append(rules,
		OpenCodePermissionRule{Permission: "read", Pattern: "*", Action: openCodeActionAllow},
		OpenCodePermissionRule{
			Permission: "edit", Pattern: "*", Action: openCodeApprovalAction(approval),
		},
		OpenCodePermissionRule{
			Permission: "external_directory", Pattern: "*", Action: openCodeApprovalAction(approval),
		},
	)
	rules = appendOpenCodeToolRules(rules, toolGovernance)
	return appendOpenCodeWebRules(rules, approval, network)
}

// openCodeGovernedTools are the built-in tools the tool-governance setting
// controls. OpenCode's task tool is absent: it is always denied.
var openCodeGovernedTools = []string{"bash", "glob", "grep", "lsp", "skill"}

func appendOpenCodeToolRules(
	rules []OpenCodePermissionRule,
	action string,
) []OpenCodePermissionRule {
	// task is governed by openCodeSubagentDenyRule, not here.
	for _, permission := range openCodeGovernedTools {
		rules = append(rules, OpenCodePermissionRule{
			Permission: permission, Pattern: "*", Action: action,
		})
	}
	return rules
}

func appendOpenCodeOffRules(
	rules []OpenCodePermissionRule,
	approval, toolGovernance string,
	network sandboxpolicy.NetworkAccess,
) []OpenCodePermissionRule {
	rules = append(rules, OpenCodePermissionRule{
		Permission: "read", Pattern: "*", Action: openCodeActionAllow,
	})
	action := openCodeApprovalAction(approval)
	rules = append(rules,
		OpenCodePermissionRule{Permission: "edit", Pattern: "*", Action: action},
		OpenCodePermissionRule{Permission: "external_directory", Pattern: "*", Action: action},
	)
	rules = appendOpenCodeToolRules(rules, toolGovernance)
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
		// OpenCode separately gates every out-of-worktree operation through
		// external_directory; relative read/edit matches are not sufficient.
		for _, pattern := range openCodeAbsolutePatterns(root) {
			rules = append(rules, OpenCodePermissionRule{
				Permission: "external_directory",
				Pattern:    pattern,
				Action:     externalAction,
			})
		}
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

func openCodeAbsolutePatterns(root string) []string {
	root = filepath.ToSlash(filepath.Clean(root))
	if root == "/" {
		return []string{"/", "/*"}
	}
	return []string{root, strings.TrimSuffix(root, "/") + "/*"}
}

func openCodePathDepth(path string) int {
	path = strings.Trim(filepath.ToSlash(filepath.Clean(path)), "/")
	if path == "" {
		return 0
	}
	return strings.Count(path, "/") + 1
}
