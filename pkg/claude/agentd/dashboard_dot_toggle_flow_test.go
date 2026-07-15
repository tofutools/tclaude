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

// Flow coverage for the clickable status-dot — the agent's SOLE
// per-row power control (the dedicated wake/shutdown row buttons were
// removed; the dot replaces them). The dot is a frontend control, so
// its transaction dialog — an online click pops the 3-way Cancel / Soft
// exit / Force kill choice — lives in the Preact island and is not
// observable here. What IS observable — and what these scenarios pin
// — is the backend effect a dot click produces: clicking an online
// dot reaches POST /api/agents/{conv}/stop (soft /exit, or a
// force-kill when the confirm's "Force kill" is picked) and clicking
// an offline dot reaches POST /api/agents/{conv}/resume — so testing
// the endpoints IS testing the dot's reachable surface.

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

// Scenario: clicking a GREEN (online) status dot and picking "Soft
// exit" in the confirm turns the agent off gently. The handler POSTs
// /stop with force=false — a soft /exit. The live tmux session goes
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

	// The confirm's "Soft exit" choice sends {"force":false} — a soft
	// /exit. (The "Force kill" choice is the next scenario.)
	code, resp := postDotVerb(t, mux, conv, "stop", `{"force":false}`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "soft_stopped", resp.Action,
		"a soft-exit choice must soft-stop, not force-kill; detail=%s", resp.Detail)
	assert.False(t, f.World.Tmux.IsAlive(tmuxSes), "the tmux session must be gone")

	snap := fetchDashSnapshot(t, mux)
	a := findDashAgent(snap, conv)
	require.NotNil(t, a, "the agent stays on the roster after a soft-stop")
	assert.False(t, a.Online, "after the toggle the agent's dot must read offline")
}

// Scenario: clicking a GREEN status dot and picking "Force kill" in
// the 3-way confirm hard-stops the agent. The handler POSTs /stop
// with force=true — a tmux kill-session, no /exit injection. This is
// the per-agent force-kill path the dedicated "shut down" row button
// used to own; folding it into the dot's confirm kept it reachable
// after that button was removed.
func TestDotToggle_OnlineDotCanForceKill(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "dtfk-1111-2222-3333-444444444444"
	const tmuxSes = "tmux-dtfk"
	f.HaveConvWithTitle(conv, "stuck-worker")
	f.HaveAliveSession(conv, "spwn-dtfk", tmuxSes, "/tmp/dtfk")
	f.HaveEnrolledAgent(conv)
	require.True(t, f.World.Tmux.IsAlive(tmuxSes), "pre: the agent's session is alive")

	// The confirm's "Force kill" choice sends {"force":true} — a tmux
	// kill-session that needs no cooperation from the agent.
	code, resp := postDotVerb(t, mux, conv, "stop", `{"force":true}`)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "killed", resp.Action,
		"a force-kill choice must tmux kill-session; detail=%s", resp.Detail)
	assert.False(t, f.World.Tmux.IsAlive(tmuxSes), "the tmux session must be gone")

	snap := fetchDashSnapshot(t, mux)
	a := findDashAgent(snap, conv)
	require.NotNil(t, a, "the agent stays on the roster after a force-kill")
	assert.False(t, a.Online, "after the force-kill the agent's dot must read offline")
}

// Scenario: clicking a GREY (offline) status dot turns the agent back
// on. The handler POSTs /resume — no confirm, resume is
// non-destructive — and the dashboard snapshot flips the agent back
// to online. The full off→on cycle proves the dot is a real toggle.
func TestDotToggle_OfflineDotWakesAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f := newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	const conv = "dtof-1111-2222-3333-444444444444"
	const tmuxSes = "tmux-dtof"
	f.HaveConvWithTitle(conv, "sleepy-worker")
	f.HaveAliveSession(conv, "spwn-dtof", tmuxSes, f.World.HomeDir)
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
