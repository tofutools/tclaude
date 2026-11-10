package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestOpenCodeToolGovernanceAllCanonicalSpelling(t *testing.T) {
	g := openCodeToolGovernance{}
	for raw, want := range map[string]string{
		"all":                                    "all",
		" all , question=deny ":                  "all,question=deny",
		"all,env=allow,plan=deny":                "all,plan=deny,env=allow",
		"all,question=allow,plan=allow,env=deny": "all",
		"allow":                                  "allow",
		"":                                       "",
	} {
		got, err := g.ValidatePolicy(raw)
		require.NoError(t, err, raw)
		assert.Equal(t, want, got, raw)
	}
	for _, bad := range []string{
		"everything", "allow,question=deny", "all,question", "all,question=ask",
		"all,bash=deny", "all,question=deny,question=allow",
	} {
		_, err := g.ValidatePolicy(bad)
		assert.Error(t, err, bad)
	}
	_, err := g.ValidatePolicy("deny,plan=deny")
	assert.ErrorContains(t, err, `needs the "all" base`)
}

func TestValidateOpenCodeToolsCombinationRefusesWhatSubagentsCannotInherit(t *testing.T) {
	for _, mode := range []string{OpenCodeSandboxTclaudeLayer, OpenCodeSandboxOff} {
		for _, approval := range []string{OpenCodeApprovalDeny, OpenCodeApprovalAllowTools} {
			assert.NoError(t, ValidateOpenCodeToolsCombination(mode, approval, "all"), mode+"/"+approval)
		}
		assert.ErrorContains(t,
			ValidateOpenCodeToolsCombination(mode, OpenCodeApprovalAsk, "all,question=deny"),
			"do not inherit ask rules")
	}
	assert.ErrorContains(t,
		ValidateOpenCodeToolsCombination(OpenCodeSandboxAccessControl, OpenCodeApprovalDeny, "all"),
		"tclaude-layer")
	assert.NoError(t, ValidateOpenCodeToolsCombination(OpenCodeSandboxAccessControl, OpenCodeApprovalAsk, "allow"),
		"narrower bases are unaffected")
}

// --tools all leaves no deny-all and no task deny, carries only allow and
// deny rules, and applies the per-tool entries.
func TestBuildOpenCodePermissionRulesAllTools(t *testing.T) {
	build := func(mode, approval, tools string, network sandboxpolicy.NetworkAccess) []OpenCodePermissionRule {
		t.Helper()
		rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
			Cwd: "/repo", Worktree: "/repo", HarnessBuiltinMode: mode,
			ApprovalPolicy: approval, ToolGovernance: tools, NetworkAccess: network,
		})
		require.NoError(t, err)
		return rules
	}

	rules := build(OpenCodeSandboxTclaudeLayer, OpenCodeApprovalAllowTools, "all", sandboxpolicy.NetworkAccessInherit)
	for _, rule := range rules {
		assert.NotEqual(t, "*", rule.Permission, "no deny-all baseline under all")
		assert.NotEqual(t, "task", rule.Permission, "sub-agents are not denied under all")
		assert.NotEqual(t, openCodeActionAsk, rule.Action, "an ask cannot reach a sub-agent")
	}
	assert.Equal(t, "allow", lastExactOpenCodeAction(rules, "edit", "*"))
	assert.Equal(t, "allow", lastExactOpenCodeAction(rules, "webfetch", "*"),
		"the web tools are allowed unless the network is closed")
	assert.Equal(t, "deny", lastMatchingOpenCodeAction(rules, "read", "app/.env"), ".env reads default to deny")
	assert.Empty(t, lastExactOpenCodeAction(rules, "question", "*"), "question is allowed by default")

	rules = build(OpenCodeSandboxOff, OpenCodeApprovalDeny, "all,question=deny,plan=deny,env=allow", sandboxpolicy.NetworkAccessNone)
	assert.Equal(t, "deny", lastExactOpenCodeAction(rules, "edit", "*"))
	assert.Equal(t, "deny", lastExactOpenCodeAction(rules, "external_directory", "*"))
	assert.Equal(t, "deny", lastExactOpenCodeAction(rules, "webfetch", "*"))
	for _, permission := range []string{"question", "plan_enter", "plan_exit"} {
		assert.Equal(t, "deny", lastExactOpenCodeAction(rules, permission, "*"), permission)
	}
	assert.Equal(t, "allow", lastMatchingOpenCodeAction(rules, "read", "app/.env"))

	_, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
		Cwd: "/repo", Worktree: "/repo", HarnessBuiltinMode: OpenCodeSandboxAccessControl,
		ApprovalPolicy: OpenCodeApprovalDeny, ToolGovernance: "all",
	})
	assert.ErrorContains(t, err, "access-control")
}

// The property all relies on: a sub-agent session receives only the parent's
// deny and external_directory rules (OpenCode 1.18.6), so every decision the
// policy makes must survive that filter. With no allow or ask rule left to
// lose except ones that restate OpenCode's own default (allow), the derived
// child decides every permission the way the parent does.
func TestOpenCodeAllToolsSurvivesSubagentDerivation(t *testing.T) {
	for _, tools := range []string{"all", "all,question=deny,plan=deny", "all,env=allow"} {
		for _, approval := range []string{OpenCodeApprovalDeny, OpenCodeApprovalAllowTools} {
			rules, err := BuildOpenCodePermissionRules(OpenCodePermissionSpec{
				Cwd: "/repo", Worktree: "/repo", HarnessBuiltinMode: OpenCodeSandboxTclaudeLayer,
				ApprovalPolicy: approval, ToolGovernance: tools,
			})
			require.NoError(t, err)
			var child []OpenCodePermissionRule
			for _, rule := range rules {
				if rule.Action == openCodeActionDeny || rule.Permission == "external_directory" {
					child = append(child, rule)
				}
			}
			for _, probe := range [][2]string{
				{"edit", "main.go"}, {"external_directory", "/elsewhere/x"}, {"webfetch", "*"},
				{"question", "*"}, {"plan_enter", "*"}, {"bash", "ls"}, {"read", "main.go"},
			} {
				parent := lastMatchingOpenCodeAction(rules, probe[0], probe[1])
				derived := lastMatchingOpenCodeAction(child, probe[0], probe[1])
				if parent == "" {
					parent = openCodeActionAllow // OpenCode's own default
				}
				if derived == "" {
					derived = openCodeActionAllow
				}
				assert.Equal(t, parent, derived, "%s/%s: %s %s", tools, approval, probe[0], probe[1])
			}
		}
	}
}

func TestOpenCodeToolGovernanceNotWiderWithEntries(t *testing.T) {
	h, ok := Get(OpenCodeName)
	require.True(t, ok)
	for _, tc := range []struct {
		limit, policy string
		want          bool
	}{
		{"all", "allow", true},
		{"allow", "all", false},
		{"all", "all,question=deny", true},
		{"all,question=deny", "all", false},
		{"all,question=deny", "all,question=deny,plan=deny", true},
		{"all", "all,env=allow", false},
		{"all,env=allow", "all", true},
		{"ask", "deny", true},
		{"deny", "ask", false},
	} {
		assert.Equal(t, tc.want, ToolGovernanceNotWider(h, tc.limit, tc.policy), "%s within %s", tc.policy, tc.limit)
	}
}
