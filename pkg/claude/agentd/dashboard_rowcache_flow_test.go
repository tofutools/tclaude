package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: a populated group whose members are resolved through the snapshot's
// per-request batch row cache (TCL-368). One conv is BOTH a group member AND a
// group owner AND surfaces on the Agents roster, so its row is built once and
// shared across the member loop, addAgent and the owners pass. This test pins
// that the memoized bundle resolves the same values on every surface: a
// divergence between the group member row and the Agents roster row would mean
// the cache produced inconsistent bundles (or a surface bypassed it).
//
// It also exercises the cache-only location resolver (TCL-367): the branch is
// scanned into conv_index up front (standing in for the fsnotify monitor), and
// the snapshot must read it from the cache without re-parsing the .jsonl.
func TestDashboardSnapshot_BatchRowBundleConsistent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)

	const leadConv = "aaaaaaaa-1111-2222-3333-444444444444" // member + owner, on a branch
	const workerConv = "bbbbbbbb-1111-2222-3333-444444444444"

	f.HaveAliveSessionOnBranch(leadConv, "spwn-lead", "tmux-lead", "/tmp/wt/lead", "feature-lead")
	f.HaveAliveSession(workerConv, "spwn-work", "tmux-work", "/tmp/wt/work")
	// Stand in for the watch model: scan the branch into conv_index so the
	// cache-only resolver reads it off the cached row (TCL-367), the same way
	// the branch-links flow test seeds it.
	require.NotNil(t, agent.FreshConvRowResolved(leadConv), "lead conv_index scan")

	g := f.HaveGroup("crew")
	f.HaveMember("crew", leadConv)
	f.HaveMember("crew", workerConv)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, leadConv, "human"), "make lead an owner")
	// Names via the actor pending-name (no /rename yet) so the assertion also
	// exercises the batch AgentsByConv path that feeds the title fallback.
	f.HavePendingName(leadConv, "lead-agent")
	f.HavePendingName(workerConv, "worker-agent")

	snap := fetchSnapshotOnly(t, agentd.BuildDashboardHandlerForTest())

	// Locate the lead's group-member row and its Agents-roster row.
	var crew *dashGroup
	for i := range snap.Groups {
		if snap.Groups[i].Name == "crew" {
			crew = &snap.Groups[i]
		}
	}
	require.NotNil(t, crew, "crew group present")
	var leadMember *dashMember
	for i := range crew.Members {
		if crew.Members[i].ConvID == leadConv {
			leadMember = &crew.Members[i]
		}
	}
	require.NotNil(t, leadMember, "lead is a crew member row")
	leadAgent := findAgent(snap.Agents, leadConv)
	require.NotNil(t, leadAgent, "lead on the Agents roster")

	// The member row and the roster row must carry identical resolved values —
	// the whole point of the shared memoized bundle.
	assert.Equal(t, "lead-agent", leadMember.Title, "member title from the batch")
	assert.Equal(t, leadMember.Title, leadAgent.Title, "member/agent title agree")
	assert.Equal(t, "feature-lead", leadMember.Branch, "branch resolved from cached conv_index")
	assert.Equal(t, leadMember.Branch, leadAgent.Branch, "member/agent branch agree")
	assert.True(t, leadMember.Online, "lead has a live tmux session")
	assert.Equal(t, leadMember.Online, leadAgent.Online, "member/agent online agree")
	assert.NotEmpty(t, leadMember.CreatedAt, "created resolved from cached conv_index")

	// The lead's Agents row records the crew membership (member + owner pass).
	assert.Contains(t, leadAgent.Groups, "crew", "lead's agent row lists crew")

	// The offline worker still resolves a correct, consistent bundle.
	worker := findAgent(snap.Agents, workerConv)
	require.NotNil(t, worker, "worker on the Agents roster")
	assert.Equal(t, "worker-agent", worker.Title)
}

// Scenario: a retired actor that still holds a permission grant. TCL-369 loads
// the retired/superseded sets BEFORE building agent rows so addAgent skips a
// retired conv up front instead of resolving its full row and discarding it in
// the output loop. The observable contract is unchanged — a retired grant
// holder must not appear on the Agents / Ungrouped roster — while a live grant
// holder must. This pins that the up-front filter did not change the output.
func TestDashboardSnapshot_RetiredGrantHolderExcluded(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)

	const liveConv = "live-1111-2222-3333-444444444444"    // live grant holder → on roster
	const retiredConv = "gone-1111-2222-3333-444444444444" // retired grant holder → filtered

	f.HaveConvWithTitle(liveConv, "live-holder")
	f.HaveConvWithTitle(retiredConv, "retired-holder")
	f.HaveEnrolledAgent(liveConv)
	f.HaveEnrolledAgent(retiredConv)

	// Both hold an explicit grant, so both land in ListAllAgentPermissions and
	// would be run through addAgent.
	require.NoError(t, db.GrantAgentPermission(liveConv, "self.rename", "human"), "grant live")
	require.NoError(t, db.GrantAgentPermission(retiredConv, "self.rename", "human"), "grant retired")

	// Retire one of them AFTER granting — the grant row survives the retire.
	f.HaveRetiredAgent(retiredConv)

	snap := fetchSnapshotOnly(t, agentd.BuildDashboardHandlerForTest())

	// The live grant holder is on the roster (and, having no group, ungrouped).
	assert.NotNil(t, findAgent(snap.Agents, liveConv), "live grant holder on Agents")
	assert.NotNil(t, findAgent(snap.Ungrouped, liveConv), "live grant holder is ungrouped")

	// The retired grant holder is excluded from both, despite its surviving grant.
	assert.Nil(t, findAgent(snap.Agents, retiredConv), "retired grant holder absent from Agents (TCL-369)")
	assert.Nil(t, findAgent(snap.Ungrouped, retiredConv), "retired grant holder absent from Ungrouped")
}
