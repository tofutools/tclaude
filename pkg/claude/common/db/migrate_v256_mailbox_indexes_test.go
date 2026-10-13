package db

import (
	"strings"
	"testing"

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

	t.Run("roster tallies", func(t *testing.T) {
		assertNoTableScan(t, queryPlan(t, `SELECT to_conv, COUNT(*),
			SUM(CASE WHEN read_at IS NULL THEN 1 ELSE 0 END), MAX(created_at)
			FROM agent_messages WHERE to_conv != '' GROUP BY to_conv`))
		assertNoTableScan(t, queryPlan(t, `SELECT from_conv, COUNT(*), MAX(created_at)
			FROM agent_messages WHERE from_conv != '' GROUP BY from_conv`))
	})
}
