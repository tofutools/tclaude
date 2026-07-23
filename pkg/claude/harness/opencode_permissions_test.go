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
	require.NotNil(t, h.ToolGovernance)

	assert.Equal(t, OpenCodeSandboxAccessControl, h.Sandbox.DefaultMode())
	assert.Equal(t, OpenCodeApprovalDeny, h.Approval.DefaultPolicy())
	assert.Equal(t, OpenCodeToolsAllow, h.ToolGovernance.DefaultPolicy())
	assert.False(t, h.ApprovalsReviewer)
	assert.Contains(t, h.Sandbox.ModeHelp(OpenCodeSandboxAccessControl), "not an OS sandbox")
	assert.Contains(t, h.Sandbox.ModeHelp(OpenCodeSandboxAccessControl), "tools remain enabled")
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
		OpenCodePermissionRule{Permission: "read", Pattern: "service/*.env.example", Action: "allow"},
		OpenCodePermissionRule{Permission: "read", Pattern: "service/secret/*", Action: "deny"})
	assert.Contains(t, rules,
		OpenCodePermissionRule{Permission: "edit", Pattern: "service/*", Action: "allow"})
	assert.Contains(t, rules,
		OpenCodePermissionRule{Permission: "external_directory", Pattern: "/outside/read", Action: "allow"})
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

	for _, permission := range []string{"bash", "glob", "grep", "lsp", "task", "skill"} {
		assert.Contains(t, rules,
			OpenCodePermissionRule{Permission: permission, Pattern: "*", Action: "allow"})
	}
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
			assert.Equal(t, "allow", lastExactOpenCodeAction(rules, "bash", "*"))
		})
	}
}

func TestBuildOpenCodePermissionRulesToolGovernanceMatrix(t *testing.T) {
	for _, action := range []string{OpenCodeToolsAllow, OpenCodeToolsAsk, OpenCodeToolsDeny} {
		t.Run(action, func(t *testing.T) {
			rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
				Cwd:            "/repo",
				Worktree:       "/repo",
				SandboxMode:    OpenCodeSandboxAccessControl,
				ApprovalPolicy: OpenCodeApprovalDeny,
				ToolGovernance: action,
			})
			require.NoError(t, err)
			for _, permission := range []string{"bash", "glob", "grep", "lsp", "task", "skill"} {
				assert.Equal(t, action, lastExactOpenCodeAction(rules, permission, "*"), permission)
			}
			assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "read", "private.env"),
				"tool governance must not change disk/environment rules")
		})
	}

	rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd: "/repo", Worktree: "/repo",
		SandboxMode: OpenCodeSandboxAccessControl, ApprovalPolicy: OpenCodeApprovalDeny,
	})
	require.NoError(t, err)
	assert.Equal(t, OpenCodeToolsAllow, lastExactOpenCodeAction(rules, "bash", "*"),
		"blank remains backward-compatible")
}

func TestBuildOpenCodePermissionRulesToolDenyDoesNotChangeProtectedDisk(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", "")
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)

	protected, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)
	require.ElementsMatch(t, []string{
		filepath.Join(canonicalHome, ".claude", "sessions"),
		filepath.Join(canonicalHome, ".tclaude", "data"),
	}, protected)

	for _, approval := range []string{
		OpenCodeApprovalDeny,
		OpenCodeApprovalAsk,
		OpenCodeApprovalAllowTools,
	} {
		t.Run(approval, func(t *testing.T) {
			rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
				Cwd:            "/repo",
				Worktree:       "/repo",
				SandboxMode:    OpenCodeSandboxAccessControl,
				ApprovalPolicy: approval,
				ToolGovernance: OpenCodeToolsDeny,
			})
			require.NoError(t, err)

			for _, permission := range []string{"bash", "glob", "grep", "lsp", "task", "skill"} {
				assert.Equal(t, "deny", lastExactOpenCodeAction(rules, permission, "*"))
			}
			for _, root := range protected {
				protectedFile := filepath.Join(root, "private.json")
				assert.Equal(t, "deny", lastMatchingOpenCodeAction(
					rules, "read", relativeOpenCodeTestPattern(t, "/repo", protectedFile)))
				assert.Equal(t, "deny", lastMatchingOpenCodeAction(
					rules, "external_directory", protectedFile))
			}
		})
	}
}

func TestBuildOpenCodePermissionRulesAllowsSkillRootsReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CODEX_HOME", "")

	const worktree = "/repo"
	skillRoots := []string{
		filepath.Join(home, ".claude", "skills"),
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(home, ".codex", "skills"),
	}
	protected, err := sandboxpolicy.ProtectedPaths()
	require.NoError(t, err)

	for _, approval := range []string{
		OpenCodeApprovalDeny,
		OpenCodeApprovalAsk,
		OpenCodeApprovalAllowTools,
	} {
		t.Run(approval, func(t *testing.T) {
			rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
				Cwd:            worktree,
				Worktree:       worktree,
				SandboxMode:    OpenCodeSandboxAccessControl,
				ApprovalPolicy: approval,
				ToolGovernance: OpenCodeToolsAsk,
			})
			require.NoError(t, err)

			for _, root := range skillRoots {
				relativeRoot := relativeOpenCodeTestPattern(t, worktree, root)
				skillFile := relativeRoot + "/agent-coord/SKILL.md"

				assert.Equal(t, "allow", lastMatchingOpenCodeAction(rules, "read", relativeRoot))
				assert.Equal(t, "allow", lastMatchingOpenCodeAction(rules, "read", skillFile))
				assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "edit", relativeRoot))
				assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "edit", skillFile))
				assert.Equal(t, "allow", lastMatchingOpenCodeAction(rules, "external_directory", root))
				assert.Equal(t, "allow", lastMatchingOpenCodeAction(
					rules, "external_directory", filepath.Join(root, "agent-coord", "SKILL.md")))
			}

			for _, path := range []string{
				filepath.Join(home, ".claude"),
				filepath.Join(home, ".claude", "settings.json"),
			} {
				assert.Equal(t, "deny", lastMatchingOpenCodeAction(
					rules, "external_directory", path),
					"skill-root carve-out must not allow non-skill Claude paths")
			}

			for _, root := range protected {
				protectedFile := relativeOpenCodeTestPattern(t, worktree, filepath.Join(root, "whatever"))
				assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "read", protectedFile))
				assert.Equal(t, "deny", lastMatchingOpenCodeAction(
					rules, "external_directory", filepath.Join(root, "whatever")))
			}
		})
	}
}

func TestBuildOpenCodePermissionRulesOffStillAppliesApproval(t *testing.T) {
	rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd:            "/repo",
		Worktree:       "/repo",
		SandboxMode:    OpenCodeSandboxOff,
		ApprovalPolicy: OpenCodeApprovalAllowTools,
		ToolGovernance: OpenCodeToolsDeny,
		NetworkAccess:  sandboxpolicy.NetworkAccessInternet,
	})
	require.NoError(t, err)

	assert.Equal(t, "allow", lastExactOpenCodeAction(rules, "read", "*"))
	assert.Equal(t, "allow", lastExactOpenCodeAction(rules, "edit", "*"))
	assert.Equal(t, "allow", lastExactOpenCodeAction(rules, "external_directory", "*"))
	assert.Equal(t, "ask", lastExactOpenCodeAction(rules, "bash", "*"),
		"sandbox off keeps its existing approval behavior and ignores tool governance")
	assert.Equal(t, "allow", lastExactOpenCodeAction(rules, "glob", "*"),
		"sandbox off remains a no-containment posture")
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

	_, err = BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd:            "/repo",
		Worktree:       "/repo",
		SandboxMode:    OpenCodeSandboxAccessControl,
		ApprovalPolicy: OpenCodeApprovalDeny,
		ToolGovernance: "sometimes",
	})
	require.ErrorContains(t, err, "tool-governance")
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
		Cwd:            "/project",
		Worktree:       "/project",
		SandboxMode:    OpenCodeSandboxAccessControl,
		ApprovalPolicy: OpenCodeApprovalAsk,
		ReadDirs:       []string{"/project/secret/public"},
		DenyDirs:       []string{"/project/secret"},
	})
	require.NoError(t, err)

	assert.Equal(t, "allow", lastMatchingOpenCodeAction(rules, "read", "secret/public/nested/file.txt"))
	assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "edit", "secret/public/nested/file.txt"))
	assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "read", "secret/sibling/file.txt"))

	// A scoped environment rule for the reopened child must not reach back
	// into its denied parent or a denied sibling, even though OpenCode's *
	// wildcard crosses path separators.
	assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "read", "secret/x.env"))
	assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "read", "secret/config.env.example"))
	assert.Equal(t, "ask", lastMatchingOpenCodeAction(rules, "read", "secret/public/nested/config.env"))
	assert.Equal(t, "allow", lastMatchingOpenCodeAction(rules, "read", "secret/public/nested/config.env.example"))
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
