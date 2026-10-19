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

type costsIgnoredResp struct {
	Agents   []costsRespConv `json:"agents"`
	TotalUSD float64         `json:"total_usd"`
	Days     []costsRespDay  `json:"days"`
	Ignored  []struct {
		ConvID    string  `json:"conv_id"`
		CostUSD   float64 `json:"cost_usd"`
		Harness   string  `json:"harness"`
		IgnoredAt string  `json:"ignored_at"`
	} `json:"ignored"`
}

func fetchCostsWithIgnored(t *testing.T, mux http.Handler) costsIgnoredResp {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/costs", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out costsIgnoredResp
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func setCostIgnored(t *testing.T, mux http.Handler, conv string, ignored bool) {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/costs/ignore",
		map[string]any{"conv_id": conv, "ignored": ignored}))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

// Repair path for spend recorded twice: the operator ignores a conversation
// in the Costs view. Its spend leaves every total — the Costs view, the
// header's month-to-date figure and `tclaude costs` — and it is listed under
// Ignored with what restoring would add back. Restore brings it back exactly.
func TestDashboardCosts_IgnoredConversationLeavesEveryTotalUntilRestored(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	for _, item := range []struct {
		session, conv string
		cost          float64
	}{{"keep-session", "conv-keep", 2}, {"dup-session", "conv-duplicate", 5}} {
		require.NoError(t, db.SaveSession(&db.SessionRow{
			ID: item.session, TmuxSession: "tmux-" + item.session, ConvID: item.conv,
			Cwd: "/tmp/" + item.session, Status: "idle", Harness: "claude",
		}))
		require.NoError(t, db.UpdateSessionCost(item.session, item.cost))
	}
	mux := agentd.BuildDashboardHandlerForTest()
	require.InDelta(t, 7, fetchCostsWithIgnored(t, mux).TotalUSD, 1e-12)

	// When: the duplicated conversation is ignored.
	setCostIgnored(t, mux, "conv-duplicate", true)

	// Then: it is gone from the totals, the rows and the daily chart …
	out := fetchCostsWithIgnored(t, mux)
	assert.InDelta(t, 2, out.TotalUSD, 1e-12)
	require.Len(t, out.Agents, 1)
	assert.Equal(t, "conv-keep", out.Agents[0].ConvID)
	assert.InDelta(t, 2, out.Days[len(out.Days)-1].CostUSD, 1e-12)
	// … listed as ignored with what it would add back …
	require.Len(t, out.Ignored, 1)
	assert.Equal(t, "conv-duplicate", out.Ignored[0].ConvID)
	assert.InDelta(t, 5, out.Ignored[0].CostUSD, 1e-12)
	assert.Equal(t, "claude", out.Ignored[0].Harness)
	_, err := time.Parse(time.RFC3339Nano, out.Ignored[0].IgnoredAt)
	assert.NoError(t, err)
	// … and left out of the header figure and `tclaude costs`.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.InDelta(t, 2, snap.Usage.TotalCostUSD, 1e-12, "header month-to-date")
	rec := accountQuery(t, f, "", "/v1/costs")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var account struct {
		TotalUSD float64 `json:"total_usd"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &account))
	assert.InDelta(t, 2, account.TotalUSD, 1e-12, "tclaude costs")

	// When: it is restored, everything comes back exactly.
	setCostIgnored(t, mux, "conv-duplicate", false)
	out = fetchCostsWithIgnored(t, mux)
	assert.InDelta(t, 7, out.TotalUSD, 1e-12)
	assert.Len(t, out.Agents, 2)
	assert.Empty(t, out.Ignored)
}

func TestDashboardCosts_IgnoreRejectsMalformedRequests(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()
	for name, body := range map[string]map[string]any{
		"missing ignored": {"conv_id": "conv-x"},
		"missing conv":    {"ignored": true},
		"unknown field":   {"conv_id": "conv-x", "ignored": true, "all": true},
	} {
		rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/costs/ignore", body))
		assert.Equal(t, http.StatusBadRequest, rec.Code, name)
	}
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/costs/ignore", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}
