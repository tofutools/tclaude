package agentd_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
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

// A broker writes hook output into an HTTP response buffer first; that is not
// delivery. Only the sandboxed callback knows whether it subsequently wrote
// those bytes to the harness's stdout, so once-per-generation cadence must
// remain open until its explicit acknowledgement arrives.
func TestHookBroker_StandingOrderCommitRequiresRelayAck(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	group := f.HaveGroup("standing-order-broker")
	f.HaveMember(group.Name, brokerLayerConv)

	orderID, err := db.InsertStandingOrder(&db.StandingOrder{
		Name:             "broker-ack",
		TargetKind:       db.StandingTargetGroup,
		GroupID:          group.ID,
		Summary:          "Do not claim delivery before stdout relay.",
		TriggerEvent:     db.StandingTriggerSessionStart,
		TriggerSources:   []string{db.StandingSourceStartup},
		Timing:           db.StandingTimingSameContinuation,
		Cadence:          db.StandingCadenceOncePerGeneration,
		Enabled:          true,
		OperatorAuthored: true,
	})
	require.NoError(t, err)

	event := session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: brokerLayerConv, HookEventName: "SessionStart",
		Source: db.StandingSourceStartup, Cwd: f.World.HomeDir,
	}}
	code, resp := postBrokeredHook(t, f, callerPID, event)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, resp.Stdout, "Do not claim delivery")
	require.NotEmpty(t, resp.AckToken)

	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	assert.Nil(t, latest, "buffering an HTTP response is not harness delivery")
	delivered, err := db.StandingOrderDeliveredInEpoch(
		orderID, 1, brokerLayerConv, brokerLayerConv)
	require.NoError(t, err)
	assert.False(t, delivered, "a failed/disconnected relay must leave cadence retryable")

	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: brokerLayerLabel,
		AckToken:         resp.AckToken,
	})
	require.Equal(t, http.StatusOK, code)
	latest, err = db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeDelivered, latest.Outcome)

	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: brokerLayerLabel,
		AckToken:         resp.AckToken,
	})
	assert.Equal(t, http.StatusConflict, code, "an acknowledgement is one-shot")
}

// A stdout write failure is an explicit negative acknowledgement: it must
// release the cadence lock without recording delivery, so the next boundary
// can retry immediately rather than waiting for the acknowledgement TTL.
func TestHookBroker_StandingOrderRelayFailureReleasesWithoutCommit(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	group := f.HaveGroup("standing-order-relay-failure")
	f.HaveMember(group.Name, brokerLayerConv)

	orderID, err := db.InsertStandingOrder(&db.StandingOrder{
		Name:             "broker-relay-failure",
		TargetKind:       db.StandingTargetGroup,
		GroupID:          group.ID,
		Summary:          "Retry after a failed stdout relay.",
		TriggerEvent:     db.StandingTriggerSessionStart,
		TriggerSources:   []string{db.StandingSourceStartup},
		Timing:           db.StandingTimingSameContinuation,
		Cadence:          db.StandingCadenceOncePerGeneration,
		Enabled:          true,
		OperatorAuthored: true,
	})
	require.NoError(t, err)

	event := session.BrokeredHookRequest{Input: session.HookCallbackInput{
		ConvID: brokerLayerConv, HookEventName: "SessionStart",
		Source: db.StandingSourceStartup, Cwd: f.World.HomeDir,
	}}
	code, first := postBrokeredHook(t, f, callerPID, event)
	require.Equal(t, http.StatusOK, code)
	require.NotEmpty(t, first.AckToken)

	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: brokerLayerLabel,
		AckToken:         first.AckToken,
		RelayFailed:      true,
	})
	require.Equal(t, http.StatusOK, code)
	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	assert.Nil(t, latest, "a failed relay must not be recorded as delivery")

	code, retry := postBrokeredHook(t, f, callerPID, event)
	require.Equal(t, http.StatusOK, code)
	assert.Contains(t, retry.Stdout, "Retry after a failed stdout relay")
	require.NotEmpty(t, retry.AckToken,
		"the failed relay must release its lock so the next boundary can retry")

	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		ClaimedSessionID: brokerLayerLabel,
		AckToken:         retry.AckToken,
	})
	require.Equal(t, http.StatusOK, code)
	latest, err = db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeDelivered, latest.Outcome)
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

// The client-side oversize test proves trimming SETS PayloadTrimmed. This
// broker flow proves the other half of the production composition: agentd
// preserves that evidence through PrepareHookEvent and the standing-order
// evaluator records "could not evaluate" rather than a false clean miss.
func TestHookBroker_TrimEvidenceReachesStandingOrderLedger(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel,
		"tmux-broker-layer", brokerPanePID)

	targetAgent, _, err := db.EnsureAgentForConv(brokerLayerConv, "test")
	require.NoError(t, err)
	require.NotEmpty(t, targetAgent)
	orderID, err := db.InsertStandingOrder(&db.StandingOrder{
		Name:             "trim-evidence",
		TargetKind:       db.StandingTargetConv,
		TargetAgent:      targetAgent,
		Summary:          "Review the tool input before proceeding.",
		TriggerEvent:     db.StandingTriggerToolBefore,
		MatchField:       db.StandingMatchFieldToolInput,
		MatchRegex:       "deploy",
		Timing:           db.StandingTimingSameContinuation,
		Cadence:          db.StandingCadenceAlways,
		Enabled:          true,
		OperatorAuthored: true,
	})
	require.NoError(t, err)

	code, response := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:         brokerLayerConv,
			HookEventName:  "PreToolUse",
			ToolName:       "Bash",
			Cwd:            f.World.HomeDir,
			PayloadTrimmed: true,
		},
	})
	require.Equal(t, http.StatusOK, code)
	assert.Empty(t, response.Stdout)
	assert.Empty(t, response.AckToken, "no model-visible delivery waits for an ACK")

	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeNotEvaluatedTrimmed, latest.Outcome)
	assert.NotEqual(t, db.StandingOutcomeNoMatch, latest.Outcome)
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

// TestHookBroker_TranscriptPathIsScopedToTheCallersOwnRollout wires the
// sanitizer into the handler. Without this the transcript gate is only
// unit-tested, and deleting the call site would leave the suite green —
// which for the PR's one cross-agent-read defence is not good enough.
//
// A wrapped Codex agent names a PEER's rollout file. The daemon must drop
// the path, so the peer's transcript is never opened and never lands in
// this caller's conversation index.
func TestHookBroker_TranscriptPathIsScopedToTheCallersOwnRollout(t *testing.T) {
	// conv_index's upsert keeps the first full_path it sees, so the two
	// cases must not share a conversation — otherwise "the peer path was
	// not recorded" would be true for the wrong reason.
	//
	// The transcript path is also consumed only on CODEX paths, so the
	// caller has to be a Codex session for either case to exercise
	// anything at all.
	brokerTranscript := func(t *testing.T, rolloutConv string) (*testharness.Flow, string) {
		t.Helper()
		f := newFlow(t)

		f.HaveAliveCodexSession(brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", f.World.HomeDir)
		row, err := db.LoadSession(brokerLayerLabel)
		require.NoError(t, err)
		require.NotNil(t, row)
		row.PID = brokerPanePID
		row.SandboxImplementation = "tclaude-layer"
		require.NoError(t, db.SaveSession(row))

		t.Cleanup(agentd.SetProcTreeForTest(
			map[int]string{
				brokerHookPID: "tclaude", brokerHarnessPID: "codex",
				brokerBwrapPID: "bwrap", brokerPanePID: "sh",
			},
			map[int]int{
				brokerHookPID: brokerHarnessPID, brokerHarnessPID: brokerBwrapPID,
				brokerBwrapPID: brokerPanePID,
			},
		))

		sessions := filepath.Join(f.World.HomeDir, ".codex", "sessions", "2026", "07", "26")
		require.NoError(t, os.MkdirAll(sessions, 0o755))
		rollout := filepath.Join(sessions, "rollout-2026-07-26T09-00-00-"+rolloutConv+".jsonl")
		require.NoError(t, os.WriteFile(rollout, []byte("{}\n"), 0o600))

		code, _ := postBrokeredHook(t, f, brokerHookPID, session.BrokeredHookRequest{
			Input: session.HookCallbackInput{
				ConvID:         brokerLayerConv,
				HookEventName:  "Stop",
				Cwd:            f.World.HomeDir,
				TranscriptPath: rollout,
			},
		})
		require.Equal(t, http.StatusOK, code, "the event is applied; only the path may be refused")
		return f, rollout
	}

	// The positive case is what gives the negative one teeth: without it, a
	// sanitizer that dropped EVERY transcript path would pass just as well.
	t.Run("its own rollout is recorded", func(t *testing.T) {
		_, rollout := brokerTranscript(t, brokerLayerConv)
		idx, err := db.GetConvIndex(brokerLayerConv)
		require.NoError(t, err)
		require.NotNil(t, idx, "the caller's own rollout must be indexed")
		assert.Equal(t, rollout, idx.FullPath,
			"a session's own rollout is legitimate telemetry and must survive the broker")
	})

	t.Run("a peer's rollout is refused", func(t *testing.T) {
		_, rollout := brokerTranscript(t, brokerVictimConv)
		idx, err := db.GetConvIndex(brokerLayerConv)
		require.NoError(t, err)
		if idx != nil {
			assert.NotEqual(t, rollout, idx.FullPath,
				"a peer's rollout must never be recorded as this conversation's transcript")
			assert.NotEqual(t, filepath.Dir(rollout), idx.ProjectDir,
				"nor may its directory become this conversation's project dir")
		}
	})
}

// TestHookBroker_RejectsPathTraversingConvID pins the second payload field
// that resolves into a host path: the conv-id is joined into the
// transcript path the /clear migration scans, and filepath.Join cleans
// ".." segments, so an unvalidated one walks out of the projects tree.
func TestHookBroker_RejectsPathTraversingConvID(t *testing.T) {
	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	for _, hostile := range []string{
		"../../../tmp/aaaaaaaa-1111-2222-3333-444444444444",
		"..",
		"sub/dir",
	} {
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
			Input: session.HookCallbackInput{
				ConvID:        hostile,
				HookEventName: "SessionStart",
				Source:        "clear",
				Cwd:           f.World.HomeDir,
			},
		})
		assert.Equal(t, http.StatusBadRequest, code,
			"a conv-id that is not a single path-safe segment must be refused: %q", hostile)
	}
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
	//
	// Scope note: this proves the DECISION SURVIVES THE ROUND TRIP, and
	// nothing about how the snapshot got there. The snapshot the guard
	// judges from is written only by the status line, which is brokered
	// through its own endpoint; seeding it directly here keeps this test
	// about the relay rather than about that path.
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
		"the guard's verdict must survive the round trip")
}
