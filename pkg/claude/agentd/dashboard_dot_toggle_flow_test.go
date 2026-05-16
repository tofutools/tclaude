package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Flow coverage for the clickable status-dot on/off toggle. The dot is
// a frontend control, so its confirm dialog — every online (green-dot)
// click pops one before stopping — lives in dashboard.html and is not
// observable here. What IS observable — and what these scenarios pin —
// is the backend effect a dot click produces: clicking an online dot
// reaches POST /api/agents/{conv}/stop (soft /exit) and clicking an
// offline dot reaches POST /api/agents/{conv}/resume. Those are the
// very endpoints the existing "shut down" / "wake" row buttons hit;
// the dot toggle adds no parallel endpoint, so testing the endpoints
// IS testing the toggle's reachable surface.

// dotOpResp decodes the per-conv result both /stop and /resume return.
type dotOpResp struct {
	ConvID string `json:"conv_id"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

// postDotVerb fires POST /api/agents/{conv}/{verb} at the dashboard
// mux. body is the raw JSON request body (e.g. `{"force":false}`),
// empty for none. Mirrors the fetch() the dot-toggle click issues —
// the sibling postAgentVerb is body-less, but the dot toggle always
// sends an explicit {"force":false} on stop, so this helper carries it.
func postDotVerb(t *testing.T, mux http.Handler, conv, verb, body string) (int, dotOpResp) {
	t.Helper()
	var reqBody any
	if body != "" {
		reqBody = json.RawMessage(body)
	}
	rec := testharness.Serve(mux, testharness.JSONRequest(
		t, http.MethodPost, "/api/agents/"+conv+"/"+verb, reqBody))
	var resp dotOpResp
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp),
			"decode %s response: %s", verb, rec.Body.String())
	}
	return rec.Code, resp
}

// Scenario: clicking a GREEN (online) status dot turns the agent off.
// The handler POSTs /stop with force=false — the same soft /exit the
// "shut down" row button's soft path uses. The live tmux session goes
// away and the dashboard snapshot flips the agent to offline.
func TestDotToggle_OnlineDotSoftStopsAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "dton-1111-2222-3333-444444444444"
	const tmuxSes = "tmux-dton"
	f.HaveConvWithTitle(conv, "busy-worker")
	f.HaveAliveSession(conv, "spwn-dton", tmuxSes, "/tmp/dton")
	f.HaveEnrolledAgent(conv)
	require.True(t, f.World.Tmux.IsAlive(tmuxSes), "pre: the agent's session is alive")

	// A green-dot click always sends a SOFT stop — force is never set
	// by the toggle, idle or not. Force-kill stays behind the explicit
	// "shut down" button's confirm.
	code, resp := postDotVerb(t, mux, conv, "stop", `{"force":false}`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "soft_stopped", resp.Action,
		"a green-dot click must soft-exit, never force-kill; detail=%s", resp.Detail)
	assert.False(t, f.World.Tmux.IsAlive(tmuxSes), "the tmux session must be gone")

	snap := fetchDashSnapshot(t, mux)
	a := findDashAgent(snap, conv)
	require.NotNil(t, a, "the agent stays on the roster after a soft-stop")
	assert.False(t, a.Online, "after the toggle the agent's dot must read offline")
}

// Scenario: clicking a GREY (offline) status dot turns the agent back
// on. The handler POSTs /resume — the same wake the "wake" row button
// uses — and the dashboard snapshot flips the agent back to online.
// The full off→on cycle proves the dot is a real toggle.
func TestDotToggle_OfflineDotWakesAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "dtof-1111-2222-3333-444444444444"
	const tmuxSes = "tmux-dtof"
	f.HaveConvWithTitle(conv, "sleepy-worker")
	f.HaveAliveSession(conv, "spwn-dtof", tmuxSes, "/tmp/dtof")
	f.HaveEnrolledAgent(conv)

	// Turn it off first so the grey dot is the real state under test.
	code, _ := postDotVerb(t, mux, conv, "stop", `{"force":false}`)
	require.Equal(t, http.StatusOK, code)
	require.False(t, f.World.Tmux.IsAlive(tmuxSes), "pre: agent is offline before the grey-dot click")
	require.False(t, findDashAgent(fetchDashSnapshot(t, mux), conv).Online,
		"pre: the snapshot agrees the agent is offline")

	// Grey-dot click → wake. Resume is non-destructive, so the toggle
	// fires it with no confirm.
	code, resp := postDotVerb(t, mux, conv, "resume", "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "resumed", resp.Action,
		"a grey-dot click must resume the agent; detail=%s", resp.Detail)

	snap := fetchDashSnapshot(t, mux)
	a := findDashAgent(snap, conv)
	require.NotNil(t, a, "the agent is still on the roster after waking")
	assert.True(t, a.Online, "after the toggle the agent's dot must read online again")
}

// Scenario: the toggle is safe to mash. The dashboard re-renders
// asynchronously, so a click can land against a stale dot. Both
// endpoints are idempotent: a /stop on an already-off agent and a
// /resume on an already-on agent are no-ops that report a skipped:*
// action rather than erroring.
func TestDotToggle_IdempotentBothDirections(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "dtid-1111-2222-3333-444444444444"
	const tmuxSes = "tmux-dtid"
	f.HaveConvWithTitle(conv, "toggle-worker")
	f.HaveAliveSession(conv, "spwn-dtid", tmuxSes, "/tmp/dtid")
	f.HaveEnrolledAgent(conv)

	// Online agent: a redundant /resume (stale grey-dot click) no-ops.
	code, resp := postDotVerb(t, mux, conv, "resume", "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "skipped:already_online", resp.Action,
		"resuming an online agent must be a reported no-op")

	// Take it offline, then a redundant /stop (stale green-dot click)
	// no-ops too.
	code, _ = postDotVerb(t, mux, conv, "stop", `{"force":false}`)
	require.Equal(t, http.StatusOK, code)
	code, resp = postDotVerb(t, mux, conv, "stop", `{"force":false}`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "skipped:already_offline", resp.Action,
		"stopping an offline agent must be a reported no-op")
}
