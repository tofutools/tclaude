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
	excl := []string{"conv-retired-1", "conv-retired-2"}

	for name, f := range map[string]MailboxFilter{
		"agent folder":         {ForConv: "conv-a"},
		"group folder":         {ScopeConvs: []string{"conv-a", "conv-b"}, ScopeGroupID: 7},
		"group folder exclude": {ScopeConvs: []string{"conv-a", "conv-b"}, ScopeGroupID: 7, ExcludeConvs: excl},
	} {
		t.Run(name, func(t *testing.T) {
			where, args := f.where()
			assertNoTableScan(t, queryPlan(t, "SELECT COUNT(*) FROM agent_messages WHERE "+where, args...))
			assertNoTableScan(t, queryPlan(t, "SELECT id FROM agent_messages WHERE "+where+" ORDER BY id DESC LIMIT 50", args...))
		})
	}

	t.Run("excluded rows of the all firehose", func(t *testing.T) {
		where, args := MailboxFilter{ExcludeConvs: excl}.excludedWhere()
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

// CountMailbox counts an exclude-scoped folder as total − excluded; it
// must agree with the rows the same filter actually lists, including the
// reincarnation-handoff carve-out on the sender side.
func TestCountMailboxExcludeMatchesListing(t *testing.T) {
	setupTestDB(t)
	const (
		live1   = "conv-live-1"
		live2   = "conv-live-2"
		retired = "conv-retired"
	)
	base := time.Now().Add(-time.Hour)
	msgs := []AgentMessage{
		{GroupID: 3, FromConv: live1, ToConv: live2, Subject: "live dm"},
		{GroupID: 3, FromConv: live2, ToConv: live1, Subject: "live reply"},
		{GroupID: 3, FromConv: live1, ToConv: retired, Subject: "to retired"},
		{GroupID: 3, FromConv: retired, ToConv: live1, Subject: "from retired"},
		{GroupID: 3, FromConv: retired, ToConv: live2, Subject: ReincarnationHandoffSubject},
		{GroupID: 3, FromConv: retired, ToConv: retired, Subject: ReincarnationHandoffSubject},
		{GroupID: 3, FromConv: live1, ToConv: "", Subject: "live multicast"},
		{GroupID: 3, FromConv: retired, ToConv: "", Subject: "retired multicast"},
		{GroupID: 0, FromConv: live2, ToConv: live2, Subject: "self note"},
		{GroupID: 4, FromConv: "conv-other", ToConv: retired, Subject: "outsider"},
	}
	for i := range msgs {
		msgs[i].CreatedAt = base.Add(time.Duration(i) * time.Minute)
		_, err := InsertAgentMessage(&msgs[i])
		require.NoError(t, err)
	}

	excl := []string{retired}
	for name, f := range map[string]MailboxFilter{
		"all":          {ExcludeConvs: excl},
		"agent folder": {ForConv: live1, ExcludeConvs: excl},
		"group folder": {ScopeConvs: []string{live1, live2}, ScopeGroupID: 3, ExcludeConvs: excl},
		"empty group":  {ScopeGroupID: 4, ExcludeConvs: excl},
	} {
		t.Run(name, func(t *testing.T) {
			rows, err := ListMailboxPage(f, 0, 0)
			require.NoError(t, err)
			n, err := CountMailbox(f)
			require.NoError(t, err)
			assert.Equal(t, len(rows), n)
		})
	}

	n, err := CountMailbox(MailboxFilter{ExcludeConvs: excl})
	require.NoError(t, err)
	assert.Equal(t, 5, n, "live traffic plus the handoff to a live successor survive")
}
