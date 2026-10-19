package agentd_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tcl-bigeme: usage sharing's cost lands where the credentials are. An agent
// whose model calls went through a peer's gateway keeps its recorded spend
// here, ignored with a billed-to reason (not restorable); the gateway's node
// lists what it paid for each peer as served to peers.
func TestDashboardCosts_BilledToPeerAndServedToPeers(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	for _, item := range []struct {
		session, conv string
		cost          float64
	}{{"own-session", "conv-own", 2}, {"shared-session", "conv-shared", 4}} {
		require.NoError(t, db.SaveSession(&db.SessionRow{
			ID: item.session, TmuxSession: "tmux-" + item.session, ConvID: item.conv,
			Cwd: "/tmp/" + item.session, Status: "idle", Harness: "claude", ExitLaunchGeneration: "gen-" + item.session,
		}))
		require.NoError(t, db.UpdateSessionCost(item.session, item.cost))
	}
	hash := sha256.Sum256([]byte("bearer"))
	require.NoError(t, db.BindModelProxyLaunch("shared-session", "team-key@inst_forge", hex.EncodeToString(hash[:])))
	d, err := db.Open()
	require.NoError(t, err)
	now := time.Now()
	for i, r := range []struct {
		peer  string
		micro int64
	}{{"inst_desk", 1_500_000}, {"inst_desk", 250_000}, {"", 9_000_000}} {
		_, err = d.Exec(`INSERT INTO model_proxy_requests(id,day,proxy,peer,session,model,charged_tokens,started_at,charged_micro_usd,priced) VALUES(?,?,?,?,?,?,?,?,?,1)`,
			"req-"+string(rune('a'+i)), now.UTC().Format("2006-01-02"), "team-key", r.peer, "s", "claude-sonnet-5-5", 10, now.UnixNano(), r.micro)
		require.NoError(t, err)
	}

	mux := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet, "/api/costs", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		TotalUSD float64 `json:"total_usd"`
		Ignored  []struct {
			ConvID   string  `json:"conv_id"`
			CostUSD  float64 `json:"cost_usd"`
			BilledTo string  `json:"billed_to"`
		} `json:"ignored"`
		Served []struct {
			Peer     string  `json:"peer"`
			Gateway  string  `json:"gateway"`
			CostUSD  float64 `json:"cost_usd"`
			Requests int     `json:"requests"`
		} `json:"served"`
		ServedTotalUSD float64 `json:"served_total_usd"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.InDelta(t, 2, out.TotalUSD, 1e-12, "the peer-billed agent is not in this node's totals")
	require.Len(t, out.Ignored, 1)
	require.Equal(t, "conv-shared", out.Ignored[0].ConvID)
	require.Equal(t, "team-key@inst_forge", out.Ignored[0].BilledTo)
	require.InDelta(t, 4, out.Ignored[0].CostUSD, 1e-12, "its spend stays recorded")
	require.Len(t, out.Served, 1, "this node's own agents' gateway use is not served to peers")
	require.Equal(t, "inst_desk", out.Served[0].Peer)
	require.Equal(t, 2, out.Served[0].Requests)
	require.InDelta(t, 1.75, out.ServedTotalUSD, 1e-9)

	rec = testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/costs/ignore", map[string]any{"conv_id": "conv-shared", "ignored": false}))
	require.Equal(t, http.StatusConflict, rec.Code, "a billed-to agent cannot be restored")
}
