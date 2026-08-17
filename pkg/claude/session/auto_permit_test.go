package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The accept table is the whole of what auto-permit will answer. Pin its shape:
// a slug per entry, compile-time keys, and no wildcard.
func TestAutoPermitAccepts_Shape(t *testing.T) {
	require.Contains(t, autoPermitAccepts, "EnterWorktree",
		"the EnterWorktree safety check is the prompt this exists for")
	for tool, accept := range autoPermitAccepts {
		assert.NotEmptyf(t, accept.slug, "%s: needs a consenting slug", tool)
		assert.NotEmptyf(t, accept.keys, "%s: needs accept keys", tool)
	}
	assert.NotContains(t, autoPermitAccepts, "", "there is no catch-all entry")
}

// The decision gate: a prompt is answered only for a KNOWN tool, on an agent
// the operator explicitly granted the matching slug, in a session with a pane.
// Anything else is a silent no-op that leaves the prompt waiting for the human.
func TestAutoPermitAcceptFor_RequiresGrant(t *testing.T) {
	setupAutoPermitTestDB(t)
	const conv = "conv-1"
	seedAutoPermitAgent(t, conv)
	state := &SessionState{ID: "sess-1", ConvID: conv, TmuxSession: "tcl-1"}

	_, ok := autoPermitAcceptFor(state, "EnterWorktree")
	assert.False(t, ok, "no grant, no answer — the default for every agent")

	require.NoError(t, db.GrantAgentPermission(conv, PermAutoPermitEnterWorktree, "human"))

	keys, ok := autoPermitAcceptFor(state, "EnterWorktree")
	assert.True(t, ok, "the grant IS the operator's standing consent")
	assert.Equal(t, []string{"Enter"}, keys)

	_, ok = autoPermitAcceptFor(state, "Bash")
	assert.False(t, ok, "consent is per named prompt, never a blanket accept")

	_, ok = autoPermitAcceptFor(&SessionState{ID: "sess-1", ConvID: conv}, "EnterWorktree")
	assert.False(t, ok, "no pane, nothing to press")

	_, ok = autoPermitAcceptFor(nil, "EnterWorktree")
	assert.False(t, ok)
}

func setupAutoPermitTestDB(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
}

// seedAutoPermitAgent enrolls conv as an agent, since a permission grant is
// keyed on the stable agent id behind it.
func seedAutoPermitAgent(t *testing.T, convID string) {
	t.Helper()
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
}
