package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Scenario: a fully-provisioned live agent runs a one-shot headless
// claude invocation from its own Bash (`claude -p`, `claude mcp get`,
// …). The child process inherits TCLAUDE_SESSION_ID and fires
// SessionEnd(reason=other) through the global hooks, carrying its own
// throwaway conv-id — against the AGENT's session row.
//
// Production bite this pins: the conv-rotation logic read that
// mismatched conv-id as a /clear and migrated the agent's whole
// identity onto the throwaway conv — the live agent was retired as
// "superseded by <conv> (clear)" where <conv> was a 2-second plugin
// probe, dropping it out of its groups; the session row was also
// marked exited, firing a spurious "Exited" notification.
//
// Expected: the foreign hook is dropped wholesale. The agent stays an
// active member of its group under its own conv-id, no succession edge
// appears, the session row keeps its conv-id and never reads exited.
func TestForeignOneShot_DoesNotStealAgentIdentity(t *testing.T) {
	f := newFlow(t)
	g := setupClearedAgent(t, f)

	cc := f.World.CCs.GetByLabel(clearAgentLabel)
	require.NotNil(t, cc, "CCSim for the agent should be registered")

	foreignConv := cc.RunForeignOneShot()

	// `tclaude agent groups members alpha`: the agent is still the live
	// member under its own conv-id; the foreign conv never appears.
	members := f.ListGroupMembers(g.Name)
	require.NotNil(t, findMember(members, clearAgentConv),
		"the agent must remain a member under its own conv-id; got %+v", members)
	assert.Nil(t, findMember(members, foreignConv),
		"the foreign one-shot conv must not appear as a member")

	// Identity untouched: still an active agent, no succession edge,
	// and the foreign conv was not promoted to anything.
	enr, err := db.EnrollmentState(clearAgentConv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentActive, enr,
		"the agent must NOT be retired by a foreign process's SessionEnd")
	succ, err := db.GetConvSuccessor(clearAgentConv)
	require.NoError(t, err)
	assert.Empty(t, succ, "no succession edge onto the foreign conv")
	foreignEnr, err := db.EnrollmentState(foreignConv)
	require.NoError(t, err)
	assert.Equal(t, db.EnrollmentNone, foreignEnr,
		"the foreign conv must not inherit the agent's enrollment")

	// The session row: conv-id untouched, and not marked exited (the
	// "Exited" notification fires on the transition to exited, so no
	// transition ⇒ no notification).
	state, err := session.LoadSessionState(clearAgentLabel)
	require.NoError(t, err)
	assert.Equal(t, clearAgentConv, state.ConvID,
		"the session row must keep tracking the agent's own conv-id")
	assert.NotEqual(t, session.StatusExited, state.Status,
		"a foreign one-shot's SessionEnd must not mark the live session exited")
}
