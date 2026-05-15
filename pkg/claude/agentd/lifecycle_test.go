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
		spawnContextMsgID int64
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
			// No startup-context message at all → agent sits idle.
			name:        "no startup context tells the agent to wait",
			alias:       "worker",
			groupName:   "alpha",
			mustContain: []string{"Wait for the first instruction"},
			mustOmit:    []string{"inbox read"},
		},
		{
			// A briefing with a task brief → point at the inbox message
			// and tell the agent to act on the brief.
			name:              "task brief points at the inbox message and says act",
			alias:             "worker",
			groupName:         "alpha",
			spawnContextMsgID: 42,
			hasInitialMessage: true,
			mustContain:       []string{"message #42", "tclaude agent inbox read 42", "act on the brief"},
			mustOmit:          []string{"Wait for the first instruction"},
		},
		{
			// A briefing with only the group's shared context (no task
			// brief) → point at the inbox message, then tell it to wait.
			name:              "group context only points at the inbox message then waits",
			alias:             "worker",
			groupName:         "alpha",
			spawnContextMsgID: 7,
			hasInitialMessage: false,
			mustContain:       []string{"message #7", "tclaude agent inbox read 7", "wait for the"},
			mustOmit:          []string{"act on the brief"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSpawnWelcome(tt.alias, tt.role, tt.descr, tt.groupName,
				tt.spawnContextMsgID, tt.hasInitialMessage, tt.worktreePath, tt.worktreeBranch)
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
// enforces for cross-agent slash follow-ups. (The startup briefing
// itself rides in via the inbox, not the welcome, so it is free to
// be multi-line — but the welcome that announces it is not.)
func TestBuildSpawnWelcome_SingleLineNoControlChars(t *testing.T) {
	cases := []struct {
		spawnContextMsgID int64
		hasInitialMessage bool
	}{
		{0, false},  // no briefing
		{42, true},  // briefing with a task brief
		{7, false},  // briefing with group context only
	}
	for _, c := range cases {
		got := buildSpawnWelcome(
			"my-agent",
			"reviewer",
			"reviews PRs and posts notes",
			"reviewers",
			c.spawnContextMsgID,
			c.hasInitialMessage,
			"/home/dev/monorepo/services/api-feature-x",
			"feature-x",
		)
		assert.False(t, strings.ContainsAny(got, "\n\t\r"), "welcome must be a single line, got %q", got)
		assert.True(t, isValidFollowUp(got), "welcome should pass isValidFollowUp; got %q", got)
	}
}
