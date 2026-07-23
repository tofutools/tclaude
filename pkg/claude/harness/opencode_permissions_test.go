package harness

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestOpenCodeCatalogDefaultsAreFailClosed(t *testing.T) {
	h, err := Resolve(OpenCodeName)
	require.NoError(t, err)
	require.NotNil(t, h.Sandbox)
	require.NotNil(t, h.Approval)

	assert.Equal(t, OpenCodeSandboxAccessControl, h.Sandbox.DefaultMode())
	assert.Equal(t, OpenCodeApprovalDeny, h.Approval.DefaultPolicy())
	assert.False(t, h.ApprovalsReviewer)
	assert.Contains(t, h.Sandbox.ModeHelp(OpenCodeSandboxAccessControl), "not an OS sandbox")
	assert.Contains(t, h.Sandbox.ModeHelp(OpenCodeSandboxAccessControl), "cannot build, test, or use git")
}

func TestBuildOpenCodePermissionRulesAccessControl(t *testing.T) {
	protected, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(protected), 2)
	breakGlass := filepath.Join(protected[0], "debug")

	rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd:                 "/repo/service",
		Worktree:            "/repo",
		SandboxMode:         OpenCodeSandboxAccessControl,
		ApprovalPolicy:      OpenCodeApprovalAllowTools,
		ReadDirs:            []string{"/outside/read"},
		WriteDirs:           []string{"/outside/write"},
		DenyDirs:            []string{"/repo/service/secret"},
		BreakGlassReadDirs:  []string{breakGlass},
		BreakGlassWriteDirs: []string{filepath.Join(protected[1], "repair")},
		NetworkAccess:       sandboxpolicy.NetworkAccessInternet,
	})
	require.NoError(t, err)
	require.NotEmpty(t, rules)
	assert.Equal(t, OpenCodePermissionRule{Permission: "*", Pattern: "*", Action: "deny"}, rules[0])

	assertRuleBefore(t, rules,
		OpenCodePermissionRule{Permission: "read", Pattern: "service/*", Action: "allow"},
		OpenCodePermissionRule{Permission: "read", Pattern: "service/secret/*", Action: "deny"})
	assertRuleBefore(t, rules,
		OpenCodePermissionRule{Permission: "read", Pattern: "*.env.example", Action: "allow"},
		OpenCodePermissionRule{Permission: "read", Pattern: "service/secret/*", Action: "deny"})
	assert.Contains(t, rules,
		OpenCodePermissionRule{Permission: "edit", Pattern: "service/*", Action: "allow"})
	assert.Contains(t, rules,
		OpenCodePermissionRule{Permission: "external_directory", Pattern: "/outside/read/*", Action: "allow"})
	assert.Contains(t, rules,
		OpenCodePermissionRule{Permission: "edit", Pattern: "../outside/read/*", Action: "deny"})
	assert.Contains(t, rules,
		OpenCodePermissionRule{Permission: "edit", Pattern: "../outside/write/*", Action: "allow"})

	protectedDeny := OpenCodePermissionRule{
		Permission: "read",
		Pattern:    relativeOpenCodeTestPattern(t, "/repo", protected[0]) + "/*",
		Action:     "deny",
	}
	breakGlassAllow := OpenCodePermissionRule{
		Permission: "read",
		Pattern:    relativeOpenCodeTestPattern(t, "/repo", breakGlass) + "/*",
		Action:     "allow",
	}
	assertRuleBefore(t, rules, protectedDeny, breakGlassAllow)

	assert.Contains(t, rules, OpenCodePermissionRule{Permission: "bash", Pattern: "*", Action: "deny"})
	assert.Contains(t, rules, OpenCodePermissionRule{Permission: "glob", Pattern: "*", Action: "deny"})
	assert.Contains(t, rules, OpenCodePermissionRule{Permission: "webfetch", Pattern: "*", Action: "allow"})
	assert.Contains(t, rules, OpenCodePermissionRule{Permission: "websearch", Pattern: "*", Action: "allow"})
}

func TestBuildOpenCodePermissionRulesApprovalAndNetworkMatrix(t *testing.T) {
	tests := []struct {
		name     string
		approval string
		network  sandboxpolicy.NetworkAccess
		wantEdit string
		wantWeb  string
		wantEnv  string
	}{
		{"deny internet", OpenCodeApprovalDeny, sandboxpolicy.NetworkAccessInternet, "deny", "deny", "deny"},
		{"ask internet", OpenCodeApprovalAsk, sandboxpolicy.NetworkAccessInternet, "ask", "ask", "ask"},
		{"allow internet", OpenCodeApprovalAllowTools, sandboxpolicy.NetworkAccessInternet, "allow", "allow", "ask"},
		{"allow inherit", OpenCodeApprovalAllowTools, sandboxpolicy.NetworkAccessInherit, "allow", "ask", "ask"},
		{"allow none", OpenCodeApprovalAllowTools, sandboxpolicy.NetworkAccessNone, "allow", "deny", "ask"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
				Cwd:            "/repo",
				Worktree:       "/repo",
				SandboxMode:    OpenCodeSandboxAccessControl,
				ApprovalPolicy: tt.approval,
				NetworkAccess:  tt.network,
			})
			require.NoError(t, err)
			assert.Contains(t, rules, OpenCodePermissionRule{Permission: "edit", Pattern: "*", Action: tt.wantEdit})
			assert.Contains(t, rules, OpenCodePermissionRule{Permission: "webfetch", Pattern: "*", Action: tt.wantWeb})
			assert.Equal(t, tt.wantEnv, lastExactOpenCodeAction(rules, "read", "*.env"))
			assert.Equal(t, "deny", lastExactOpenCodeAction(rules, "bash", "*"))
		})
	}
}

func TestBuildOpenCodePermissionRulesOffStillAppliesApproval(t *testing.T) {
	rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd:            "/repo",
		Worktree:       "/repo",
		SandboxMode:    OpenCodeSandboxOff,
		ApprovalPolicy: OpenCodeApprovalAllowTools,
		NetworkAccess:  sandboxpolicy.NetworkAccessInternet,
	})
	require.NoError(t, err)

	assert.Equal(t, "allow", lastExactOpenCodeAction(rules, "read", "*"))
	assert.Equal(t, "allow", lastExactOpenCodeAction(rules, "edit", "*"))
	assert.Equal(t, "allow", lastExactOpenCodeAction(rules, "external_directory", "*"))
	assert.Equal(t, "ask", lastExactOpenCodeAction(rules, "bash", "*"),
		"even off+allow-tools must not mint automatic arbitrary commands")
}

func TestBuildOpenCodePermissionRulesUsesRootForNonGitWorktree(t *testing.T) {
	rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd:            "/home/operator/project",
		Worktree:       "/",
		SandboxMode:    OpenCodeSandboxAccessControl,
		ApprovalPolicy: OpenCodeApprovalDeny,
	})
	require.NoError(t, err)
	assert.Contains(t, rules, OpenCodePermissionRule{
		Permission: "read", Pattern: "home/operator/project/*", Action: "allow",
	})
}

func TestBuildOpenCodePermissionRulesRejectsUnrepresentableInputs(t *testing.T) {
	_, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd:            "/repo",
		Worktree:       "/repo",
		SandboxMode:    OpenCodeSandboxAccessControl,
		ApprovalPolicy: OpenCodeApprovalDeny,
		ReadBaseline:   "minimal",
	})
	require.ErrorContains(t, err, "legacy read baseline")

	_, err = BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd:            "/repo",
		Worktree:       "/repo",
		SandboxMode:    OpenCodeSandboxAccessControl,
		ApprovalPolicy: OpenCodeApprovalDeny,
		ReadDirs:       []string{"/tmp/project*"},
	})
	require.ErrorContains(t, err, "unrepresentable wildcard")

	_, err = BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd:            "/repo",
		Worktree:       "/repo",
		SandboxMode:    OpenCodeSandboxAccessControl,
		ApprovalPolicy: "",
	})
	require.ErrorContains(t, err, "approval policy is required")
}

func TestBuildOpenCodePermissionRulesRejectsWildcardMetacharactersInRoots(t *testing.T) {
	for _, metachar := range []string{"*", "?", "[", "]", "{", "}", "!", `\`} {
		t.Run(metachar, func(t *testing.T) {
			_, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
				Cwd:            "/repo",
				Worktree:       "/repo",
				SandboxMode:    OpenCodeSandboxAccessControl,
				ApprovalPolicy: OpenCodeApprovalDeny,
				DenyDirs:       []string{"/repo/private" + metachar + "cache"},
			})
			require.ErrorContains(t, err, "unrepresentable wildcard metacharacter")
		})
	}
}

func TestBuildOpenCodePermissionRulesReopensOrdinaryDenyBySpecificity(t *testing.T) {
	rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd:            "/repo",
		Worktree:       "/repo",
		SandboxMode:    OpenCodeSandboxAccessControl,
		ApprovalPolicy: OpenCodeApprovalDeny,
		ReadDirs:       []string{"/repo/private/public"},
		DenyDirs:       []string{"/repo/private"},
	})
	require.NoError(t, err)

	assert.Equal(t, "allow", lastMatchingOpenCodeAction(rules, "read", "private/public/nested/file.txt"))
	assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "edit", "private/public/nested/file.txt"))
	assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "read", "private/sibling/file.txt"))
	assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "read", "private/public/nested/config.env"))
	assert.Equal(t, "allow", lastMatchingOpenCodeAction(rules, "read", "private/public/nested/config.env.example"))
}

func assertRuleBefore(t *testing.T, rules []OpenCodePermissionRule, before, after OpenCodePermissionRule) {
	t.Helper()
	left, right := -1, -1
	for i, rule := range rules {
		if rule == before {
			left = i
		}
		if rule == after {
			right = i
		}
	}
	require.NotEqual(t, -1, left, "missing before rule: %#v", before)
	require.NotEqual(t, -1, right, "missing after rule: %#v", after)
	assert.Less(t, left, right)
}

func lastExactOpenCodeAction(rules []OpenCodePermissionRule, permission, pattern string) string {
	action := ""
	for _, rule := range rules {
		if rule.Permission == permission && rule.Pattern == pattern {
			action = rule.Action
		}
	}
	return action
}

func lastMatchingOpenCodeAction(rules []OpenCodePermissionRule, permission, pattern string) string {
	action := ""
	for _, rule := range rules {
		if openCodeWildcardMatch(permission, rule.Permission) &&
			openCodeWildcardMatch(pattern, rule.Pattern) {
			action = rule.Action
		}
	}
	return action
}

// openCodeWildcardMatch mirrors OpenCode v1.18.4's permission matcher for the
// path patterns exercised here: regex syntax is literal, while * and ? span
// path separators. The command-only trailing " *" special case is irrelevant.
func openCodeWildcardMatch(value, pattern string) bool {
	value = strings.ReplaceAll(value, `\`, "/")
	pattern = strings.ReplaceAll(pattern, `\`, "/")
	expression := regexp.QuoteMeta(pattern)
	expression = strings.ReplaceAll(expression, `\*`, ".*")
	expression = strings.ReplaceAll(expression, `\?`, ".")
	return regexp.MustCompile("(?s)^" + expression + "$").MatchString(value)
}

func relativeOpenCodeTestPattern(t *testing.T, worktree, path string) string {
	t.Helper()
	relative, err := filepath.Rel(worktree, path)
	require.NoError(t, err)
	return filepath.ToSlash(relative)
}
