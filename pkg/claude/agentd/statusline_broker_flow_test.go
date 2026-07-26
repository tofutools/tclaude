package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TCL-754, second half. A `tclaude-layer` agent's status line cannot
// reach the conversation database, so it POSTs its render to agentd and
// the daemon performs the writes and returns the reads.
//
// These drive /v1/whoami/statusline through the production mux and assert
// on the dashboard's own read path, because that is what a status line
// exists to feed: the context meter, the model and effort labels, the
// cost badge and the "where is this agent" cells.

const (
	slLayerConv  = "51000000-1111-2222-3333-444444444444"
	slLayerLabel = "spwn-sl-layer"

	slVictimConv  = "5100beef-1111-2222-3333-444444444444"
	slVictimLabel = "spwn-sl-victim"
)

// statuslinePayload builds the stdin document Claude Code hands its
// statusline command.
func statuslinePayload(convID, model, modelID, effort string, pct float64, tokIn, tokOut, window int64, cost float64) []byte {
	doc := map[string]any{
		"session_id": convID,
		"model":      map[string]any{"id": modelID, "display_name": model},
		"workspace":  map[string]any{"current_dir": "/home/agent/proj"},
		"context_window": map[string]any{
			"used_percentage":     pct,
			"total_input_tokens":  tokIn,
			"total_output_tokens": tokOut,
			"context_window_size": window,
		},
		"cost":   map[string]any{"total_cost_usd": cost},
		"effort": map[string]any{"level": effort},
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return raw
}

// postBrokeredRender drives POST /v1/whoami/statusline as a caller at
// callerPID.
func postBrokeredRender(t *testing.T, f *testharness.Flow, callerPID int, body statusbar.BrokeredRenderRequest) (int, statusbar.BrokeredRenderResponse) {
	t.Helper()
	req := testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/statusline", body)
	req = agentd.AsAgentPeerWithPID(req, "", callerPID)
	rec := testharness.Serve(f.Mux, req)
	var out statusbar.BrokeredRenderResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode broker response")
	}
	return rec.Code, out
}

// A wrapped agent's status line must reach the dashboard exactly like an
// unwrapped one's. This is the acceptance property of the ticket: every
// per-agent cell the status line is the sole writer of — context meter,
// model, effort, cost, working location — must be populated for an agent
// that never touched the database itself.
func TestStatuslineBroker_PopulatesTheDashboardForAWrappedAgent(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, slLayerConv, slLayerLabel, "tmux-sl-layer", brokerPanePID)
	f.HaveEnrolledAgent(slLayerConv)

	code, resp := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
		RenderConvID: slLayerConv,
		Payload:      statuslinePayload(slLayerConv, "Opus 5", "claude-opus-5", "high", 42, 84000, 3000, 200000, 1.25),
		Git: &statusbar.GitSnapshot{
			Branch: "feature/sandbox", RepoURL: "https://github.com/x/y", DefaultBranch: "main",
		},
		ApplyWrites: true,
	})
	require.Equal(t, http.StatusOK, code, "the daemon should apply a wrapped agent's render")
	assert.True(t, resp.Owned, "a render naming its own conversation owns the row")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	row := findDashAgent(snap, slLayerConv)
	require.NotNil(t, row, "agent %s missing from the dashboard", slLayerConv)

	assert.EqualValues(t, 42, row.State.ContextPct,
		"the context meter must show what the wrapped agent's bar showed")
	assert.EqualValues(t, 84000, row.State.TokensInput)
	assert.EqualValues(t, 3000, row.State.TokensOutput)
	assert.EqualValues(t, 200000, row.State.ContextWindowSize,
		"the STORED window stays the model's real one, not a re-based value")

	snapshot, err := db.GetContextSnapshot(slLayerLabel)
	require.NoError(t, err)
	assert.Equal(t, "Opus 5", snapshot.Model, "model label")
	assert.Equal(t, "claude-opus-5", snapshot.ModelID, "full model id, which resume feeds back to --model")
	assert.Equal(t, "high", snapshot.EffortLevel, "reasoning effort")
	assert.InDelta(t, 1.25, snapshot.CostUSD, 0.0001,
		"with no rate-limit buckets this is an API-plan cost, not a virtual one")

	ws, err := db.GetAgentWorkspace(slLayerConv)
	require.NoError(t, err)
	assert.Equal(t, "feature/sandbox", ws.Branch,
		"the workspace snapshot drives the dashboard's location cells")
	assert.Equal(t, "/home/agent/proj", ws.Cwd)
}

// Identity comes from the process ancestry the daemon walks, never from
// the request. A render that claims somebody else's session must be
// refused outright rather than quietly applied to whichever row the
// ancestry produced — a silent re-target would be just as wrong as
// honouring the claim.
func TestStatuslineBroker_RefusesARenderClaimingAnotherSession(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, slLayerConv, slLayerLabel, "tmux-sl-layer", brokerPanePID)
	f.HaveAliveSession(slVictimConv, slVictimLabel, "tmux-sl-victim", f.World.HomeDir)

	code, _ := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
		ClaimedSessionID: slVictimLabel,
		RenderConvID:     slVictimConv,
		Payload:          statuslinePayload(slVictimConv, "Haiku 4.5", "claude-haiku-4-5", "low", 99, 1, 1, 200000, 0),
		ApplyWrites:      true,
	})
	assert.Equal(t, http.StatusForbidden, code,
		"a claimed session id that disagrees with the resolved row must be refused")

	victim, err := db.GetContextSnapshot(slVictimLabel)
	require.NoError(t, err)
	assert.Empty(t, victim.Model,
		"the victim's model must not have been rewritten by a foreign render")
}

// The attribution gate: a render that names a conversation the resolved
// row does not track is a foreign process (a nested `claude` a human
// started in the agent's own pane, say). It must write nothing.
//
// Brokered, this is stricter than the direct path on purpose. Directly,
// such a render still writes its OWN workspace row, which is harmless
// because the writer is that process and the row is its own. Through the
// daemon the writer is the host and the conversation is a caller-supplied
// string, so the same permissiveness would let any wrapped agent overwrite
// any peer's location cells.
func TestStatuslineBroker_ForeignRenderWritesNothing(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, slLayerConv, slLayerLabel, "tmux-sl-layer", brokerPanePID)
	f.HaveEnrolledAgent(slLayerConv)

	const foreignConv = "f0000000-1111-2222-3333-444444444444"
	code, resp := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
		RenderConvID: foreignConv,
		Payload:      statuslinePayload(foreignConv, "Haiku 4.5", "claude-haiku-4-5", "low", 17, 34000, 100, 200000, 0),
		Git:          &statusbar.GitSnapshot{Branch: "someone-elses-branch"},
		ApplyWrites:  true,
	})
	require.Equal(t, http.StatusOK, code,
		"a foreign render is ignored, not an error — the pane still has a bar to draw")
	assert.False(t, resp.Owned, "the daemon must report that it wrote nothing")

	snapshot, err := db.GetContextSnapshot(slLayerLabel)
	require.NoError(t, err)
	assert.Empty(t, snapshot.Model,
		"a nested harness must not stamp its own model onto the agent's row")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	row := findDashAgent(snap, slLayerConv)
	require.NotNil(t, row)
	assert.Zero(t, row.State.ContextPct,
		"a nested harness must not stamp its own context usage onto the agent's meter")

	foreignWS, err := db.GetAgentWorkspace(foreignConv)
	require.NoError(t, err)
	assert.Empty(t, foreignWS.ConvID,
		"the brokered path must not key a workspace row by a conversation the caller merely named")
}

// The conv-id reaches host-side path joins through the shared write path,
// so the same segment rule the hook endpoint applies must apply here.
func TestStatuslineBroker_RejectsPathTraversingConvID(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, slLayerConv, slLayerLabel, "tmux-sl-layer", brokerPanePID)

	for _, convID := range []string{"../../../etc/passwd", "..", "a/b"} {
		code, _ := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
			RenderConvID: convID,
			Payload:      statuslinePayload(convID, "Opus 5", "claude-opus-5", "high", 5, 1, 1, 200000, 0),
			ApplyWrites:  true,
		})
		assert.Equal(t, http.StatusBadRequest, code,
			"conv-id %q is not a single path-safe segment and must be refused", convID)
	}
}

// A caller the daemon cannot place has no row to write, and guessing is
// exactly the failure mode the ancestry walk exists to prevent.
func TestStatuslineBroker_RefusesCallersItCannotPlace(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)
	// No session row is recorded for this ancestry at all.

	code, _ := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
		RenderConvID: slLayerConv,
		Payload:      statuslinePayload(slLayerConv, "Opus 5", "claude-opus-5", "high", 5, 1, 1, 200000, 0),
		ApplyWrites:  true,
	})
	assert.Equal(t, http.StatusForbidden, code,
		"an unplaceable caller must be refused, not applied to a guessed row")
}

// A render can ask for its reads without asking for its writes — that is
// what a coasting bar does when its payload has not changed. The daemon
// must honour the distinction, or the change gate would be pointless.
func TestStatuslineBroker_ReadsOnlyRenderRecordsNothing(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, slLayerConv, slLayerLabel, "tmux-sl-layer", brokerPanePID)
	f.HaveEnrolledAgent(slLayerConv)

	code, resp := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
		RenderConvID: slLayerConv,
		Payload:      statuslinePayload(slLayerConv, "Opus 5", "claude-opus-5", "high", 42, 84000, 3000, 200000, 0),
		ApplyWrites:  false,
	})
	require.Equal(t, http.StatusOK, code)
	assert.True(t, resp.Owned, "the gate still runs — the render just records nothing")

	snapshot, err := db.GetContextSnapshot(slLayerLabel)
	require.NoError(t, err)
	assert.Empty(t, snapshot.Model, "a reads-only render must not write the model")

	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	row := findDashAgent(snap, slLayerConv)
	require.NotNil(t, row)
	assert.Zero(t, row.State.ContextPct, "a reads-only render must not write the context snapshot")
}

// The auto-compaction pin the bar re-bases against is a database read the
// sandbox cannot perform. It must come back through the broker, and it
// must come back through the ownership gate — a foreign render must not
// learn (or be re-based by) somebody else's pin.
func TestStatuslineBroker_ReturnsTheRowsPinnedWindow(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, slLayerConv, slLayerLabel, "tmux-sl-layer", brokerPanePID)
	require.NoError(t, db.UpdateSessionAutoCompactWindow(slLayerLabel, "450000"),
		"record a pin on the row, as a pinned launch would")

	payload := statuslinePayload(slLayerConv, "Opus 5", "claude-opus-5", "high", 21, 210000, 1000, 1000000, 0)

	code, resp := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
		RenderConvID: slLayerConv,
		Payload:      payload,
	})
	require.Equal(t, http.StatusOK, code)
	assert.EqualValues(t, 450000, resp.PinnedWindow,
		"the pane has no way to read this for itself; the bar is wrong without it")

	const foreignConv = "f0000000-1111-2222-3333-444444444444"
	_, foreign := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
		RenderConvID: foreignConv,
		Payload:      statuslinePayload(foreignConv, "Opus 5", "claude-opus-5", "high", 21, 210000, 1000, 1000000, 0),
	})
	assert.Zero(t, foreign.PinnedWindow,
		"a foreign render must not be re-based against the row's pin")
}

// The operator's headline condition on the limiter: measured everywhere,
// enforced only when the config says so. This drives both halves through
// the real endpoint, because the difference between "shadow mode" and
// "broken" is exactly whether a caller past the ceiling still gets its
// render applied.
func TestStatuslineBroker_RateLimitIsShadowUntilTheOperatorEnablesIt(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBrokerLimiterForTest())

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, slLayerConv, slLayerLabel, "tmux-sl-layer", brokerPanePID)

	render := statusbar.BrokeredRenderRequest{
		RenderConvID: slLayerConv,
		Payload:      statuslinePayload(slLayerConv, "Opus 5", "claude-opus-5", "high", 5, 1, 1, 200000, 0),
	}

	// Shadow mode (the default): well past the ceiling, still served.
	for i := range 40 {
		code, _ := postBrokeredRender(t, f, callerPID, render)
		require.Equal(t, http.StatusOK, code,
			"request %d must still be served while enforcement is off", i+1)
	}

	// The operator flips the toggle.
	cfg := config.DefaultConfig()
	cfg.Broker = &config.BrokerConfig{EnforceLimits: true}
	require.NoError(t, config.Save(cfg))
	agentd.ResetBrokerLimiterForTest()

	var refused bool
	for range 40 {
		code, _ := postBrokeredRender(t, f, callerPID, render)
		if code == http.StatusTooManyRequests {
			refused = true
			break
		}
	}
	assert.True(t, refused,
		"with broker.enforce_limits on, a caller past the ceiling must actually be refused")
}

// The 10 MiB body cap, driven through the real endpoint. The previous
// version of this test asserted a constant against its own literal, which
// would have survived deleting the guard entirely.
func TestStatuslineBroker_RefusesAnOverCapBody(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBrokerLimiterForTest())

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, slLayerConv, slLayerLabel, "tmux-sl-layer", brokerPanePID)

	// A payload past the ceiling. It is the request BODY that is capped,
	// and the statusline payload is the only field that can grow, so
	// oversizing it is the honest way in.
	huge := make([]byte, (10<<20)+4096)
	for i := range huge {
		huge[i] = 'x'
	}
	code, _ := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
		RenderConvID: slLayerConv,
		Payload:      huge,
		ApplyWrites:  true,
	})
	assert.Equal(t, http.StatusRequestEntityTooLarge, code,
		"a body past the cap must be refused, and refused whatever enforcement says: "+
			"the reader has already truncated it, so there is nothing left to apply")
}

// A payload that names NO conversation is accepted as the resolved row's
// own. This pins a deliberate fail-soft rather than an oversight: Claude
// Code versions predating session_id emit exactly this, and refusing them
// would cost real agents their telemetry to guard against a case there is
// no evidence for.
//
// It is not an escalation, and the reason is worth stating because the
// identity half of the gate makes it true: the only row a brokered render
// can reach is the one the daemon resolved from the caller's OWN process
// ancestry, which that caller's legitimate status line already writes.
// A caller gains nothing by omitting the field that it does not already
// have by sending the field correctly.
func TestStatuslineBroker_PayloadWithNoConversationWritesTheCallersOwnRow(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, slLayerConv, slLayerLabel, "tmux-sl-layer", brokerPanePID)
	f.HaveAliveSession(slVictimConv, slVictimLabel, "tmux-sl-victim", f.World.HomeDir)

	code, resp := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
		RenderConvID: "",
		Payload:      statuslinePayload("", "Opus 5", "claude-opus-5", "high", 33, 66000, 500, 200000, 0),
		ApplyWrites:  true,
	})
	require.Equal(t, http.StatusOK, code)
	assert.True(t, resp.Owned, "an old payload with no conversation is fail-soft, as on the direct path")

	own, err := db.GetContextSnapshot(slLayerLabel)
	require.NoError(t, err)
	assert.Equal(t, "Opus 5", own.Model, "it writes the caller's OWN resolved row")

	victim, err := db.GetContextSnapshot(slVictimLabel)
	require.NoError(t, err)
	assert.Empty(t, victim.Model,
		"and reaches no other row — identity, not the payload, chose the target")

	// The workspace key falls back to the row's own conversation rather
	// than to the empty string the caller sent.
	ws, err := db.GetAgentWorkspace(slLayerConv)
	require.NoError(t, err)
	assert.Equal(t, slLayerConv, ws.ConvID)
}
