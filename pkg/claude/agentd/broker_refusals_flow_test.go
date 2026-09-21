package agentd_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
	"github.com/tofutools/tclaude/pkg/claude/statusbar"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// TCL-761 flow tests. Two halves of one failure:
//
//  1. RESOLUTION. A pid is not unique over a machine's lifetime, and a
//     session row records the pid its pane had at spawn. So a long-dead
//     session and a live one can hold the same number, and "most recently
//     updated wins" hands a wrapped agent's brokered callbacks to the
//     corpse — which refuses them, because the caller's own session id
//     disagrees. The failure then SUSTAINS ITSELF: the live agent's row is
//     updated mainly by the hooks now being refused, so its updated_at can
//     never overtake the stale row's.
//
//  2. SURFACING. Until this, the only trace was an ERROR in the agent's own
//     log — detection for somebody already reading logs. The dashboard has
//     to show the condition, and it has to show it on the row the DAEMON
//     resolved, never on the one the refused request named.
//
// Both drive the production mux; nothing here reaches into the resolver.

const (
	pidReuseLiveConv  = "11ee0000-1111-2222-3333-444444444444"
	pidReuseDeadConv  = "dead0000-1111-2222-3333-444444444444"
	pidReuseOtherConv = "07be0000-1111-2222-3333-444444444444"

	pidReuseLiveLabel  = "spwn-pidreuse-live"
	pidReuseDeadLabel  = "spwn-pidreuse-dead"
	pidReuseOtherLabel = "spwn-pidreuse-other"

	// One pid, two rows: the OS reused it after the first pane died.
	pidReuseSharedPanePID = 7304

	pidReuseHookPID    = 7300
	pidReuseHarnessPID = 7301
	pidReuseInnerShPID = 7302
	pidReuseBwrapPID   = 7303
)

// pidReuseProcTree models a wrapped agent's ancestry ending at the shared
// pane pid, and returns the pid its brokered callback connects from.
func pidReuseProcTree(t *testing.T) int {
	t.Helper()
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{
			pidReuseHookPID:       "tclaude",
			pidReuseHarnessPID:    "node",
			pidReuseInnerShPID:    "sh",
			pidReuseBwrapPID:      "bwrap",
			pidReuseSharedPanePID: "sh",
		},
		map[int]int{
			pidReuseHookPID:    pidReuseHarnessPID,
			pidReuseHarnessPID: pidReuseInnerShPID,
			pidReuseInnerShPID: pidReuseBwrapPID,
			pidReuseBwrapPID:   pidReuseSharedPanePID,
		},
	))
	return pidReuseHookPID
}

// stampUpdatedAt pins a row's updated_at so the ORDER BY the resolver reads
// is deterministic. SaveSession always stamps time.Now(), and two saves in
// the same test can land close enough together to collide.
func stampUpdatedAt(t *testing.T, sessionID string, at time.Time) {
	t.Helper()
	handle, err := db.Open()
	require.NoError(t, err)
	res, err := handle.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		at.UTC().Truncate(time.Second).UnixNano(), sessionID)
	require.NoError(t, err)
	affected, err := res.RowsAffected()
	require.NoError(t, err)
	require.Equal(t, int64(1), affected, "expected to stamp exactly one row (%s)", sessionID)
}

// haveSharedPIDRows stands up a live wrapped agent and a dead session that
// share a pane pid, with the DEAD one updated more recently — the shape
// that makes the plain query pick the corpse.
func haveSharedPIDRows(t *testing.T, f *testharness.Flow) {
	t.Helper()
	haveLayerSession(t, f, pidReuseLiveConv, pidReuseLiveLabel, "tmux-pidreuse-live", pidReuseSharedPanePID)
	haveLayerSession(t, f, pidReuseDeadConv, pidReuseDeadLabel, "tmux-pidreuse-dead", pidReuseSharedPanePID)
	// Its pane is gone; only the row survives.
	f.MarkOffline("tmux-pidreuse-dead")

	now := time.Now()
	stampUpdatedAt(t, pidReuseLiveLabel, now.Add(-2*time.Minute))
	stampUpdatedAt(t, pidReuseDeadLabel, now.Add(-1*time.Minute))
}

// TestBrokerIdentity_LiveRowWinsAPidItSharesWithACorpse is the headline
// fix. A wrapped agent whose pane pid is shadowed by a dead session's row
// must have its brokered hooks applied to ITS row.
func TestBrokerIdentity_LiveRowWinsAPidItSharesWithACorpse(t *testing.T) {
	f := newFlow(t)
	callerPID := pidReuseProcTree(t)
	haveSharedPIDRows(t, f)

	// The fixture has to actually reproduce the shadowing, or everything
	// below passes for the wrong reason.
	shadow, err := db.FindSessionByPID(pidReuseSharedPanePID)
	require.NoError(t, err)
	require.NotNil(t, shadow)
	require.Equal(t, pidReuseDeadLabel, shadow.ID,
		"fixture must reproduce the corpse shadowing the live row")

	deadBefore, err := session.LoadSessionState(pidReuseDeadLabel)
	require.NoError(t, err)

	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        pidReuseLiveConv,
			HookEventName: "UserPromptSubmit",
			Prompt:        "do the thing",
			Cwd:           f.World.HomeDir,
		},
		// The live agent presents its own session id, exactly as its hook
		// callback does in production. Before the fix this is what made
		// the daemon refuse: it disagreed with the corpse.
		ClaimedSessionID: pidReuseLiveLabel,
	})
	require.Equal(t, http.StatusOK, code,
		"a live agent must not be refused because a dead row shares its pid")

	live, err := session.LoadSessionState(pidReuseLiveLabel)
	require.NoError(t, err)
	assert.False(t, live.LastHook.IsZero(), "the live agent's own row must record the hook")

	deadAfter, err := session.LoadSessionState(pidReuseDeadLabel)
	require.NoError(t, err)
	assert.Equal(t, deadBefore.LastHook, deadAfter.LastHook,
		"the dead row must not record a hook it did not fire")
}

// A brokered layer callback can only originate below its live pane. Historical
// PID matches are not authority after that pane is gone.
func TestBrokerIdentity_AllStaleRowsFailClosed(t *testing.T) {
	f := newFlow(t)
	callerPID := pidReuseProcTree(t)
	haveSharedPIDRows(t, f)
	// Now BOTH panes are gone. (The rows are what the resolver reads; the
	// caller pid is simulated, so the request still arrives.)
	f.MarkOffline("tmux-pidreuse-live")

	baseline, err := db.FindSessionByPID(pidReuseSharedPanePID)
	require.NoError(t, err)
	require.NotNil(t, baseline)
	require.Equal(t, pidReuseDeadLabel, baseline.ID)

	// Even claiming the row the legacy resolver picks is refused because no
	// live generation-bound pane can own the caller.
	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        pidReuseDeadConv,
			HookEventName: "Stop",
			Cwd:           f.World.HomeDir,
		},
		ClaimedSessionID: baseline.ID,
	})
	assert.Equal(t, http.StatusForbidden, code)

	// Claiming the other stale row is refused too.
	code, _ = postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        pidReuseLiveConv,
			HookEventName: "Stop",
			Cwd:           f.World.HomeDir,
		},
		ClaimedSessionID: pidReuseLiveLabel,
	})
	assert.Equal(t, http.StatusForbidden, code,
		"refusal semantics are untouched: a claim that disagrees is still refused")
}

// TestBrokerIdentity_TheSelfSustainingLoopCloses is why the preference is
// worth having at all, rather than telling operators to wait it out. The
// loop is: the live agent's row only advances through the hooks that are
// being refused, so it can never win the tie that caused the refusal.
//
// One accepted hook breaks it permanently, and not by a hair's-breadth
// re-ordering: the hook path re-keys the row onto the agent's real harness
// pid (the same correction the direct path performs), so the live agent
// stops sharing a pid with the corpse altogether and no longer depends on
// the preference to be reachable.
func TestBrokerIdentity_TheSelfSustainingLoopCloses(t *testing.T) {
	f := newFlow(t)
	callerPID := pidReuseProcTree(t)
	haveSharedPIDRows(t, f)

	before, err := db.FindSessionByPID(pidReuseSharedPanePID)
	require.NoError(t, err)
	require.Equal(t, pidReuseDeadLabel, before.ID, "the loop starts closed against the live agent")

	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        pidReuseLiveConv,
			HookEventName: "UserPromptSubmit",
			Prompt:        "first accepted hook",
			Cwd:           f.World.HomeDir,
		},
		ClaimedSessionID: pidReuseLiveLabel,
	})
	require.Equal(t, http.StatusOK, code)

	live, err := db.LoadSession(pidReuseLiveLabel)
	require.NoError(t, err)
	require.NotNil(t, live)
	assert.Equal(t, pidReuseHarnessPID, live.PID,
		"the accepted hook must re-key the live row onto its own harness pid")

	byHarness, err := db.FindSessionByPID(pidReuseHarnessPID)
	require.NoError(t, err)
	require.NotNil(t, byHarness)
	assert.Equal(t, pidReuseLiveLabel, byHarness.ID,
		"the live agent now resolves on a pid it shares with nothing")

	// And the contested pid is left to the corpse alone — the collision is
	// gone rather than merely outvoted.
	shared, err := db.FindSessionsByPID(pidReuseSharedPanePID)
	require.NoError(t, err)
	require.Len(t, shared, 1)
	assert.Equal(t, pidReuseDeadLabel, shared[0].ID)
}

// TestBrokerRefusals_BadgeTheResolvedRowNeverTheClaimedOne is the
// surfacing half, and its security property. A refused request names a
// session id; attributing the refusal to THAT row would let any wrapped
// agent paint a warning on a peer's dashboard row. The mark goes on the
// row the daemon's own ancestry walk produced.
func TestBrokerRefusals_BadgeTheResolvedRowNeverTheClaimedOne(t *testing.T) {
	t.Cleanup(agentd.ResetBrokerRefusalsForTest())
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	haveLayerSession(t, f, pidReuseOtherConv, pidReuseOtherLabel, "tmux-pidreuse-other", brokerVictimPanePID)

	// The caller resolves to brokerLayerLabel but names the peer.
	for range 3 {
		code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
			Input: session.HookCallbackInput{
				ConvID:        pidReuseOtherConv,
				HookEventName: "Stop",
				Cwd:           f.World.HomeDir,
			},
			ClaimedSessionID: pidReuseOtherLabel,
		})
		require.Equal(t, http.StatusForbidden, code)
	}

	f.HaveGroup("refusalsquad")
	f.HaveMember("refusalsquad", brokerLayerConv)
	f.HaveMember("refusalsquad", pidReuseOtherConv)
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	resolved := findDashMember(snap, "refusalsquad", brokerLayerConv)
	require.NotNil(t, resolved)
	assert.Equal(t, 3, resolved.State.BrokerRefusals,
		"the row the daemon resolved carries the count")
	assert.NotEmpty(t, resolved.State.BrokerRefusalDetail, "and says what kind of refusal")
	assert.NotEmpty(t, resolved.State.BrokerRefusalSince, "and when the run started")

	named := findDashMember(snap, "refusalsquad", pidReuseOtherConv)
	require.NotNil(t, named)
	assert.Zero(t, named.State.BrokerRefusals,
		"a peer the refused request merely NAMED must stay unmarked")
	assert.Zero(t, snap.BrokerRefusalsUnplaceable,
		"a placeable refusal is not also counted as unplaceable")
	assert.Equal(t, 3, snap.BrokerRefusalsTotal,
		"but it does count towards the machine-level total, which is what the "+
			"operator sees when the badged row is one the dashboard is not showing")
}

// TestBrokerRefusals_UnplaceableIsCountedNotAttributed pins the asymmetry.
// A caller the daemon cannot place has no trustworthy identifier at all, so
// there is nothing to badge — falling back to the id it claimed is exactly
// the spoof the design refuses. It still has to be VISIBLE: those callbacks
// carry some agent's telemetry, and it is being dropped.
func TestBrokerRefusals_UnplaceableIsCountedNotAttributed(t *testing.T) {
	t.Cleanup(agentd.ResetBrokerRefusalsForTest())
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	const orphanPID = 8300
	t.Cleanup(agentd.SetProcTreeForTest(map[int]string{orphanPID: "node"}, map[int]int{}))

	for range 2 {
		code, _ := postBrokeredHook(t, f, orphanPID, session.BrokeredHookRequest{
			Input: session.HookCallbackInput{
				ConvID:        brokerLayerConv,
				HookEventName: "Stop",
			},
			ClaimedSessionID: brokerLayerLabel,
		})
		require.Equal(t, http.StatusForbidden, code)
	}

	f.HaveGroup("refusalsquad")
	f.HaveMember("refusalsquad", brokerLayerConv)
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	assert.Equal(t, 2, snap.BrokerRefusalsUnplaceable,
		"an unplaceable refusal must still reach the operator, as a counter")
	row := findDashMember(snap, "refusalsquad", brokerLayerConv)
	require.NotNil(t, row)
	assert.Zero(t, row.State.BrokerRefusals,
		"the session the unplaceable caller claimed must not be badged for it")
}

// A tclaude-layer row without its tmux launch identity cannot prove a brokered
// caller, even if the legacy PID resolver would choose that row.
func TestBrokerIdentity_ANamelessLayerRowFailsClosed(t *testing.T) {
	f := newFlow(t)
	callerPID := pidReuseProcTree(t)

	// The older row keeps a live tmux session. The newer one — the
	// incumbent, and the agent actually making the request — has none.
	haveLayerSession(t, f, pidReuseDeadConv, pidReuseDeadLabel, "tmux-pidreuse-dead", pidReuseSharedPanePID)
	haveLayerSession(t, f, pidReuseLiveConv, pidReuseLiveLabel, "tmux-pidreuse-live", pidReuseSharedPanePID)
	nameless, err := db.LoadSession(pidReuseLiveLabel)
	require.NoError(t, err)
	require.NotNil(t, nameless)
	nameless.TmuxSession = ""
	require.NoError(t, db.SaveSession(nameless))

	now := time.Now()
	stampUpdatedAt(t, pidReuseDeadLabel, now.Add(-2*time.Minute))
	stampUpdatedAt(t, pidReuseLiveLabel, now.Add(-1*time.Minute))

	incumbent, err := db.FindSessionByPID(pidReuseSharedPanePID)
	require.NoError(t, err)
	require.Equal(t, pidReuseLiveLabel, incumbent.ID, "fixture: the nameless row is the incumbent")

	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        pidReuseLiveConv,
			HookEventName: "UserPromptSubmit",
			Prompt:        "do the thing",
			Cwd:           f.World.HomeDir,
		},
		ClaimedSessionID: pidReuseLiveLabel,
	})
	assert.Equal(t, http.StatusForbidden, code)
}

// A simulated caller below an offline historical pane has no trustworthy row
// to badge. The refusal remains visible in the machine-level unplaceable
// count; it must not be projected onto another live row for the same conv.
func TestBrokerRefusals_OfflineHistoricalRowIsNotTrustedForBadge(t *testing.T) {
	t.Cleanup(agentd.ResetBrokerRefusalsForTest())
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)

	// One conversation, two session rows: an older dead one the caller's
	// ancestry resolves to, and a live one the dashboard renders from.
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	f.HaveAliveSession(brokerLayerConv, "spwn-broker-layer-2", "tmux-broker-layer-2", f.World.HomeDir)
	f.MarkOffline("tmux-broker-layer")

	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        pidReuseOtherConv,
			HookEventName: "Stop",
			Cwd:           f.World.HomeDir,
		},
		ClaimedSessionID: pidReuseOtherLabel,
	})
	require.Equal(t, http.StatusForbidden, code)

	f.HaveGroup("refusalsquad")
	f.HaveMember("refusalsquad", brokerLayerConv)
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	row := findDashMember(snap, "refusalsquad", brokerLayerConv)
	require.NotNil(t, row)
	assert.Zero(t, row.State.BrokerRefusals,
		"an unproved historical row must not badge a different live launch")
	assert.Equal(t, 1, snap.BrokerRefusalsUnplaceable)
}

// TestBrokerRefusals_StatuslineRefusalsAreRecordedToo: the status line is
// the endpoint whose loss is most visible (model, cost, context all stop),
// and it has its own copy of the refusal path. A recorder wired into only
// one of the two endpoints would leave that half silent.
func TestBrokerRefusals_StatuslineRefusalsAreRecordedToo(t *testing.T) {
	t.Cleanup(agentd.ResetBrokerRefusalsForTest())
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	callerPID := layerProcTree(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)
	haveLayerSession(t, f, pidReuseOtherConv, pidReuseOtherLabel, "tmux-pidreuse-other", brokerVictimPanePID)

	code, _ := postBrokeredRender(t, f, callerPID, statusbar.BrokeredRenderRequest{
		ClaimedSessionID: pidReuseOtherLabel,
		RenderConvID:     pidReuseOtherConv,
		Payload:          statuslinePayload(pidReuseOtherConv, "Haiku 4.5", "claude-haiku-4-5", "low", 99, 1, 1, 200000, 0),
		ApplyWrites:      true,
	})
	require.Equal(t, http.StatusForbidden, code)

	f.HaveGroup("refusalsquad")
	f.HaveMember("refusalsquad", brokerLayerConv)
	f.HaveMember("refusalsquad", pidReuseOtherConv)
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	resolved := findDashMember(snap, "refusalsquad", brokerLayerConv)
	require.NotNil(t, resolved)
	assert.Equal(t, 1, resolved.State.BrokerRefusals,
		"a refused render must mark the resolved row, like a refused hook")
	assert.Contains(t, resolved.State.BrokerRefusalDetail, "statusline",
		"and say which endpoint it came from, so the operator knows what is missing")

	named := findDashMember(snap, "refusalsquad", pidReuseOtherConv)
	require.NotNil(t, named)
	assert.Zero(t, named.State.BrokerRefusals,
		"the peer the render named must stay unmarked here too")
}

// captureDaemonLog swaps slog's default handler for a JSON buffer for the
// rest of the test, so a flow can assert on what the daemon LOGGED — the
// half of refusal surfacing the dashboard notice points the operator at
// ("the daemon log has the caller pid").
func captureDaemonLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &logs
}

func refusalLogLines(t *testing.T, logs *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line %q", line)
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, "broker: refused a brokered callback") {
			out = append(out, rec)
		}
	}
	return out
}

// Every refusal used to record a dashboard badge and return silently, so
// the operator saw 🚫 and had nothing to read. Each refusal branch now
// writes a WARN naming the caller pid and, where a live-pane proof failed,
// WHICH fact was missing — the difference between "dead row" and "tmux was
// busy" that the badge alone cannot express.
func TestBrokerRefusals_AreLoggedWithTheCallerPidAndReason(t *testing.T) {
	t.Cleanup(agentd.ResetBrokerRefusalsForTest())
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	logs := captureDaemonLog(t)

	f := newFlow(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	// An orphan caller: no row resolves, the layer claim cannot be proved
	// because the pane pid is not among its ancestors.
	const orphanPID = 8300
	t.Cleanup(agentd.SetProcTreeForTest(map[int]string{orphanPID: "node"}, map[int]int{}))

	code, _ := postBrokeredHook(t, f, orphanPID, session.BrokeredHookRequest{
		Input:            session.HookCallbackInput{ConvID: brokerLayerConv, HookEventName: "Stop"},
		ClaimedSessionID: brokerLayerLabel,
	})
	require.Equal(t, http.StatusForbidden, code)

	lines := refusalLogLines(t, logs)
	require.Len(t, lines, 1, "one refusal, one log line; got %s", logs.String())
	rec := lines[0]
	assert.Equal(t, "WARN", rec["level"])
	assert.EqualValues(t, orphanPID, rec["caller_pid"])
	assert.Equal(t, "/v1/whoami/hook", rec["endpoint"])
	assert.Equal(t, brokerLayerLabel, rec["claimed_session"],
		"the claimed id is logged for correlation (labelled as the caller's own)")
	assert.Equal(t, "", rec["resolved_session"], "nothing resolved for an orphan")
	assert.Equal(t, "Stop", rec["event"])
	assert.Contains(t, rec["reason"], "failed live-pane proof")
	assert.Contains(t, rec["detail"], "is not an ancestor of caller pid 8300",
		"the proof names the fact that was missing")

	// A second refusal inside the throttle interval is counted, not logged.
	code, _ = postBrokeredHook(t, f, orphanPID, session.BrokeredHookRequest{
		Input:            session.HookCallbackInput{ConvID: brokerLayerConv, HookEventName: "Stop"},
		ClaimedSessionID: brokerLayerLabel,
	})
	require.Equal(t, http.StatusForbidden, code)
	assert.Len(t, refusalLogLines(t, logs), 1, "the per-run throttle holds the second line back")
}

// The statusline endpoint refuses through the same recorder, and so logs
// the same way.
func TestBrokerRefusals_StatuslineRefusalsAreLoggedToo(t *testing.T) {
	t.Cleanup(agentd.ResetBrokerRefusalsForTest())
	logs := captureDaemonLog(t)

	f := newFlow(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	const orphanPID = 8301
	t.Cleanup(agentd.SetProcTreeForTest(map[int]string{orphanPID: "node"}, map[int]int{}))

	req := testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/statusline",
		statusbar.BrokeredRenderRequest{ClaimedSessionID: brokerLayerLabel})
	req = agentd.AsAgentPeerWithPID(req, "", orphanPID)
	rec := testharness.Serve(f.Mux, req)
	require.Equal(t, http.StatusForbidden, rec.Code)

	lines := refusalLogLines(t, logs)
	require.Len(t, lines, 1, "got %s", logs.String())
	assert.Equal(t, "/v1/whoami/statusline", lines[0]["endpoint"])
	assert.EqualValues(t, orphanPID, lines[0]["caller_pid"])
	assert.Contains(t, lines[0]["reason"], "statusline:")
}

func logLinesWithPrefix(t *testing.T, logs *bytes.Buffer, prefix string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &rec), "log line %q", line)
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, prefix) {
			out = append(out, rec)
		}
	}
	return out
}

// A statusline client gives up after three seconds and exits. When the
// daemon is slow enough to lose that race, the caller pid is gone by the
// time the daemon looks — which is the client's timeout, not an ancestry
// mismatch, and not an agent whose telemetry is stuck. It must be logged
// as what it is and must NOT badge the row or count as a refusal, because
// the client retries on its own.
func TestBrokerRefusals_AVanishedCallerIsLoggedNotBadged(t *testing.T) {
	t.Cleanup(agentd.ResetBrokerRefusalsForTest())
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBrokerLimiterForTest())
	logs := captureDaemonLog(t)

	f := newFlow(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	// The wrapped ancestry is intact, but the caller itself is not in the
	// tree at all: it exited. (SetProcTreeForTest reports unlisted pids as
	// not alive.)
	const gonePID = 7199
	t.Cleanup(agentd.SetProcTreeForTest(
		map[int]string{
			brokerHarnessPID: "node", brokerInnerShPID: "sh", brokerBwrapPID: "bwrap", brokerPanePID: "sh",
		},
		map[int]int{
			brokerHarnessPID: brokerInnerShPID, brokerInnerShPID: brokerBwrapPID, brokerBwrapPID: brokerPanePID,
		},
	))

	code, _ := postBrokeredHook(t, f, gonePID, session.BrokeredHookRequest{
		Input:            session.HookCallbackInput{ConvID: brokerLayerConv, HookEventName: "Stop"},
		ClaimedSessionID: brokerLayerLabel,
	})
	require.Equal(t, http.StatusForbidden, code, "nothing can be applied for a caller that is gone")

	f.HaveGroup("gonesquad")
	f.HaveMember("gonesquad", brokerLayerConv)
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	assert.Zero(t, snap.BrokerRefusalsTotal, "a vanished caller is not a refusal")
	assert.Zero(t, snap.BrokerRefusalsUnplaceable)
	row := findDashMember(snap, "gonesquad", brokerLayerConv)
	require.NotNil(t, row)
	assert.Zero(t, row.State.BrokerRefusals, "the row the caller claimed is not badged")

	assert.Empty(t, refusalLogLines(t, logs), "no refusal line either")
	gone := logLinesWithPrefix(t, logs, "broker: caller exited before its callback could be verified")
	require.Len(t, gone, 1, "got %s", logs.String())
	assert.Equal(t, "WARN", gone[0]["level"])
	assert.EqualValues(t, gonePID, gone[0]["caller_pid"])
	assert.Equal(t, brokerLayerLabel, gone[0]["claimed_session"])
	assert.Equal(t, "Stop", gone[0]["event"])
}

// The proof must not depend on the caller surviving the request. With the
// ancestry served from the shared snapshot, the per-pid readers can report
// every process dead and the proof still succeeds — this is the exact race
// that produced "walk ended at pid 0 after 1 hops" on macOS.
func TestBrokerIdentity_ProofIsServedFromTheSnapshotNotLiveReads(t *testing.T) {
	t.Cleanup(agentd.ResetBrokerRefusalsForTest())
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	t.Cleanup(agentd.ResetBrokerLimiterForTest())

	f := newFlow(t)
	haveLayerSession(t, f, brokerLayerConv, brokerLayerLabel, "tmux-broker-layer", brokerPanePID)

	// Per-pid readers know nothing (every process "gone")...
	t.Cleanup(agentd.SetProcTreeForTest(map[int]string{}, map[int]int{}))
	// ...but the caller is alive and the snapshot carries the chain.
	t.Cleanup(agentd.SetProcAliveForTest(func(pid int) bool { return pid == brokerHookPID }))
	t.Cleanup(agentd.SetBrokerProcSnapshotForTest(map[int]struct {
		PPID int
		Name string
	}{
		brokerHookPID:    {PPID: brokerHarnessPID, Name: "tclaude"},
		brokerHarnessPID: {PPID: brokerInnerShPID, Name: "node"},
		brokerInnerShPID: {PPID: brokerBwrapPID, Name: "sh"},
		brokerBwrapPID:   {PPID: brokerPanePID, Name: "bwrap"},
		brokerPanePID:    {PPID: 1, Name: "sh"},
	}))

	token, err := agentd.RegisterHookAckForTest(brokerLayerLabel)
	require.NoError(t, err)
	code, _ := postBrokeredHook(t, f, brokerHookPID, session.BrokeredHookRequest{
		ClaimedSessionID: brokerLayerLabel,
		AckToken:         token,
	})
	assert.Equal(t, http.StatusOK, code, "the snapshot proves the ancestry without a single live per-pid read")
	total, _ := snapshotRefusalCounts(t)
	assert.Zero(t, total)
}

func snapshotRefusalCounts(t *testing.T) (int, int) {
	t.Helper()
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
	return snap.BrokerRefusalsTotal, snap.BrokerRefusalsUnplaceable
}
