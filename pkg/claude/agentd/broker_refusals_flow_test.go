package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
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
// the same test can land close enough together that RFC3339Nano's
// variable-width fraction does not sort the way wall-clock did.
func stampUpdatedAt(t *testing.T, sessionID string, at time.Time) {
	t.Helper()
	handle, err := db.Open()
	require.NoError(t, err)
	// Truncated to the second so both rows format to the same width and
	// the string comparison the query performs is unambiguous.
	res, err := handle.Exec(`UPDATE sessions SET updated_at = ? WHERE id = ?`,
		at.UTC().Truncate(time.Second).Format(time.RFC3339Nano), sessionID)
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

// TestBrokerIdentity_AllStaleRowsResolveExactlyAsBefore is the other side
// of the ruling: liveness is a PREFERENCE among candidates, never a filter.
// With no live candidate the resolver must land on the same row the plain
// most-recently-updated query picks — resolving nothing, or resolving
// differently, would be a new behaviour change smuggled in with the fix.
func TestBrokerIdentity_AllStaleRowsResolveExactlyAsBefore(t *testing.T) {
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

	// Claiming the row the old rule picks is accepted...
	code, _ := postBrokeredHook(t, f, callerPID, session.BrokeredHookRequest{
		Input: session.HookCallbackInput{
			ConvID:        pidReuseDeadConv,
			HookEventName: "Stop",
			Cwd:           f.World.HomeDir,
		},
		ClaimedSessionID: baseline.ID,
	})
	assert.Equal(t, http.StatusOK, code,
		"with nothing alive, resolution must be unchanged from FindSessionByPID")

	// ...and claiming the other one is still refused. Together these pin
	// WHICH row resolved, not merely that something did.
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
