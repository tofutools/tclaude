package db

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queryPlan returns the EXPLAIN QUERY PLAN details of q, one per line.
func queryPlan(t *testing.T, q string, args ...any) string {
	t.Helper()
	d, err := Open()
	require.NoError(t, err)
	rows, err := d.Query("EXPLAIN QUERY PLAN "+q, args...)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var plan strings.Builder
	for rows.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, rows.Scan(&id, &parent, &unused, &detail))
		plan.WriteString(detail)
		plan.WriteByte('\n')
	}
	require.NoError(t, rows.Err())
	return plan.String()
}

// assertNoTableScan fails when the plan walks the agent_messages table
// itself. A scan of a covering index is fine — it never touches the bodies.
func assertNoTableScan(t *testing.T, plan string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(plan), "\n") {
		if strings.HasPrefix(line, "SCAN agent_messages") && !strings.Contains(line, "COVERING INDEX") {
			t.Errorf("full table scan in plan:\n%s", plan)
		}
	}
}

// The Messages tab's folder scopes and roster tallies must be index-driven:
// at tens of thousands of messages a table scan drags every body through
// the page cache on each 2s dashboard refresh.
func TestMailboxQueriesUseIndexes(t *testing.T) {
	setupTestDB(t)
	for name, f := range map[string]MailboxFilter{
		"agent folder":         {ForConv: "conv-a"},
		"group folder":         {ScopeGroupID: 7},
		"group folder exclude": {ScopeGroupID: 7, ExcludeRetired: true},
	} {
		t.Run(name, func(t *testing.T) {
			where, args := f.where()
			assertNoTableScan(t, queryPlan(t, "SELECT COUNT(*) FROM agent_messages WHERE "+where, args...))
			assertNoTableScan(t, queryPlan(t, "SELECT id FROM agent_messages WHERE "+where+" ORDER BY id DESC LIMIT 50", args...))
		})
	}

	// The unscoped firehose pages in rowid order and stops at the page;
	// only its count has to stay off the table.
	t.Run("all firehose exclude count", func(t *testing.T) {
		where, args := MailboxFilter{ExcludeRetired: true}.where()
		assertNoTableScan(t, queryPlan(t, "SELECT COUNT(*) FROM agent_messages WHERE "+where, args...))
	})

	t.Run("roster tallies", func(t *testing.T) {
		assertNoTableScan(t, queryPlan(t, `SELECT to_conv, COUNT(*),
			SUM(CASE WHEN read_at IS NULL THEN 1 ELSE 0 END), MAX(created_at)
			FROM agent_messages WHERE to_conv != '' GROUP BY to_conv`))
		assertNoTableScan(t, queryPlan(t, `SELECT from_conv, COUNT(*), MAX(created_at)
			FROM agent_messages WHERE from_conv != '' GROUP BY from_conv`))
	})
}

// The roster's one-pass badges must count exactly what each aggregate
// folder lists — member traffic plus the group's channel, each row once —
// with and without the retired exclude.
func TestMailboxRosterTotalsMatchFolderCounts(t *testing.T) {
	setupTestDB(t)
	const (
		alice   = "conv-alice"
		bob     = "conv-bob"
		carol   = "conv-carol"
		retired = "conv-retired"
	)
	g1, err := CreateAgentGroup("one", "")
	require.NoError(t, err)
	g2, err := CreateAgentGroup("two", "")
	require.NoError(t, err)
	g3, err := CreateAgentGroup("quiet", "")
	require.NoError(t, err)
	for _, m := range []AgentGroupMember{
		{GroupID: g1, ConvID: alice}, {GroupID: g1, ConvID: bob},
		{GroupID: g2, ConvID: bob}, {GroupID: g2, ConvID: carol},
		{GroupID: g2, ConvID: retired},
	} {
		require.NoError(t, AddAgentGroupMember(&m))
	}
	_, err = RetireAgent(retired, "test", "")
	require.NoError(t, err)

	base := time.Now().Add(-time.Hour)
	for i, m := range []AgentMessage{
		{GroupID: g1, FromConv: alice, ToConv: bob},
		{GroupID: g2, FromConv: bob, ToConv: carol},
		{GroupID: g1, FromConv: alice, ToConv: ""},
		{GroupID: 0, FromConv: bob, ToConv: alice},
		{GroupID: g2, FromConv: carol, ToConv: retired},
		{GroupID: g2, FromConv: retired, ToConv: carol, Subject: ReincarnationHandoffSubject},
		{GroupID: g2, FromConv: retired, ToConv: ""},
		{GroupID: 0, FromConv: "conv-outsider", ToConv: carol},
		{GroupID: g1, FromConv: "conv-outsider", ToConv: "conv-other"},
	} {
		m.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		_, err := InsertAgentMessage(&m)
		require.NoError(t, err)
	}

	for _, excludeRetired := range []bool{false, true} {
		all, totals, err := MailboxRosterTotals(excludeRetired)
		require.NoError(t, err)
		wantAll, err := CountMailbox(MailboxFilter{ExcludeRetired: excludeRetired})
		require.NoError(t, err)
		assert.Equal(t, wantAll, all, "firehose total excludeRetired=%v", excludeRetired)
		for _, g := range []int64{g1, g2, g3} {
			want, err := CountMailbox(MailboxFilter{ScopeGroupID: g, ExcludeRetired: excludeRetired})
			require.NoError(t, err)
			assert.Equal(t, want, totals[g], "group %d excludeRetired=%v", g, excludeRetired)
		}
	}
}

// Mailbox search matches subject/body through the FTS index: word prefixes,
// operator characters taken literally, and the index tracks edits and
// deletes of the underlying rows.
func TestMailboxSearchUsesFullTextIndex(t *testing.T) {
	setupTestDB(t)
	insert := func(subject, body string) int64 {
		id, err := InsertAgentMessage(&AgentMessage{FromConv: "conv-a", ToConv: "conv-b",
			Subject: subject, Body: body, CreatedAt: time.Now()})
		require.NoError(t, err)
		return id
	}
	deploy := insert("deploy plan", "rolling out the new mailbox indexes tonight")
	review := insert("review", "please review PR #3003 (federation-safe?)")
	lunch := insert("lunch", "pizza")

	search := func(q string) []int64 {
		t.Helper()
		rows, err := ListMailboxPage(MailboxFilter{Text: q}, 0, 0)
		require.NoError(t, err)
		ids := make([]int64, 0, len(rows))
		for _, m := range rows {
			ids = append(ids, m.ID)
		}
		return ids
	}
	assert.Equal(t, []int64{deploy}, search("mailbox"), "body word")
	assert.Equal(t, []int64{deploy}, search("DEPLOY pl"), "case-insensitive subject word prefixes, all must match")
	assert.Equal(t, []int64{review}, search("federation-safe?"), "punctuation is a separator, not FTS syntax")
	assert.Equal(t, []int64{review}, search(`"please" -review* (3003`), "FTS syntax characters are ignored, not parsed")
	assert.Empty(t, search("ndexes"), "matching is by word prefix, not arbitrary substring")
	assert.ElementsMatch(t, []int64{deploy, review, lunch}, search("conv-a"), "conv-id substrings still match")

	d, err := Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE agent_messages SET body = 'postponed' WHERE id = ?`, deploy)
	require.NoError(t, err)
	assert.Empty(t, search("mailbox"), "an edited body is re-indexed")
	assert.Equal(t, []int64{deploy}, search("postponed"))
	_, err = DeleteAgentMessagesByIDs([]int64{review})
	require.NoError(t, err)
	assert.Empty(t, search("federation"), "a deleted row leaves the index")
}
