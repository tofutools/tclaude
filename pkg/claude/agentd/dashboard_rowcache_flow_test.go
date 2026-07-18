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

	f.HaveAliveSessionOnBranch(leadConv, "spwn-lead", "tmux-lead", f.TestCwd("wt/lead"), "feature-lead")
	f.HaveAliveSession(workerConv, "spwn-work", "tmux-work", f.TestCwd("wt/work"))
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

// Scenario: a freshly-spawned agent whose .jsonl has NOT been parsed into
// conv_index yet — the several-seconds window right after spawn. Its actor row
// (agents.created_at) exists the instant enrollment ran, before the harness
// wrote its first event. The member Age must resolve from that birth timestamp
// immediately, not stay blank until conv_index materialises. Before the
// createdFor fix, the snapshot read conv_index.Created only, so this member's
// Age was empty for the whole gap; this pins the actor-created-at primary.
func TestDashboardSnapshot_AgeFromActorBeforeConvIndex(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)

	const freshConv = "cccccccc-1111-2222-3333-444444444444"

	f.HaveGroup("crew")
	// HaveMember enrolls the actor (agents.created_at stamped) but writes NO
	// conv_index row — exactly the not-yet-indexed state of a just-spawned agent.
	f.HaveMember("crew", freshConv)

	// Precondition: the actor exists with a birth timestamp, and conv_index does
	// not — the gap this fix closes.
	actor, err := db.GetAgentByConv(freshConv)
	require.NoError(t, err)
	require.NotNil(t, actor, "actor enrolled")
	require.False(t, actor.CreatedAt.IsZero(), "actor has a birth timestamp")
	convRow, err := db.GetConvIndex(freshConv)
	require.NoError(t, err)
	require.Nil(t, convRow, "conv_index not yet populated")

	snap := fetchSnapshotOnly(t, agentd.BuildDashboardHandlerForTest())

	var crew *dashGroup
	for i := range snap.Groups {
		if snap.Groups[i].Name == "crew" {
			crew = &snap.Groups[i]
		}
	}
	require.NotNil(t, crew, "crew group present")
	var member *dashMember
	for i := range crew.Members {
		if crew.Members[i].ConvID == freshConv {
			member = &crew.Members[i]
		}
	}
	require.NotNil(t, member, "fresh conv is a crew member")
	assert.NotEmpty(t, member.CreatedAt, "Age resolves from agents.created_at before conv_index exists")
	assert.Equal(t, db.CanonicalAgeTimestampFromTime(actor.CreatedAt), member.CreatedAt,
		"Age is the actor's birth timestamp in the canonical fixed-width Age layout")
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

// Scenario: one session row carries a corrupt effective_sandbox_config that
// scanSessionRow cannot decode. FindSessionsByConvIDs is best-effort per row, so
// it skips just that row — a regression to the old all-or-nothing scan would
// abort the batch, empty the sessions map for the WHOLE poll, and render EVERY
// agent offline with blank state. This pins that a single bad row degrades only
// its own conv, never its siblings.
func TestDashboardSnapshot_CorruptSandboxRowDoesNotBlankSiblings(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)

	const goodConv = "good-1111-2222-3333-444444444444"
	const badConv = "badd-1111-2222-3333-444444444444"

	f.HaveConvWithTitle(goodConv, "good-agent")
	f.HaveConvWithTitle(badConv, "bad-agent")
	f.HaveAliveSession(goodConv, "spwn-good", "tmux-good", f.TestCwd("good"))
	f.HaveAliveSession(badConv, "spwn-bad", "tmux-bad", f.TestCwd("bad"))
	f.HaveGroup("crew")
	f.HaveMember("crew", goodConv)
	f.HaveMember("crew", badConv)

	// Corrupt the bad conv's stored effective_sandbox_config so the row fails to
	// decode. Written directly since SaveSession would reject a bad snapshot.
	conn, err := db.Open()
	require.NoError(t, err)
	_, err = conn.Exec(`UPDATE sessions SET effective_sandbox_config = ? WHERE conv_id = ?`,
		"{not valid json", badConv)
	require.NoError(t, err)

	snap := fetchSnapshotOnly(t, agentd.BuildDashboardHandlerForTest())

	var crew *dashGroup
	for i := range snap.Groups {
		if snap.Groups[i].Name == "crew" {
			crew = &snap.Groups[i]
		}
	}
	require.NotNil(t, crew, "crew group present")
	memberByConv := func(conv string) *dashMember {
		for i := range crew.Members {
			if crew.Members[i].ConvID == conv {
				return &crew.Members[i]
			}
		}
		return nil
	}

	// The good conv is unaffected — still online with a real status.
	good := memberByConv(goodConv)
	require.NotNil(t, good, "good conv is a crew member")
	assert.True(t, good.Online, "good conv stays online despite a sibling's corrupt sandbox row")
	assert.NotEmpty(t, good.State.Status, "good conv keeps its resolved state")

	// The corrupt conv degrades to offline (its only session row was skipped) —
	// the blast radius is one conv, not the whole poll.
	bad := memberByConv(badConv)
	require.NotNil(t, bad, "bad conv still listed as a member")
	assert.False(t, bad.Online, "corrupt-sandbox conv degrades to offline, not the whole poll")
}
