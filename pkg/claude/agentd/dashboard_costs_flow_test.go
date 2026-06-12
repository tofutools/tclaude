package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Wire-shape mirrors of agentd's /api/costs response — the Costs tab
// renders straight from these fields.
type costsResp struct {
	From     string          `json:"from"`
	To       string          `json:"to"`
	Days     []costsRespDay  `json:"days"`
	Agents   []costsRespConv `json:"agents"`
	TotalUSD float64         `json:"total_usd"`
}
type costsRespDay struct {
	Day     string  `json:"day"`
	CostUSD float64 `json:"cost_usd"`
}
type costsRespConv struct {
	ConvID       string  `json:"conv_id"`
	Title        string  `json:"title"`
	CostUSD      float64 `json:"cost_usd"`
	LastDay      string  `json:"last_day"`
	LastActivity string  `json:"last_activity"`
	Model        string  `json:"model"`
}

func fetchCosts(t *testing.T, mux http.Handler, query string) costsResp {
	t.Helper()
	r := testharness.JSONRequest(t, http.MethodGet, "/api/costs"+query, nil)
	rec := testharness.Serve(mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "/api/costs body=%s", rec.Body.String())
	var out costsResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode costs")
	return out
}

// Scenario: the Costs tab opens. Two agents have accrued API cost —
// one across two sessions (a reincarnation), one of which is already
// retired and its sessions row deleted. GET /api/costs must return a
// zero-filled daily series from the first of the month through today
// with today's bar carrying the combined spend, plus a per-agent
// breakdown grouped by conv and sorted by cost — retired history
// included, since session_cost_daily outlives the sessions rows.
func TestDashboardCosts_DailySeriesAndBreakdown(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const convA = "wcsa-1111-2222-3333-4444"
	const convB = "wcsb-1111-2222-3333-4444"

	// Agent A: two sessions (think reincarnation) — one still live, one
	// whose sessions row is gone. The daily rows must carry both.
	seedAgentCostSession(t, "wcs-a1", convA, 1.00)
	seedAgentCostSession(t, "wcs-a2", convA, 0.25)
	require.NoError(t, db.DeleteSession("wcs-a2"), "retire one of A's sessions")
	// Agent B: a single cheaper session.
	seedAgentCostSession(t, "wcs-b1", convB, 0.50)

	out := fetchCosts(t, agentd.BuildDashboardHandlerForTest(), "")

	now := time.Now()
	today := now.Format("2006-01-02")
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
	assert.Equal(t, monthStart, out.From, "default span starts at the first of the month")
	assert.Equal(t, today, out.To, "span ends today")

	require.NotEmpty(t, out.Days, "zero-filled day series present")
	assert.Equal(t, monthStart, out.Days[0].Day, "series starts at from")
	last := out.Days[len(out.Days)-1]
	assert.Equal(t, today, last.Day, "series ends at to")
	assert.InDelta(t, 1.75, last.CostUSD, 1e-9, "today's bar carries all spend recorded today")
	assert.Equal(t, len(out.Days), daysInclusive(t, monthStart, today), "one point per calendar day, gaps zero-filled")

	// Look the rows up by conv rather than by index: ordering is by
	// last-activity time now (exercised by AgentOrderingAndModels), and
	// this scenario is about the sums — A's spend includes its retired
	// session's history, which outlives the deleted sessions row.
	require.Len(t, out.Agents, 2, "one breakdown row per conv")
	byConv := map[string]costsRespConv{}
	for _, a := range out.Agents {
		byConv[a.ConvID] = a
	}
	assert.InDelta(t, 1.25, byConv[convA].CostUSD, 1e-9, "A's two sessions summed, retired one included")
	assert.Equal(t, today, byConv[convA].LastDay)
	assert.NotEmpty(t, byConv[convA].LastActivity, "live session's spend carries a precise last-activity time")
	assert.InDelta(t, 0.50, byConv[convB].CostUSD, 1e-9)

	assert.InDelta(t, 1.75, out.TotalUSD, 1e-9, "span total matches the series")
}

// Scenario: explicit spans. A from date in the past widens the series
// (still zero-filled); a malformed from is a 400, not a silent
// default.
func TestDashboardCosts_FromParam(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	from := time.Now().AddDate(0, 0, -29).Format("2006-01-02")
	out := fetchCosts(t, mux, "?from="+from)
	assert.Equal(t, from, out.From, "explicit from echoed")
	assert.Len(t, out.Days, 30, "last-30d span zero-fills 30 points")
	assert.Zero(t, out.TotalUSD, "no cost recorded")

	r := testharness.JSONRequest(t, http.MethodGet, "/api/costs?from=junk", nil)
	rec := testharness.Serve(mux, r)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "malformed from rejected")
}

// Scenario: the per-agent breakdown's ordering and model column. Two
// agents spent today on different models; a third spent more, but
// days ago and its sessions row is long gone. Rows must come back
// latest-activity-first (cost only breaks same-day ties — recency
// outranks spend), today's agents must carry the model their session
// reported, and the retired agent has no live session to resolve a
// model from, so its model is empty.
func TestDashboardCosts_AgentOrderingAndModels(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const convA = "wcoa-1111-2222-3333-4444"
	const convB = "wcob-1111-2222-3333-4444"
	const convC = "wcoc-1111-2222-3333-4444"

	seedAgentCostSession(t, "wco-a1", convA, 1.00)
	require.NoError(t, db.UpdateSessionModel("wco-a1", "Fable 5"), "model for A")
	seedAgentCostSession(t, "wco-b1", convB, 2.00)
	require.NoError(t, db.UpdateSessionModel("wco-b1", "Opus 4.8"), "model for B")

	// Agent C: the biggest spender, but days ago. The production write
	// path always stamps today, so its daily row is inserted directly —
	// exactly what history left behind by a deleted session looks like.
	oldDay := time.Now().AddDate(0, 0, -3).Format("2006-01-02")
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd)
		VALUES (?, ?, ?, ?)`, "wco-c1", oldDay, convC, 5.00)
	require.NoError(t, err, "seed C's historical daily row")

	from := time.Now().AddDate(0, 0, -9).Format("2006-01-02")
	out := fetchCosts(t, agentd.BuildDashboardHandlerForTest(), "?from="+from)

	require.Len(t, out.Agents, 3, "one breakdown row per conv")
	assert.Equal(t, convB, out.Agents[0].ConvID, "today's agents first; B was costed last (and outspent A), so it leads")
	assert.Equal(t, "Opus 4.8", out.Agents[0].Model)
	assert.Equal(t, convA, out.Agents[1].ConvID)
	assert.Equal(t, "Fable 5", out.Agents[1].Model)
	assert.Equal(t, convC, out.Agents[2].ConvID, "older last-day sorts below today despite the larger spend")
	assert.Equal(t, oldDay, out.Agents[2].LastDay)
	assert.Empty(t, out.Agents[2].Model, "no live session row → no model to show")
}

// Scenario: within a single day, the precise last-activity timestamp
// orders the breakdown — not spend. A cheaper agent that was active
// later in the day must sort ahead of a pricier one that went quiet
// earlier, and the wire must carry the timestamp the UI renders. The
// day-only tie-break this replaced would have ordered them by cost.
func TestDashboardCosts_LastActivityTimeOrdersWithinDay(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)

	const convLate = "wcla-1111-2222-3333-4444"  // cheaper, active later
	const convEarly = "wcea-1111-2222-3333-4444" // pricier, quiet since morning

	// Direct daily rows with explicit timestamps so the ordering is
	// pinned to the times, not to wall-clock seed order. Same day; the
	// cheaper agent's stamp is the later one.
	now := time.Now()
	day := now.Format("2006-01-02")
	y, m, d0 := now.Date()
	early := time.Date(y, m, d0, 1, 0, 0, 0, now.Location()).Format(time.RFC3339Nano)
	late := time.Date(y, m, d0, 2, 0, 0, 0, now.Location()).Format(time.RFC3339Nano)

	conn, err := db.Open()
	require.NoError(t, err)
	_, err = conn.Exec(`INSERT INTO session_cost_daily (session_id, day, conv_id, cost_usd, updated_at)
		VALUES (?, ?, ?, ?, ?), (?, ?, ?, ?, ?)`,
		"wcl-1", day, convLate, 0.10, late,
		"wce-1", day, convEarly, 5.00, early)
	require.NoError(t, err, "seed two same-day rows with explicit times")

	out := fetchCosts(t, agentd.BuildDashboardHandlerForTest(), "")

	require.Len(t, out.Agents, 2, "one breakdown row per conv")
	assert.Equal(t, convLate, out.Agents[0].ConvID, "later activity sorts first despite the lower spend")
	assert.Equal(t, late, out.Agents[0].LastActivity, "precise last-activity timestamp surfaced on the wire")
	assert.Equal(t, convEarly, out.Agents[1].ConvID, "pricier-but-earlier agent sorts below")
	assert.Equal(t, early, out.Agents[1].LastActivity)
}

// seedAgentCostSession writes a sessions row tied to a conv and
// records cost through the production statusline write path, which
// also lands the session_cost_daily snapshot.
func seedAgentCostSession(t *testing.T, sessionID, convID string, cost float64) {
	t.Helper()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID:          sessionID,
		TmuxSession: "tmux-" + sessionID,
		ConvID:      convID,
		Cwd:         "/tmp/" + sessionID,
		Status:      "idle",
	}), "SaveSession %s", sessionID)
	require.NoError(t, db.UpdateSessionCost(sessionID, cost), "UpdateSessionCost %s", sessionID)
}

// daysInclusive counts calendar days from a through b (both
// "2006-01-02" keys), inclusive.
func daysInclusive(t *testing.T, a, b string) int {
	t.Helper()
	ta, err := time.Parse("2006-01-02", a)
	require.NoError(t, err)
	tb, err := time.Parse("2006-01-02", b)
	require.NoError(t, err)
	return int(tb.Sub(ta).Hours()/24) + 1
}
