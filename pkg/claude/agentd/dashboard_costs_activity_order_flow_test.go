package agentd_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TCL-935. The Costs breakdown picks each slice's last activity, model and
// harness by comparing session_cost_daily.updated_at stamps. Those are
// RFC3339Nano, whose spelling does not sort chronologically (TCL-932), so a
// string comparison could show the EARLIER spend as a slice's last activity —
// and since the breakdown sorts on that field, mis-order whole rows.
//
// Every fixture here seeds stamps whose lexical order is the REVERSE of their
// chronological order, and asserts that precondition rather than assuming it.
// Rows go in with raw SQL deliberately: the production writer stamps
// time.Now(), which would hand these fixtures back to the clock.
//
// Note what these tests pin that TestDashboardCosts_LastActivityTimeOrdersWithinDay
// does not. That test seeds ONE row per conversation, so each slice's
// comparison only ever runs against the initial "" — the two-real-stamps path
// is never reached, and its stamps are whole seconds in one zone, which cannot
// invert. It pins the cross-row SORT, not the within-slice PICK.

// costOrderStamps returns two same-day stamps whose lexical order is inverted
// relative to time: the earlier instant sorts last as a string.
func costOrderStamps(t *testing.T) (day, earlier, later string) {
	t.Helper()
	day = time.Now().Format("2006-01-02")
	// ".9" is a proper prefix of ".95", so 'Z' (0x5A) loses to '5' (0x35).
	earlier = day + "T12:00:07.9Z"
	later = day + "T12:00:07.95Z"
	require.Greater(t, earlier, later,
		"fixture precondition: the EARLIER instant must sort LAST as a string, or nothing here is under test")
	return day, earlier, later
}

func TestDashboardCosts_LastActivityIsTheLatestInstantNotTheLargestString(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const conv = "wcaa-1111-2222-3333-4444"
	day, earlier, later := costOrderStamps(t)

	conn, err := db.Open()
	require.NoError(t, err)
	// One conversation, one day, two sessions: an original and its resume.
	// Cumulative cost rises, so both rows contribute a delta.
	_, err = conn.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd, updated_at)
		VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
		"wca-1", day, conv, 1.00, earlier,
		"wca-2", day, conv, 1.25, later)
	require.NoError(t, err, "seed one conv's original and resume with inverted stamps")

	out := fetchCosts(t, agentd.BuildDashboardHandlerForTest(), "")

	require.Len(t, out.Agents, 1, "one breakdown row per (conv, day)")
	assert.Equal(t, later, out.Agents[0].LastActivity,
		"the slice's last activity is its latest INSTANT, not the stamp that spells the largest string")
	// The cost is the conversation's high-water cumulative — TCL-932's fix,
	// asserted here so a regression in either half is distinguishable.
	assert.InDelta(t, 1.25, out.Agents[0].CostUSD, 1e-9,
		"cost stays the high-water cumulative, not the per-session sum")
}

// The case that decides the remedy. Rows with no conv_id are grouped by their
// own session id in the db walk, but the breakdown's slice key is the RAW
// conv id — so every conv-less row on a day collapses into ONE slice whose
// deltas arrive in session-id order, NOT in time order.
//
// That is why "the rows arrive chronologically now, so take the last one" is
// wrong: here the last delta is the EARLIER spend. Only comparing instants
// gets it right.
func TestDashboardCosts_ConvLessRowsShareASliceAndStillReportTheLatestInstant(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	day, earlier, later := costOrderStamps(t)

	conn, err := db.Open()
	require.NoError(t, err)
	// Session ids chosen so the walk's per-session grouping puts the LATER
	// spend first and the EARLIER one last: "aaa" < "zzz".
	_, err = conn.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd, updated_at)
		VALUES (?, ?, '', ?, ?), (?, ?, '', ?, ?)`,
		"aaa-late", day, 2.00, later,
		"zzz-early", day, 1.00, earlier)
	require.NoError(t, err, "seed two conv-less sessions whose id order opposes their time order")

	out := fetchCosts(t, agentd.BuildDashboardHandlerForTest(), "")

	require.Len(t, out.Agents, 1, "conv-less rows share the one slice keyed by an empty conv id")
	assert.Equal(t, later, out.Agents[0].LastActivity,
		"last activity is the latest instant even when the last delta in the slice is the earlier spend")
}

func TestDashboardCosts_BreakdownOrdersByInstantWithinADay(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const convLate = "wclb-1111-2222-3333-4444"
	const convEarly = "wceb-1111-2222-3333-4444"
	day, earlier, later := costOrderStamps(t)

	conn, err := db.Open()
	require.NoError(t, err)
	// The later-active conv is the cheaper one, so recency — not cost — has
	// to be what orders them.
	_, err = conn.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd, updated_at)
		VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
		"wcb-late", day, convLate, 0.10, later,
		"wcb-early", day, convEarly, 5.00, earlier)
	require.NoError(t, err, "seed two convs whose stamps invert lexically")

	out := fetchCosts(t, agentd.BuildDashboardHandlerForTest(), "")

	require.Len(t, out.Agents, 2, "one breakdown row per conv")
	assert.Equal(t, convLate, out.Agents[0].ConvID,
		"the more recently active conv sorts first, by instant rather than by spelling")
	assert.Equal(t, convEarly, out.Agents[1].ConvID,
		"the pricier but earlier conv sorts below")
}

// A row with only a calendar day must still sort below a same-day row that
// carries a precise time — the behaviour the old single-key form got by
// appending "T00:00:00", and which the two-tier comparator has to preserve.
func TestDashboardCosts_DatedRowOutranksUnstampedRowOnTheSameDay(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const convStamped = "wcsb-1111-2222-3333-4444"
	const convBare = "wcbb-1111-2222-3333-4444"
	day, earlier, _ := costOrderStamps(t)

	conn, err := db.Open()
	require.NoError(t, err)
	// The unstamped conv is the pricier one, so precision — not cost — has to
	// be what orders them.
	_, err = conn.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd, updated_at)
		VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, '')`,
		"wcs-stamped", day, convStamped, 0.10, earlier,
		"wcs-bare", day, convBare, 5.00)
	require.NoError(t, err, "seed one stamped and one unstamped row on the same day")

	out := fetchCosts(t, agentd.BuildDashboardHandlerForTest(), "")

	require.Len(t, out.Agents, 2, "one breakdown row per conv")
	assert.Equal(t, convStamped, out.Agents[0].ConvID,
		"a known time on a day outranks the same day with no time")
	assert.Empty(t, out.Agents[1].LastActivity,
		"the unstamped row reports no last-activity time")
}
