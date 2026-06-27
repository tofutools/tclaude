package agent

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRenderSudoGrants covers the `sudo ls` stdout renderer (PR3c): grants
// group into one block per stable agent_id (so an actor's rotated
// generations collapse together), each block headed by the full agent_id
// (lookupID) + conv title, with a conv-id fallback for a non-actor grant.
func TestRenderSudoGrants(t *testing.T) {
	const agentA = "agt_aaaa1111bbbb2222cccc3333dddd4444"
	const agentB = "agt_bbbb1111cccc2222dddd3333eeee4444"
	rows := []sudoGrantJSON{
		// agent A, generation 1
		{ID: 1, AgentID: agentA, ConvID: "conv-a-gen1", ConvTitle: "alpha", Slug: "groups.spawn", RemainingSeconds: 600},
		// agent B
		{ID: 2, AgentID: agentB, ConvID: "conv-b", ConvTitle: "bravo", Slug: "member.add", RemainingSeconds: 120},
		// agent A, generation 2 (rotated conv) — must collapse into A's block
		{ID: 3, AgentID: agentA, ConvID: "conv-a-gen2", ConvTitle: "alpha", Slug: "agent.rename", RemainingSeconds: 60},
		// non-actor grant (no agent_id) — falls back to its conv-id
		{ID: 4, ConvID: "plain-conv", ConvTitle: "plain", Slug: "self.compact", RemainingSeconds: 30},
	}

	var buf bytes.Buffer
	require.Equal(t, rcOK, renderSudoGrants(rows, &buf))
	out := buf.String()

	// Header leads with the full agent_id (lookupID), not a conv-id prefix.
	assert.Contains(t, out, agentA+"  alpha")
	assert.Contains(t, out, agentB+"  bravo")
	// Non-actor grant falls back to its conv-id in the header.
	assert.Contains(t, out, "plain-conv  plain")
	// Agent A's two generations collapse into ONE block: its header appears once.
	assert.Equal(t, 1, strings.Count(out, agentA+"  alpha"))
	// Both of A's slugs render under that single block.
	assert.Contains(t, out, "groups.spawn")
	assert.Contains(t, out, "agent.rename")
}

// TestRenderSudoGrantsEmpty: no grants → the friendly empty line, rcOK.
func TestRenderSudoGrantsEmpty(t *testing.T) {
	var buf bytes.Buffer
	require.Equal(t, rcOK, renderSudoGrants(nil, &buf))
	assert.Contains(t, buf.String(), "No active sudo grants.")
}
