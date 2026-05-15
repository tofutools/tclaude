package agentd

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestBuildSpawnWelcome_IncludesIdentityFields confirms the welcome
// composition surfaces every identity field that's set, and skips
// the ones that aren't, so the new agent gets a single-line summary
// of who it is. The exact wording is not load-bearing — we just
// guard against a future refactor accidentally dropping a field.
func TestBuildSpawnWelcome_IncludesIdentityFields(t *testing.T) {
	tests := []struct {
		name              string
		alias             string
		role              string
		descr             string
		groupName         string
		hasInitialMessage bool
		worktreePath      string
		worktreeBranch    string
		mustContain       []string
		mustOmit          []string
	}{
		{
			name:      "all fields",
			alias:     "tclaude-devs-product-owner",
			role:      "product-owner",
			descr:     "receives feature requests",
			groupName: "tclaude-devs",
			mustContain: []string{
				"tclaude-devs-product-owner",
				"product-owner",
				"receives feature requests",
				"tclaude-devs",
				"[system:",
				"]",
			},
		},
		{
			name:      "alias + group only",
			alias:     "worker",
			groupName: "alpha",
			mustContain: []string{"worker", "alpha"},
			mustOmit:   []string{"role:", "Descr:", "worktree"},
		},
		{
			name:        "no alias, no group",
			mustContain: []string{"spawned by the human"},
			mustOmit:    []string{"as ", "in group ", "worktree"},
		},
		{
			name:           "sub-repo worktree",
			alias:          "worker",
			groupName:      "alpha",
			worktreePath:   "/home/dev/monorepo/cat/actual-repo-feature-x",
			worktreeBranch: "feature-x",
			mustContain: []string{
				"/home/dev/monorepo/cat/actual-repo-feature-x",
				"feature-x",
				"worktree",
			},
		},
		{
			name:         "worktree path without branch",
			alias:        "worker",
			worktreePath: "/home/dev/monorepo/cat/actual-repo-wt",
			mustContain:  []string{"/home/dev/monorepo/cat/actual-repo-wt", "worktree"},
			mustOmit:     []string{"(branch "},
		},
		{
			// Without an initial message the agent is told to sit idle.
			name:        "no initial message tells the agent to wait",
			alias:       "worker",
			groupName:   "alpha",
			mustContain: []string{"Wait for the first instruction"},
			mustOmit:    []string{"follow in the next message"},
		},
		{
			// With one queued the welcome must NOT tell it to wait —
			// the initial message IS the first instruction.
			name:              "initial message replaces the wait line",
			alias:             "worker",
			groupName:         "alpha",
			hasInitialMessage: true,
			mustContain:       []string{"follow in the next message"},
			mustOmit:          []string{"Wait for the first instruction"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSpawnWelcome(tt.alias, tt.role, tt.descr, tt.groupName,
				tt.hasInitialMessage, tt.worktreePath, tt.worktreeBranch)
			for _, s := range tt.mustContain {
				assert.Contains(t, got, s, "welcome should contain %q", s)
			}
			for _, s := range tt.mustOmit {
				assert.NotContains(t, got, s, "welcome should NOT contain %q", s)
			}
		})
	}
}

// TestBuildSpawnWelcome_SingleLineNoControlChars guards the
// invariant that the welcome can be safely passed through tmux
// send-keys. Newlines / tabs would each become a submit and split
// the prompt into multiple turns — same gate isValidFollowUp
// enforces for cross-agent slash follow-ups.
func TestBuildSpawnWelcome_SingleLineNoControlChars(t *testing.T) {
	for _, hasInitialMessage := range []bool{false, true} {
		got := buildSpawnWelcome(
			"my-agent",
			"reviewer",
			"reviews PRs and posts notes",
			"reviewers",
			hasInitialMessage,
			"/home/dev/monorepo/services/api-feature-x",
			"feature-x",
		)
		assert.False(t, strings.ContainsAny(got, "\n\t\r"), "welcome must be a single line, got %q", got)
		assert.True(t, isValidFollowUp(got), "welcome should pass isValidFollowUp; got %q", got)
	}
}
