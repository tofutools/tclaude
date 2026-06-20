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
		agentName         string
		role              string
		descr             string
		groupName         string
		spawnContextMsgID int64
		hasInitialMessage bool
		worktreePath      string
		worktreeBranch    string
		spawnedBy         string
		mustContain       []string
		mustOmit          []string
	}{
		{
			name:      "all fields",
			agentName: "tclaude-devs-product-owner",
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
			name:        "name + group only",
			agentName:   "worker",
			groupName:   "alpha",
			mustContain: []string{"worker", "alpha"},
			mustOmit:    []string{"role:", "Descr:", "worktree"},
		},
		{
			name:        "no name, no group",
			mustContain: []string{"spawned by the human"},
			mustOmit:    []string{"as ", "in group ", "worktree"},
		},
		{
			// An agent-initiated spawn: the welcome attributes the
			// spawning agent by name, NOT "the human".
			name:        "agent spawner is attributed by name",
			agentName:   "worker",
			groupName:   "alpha",
			spawnedBy:   "tclaude-PO",
			mustContain: []string{"spawned by tclaude-PO"},
			mustOmit:    []string{"spawned by the human"},
		},
		{
			// Spawner whose name couldn't be resolved: resolveSpawnerTitle
			// hands buildSpawnWelcome "another agent" rather than ""; the
			// welcome must still not claim the human did it.
			name:        "unresolved agent spawner falls back to 'another agent'",
			agentName:   "worker",
			groupName:   "alpha",
			spawnedBy:   "another agent",
			mustContain: []string{"spawned by another agent"},
			mustOmit:    []string{"spawned by the human"},
		},
		{
			name:           "sub-repo worktree",
			agentName:      "worker",
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
			agentName:    "worker",
			worktreePath: "/home/dev/monorepo/cat/actual-repo-wt",
			mustContain:  []string{"/home/dev/monorepo/cat/actual-repo-wt", "worktree"},
			mustOmit:     []string{"(branch "},
		},
		{
			// No startup-context message at all → agent sits idle.
			name:        "no startup context tells the agent to wait",
			agentName:   "worker",
			groupName:   "alpha",
			mustContain: []string{"Wait for the first instruction"},
			mustOmit:    []string{"inbox read"},
		},
		{
			// A briefing with a task brief → point at the inbox message
			// and tell the agent to act on the brief.
			name:              "task brief points at the inbox message and says act",
			agentName:         "worker",
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
			agentName:         "worker",
			groupName:         "alpha",
			spawnContextMsgID: 7,
			hasInitialMessage: false,
			mustContain:       []string{"message #7", "tclaude agent inbox read 7", "wait for the"},
			mustOmit:          []string{"act on the brief"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildSpawnWelcome(tt.agentName, tt.role, tt.descr, tt.groupName,
				tt.spawnContextMsgID, tt.hasInitialMessage, tt.worktreePath, tt.worktreeBranch,
				tt.spawnedBy)
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
		spawnedBy         string
	}{
		{0, false, ""},           // no briefing, human spawner
		{42, true, ""},           // briefing with a task brief, human spawner
		{7, false, ""},           // briefing with group context only
		{42, true, "tclaude-PO"}, // agent-attributed welcome
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
			c.spawnedBy,
		)
		assert.False(t, strings.ContainsAny(got, "\n\t\r"), "welcome must be a single line, got %q", got)
		assert.True(t, isValidFollowUp(got), "welcome should pass isValidFollowUp; got %q", got)
	}
}

// TestBuildSpawnLaunchPrompt covers the launch-enrollment prompt builder:
// inline the briefing when it's short, fall back to the single-line pointer
// welcome otherwise. The fallback must be byte-identical to buildSpawnWelcome
// so the legacy and over-cap paths stay consistent.
func TestBuildSpawnLaunchPrompt(t *testing.T) {
	const (
		name      = "worker"
		role      = "reviewer"
		descr     = "reviews PRs"
		groupName = "alpha"
	)

	t.Run("short task brief is inlined", func(t *testing.T) {
		body := "Your task brief:\n\nAudit the auth module and report back."
		got := buildSpawnLaunchPrompt(name, role, descr, groupName, 42, true,
			body, "", "", "", 2000)
		assert.Contains(t, got, body, "the whole briefing rides inline, newlines and all")
		assert.Contains(t, got, "[system:", "still opens with the system welcome")
		assert.Contains(t, got, "tclaude agent", "keeps the coordination pointer")
		assert.Contains(t, got, "message #42", "notes the inbox copy by id")
		assert.Contains(t, got, "act on the brief", "a task brief tells the agent to act")
		assert.NotContains(t, got, "inbox read", "an inlined brief needs no inbox round-trip")
	})

	t.Run("group-context-only inline tells the agent to wait", func(t *testing.T) {
		body := "Group \"alpha\" startup context — shared guidance:\n\nSmall commits, tests first."
		got := buildSpawnLaunchPrompt(name, role, descr, groupName, 7, false,
			body, "", "", "", 2000)
		assert.Contains(t, got, body, "context inlined")
		assert.Contains(t, got, "wait for the first instruction", "no brief → wait")
		assert.NotContains(t, got, "act on the brief", "no brief → don't say act")
	})

	t.Run("over-cap brief falls back to the pointer welcome", func(t *testing.T) {
		body := "Your task brief:\n\n" + strings.Repeat("x", 5000)
		got := buildSpawnLaunchPrompt(name, role, descr, groupName, 42, true,
			body, "", "", "", 2000)
		want := buildSpawnWelcome(name, role, descr, groupName, 42, true, "", "", "")
		assert.Equal(t, want, got, "over-cap must be byte-identical to the pointer welcome")
		assert.NotContains(t, got, "xxxxx", "the long brief must not be inlined")
	})

	t.Run("inlining disabled (<=0) falls back to the pointer welcome", func(t *testing.T) {
		body := "Your task brief:\n\nshort"
		got := buildSpawnLaunchPrompt(name, role, descr, groupName, 42, true,
			body, "", "", "", 0)
		want := buildSpawnWelcome(name, role, descr, groupName, 42, true, "", "", "")
		assert.Equal(t, want, got, "disabled inlining must use the pointer welcome")
	})

	t.Run("empty body falls back to the pointer welcome", func(t *testing.T) {
		got := buildSpawnLaunchPrompt(name, role, descr, groupName, 0, false,
			"   ", "", "", "", 2000)
		want := buildSpawnWelcome(name, role, descr, groupName, 0, false, "", "", "")
		assert.Equal(t, want, got, "no briefing → pointer welcome (which says wait)")
		assert.Contains(t, got, "Wait for the first instruction")
	})

	t.Run("inline without inbox id omits the inbox note", func(t *testing.T) {
		// Edge case: the inbox insert failed (spawnContextMsgID <= 0) but the
		// caller still has the body. Inlining is then the agent's only copy, so
		// we must not claim a non-existent inbox message.
		body := "Your task brief:\n\ndo the thing"
		got := buildSpawnLaunchPrompt(name, role, descr, groupName, 0, true,
			body, "", "", "", 2000)
		assert.Contains(t, got, body, "brief still inlined")
		assert.Contains(t, got, "act on the brief")
		assert.NotContains(t, got, "saved to your inbox", "no inbox id → no inbox-copy claim")
		assert.NotContains(t, got, "message #", "no inbox id → no message-number reference")
	})
}
