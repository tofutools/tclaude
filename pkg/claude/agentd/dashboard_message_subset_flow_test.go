package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// These tests exercise the group-scoped one-shot message modal: when
// the dashboard's per-group ✉ button opens the modal it locks to that
// group and lets the human tick a subset of its members. The browser
// turns "all ticked" into a plain group: multicast and "a subset" into
// a group: multicast carrying an explicit `members` conv-id list.
//
// The `members` field narrows fanOutToGroup the same way `role` does —
// it is applied AFTER the live roster is read, so it can only shrink
// the recipient set, never widen it. These tests assert that property
// at the real surface: the agent_messages rows each recipient ends up
// with.

// Scenario: a subset send reaches exactly the ticked members. A group
// of three members is messaged with `members` naming only two of them;
// the third receives nothing, and the response lists exactly the two.
func TestDashboardMessage_GroupSubset_ReachesOnlySelectedMembers(t *testing.T) {
	f := newFlow(t)

	g := f.HaveGroup("team")
	const sender = "dsub-send-bbbb-cccc-000000000001"
	const memberA = "dsub-aaaa-bbbb-cccc-000000000002"
	const memberB = "dsub-bbbb-bbbb-cccc-000000000003"
	const memberC = "dsub-cccc-bbbb-cccc-000000000004"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)
	f.HaveMember("team", memberB)
	f.HaveMember("team", memberC)
	f.HaveAliveSession(memberA, "spwn-dsub-a", "tclaude-spwn-dsub-a", "/tmp/work")

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "just A and C",
		"members": []string{memberA, memberC},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.Equal(t, "team", resp.ViaGroup)
	assert.ElementsMatch(t, []string{memberA, memberC}, recipientConvIDs(resp),
		"the multicast fans out to exactly the ticked subset")

	// Real surface: the two ticked members each got one group-stamped
	// row; the unticked member got nothing.
	for _, m := range []string{memberA, memberC} {
		rows, err := db.ListAgentMessagesForConv(m, 100)
		require.NoError(t, err)
		require.Len(t, rows, 1, "ticked member %s got a row", m)
		assert.Equal(t, "just A and C", rows[0].Body)
		assert.Equal(t, sender, rows[0].FromConv, "row attributed to the picked From conv")
		assert.Equal(t, g.ID, rows[0].GroupID, "row stamped with the target group")
	}
	bRows, err := db.ListAgentMessagesForConv(memberB, 100)
	require.NoError(t, err)
	assert.Empty(t, bRows, "an unticked member receives nothing")

	// The alive ticked member is nudged over tmux.
	f.AssertSentContains("tclaude-spwn-dsub-a:0.0", "new agent message", 2*time.Second)
}

// Scenario: a `members` list naming every member behaves identically
// to a bare group: multicast — all members are reached. This is the
// backend guarantee that makes the dashboard's "omit members when all
// ticked" optimisation merely an optimisation, not load-bearing.
func TestDashboardMessage_GroupSubset_ExplicitFullListReachesEveryMember(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "dful-send-bbbb-cccc-000000000001"
	const memberA = "dful-aaaa-bbbb-cccc-000000000002"
	const memberB = "dful-bbbb-bbbb-cccc-000000000003"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)
	f.HaveMember("team", memberB)

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "everyone",
		"members": []string{memberA, memberB},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{memberA, memberB}, recipientConvIDs(resp),
		"an explicit full list reaches every non-sender member")
	for _, m := range []string{memberA, memberB} {
		rows, err := db.ListAgentMessagesForConv(m, 100)
		require.NoError(t, err)
		require.Len(t, rows, 1, "member %s got a row", m)
		assert.Equal(t, "everyone", rows[0].Body)
	}
}

// Scenario: the `members` filter can only shrink reach, never widen
// it. A list naming a conv that is NOT a member of the target group
// leaves that conv untouched — the filter is intersected against the
// live roster, so an outside id simply matches nobody.
func TestDashboardMessage_GroupSubset_NonMemberIDsAreIgnored(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "dnon-send-bbbb-cccc-000000000001"
	const memberA = "dnon-aaaa-bbbb-cccc-000000000002"
	const outsider = "dnon-outs-bbbb-cccc-000000000003"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)
	f.HaveConvWithTitle(outsider, "unrelated-agent")
	f.HaveEnrolledAgent(outsider)

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "scoped",
		"members": []string{memberA, outsider},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{memberA}, recipientConvIDs(resp),
		"only the genuine member is reached — the outside id matches nothing")

	aRows, err := db.ListAgentMessagesForConv(memberA, 100)
	require.NoError(t, err)
	require.Len(t, aRows, 1)
	outRows, err := db.ListAgentMessagesForConv(outsider, 100)
	require.NoError(t, err)
	assert.Empty(t, outRows, "a non-member named in `members` is never messaged")
}

// Scenario: the sender is excluded from the fan-out even when its own
// conv-id appears in the `members` list — the same self-skip a bare
// multicast applies. A subset naming [sender, memberA] reaches only A.
func TestDashboardMessage_GroupSubset_ExcludesSenderEvenIfListed(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "dslf-send-bbbb-cccc-000000000001"
	const memberA = "dslf-aaaa-bbbb-cccc-000000000002"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "not to myself",
		"members": []string{sender, memberA},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{memberA}, recipientConvIDs(resp),
		"the sender is skipped even when ticked in the subset")
	senderRows, err := db.ListAgentMessagesForConv(sender, 100)
	require.NoError(t, err)
	assert.Empty(t, senderRows, "the From conv does not message itself")
}

// Scenario: `members` is meaningless on a 1:1 send — there is no
// roster to narrow. dispatchSend rejects it with a 400 before any row
// is written, mirroring how it rejects `role` on a solo target.
func TestDashboardMessage_GroupSubset_RejectedOnSoloTarget(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const alice = "dbad-alic-bbbb-cccc-000000000001"
	const bob = "dbad-bobb-bbbb-cccc-000000000002"
	f.HaveMember("team", alice)
	f.HaveMember("team", bob)

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": alice, "to": bob, "body": "solo with a subset",
		"members": []string{bob},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "members is only valid with a 'group:' multicast target")

	rows, err := db.ListAgentMessagesForConv(bob, 100)
	require.NoError(t, err)
	assert.Empty(t, rows, "a rejected send writes no row")
}

// Scenario: a subset that names a member who has since reincarnated
// still reaches the live successor. The dashboard's member list is a
// point-in-time snapshot; if a member reincarnates while the modal
// sits open the ticked conv-id is the superseded one. fanOutToGroup
// resolves each `members` entry to its live successor, so the message
// still lands on the reincarnated agent rather than silently missing.
func TestDashboardMessage_GroupSubset_FollowsSuccessionToReincarnatedMember(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "drei-send-bbbb-cccc-000000000001"
	const oldX = "drei-oldx-bbbb-cccc-000000000002"
	f.HaveMember("team", sender)
	f.HaveConvWithTitle(oldX, "worker")
	f.HaveMember("team", oldX)
	f.HaveAliveSession(oldX, "spwn-drei-x", "tclaude-spwn-drei-x", "/tmp/work")

	// The worker reincarnates: oldX is superseded, the live head is Y,
	// and Reincarnate migrates the group membership to Y.
	r := f.Reincarnate(oldX, "fresh start")
	newY := r.NewConv
	require.NotEqual(t, oldX, newY, "reincarnation produced a fresh conv-id")

	// A stale dashboard snapshot still names oldX — the subset send
	// carries the superseded id.
	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "still reaches you",
		"members": []string{oldX},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{newY}, recipientConvIDs(resp),
		"the subset filter follows the succession chain to the live head")
	// newY's inbox also holds the reincarnation follow-up, so assert on
	// the subset message specifically rather than the row count.
	newRows, err := db.ListAgentMessagesForConv(newY, 100)
	require.NoError(t, err)
	matched := 0
	for _, m := range newRows {
		if m.Body == "still reaches you" {
			matched++
		}
	}
	assert.Equal(t, 1, matched, "the reincarnated successor received the subset message exactly once")
	oldRows, err := db.ListAgentMessagesForConv(oldX, 100)
	require.NoError(t, err)
	assert.Empty(t, oldRows, "nothing landed in the superseded conv")
}

// Scenario: the `members` narrowing also works on the agent-facing
// POST /v1/messages — it is part of the shared sendReq, so an agent
// sending a group: multicast can scope it the same way the dashboard
// does. The sender stays gated by handleMulticast's member/owner
// check; `members` only shrinks reach, so no authority is gained.
func TestMulticast_MembersSubset_OnAgentMessagesEndpoint(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "amem-send-bbbb-cccc-000000000001"
	const memberA = "amem-aaaa-bbbb-cccc-000000000002"
	const memberB = "amem-bbbb-bbbb-cccc-000000000003"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)
	f.HaveMember("team", memberB)

	rec := postMessage(t, f, sender, map[string]any{
		"to": "group:team", "body": "agent subset",
		"members": []string{memberA},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{memberA}, recipientConvIDs(resp),
		"the agent-path multicast honours the members subset")
	aRows, err := db.ListAgentMessagesForConv(memberA, 100)
	require.NoError(t, err)
	require.Len(t, aRows, 1)
	bRows, err := db.ListAgentMessagesForConv(memberB, 100)
	require.NoError(t, err)
	assert.Empty(t, bRows, "the unlisted member is not reached")
}

// Scenario: the `members` subset can be keyed by the stable agent_id —
// the canonical key per JOH-27 (conv-ids stay accepted for back-compat).
// A list naming a member by its agent_id reaches exactly that member, and
// the send receipt names each recipient by its agent_id. This is the
// backend half of the agent-primary multicast keying; the dashboard modal
// switches to sending agent_ids in the -web follow-up.
func TestDashboardMessage_GroupSubset_KeyedByAgentID(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "daid-send-bbbb-cccc-000000000001"
	const memberA = "daid-aaaa-bbbb-cccc-000000000002"
	const memberB = "daid-bbbb-bbbb-cccc-000000000003"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)
	f.HaveMember("team", memberB)

	// Joining the group enrolled each member as an actor — resolve A's
	// stable agent_id and address the subset by it, not its conv-id.
	agentA, err := db.AgentIDForConv(memberA)
	require.NoError(t, err)
	require.NotEmpty(t, agentA, "a group member is minted as an actor")

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "by agent id",
		"members": []string{agentA},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{memberA}, recipientConvIDs(resp),
		"an agent_id-keyed subset reaches exactly the named member")
	// Receipt carries the stable agent_id for each recipient.
	require.Len(t, resp.Recipients, 1)
	assert.Equal(t, agentA, resp.Recipients[0].AgentID,
		"the send receipt names the recipient by its stable agent_id")

	bRows, err := db.ListAgentMessagesForConv(memberB, 100)
	require.NoError(t, err)
	assert.Empty(t, bRows, "a member not named (by agent_id) is not reached")
}

// Scenario: addressing the subset by agent_id is rotation-immune — the
// headline reason for keying on it. A member named by its stable agent_id
// is reached even after it reincarnates to a fresh conv-id between the
// dashboard snapshot and the send. Unlike the conv-id path (which has to
// walk the succession chain), the agent_id never changed, so the match is
// direct.
func TestDashboardMessage_GroupSubset_AgentIDSurvivesReincarnation(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "dais-send-bbbb-cccc-000000000001"
	const oldX = "dais-oldx-bbbb-cccc-000000000002"
	f.HaveMember("team", sender)
	f.HaveConvWithTitle(oldX, "worker")
	f.HaveMember("team", oldX)
	f.HaveAliveSession(oldX, "spwn-dais-x", "tclaude-spwn-dais-x", "/tmp/work")

	// Capture the stable agent_id BEFORE reincarnation; it must stay valid
	// across the rotation.
	agentX, err := db.AgentIDForConv(oldX)
	require.NoError(t, err)
	require.NotEmpty(t, agentX)

	r := f.Reincarnate(oldX, "fresh start")
	newY := r.NewConv
	require.NotEqual(t, oldX, newY, "reincarnation produced a fresh conv-id")
	// The agent_id is unchanged by reincarnation.
	agentY, err := db.AgentIDForConv(newY)
	require.NoError(t, err)
	assert.Equal(t, agentX, agentY, "agent_id is stable across reincarnation")

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "by stable id",
		"members": []string{agentX},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{newY}, recipientConvIDs(resp),
		"an agent_id-keyed subset lands on the live successor after reincarnation")

	newRows, err := db.ListAgentMessagesForConv(newY, 100)
	require.NoError(t, err)
	matched := 0
	for _, m := range newRows {
		if m.Body == "by stable id" {
			matched++
		}
	}
	assert.Equal(t, 1, matched, "the reincarnated successor received the subset message exactly once")
	oldRows, err := db.ListAgentMessagesForConv(oldX, 100)
	require.NoError(t, err)
	assert.Empty(t, oldRows, "nothing landed in the superseded conv")
}

// Scenario: a `members` list may MIX keys — an agent_id for one member and
// a conv-id for another — and both are reached. Confirms the two match
// paths coexist (so the dashboard can migrate to agent_ids incrementally
// without a flag-day cutover).
func TestDashboardMessage_GroupSubset_MixedAgentIDAndConvID(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "dmix-send-bbbb-cccc-000000000001"
	const memberA = "dmix-aaaa-bbbb-cccc-000000000002"
	const memberB = "dmix-bbbb-bbbb-cccc-000000000003"
	const memberC = "dmix-cccc-bbbb-cccc-000000000004"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)
	f.HaveMember("team", memberB)
	f.HaveMember("team", memberC)

	agentA, err := db.AgentIDForConv(memberA)
	require.NoError(t, err)
	require.NotEmpty(t, agentA)

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "mixed keys",
		// A by agent_id, B by conv-id; C named by neither.
		"members": []string{agentA, memberB},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.ElementsMatch(t, []string{memberA, memberB}, recipientConvIDs(resp),
		"a mixed agent_id + conv_id list reaches both named members")
	cRows, err := db.ListAgentMessagesForConv(memberC, 100)
	require.NoError(t, err)
	assert.Empty(t, cRows, "the unnamed member is not reached")
}

// Scenario: a `members` list whose entries are all blank narrows to
// NOBODY — it does not fall back to a full-group broadcast. A caller
// that passed a non-empty members list asked to narrow; a list with no
// usable id can only shrink reach. (Regression: a memberSet that
// stayed nil because every entry trimmed away was once misread as "no
// filter", widening a malformed subset send into a whole-group blast.)
func TestDashboardMessage_GroupSubset_BlankMembersListReachesNobody(t *testing.T) {
	f := newFlow(t)

	f.HaveGroup("team")
	const sender = "dbnk-send-bbbb-cccc-000000000001"
	const memberA = "dbnk-aaaa-bbbb-cccc-000000000002"
	const memberB = "dbnk-bbbb-bbbb-cccc-000000000003"
	f.HaveMember("team", sender)
	f.HaveMember("team", memberA)
	f.HaveMember("team", memberB)

	mux := dashMessageMux(t)
	rec := postDashMessage(t, mux, map[string]any{
		"from": sender, "to": "group:team", "body": "should reach nobody",
		"members": []string{" ", ""},
	})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	resp := decodeMcast(t, rec)
	assert.Empty(t, recipientConvIDs(resp),
		"a blank members list narrows to nobody — never a full-group fallback")
	for _, m := range []string{memberA, memberB} {
		rows, err := db.ListAgentMessagesForConv(m, 100)
		require.NoError(t, err)
		assert.Empty(t, rows, "member %s receives nothing from a blank subset", m)
	}
}
