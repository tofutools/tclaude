package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A realistic capture of the pane while Claude Code's EnterWorktree safety
// check is up: the question, then the choice options. The option label is the
// real string (see the dialog Claude Code renders) — a paraphrase here would
// pass the test while production never fired.
const enterWorktreePane = `● I'll move into the worktree.

Enter the worktree at "/home/dev/git/proj-wt"? This moves the session's working
directory and write access there, and loads project configuration (CLAUDE.md,
settings) from that location.

❯ 1. Yes
  2. No, and tell Claude what to do differently (esc)

claude-opus  12%  7d
`

// Every registered condition must satisfy the invariants the rest of the
// feature assumes: a storable name, a harness, and BOTH matchers populated —
// a condition with no pane evidence would auto-answer on a status projection
// alone, which is exactly what the design refuses to do.
func TestAutoPermitConditions_RegistryInvariants(t *testing.T) {
	require.NotEmpty(t, autoPermitConditions, "the registry must not be empty")
	seen := map[string]bool{}
	for _, c := range autoPermitConditions {
		name, err := db.NormalizeAutoPermitCondition(c.Name)
		require.NoErrorf(t, err, "condition %q must be storable", c.Name)
		assert.Equal(t, c.Name, name, "condition names are already normalized")
		assert.Falsef(t, seen[c.Name], "duplicate condition %q", c.Name)
		seen[c.Name] = true

		assert.NotEmptyf(t, c.Summary, "%s: needs an operator-facing summary", c.Name)
		assert.NotEmptyf(t, c.Harness, "%s: needs a harness", c.Name)
		assert.NotNilf(t, c.DetailMatch, "%s: needs a status-detail matcher", c.Name)
		assert.NotEmptyf(t, c.PaneRequire, "%s: needs pane evidence", c.Name)
		assert.NotEmptyf(t, c.AcceptKeys, "%s: needs accept keys", c.Name)
	}
}

func TestAutoPermitCondition_EnterWorktreeMatching(t *testing.T) {
	cond := lookupAutoPermitCondition("enter-worktree")
	require.NotNil(t, cond)

	// The hook reports the tool name; the legacy notification path reports the
	// question text. Both are the same prompt.
	assert.True(t, cond.matchesDetail(harness.DefaultName, "EnterWorktree"))
	assert.True(t, cond.matchesDetail(harness.DefaultName,
		`Enter the worktree at "/home/dev/git/proj-wt"?`))
	// A row that predates the harness column reads as the default harness
	// rather than being skipped.
	assert.True(t, cond.matchesDetail("", "EnterWorktree"))

	// Another harness's prompt is never this condition, whatever it says.
	assert.False(t, cond.matchesDetail(harness.CodexName, "EnterWorktree"))
	// Any other prompt is out of scope — this is not a blanket accept.
	assert.False(t, cond.matchesDetail(harness.DefaultName, "Bash"))
	assert.False(t, cond.matchesDetail(harness.DefaultName, "ExitWorktree"))
}

func TestAutoPermitCondition_PaneEvidence(t *testing.T) {
	cond := lookupAutoPermitCondition("enter-worktree")
	require.NotNil(t, cond)

	assert.True(t, cond.matchesPane(enterWorktreePane), "the live dialog matches")

	// A transcript of an ALREADY-ANSWERED prompt still contains the question,
	// but no live choice. Answering it would press Enter into the composer.
	answered := "Enter the worktree at \"/home/dev/git/proj-wt\"? …\n" +
		"● Entered worktree at /home/dev/git/proj-wt on branch wt-1.\n"
	assert.False(t, cond.matchesPane(answered),
		"a resolved prompt in the scrollback must not count as evidence")

	// A DIFFERENT dialog being up is not this condition, even though the pane
	// carries a live choice prompt.
	other := "Run `rm -rf build`?\n❯ 1. Yes\n  2. No, and tell Claude what to do differently (esc)\n"
	assert.False(t, cond.matchesPane(other), "another prompt is never auto-answered")

	// A failed capture reads as no evidence — the safe direction.
	assert.False(t, cond.matchesPane(""))
	assert.False(t, cond.matchesPane("   \n  "))
}

// A condition an operator consented to under an older build is inert, not a
// wildcard: the sweep must simply not find it.
func TestAutoPermitConditionFor_UnknownNameIsInert(t *testing.T) {
	row := &db.SessionRow{Harness: harness.DefaultName, StatusDetail: "EnterWorktree"}
	assert.Nil(t, autoPermitConditionFor(map[string]bool{"retired-condition": true}, row))
	assert.Nil(t, autoPermitConditionFor(nil, row), "no consent, no condition")
	assert.NotNil(t, autoPermitConditionFor(map[string]bool{"enter-worktree": true}, row))
}
