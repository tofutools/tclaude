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
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TCL-754 flow tests: a `tclaude-layer` agent cannot reach the
// conversation database from inside its mount namespace, so its hook
// callbacks POST the parsed event to agentd and the daemon applies it
// host-side. These scenarios drive that endpoint through the production
// mux and assert on the same read paths a direct callback feeds.
//
// The property that matters is PARITY: a wrapped agent's hooks must land
// on the dashboard exactly like an unwrapped agent's. So the parity test
// below runs one identical event sequence twice — once direct, once
// brokered — and compares the resulting rows rather than asserting
// hand-written expectations, which would drift.

const (
	brokerLayerConv  = "b0000000-1111-2222-3333-444444444444"
	brokerDirectConv = "d0000000-1111-2222-3333-444444444444"
	brokerVictimConv = "5ac10000-1111-2222-3333-444444444444"

	brokerLayerLabel  = "spwn-broker-layer"
	brokerDirectLabel = "spwn-broker-direct"
	brokerVictimLabel = "spwn-broker-victim"

	// The wrapped ancestry the layer produces: the hook callback runs
	// under the harness, which runs under bubblewrap's inner shell, which
	// runs under bwrap, which runs under the pane shell whose pid the
	// sessions row was keyed by at spawn.
	brokerHookPID    = 7100
	brokerHarnessPID = 7101
	brokerInnerShPID = 7102
	brokerBwrapPID   = 7103
	brokerPanePID    = 7104

	brokerVictimPanePID = 7204
)

// layerProcTree models the wrapped ancestry above and returns the caller
// pid a brokered hook would connect from.
func layerProcTree(t *testing.T) int {
	t.Helper()
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{
			brokerHookPID:    "tclaude",
			brokerHarnessPID: "node",
			brokerInnerShPID: "sh",
			brokerBwrapPID:   "bwrap",
			brokerPanePID:    "sh",
		},
		map[int]int{
			brokerHookPID:    brokerHarnessPID,
			brokerHarnessPID: brokerInnerShPID,
			brokerInnerShPID: brokerBwrapPID,
			brokerBwrapPID:   brokerPanePID,
		},
	))
	return brokerHookPID
}

// haveLayerSession stands up a session row recorded as a tclaude-layer
// launch and keyed by the pane pid, which is what the ancestor walk has
// to cross the bwrap wrappers to reach.
func haveLayerSession(t *testing.T, f *testharness.Flow, conv, label, tmux string, panePID int) {
	t.Helper()
	f.HaveAliveSession(conv, label, tmux, f.World.HomeDir)
	row, err := db.LoadSession(label)
	require.NoError(t, err, "LoadSession(%s)", label)
	require.NotNil(t, row, "session row %s should exist", label)
	row.PID = panePID
	row.SandboxImplementation = "tclaude-layer"
	require.NoError(t, db.SaveSession(row), "record the layer launch")
}

// postBrokeredHook drives POST /v1/whoami/hook as a caller at callerPID.
func postBrokeredHook(t *testing.T, f *testharness.Flow, callerPID int, body session.BrokeredHookRequest) (int, session.BrokeredHookResponse) {
	t.Helper()
	req := testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/hook", body)
	req = agentd.AsAgentPeerWithPID(req, "", callerPID)
	rec := testharness.Serve(f.Mux, req)
	var out session.BrokeredHookResponse
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode broker response")
	}
	return rec.Code, out
}

// TestHookBroker_ParityWithDirectCallback is the acceptance property from
// TCL-754: a tclaude-layer agent has hook parity with a harness-builtin
// one. Two sessions get the same event sequence by the two different
// routes; every asserted surface must agree.
func TestHookBroker_ParityWithDirectCallback(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)

	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	f.HaveAliveSession(brokerDirectConv, brokerDirectLabel, "tmux-broker-direct", f.World.HomeDir)

	events := []session.HookCallbackInput{
		{HookEventName: "SessionStart", Source: "startup"},
		{HookEventName: "UserPromptSubmit", Prompt: "do the thing"},
		{HookEventName: "PostToolUse", ToolName: "Edit"},
		{HookEventName: "Stop"},
	}

	for _, base := range events {
		// Brokered: the layer agent's callback hands the event to agentd.
		// Note it sends NO session id it could be trusted on — the daemon
		// resolves the row from the caller's ancestry.
		brokered := base
		brokered.ConvID = brokerLayerConv
		brokered.Cwd = f.World.HomeDir
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{Input: brokered})
		require.Equal(t, http.StatusOK, code, "brokered %s should be applied", base.HookEventName)

		// Direct: the harness-builtin agent's callback writes for itself.
		direct := base
		direct.ConvID = brokerDirectConv
		direct.Cwd = f.World.HomeDir
		require.NoError(t, session.ApplyHook(direct, brokerDirectLabel),
			"direct %s should be applied", base.HookEventName)
	}

	layer, err := session.LoadSessionState(brokerLayerLabel)
	require.NoError(t, err)
	builtin, err := session.LoadSessionState(brokerDirectLabel)
	require.NoError(t, err)

	assert.Equal(t, builtin.Status, layer.Status,
		"a wrapped agent's status must track exactly like an unwrapped one's")
	assert.Equal(t, brokerLayerConv, layer.ConvID,
		"the brokered SessionStart must stamp the conv-id onto the resolved row")
	assert.False(t, layer.LastHook.IsZero(), "brokered events must stamp last_hook")

	// The dashboard read path, not just the row: both agents must render
	// identically on the surface the human actually looks at.
	f.HaveGroup("brokersquad")
	f.HaveMember("brokersquad", brokerLayerConv)
	f.HaveMember("brokersquad", brokerDirectConv)
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	layerRow := findDashMember(snap, "brokersquad", brokerLayerConv)
	builtinRow := findDashMember(snap, "brokersquad", brokerDirectConv)
	require.NotNil(t, layerRow, "the wrapped agent must appear on the dashboard")
	require.NotNil(t, builtinRow, "the unwrapped agent must appear on the dashboard")
	assert.Equal(t, builtinRow.State.Status, layerRow.State.Status,
		"dashboard status must match between a brokered and a direct agent")
}

// TestHookBroker_IdentityComesFromAncestryNotThePayload pins finding 3 of
// the inventory: the caller's TCLAUDE_SESSION_ID is a cross-check, never
// the authority. A wrapped agent that names another agent's session must
// be refused outright rather than quietly writing its own row (which
// would hide the attempt) or the victim's (which would be the bug).
func TestHookBroker_IdentityComesFromAncestryNotThePayload(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)

	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	haveLayerSession(t, f, brokerVictimConv, brokerVictimLabel, "tmux-broker-victim", brokerVictimPanePID)

	victimBefore, err := session.LoadSessionState(brokerVictimLabel)
	require.NoError(t, err)

	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        brokerVictimConv,
			HookEventName: "SessionStart",
			Source:        "startup",
			Cwd:           f.World.HomeDir,
		},
		ClaimedSessionID: brokerVictimLabel,
	})
	assert.Equal(t, http.StatusForbidden, code,
		"a claimed session id that disagrees with the resolved row must be refused")

	victimAfter, err := session.LoadSessionState(brokerVictimLabel)
	require.NoError(t, err)
	assert.Equal(t, victimBefore.ConvID, victimAfter.ConvID,
		"the victim's conv-id must be untouched")
	assert.Equal(t, victimBefore.LastHook, victimAfter.LastHook,
		"the victim's row must not record a hook it never fired")
}

// TestHookBroker_RefusesCallersItCannotPlace is the fail-closed half: a
// caller whose ancestry reaches no recorded session row gets nothing,
// rather than falling back to anything the request asserts about itself.
func TestHookBroker_RefusesCallersItCannotPlace(t *testing.T) {
	f := newFlow(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	// A harness-named process with no recorded ancestry at all.
	const orphanPID = 8100
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{orphanPID: "node"},
		map[int]int{},
	))

	code, _ := postBrokeredHook(t, f, orphanPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        brokerLayerConv,
			HookEventName: "SessionStart",
			Source:        "startup",
		},
		ClaimedSessionID: brokerLayerLabel,
	})
	assert.Equal(t, http.StatusForbidden, code,
		"an unplaceable caller must not be able to name its own session")
}

// TestHookBroker_PreCompactDecisionIsRelayed proves the one hook event
// whose OUTPUT matters survives the round trip. PreCompact may answer
// {"decision":"block"} to refuse an early auto-compaction; if the broker
// swallowed that, a wrapped agent would silently lose the guard.
func TestHookBroker_PreCompactDecisionIsRelayed(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	// The guard is opt-in, so turn it on, then seed a context snapshot at
	// the 200K boundary of a 1M window — the headline case the guard
	// exists to refuse.
	cfg := config.DefaultConfig()
	cfg.PreCompactGuard = &config.PreCompactGuardConfig{Enabled: true}
	require.NoError(t, config.Save(cfg))
	require.NoError(t, db.UpdateContextSnapshot(brokerLayerLabel, 20, 1, 0, 1_000_000))

	code, resp := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        brokerLayerConv,
			HookEventName: "PreCompact",
			Trigger:       "auto",
		},
	})
	require.Equal(t, http.StatusOK, code)

	var dec struct {
		Decision string `json:"decision"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Stdout), &dec),
		"the guard's decision document must be relayed verbatim, got %q", resp.Stdout)
	assert.Equal(t, "block", dec.Decision,
		"a wrapped agent must keep the pre-compact guard it would have had unwrapped")
}
