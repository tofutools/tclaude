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
	ConvID  string  `json:"conv_id"`
	Title   string  `json:"title"`
	CostUSD float64 `json:"cost_usd"`
	LastDay string  `json:"last_day"`
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

	require.Len(t, out.Agents, 2, "one breakdown row per conv")
	assert.Equal(t, convA, out.Agents[0].ConvID, "sorted by cost descending")
	assert.InDelta(t, 1.25, out.Agents[0].CostUSD, 1e-9, "A's two sessions summed, retired one included")
	assert.Equal(t, today, out.Agents[0].LastDay)
	assert.Equal(t, convB, out.Agents[1].ConvID)
	assert.InDelta(t, 0.50, out.Agents[1].CostUSD, 1e-9)

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
