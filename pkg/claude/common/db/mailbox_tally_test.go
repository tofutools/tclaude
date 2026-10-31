package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mailboxTallyGen(t *testing.T) int64 {
	t.Helper()
	d, err := Open()
	require.NoError(t, err)
	var gen int64
	require.NoError(t, d.QueryRow(`SELECT gen FROM mailbox_tally_generation`).Scan(&gen))
	return gen
}

// Every input of the roster tallies must move the generation on the current
// schema. This runs after all migrations, so a later table rebuild that drops
// one of these triggers fails here instead of freezing the unread badges.
func TestMailboxTallyGeneration_SeesEveryInput(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	groupID, err := CreateAgentGroup("tally", "")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: groupID, ConvID: "conv-a"}))
	var agentID string
	require.NoError(t, d.QueryRow(`SELECT agent_id FROM agents WHERE current_conv_id = 'conv-a'`).Scan(&agentID))
	id, err := InsertAgentMessage(&AgentMessage{FromConv: "conv-a", ToConv: "conv-b", Subject: "hi", CreatedAt: time.Now()})
	require.NoError(t, err)

	for _, step := range []struct{ name, stmt string }{
		{"message", `INSERT INTO agent_messages (from_conv, to_conv, created_at) VALUES ('conv-b', 'conv-a', 1)`},
		{"read", `UPDATE agent_messages SET read_at = 1 WHERE from_conv = 'conv-a'`},
		{"delete", `DELETE FROM agent_messages WHERE from_conv = 'conv-b'`},
		{"away", `INSERT INTO agent_federation_presence (agent_id, home_instance, state, current_instance, current_peer,
			predecessor_instance, departure_offer, arrival_offer, hop_count, visit_epoch, continuation_nonce_hash, transfer_json, updated_at)
			VALUES ('` + agentID + `', 'home', 'away', 'there', 'peer', 'home', '', '', 1, 1, '', '{}', 1)`},
		{"back", `UPDATE agent_federation_presence SET state = 'here'`},
		{"presence gone", `DELETE FROM agent_federation_presence`},
		{"retire", `UPDATE agents SET retired_at = 1 WHERE current_conv_id = 'conv-a'`},
		{"rotate", `UPDATE agents SET current_conv_id = 'conv-c' WHERE current_conv_id = 'conv-a'`},
		{"leave group", `DELETE FROM agent_group_members`},
	} {
		before := mailboxTallyGen(t)
		_, err := d.Exec(step.stmt)
		require.NoError(t, err, step.name)
		assert.Greater(t, mailboxTallyGen(t), before, step.name)
	}

	before := mailboxTallyGen(t)
	_, err = d.Exec(`UPDATE agent_messages SET nudge_attempts = nudge_attempts + 1 WHERE id = ?`, id)
	require.NoError(t, err)
	assert.Equal(t, before, mailboxTallyGen(t), "delivery bookkeeping is not a tally input")
}

// The memo is exact: a repeat read serves the same answer without scanning, and
// any input change is visible on the very next read.
func TestMailboxTallies_MemoizedUntilAnInputChanges(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	groupID, err := CreateAgentGroup("tally", "")
	require.NoError(t, err)
	require.NoError(t, AddAgentGroupMember(&AgentGroupMember{GroupID: groupID, ConvID: "conv-a"}))
	id, err := InsertAgentMessage(&AgentMessage{FromConv: "conv-a", ToConv: "conv-b", Subject: "hi", CreatedAt: time.Now()})
	require.NoError(t, err)

	counts, err := MailboxCounts()
	require.NoError(t, err)
	assert.Equal(t, 1, counts["conv-b"].Unread)
	all, groups, err := MailboxRosterTotals(true)
	require.NoError(t, err)
	assert.Equal(t, 1, all)
	assert.Equal(t, 1, groups[groupID])

	again, err := MailboxCounts()
	require.NoError(t, err)
	mailboxTally.counts["memo-probe"] = MailboxCount{In: 9}
	assert.Equal(t, 9, again["memo-probe"].In, "a repeat read shares the memo instead of scanning")
	delete(mailboxTally.counts, "memo-probe")

	_, err = d.Exec(`UPDATE agent_messages SET read_at = 1 WHERE id = ?`, id)
	require.NoError(t, err)
	counts, err = MailboxCounts()
	require.NoError(t, err)
	assert.Zero(t, counts["conv-b"].Unread, "a read clears the badge on the next poll")

	_, err = d.Exec(`DELETE FROM agent_group_members`)
	require.NoError(t, err)
	_, groups, err = MailboxRosterTotals(true)
	require.NoError(t, err)
	assert.Zero(t, groups[groupID], "membership changes reach the group total")
}

func TestAgentMessageLists_RequireALimit(t *testing.T) {
	setupTestDB(t)
	_, err := ListAgentMessagesForConv("a", 0)
	require.ErrorIs(t, err, errMessageListLimit)
	_, err = ListAgentMessagesFromConv("a", -1)
	require.ErrorIs(t, err, errMessageListLimit)
	_, err = ListInboxForActor("a", "b", 0)
	require.ErrorIs(t, err, errMessageListLimit)
	_, err = ListOutboxForActor("a", "b", 0)
	require.ErrorIs(t, err, errMessageListLimit)
	_, err = ListMailboxPage(MailboxFilter{}, 0, 0)
	require.ErrorIs(t, err, errMessageListLimit)
}
