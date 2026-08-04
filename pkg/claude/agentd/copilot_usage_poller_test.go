package agentd

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// resetCopilotUsageStateForTest drops the sweep's whole memory, including the
// shutdown latch a stop() in an earlier test would otherwise leave set.
func resetCopilotUsageStateForTest() {
	copilotUsageState.Lock()
	defer copilotUsageState.Unlock()
	for _, home := range copilotUsageState.homes {
		if home.store != nil {
			_ = home.store.Close()
		}
	}
	copilotUsageState.sessions = nil
	copilotUsageState.homes = nil
	copilotUsageState.stopping = false
	copilotUsageState.liveCount = 0
	copilotUsageState.liveCountKnown = false
}

func copilotUsageSession(t *testing.T, sessionID, convID string) *db.SessionRow {
	t.Helper()
	sess := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "copilot-pane-" + sessionID,
		Status: "idle", Harness: harness.CopilotName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(sess))
	return sess
}

func copilotUsageCall(eventID, input, output int64) harness.CopilotUsageCall {
	return harness.CopilotUsageCall{
		SessionID: "conv-1", EventID: eventID, TurnIndex: eventID,
		Model: "gpt-5", InputTokens: input, OutputTokens: output,
		ReasoningEffort: "medium", FinishReason: "stop",
		CreatedAt: "2026-08-04T12:00:00Z",
	}
}

// TestApplyCopilotUsageCallsFoldsCumulativeAndPerCall pins the one distinction
// the whole meter rests on: the cumulative columns add up across calls, while
// the occupancy numerator is the NEWEST call alone.
//
// Summing input_tokens would be the natural-looking bug and would render an
// occupancy several times the size of the window, because Copilot re-sends the
// whole conversation prefix on every turn.
func TestApplyCopilotUsageCallsFoldsCumulativeAndPerCall(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{
		copilotUsageCall(1, 25114, 300),
		copilotUsageCall(2, 28725, 700),
	})

	snapshot, err := db.LoadCopilotUsageSnapshot(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)

	assert.Equal(t, int64(2), snapshot.LastEventID, "the cursor tracks the newest row")
	assert.Equal(t, int64(2), snapshot.Requests)
	assert.Equal(t, int64(1000), snapshot.OutputTokens, "output is cumulative")
	assert.Equal(t, int64(53839), snapshot.InputTokens, "input is cumulative accounting")
	assert.Equal(t, int64(28725), snapshot.LastCallInputTokens,
		"occupancy is the newest call's prompt, never the running sum")
	assert.Equal(t, "gpt-5", snapshot.Model)
	assert.Equal(t, "medium", snapshot.ReasoningEffort)
	assert.Equal(t, "stop", snapshot.FinishReason)
}

// TestCopilotUsagePersistsObservedModelAndEffort covers the dashboard-facing
// projection. The side-table fold is the source of truth for the newest
// top-level call; a repeated row must not flap the displayed values, while a
// later call may change both model and effort. An explicit empty effort is the
// observed value for a model without reasoning support and must clear the
// preceding effort rather than fabricate one.
func TestCopilotUsagePersistsObservedModelAndEffort(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	first := copilotUsageCall(1, 25114, 300)
	first.Model = "gpt-5.4"
	first.ReasoningEffort = "high"
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{first})

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.4", snap.Model)
	assert.Equal(t, "high", snap.EffortLevel)

	// A re-delivered row is ignored by the fold and must leave the dashboard
	// values unchanged.
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{first})
	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.4", snap.Model)
	assert.Equal(t, "high", snap.EffortLevel)

	// The observed model can change even when the context projection does not:
	// an aborted call may report no output, the same prompt, and no known
	// window. This specifically proves model/effort participate in the
	// persist helper's no-change short-circuit.
	sameProjection := copilotUsageCall(2, 25114, 0)
	sameProjection.Model = "gpt-5.4-rerouted"
	sameProjection.ReasoningEffort = "medium"
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{sameProjection})
	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.4-rerouted", snap.Model)
	assert.Equal(t, "medium", snap.EffortLevel)

	second := copilotUsageCall(3, 28725, 700)
	second.Model = "gpt-5.5"
	second.ReasoningEffort = ""
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{second})

	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.5", snap.Model)
	assert.Empty(t, snap.EffortLevel,
		"an explicit null/empty effort must render blank for a non-reasoning model")

	// Polling again after the model switch is stable too.
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{second})
	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5.5", snap.Model)
	assert.Empty(t, snap.EffortLevel)
}

// TestApplyCopilotUsageCallsReplacesRunningCost covers the one field that is
// already a session total in Copilot's own rows. Adding it up would inflate the
// reported cost by roughly the number of calls.
func TestApplyCopilotUsageCallsReplacesRunningCost(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	first := copilotUsageCall(1, 100, 10)
	first.TotalNanoAIU, first.HasNanoAIU = 1200, true
	second := copilotUsageCall(2, 200, 20)
	second.TotalNanoAIU, second.HasNanoAIU = 4000, true

	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{first, second})

	snapshot, err := db.LoadCopilotUsageSnapshot(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	require.NotNil(t, snapshot.TotalNanoAIU)
	assert.Equal(t, int64(4000), *snapshot.TotalNanoAIU,
		"Copilot's total_nano_aiu is already cumulative; summing it double-counts")
}

// TestApplyCopilotUsageCallsIgnoresAlreadyConsumedRows is defence in depth: the
// query already excludes them, but the cumulative columns are the one place a
// re-delivered row would corrupt rather than merely repeat.
func TestApplyCopilotUsageCallsIgnoresAlreadyConsumedRows(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(1, 100, 10)})
	// The same row again, plus one genuinely new one.
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{
		copilotUsageCall(1, 100, 10),
		copilotUsageCall(2, 200, 20),
	})

	snapshot, err := db.LoadCopilotUsageSnapshot(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	assert.Equal(t, int64(2), snapshot.Requests, "the replayed row must not be counted twice")
	assert.Equal(t, int64(30), snapshot.OutputTokens)
	assert.Equal(t, int64(300), snapshot.InputTokens)
}

// TestApplyCopilotUsageCallsAccumulatesAcrossSweeps is the resume case: a
// second sweep continues the fold from the persisted snapshot rather than
// starting over.
func TestApplyCopilotUsageCallsAccumulatesAcrossSweeps(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(1, 100, 10)})

	// A daemon restart loses the in-memory state but not the durable snapshot.
	resetCopilotUsageStateForTest()
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(2, 200, 20)})

	snapshot, err := db.LoadCopilotUsageSnapshot(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	assert.Equal(t, int64(2), snapshot.Requests, "the earlier sweep's rows survive a restart")
	assert.Equal(t, int64(30), snapshot.OutputTokens)
	assert.Equal(t, int64(2), snapshot.LastEventID)
}

// TestCopilotUsageWritesContextColumnsWithoutWindow is the honesty case. The
// store carries no token limit, and — verified against the shipped CLI — the
// limit exists nowhere on disk. So the tokens are reported and the percentage
// stays 0, which every harness's snapshot already means as "unknown".
func TestCopilotUsageWritesContextColumnsWithoutWindow(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(1, 25114, 300)})

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(25114), snap.TokensInput,
		"an occupancy with no denominator is still worth reporting")
	assert.Equal(t, int64(300), snap.TokensOutput)
	assert.Zero(t, snap.ContextPct, "0 means unknown, not 'empty window'")
	assert.Zero(t, snap.ContextWindowSize)
}

// TestCopilotUsageUsesDurableWindowAsDenominator is the precedence rule's other
// half: the sweep owns the numerator, the durable log owns the denominator, and
// the percentage is the two combined.
func TestCopilotUsageUsesDurableWindowAsDenominator(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	// A compaction disclosed a limit, exactly as the durable follower records it.
	updated, err := db.UpdateContextSnapshotForGeneration(
		sess.ID, sess.ConvID, sess.CreatedAt, 50.0, 0, 0, 128000)
	require.NoError(t, err)
	require.True(t, updated)

	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(1, 64000, 300)})

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(128000), snap.ContextWindowSize, "the sweep must not clear the window")
	assert.Equal(t, int64(64000), snap.TokensInput)
	assert.InDelta(t, 50.0, snap.ContextPct, 0.001)
}

// TestCopilotUsagePrecedenceSurvivesCompaction is the self-correcting argument
// made checkable: after a compaction the next call's input_tokens already
// reflects the smaller window, so the sweep's numerator moves down with it and
// no clock comparison against the durable log is needed.
func TestCopilotUsagePrecedenceSurvivesCompaction(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	require.NoError(t, func() error {
		_, err := db.UpdateContextSnapshotForGeneration(
			sess.ID, sess.ConvID, sess.CreatedAt, 0, 0, 0, 128000)
		return err
	}())

	// Nearly full, then a compaction, then a much smaller prompt.
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(1, 120000, 300)})
	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.InDelta(t, 93.75, snap.ContextPct, 0.01)

	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(2, 20000, 100)})
	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(20000), snap.TokensInput,
		"the post-compaction call's prompt is the new occupancy")
	assert.InDelta(t, 15.625, snap.ContextPct, 0.01)
}

// TestCopilotUsageRefusesStaleGeneration covers a session pruned and recreated
// between the store read and the write. The refused write must also drop the
// in-memory entry, so the next sweep reloads for the new generation rather than
// continuing a fold that belongs to a dead conversation.
func TestCopilotUsageRefusesStaleGeneration(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	ghost := *sess
	ghost.CreatedAt = sess.CreatedAt.Add(-time.Hour)

	applyCopilotUsageCalls(&ghost, []harness.CopilotUsageCall{copilotUsageCall(1, 100, 10)})

	snapshot, err := db.LoadCopilotUsageSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Nil(t, snapshot, "a stale generation must leave no snapshot behind")

	_, ok := lookupCopilotLiveUsage(sess.ID, sess.ConvID, sess.CreatedAt)
	assert.False(t, ok, "and must publish nothing for the live generation")
}

// TestLookupCopilotLiveUsageIsGenerationScoped keeps the durable follower fully
// in charge whenever the sweep has nothing for THIS generation. A false here is
// what makes the read-through path behave exactly as it did before the sweep
// existed.
func TestLookupCopilotLiveUsageIsGenerationScoped(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	_, ok := lookupCopilotLiveUsage(sess.ID, sess.ConvID, sess.CreatedAt)
	assert.False(t, ok, "no rows swept yet means the durable follower still owns the row")

	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(1, 25114, 300)})

	live, ok := lookupCopilotLiveUsage(sess.ID, sess.ConvID, sess.CreatedAt)
	require.True(t, ok)
	assert.Equal(t, int64(25114), live.ContextTokens)
	assert.Equal(t, int64(300), live.OutputTokens)

	_, ok = lookupCopilotLiveUsage(sess.ID, "conv-other", sess.CreatedAt)
	assert.False(t, ok, "a different conversation must not read this one's figures")
	_, ok = lookupCopilotLiveUsage(sess.ID, sess.ConvID, sess.CreatedAt.Add(time.Hour))
	assert.False(t, ok, "a different generation must not read the previous one's figures")
}

func TestCopilotContextPct(t *testing.T) {
	assert.InDelta(t, 50.0, copilotContextPct(64000, 128000), 0.001)
	assert.Zero(t, copilotContextPct(64000, 0), "no window means unknown, not infinite")
	assert.Zero(t, copilotContextPct(0, 128000), "no occupancy means unknown, not 0%")
	assert.Zero(t, copilotContextPct(-1, 128000))
}

// TestCopilotHomeForSessionPrefersLaunchEnvironment covers the relocated-home
// case. COPILOT_HOME is not a reserved variable, so a sandbox profile may move
// it; reading the ambient home for such a pane would attribute a different (or
// empty) store to it.
func TestCopilotHomeForSessionPrefersLaunchEnvironment(t *testing.T) {
	t.Setenv(harness.CopilotHomeEnvVar, "/ambient/copilot")

	sess := &db.SessionRow{ID: "s", ConvID: "conv-1", Harness: harness.CopilotName}
	assert.Equal(t, "/ambient/copilot", copilotHomeForSession(sess),
		"a session with no frozen environment uses the ambient home")

	sess.EffectiveSandbox = &sandboxpolicy.Snapshot{
		Effective: sandboxpolicy.EffectiveProfile{
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "SOMETHING_ELSE", Value: "/x"},
				{Name: harness.CopilotHomeEnvVar, Value: "/relocated/copilot/"},
			},
		},
	}
	assert.Equal(t, "/relocated/copilot", copilotHomeForSession(sess),
		"the pane's own COPILOT_HOME wins, cleaned of its trailing slash")

	// A relative COPILOT_HOME would resolve against the daemon's cwd, which is
	// not the pane's, so it falls back rather than reading the wrong directory.
	sess.EffectiveSandbox.Effective.Environment = []sandboxpolicy.EnvironmentEntry{
		{Name: harness.CopilotHomeEnvVar, Value: "relative/copilot"},
	}
	assert.Equal(t, "/ambient/copilot", copilotHomeForSession(sess))
}

func TestSweepCopilotUsageIsInertWithoutSessions(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	// The steady state on a host that does not run Copilot: no panes, so no
	// store is opened and nothing is written.
	sweepCopilotUsage(t.Context())

	copilotUsageState.Lock()
	defer copilotUsageState.Unlock()
	assert.Empty(t, copilotUsageState.homes, "an inert sweep must open nothing")
	assert.Empty(t, copilotUsageState.sessions)
}

func TestStopCopilotUsagePollerRefusesFurtherWork(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	stopCopilotUsagePoller()
	store, ok := copilotUsageStoreFor("/nonexistent/copilot-home")
	assert.False(t, ok, "a stopping daemon must not open new stores")
	assert.Nil(t, store)
}

// TestCopilotDurableFollowerYieldsNumeratorToSweep is the precedence rule where
// it actually matters: both writers touch the same four context columns, and
// without an explicit rule they would flap the row between two answers on
// alternating polls.
//
// The durable log here discloses a compaction at 50% of a 128k window. The
// sweep then reports a much smaller live prompt. The row must end up with the
// durable WINDOW, the sweep's NUMERATOR, and a percentage computed from the
// two — not the compaction's stale 50%.
func TestCopilotDurableFollowerYieldsNumeratorToSweep(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	home := copilotRefreshHome(t)
	appendCopilotRefreshEvents(t, copilotRefreshLogPath(home),
		`{"type":"session.start","data":{"sessionId":"s","copilotVersion":"1.0.77","selectedModel":"gpt-5.4"}}`,
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":40}}`,
		`{"type":"session.compaction_start","data":{"currentTokens":64000,"tokenLimit":128000,"trigger":"threshold"}}`)

	sess := copilotUsageSession(t, "s-copilot", copilotRefreshConvID)

	// The durable follower alone, exactly as it behaved before the sweep existed.
	refreshCopilotContextSnapshotOnRead(sess, true)
	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	require.Equal(t, int64(128000), snap.ContextWindowSize)
	require.InDelta(t, 50.0, snap.ContextPct, 0.001)

	// Now the sweep sees a live post-compaction call.
	call := copilotUsageCall(1, 20000, 300)
	call.SessionID = copilotRefreshConvID
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{call})

	// And the read-through refresh must AGREE with it rather than overwrite it.
	resetCopilotContextRefreshStateForTest()
	refreshCopilotContextSnapshotOnRead(sess, true)

	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(128000), snap.ContextWindowSize,
		"the durable log still owns the denominator")
	assert.Equal(t, int64(20000), snap.TokensInput,
		"the sweep owns the numerator once it has a row")
	assert.InDelta(t, 15.625, snap.ContextPct, 0.01,
		"the percentage is recomputed, not left at the compaction's reading")

	// Repeated polls must be stable: this is the flapping the shared
	// copilotContextPct and window read exist to prevent.
	for range 3 {
		resetCopilotContextRefreshStateForTest()
		refreshCopilotContextSnapshotOnRead(sess, true)
		again, err := db.GetContextSnapshot(sess.ID)
		require.NoError(t, err)
		assert.Equal(t, snap.TokensInput, again.TokensInput)
		assert.InDelta(t, snap.ContextPct, again.ContextPct, 0.0001)
		assert.Equal(t, snap.ContextWindowSize, again.ContextWindowSize)
	}
}

// TestCopilotUsageNeverRegressesOutputTokens is the review's blocking case.
//
// The two sources count different things and EITHER may be ahead: the durable
// log's shutdown total is session-lifetime and restored across a resume, while
// the sweep counts only the rows it has consumed — legitimately behind
// mid-drain of a large backlog, and starting from zero for a session first seen
// after a resume.
//
// Writing the lower figure would not merely blink, it would STICK: the
// read-through follower skips its write when its projection matches its own
// persisted mirror, and that mirror still holds the higher value, so no
// corrective write ever follows.
func TestCopilotUsageNeverRegressesOutputTokens(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	// The durable follower is far ahead — a resumed session's shutdown total.
	updated, err := db.UpdateContextSnapshotForGeneration(
		sess.ID, sess.ConvID, sess.CreatedAt, 0, 0, 50000, 0)
	require.NoError(t, err)
	require.True(t, updated)

	// The sweep folds a handful of rows and is nowhere near that figure.
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(1, 25114, 300)})

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(50000), snap.TokensOutput,
		"the sweep must not drag tokens_output backwards")
	assert.Equal(t, int64(25114), snap.TokensInput,
		"the numerator still comes from the sweep")

	// And it must still ADVANCE once the sweep genuinely overtakes the durable
	// figure — a max() that never moves would be its own bug.
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(2, 26000, 60000)})
	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(60300), snap.TokensOutput,
		"once ahead, the sweep's cumulative total takes over")
}

// TestCopilotUsageOutputRegressionWouldStick is the reason the fix belongs in
// the sweep rather than being left for the follower to repair: the follower
// dedupes against its OWN mirror, so a regressed row is never corrected.
func TestCopilotUsageOutputRegressionWouldStick(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	resetCopilotContextRefreshStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)
	t.Cleanup(resetCopilotContextRefreshStateForTest)

	home := copilotRefreshHome(t)
	appendCopilotRefreshEvents(t, copilotRefreshLogPath(home),
		`{"type":"session.start","data":{"sessionId":"s","copilotVersion":"1.0.77","selectedModel":"gpt-5.4"}}`,
		`{"type":"assistant.message","data":{"model":"gpt-5.4","outputTokens":50000}}`)

	sess := copilotUsageSession(t, "s-copilot", copilotRefreshConvID)
	refreshCopilotContextSnapshotOnRead(sess, true)
	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	require.Equal(t, int64(50000), snap.TokensOutput)

	// The sweep now reports a much smaller cumulative output.
	call := copilotUsageCall(1, 20000, 300)
	call.SessionID = copilotRefreshConvID
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{call})

	// Polling the follower again must not be needed to repair anything, and
	// must not itself regress the row either.
	for range 3 {
		resetCopilotContextRefreshStateForTest()
		refreshCopilotContextSnapshotOnRead(sess, true)
		again, err := db.GetContextSnapshot(sess.ID)
		require.NoError(t, err)
		assert.Equal(t, int64(50000), again.TokensOutput,
			"neither writer may regress tokens_output on any poll")
		assert.Equal(t, int64(20000), again.TokensInput)
	}
}

// TestSweepCopilotUsagePreservesStateWhenLivenessUnavailable covers the
// should-fix: a transient tmux hiccup is the ABSENCE of an observation, not an
// observation that no panes are running, and must not close cached stores or
// drop cursors.
func TestSweepCopilotUsagePreservesStateWhenLivenessUnavailable(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(1, 25114, 300)})
	_, ok := lookupCopilotLiveUsage(sess.ID, sess.ConvID, sess.CreatedAt)
	require.True(t, ok, "state to preserve")

	restore := liveTmuxCache
	t.Cleanup(func() { liveTmuxCache = restore })
	liveTmuxCache = newTmuxSessionCache(0, time.Now, func() (map[string]struct{}, error) {
		return nil, assert.AnError
	})

	sweepCopilotUsage(t.Context())

	_, ok = lookupCopilotLiveUsage(sess.ID, sess.ConvID, sess.CreatedAt)
	assert.True(t, ok, "an unavailable liveness probe must not drop poller state")

	// A SUCCESSFUL probe reporting no live panes is a real observation, and
	// that one does retire the state.
	liveTmuxCache = newTmuxSessionCache(0, time.Now, func() (map[string]struct{}, error) {
		return map[string]struct{}{}, nil
	})
	sweepCopilotUsage(t.Context())

	_, ok = lookupCopilotLiveUsage(sess.ID, sess.ConvID, sess.CreatedAt)
	assert.False(t, ok, "a genuinely empty live set does retire the state")
}

// TestApplyCopilotUsageCallsExcludesNestedCallsFromNumerator covers the
// reviewer's subagent question. Copilot records a call made from inside a tool
// call under the SAME session_id, and its prompt is its own — much smaller than
// the conversation's. Letting it win the numerator would dip the meter every
// time a tool ran, then restore it on the next real turn.
func TestApplyCopilotUsageCallsExcludesNestedCallsFromNumerator(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")

	topLevel := copilotUsageCall(1, 120000, 300)
	nested := copilotUsageCall(2, 800, 50)
	nested.Nested = true
	nested.Model = "gpt-5-mini"

	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{topLevel, nested})

	snapshot, err := db.LoadCopilotUsageSnapshot(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)

	assert.Equal(t, int64(120000), snapshot.LastCallInputTokens,
		"a nested call must not become the conversation's occupancy")
	assert.Equal(t, "gpt-5", snapshot.Model,
		"nor rename the model shown beside it")

	// Its spend is still fully accounted for, and its id still advances the
	// cursor — skipping that would re-read it forever.
	assert.Equal(t, int64(2), snapshot.Requests)
	assert.Equal(t, int64(120800), snapshot.InputTokens)
	assert.Equal(t, int64(350), snapshot.OutputTokens)
	assert.Equal(t, int64(2), snapshot.LastEventID)
}

// TestApplyCopilotUsageCallsNestedOnlyBatchKeepsPriorNumerator is the boundary:
// a sweep that sees ONLY nested calls has learned nothing about the
// conversation's window, so the previous reading must stand rather than reset.
func TestApplyCopilotUsageCallsNestedOnlyBatchKeepsPriorNumerator(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{copilotUsageCall(1, 120000, 300)})

	nested := copilotUsageCall(2, 800, 50)
	nested.Nested = true
	applyCopilotUsageCalls(sess, []harness.CopilotUsageCall{nested})

	snapshot, err := db.LoadCopilotUsageSnapshot(sess.ID)
	require.NoError(t, err)
	require.NotNil(t, snapshot)
	assert.Equal(t, int64(120000), snapshot.LastCallInputTokens,
		"an all-nested sweep leaves the occupancy where it was")
	assert.Equal(t, int64(2), snapshot.LastEventID, "but still advances the cursor")
	assert.Equal(t, int64(2), snapshot.Requests)
}

// TestCopilotUsageBackfillsStoredModelOnRestart is the restart case the sweep
// used to leave dark.
//
// sessions.model and sessions.effort_level are owned by this sweep once a usage
// row exists, and the sweep only wrote them when it folded NEW rows. A daemon
// restart in front of an idle pane therefore left both columns blank until the
// conversation happened to take another turn — while the answer sat in
// copilot_usage_snapshots the whole time.
//
// The home here deliberately has no store at all: the backfill must ride the
// sweep tick BEFORE the store is opened, so a stored reading is restored even
// on a host where the store has become unreadable.
func TestCopilotUsageBackfillsStoredModelOnRestart(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	// Exactly the state a daemon restart leaves behind: the durable snapshot
	// survives, the sessions row's own columns are whatever they were, and no
	// sweep memory exists at all. Written directly rather than through
	// applyCopilotUsageCalls so the session columns start genuinely blank.
	saved, err := db.SaveCopilotUsageSnapshot(db.CopilotUsageSnapshot{
		SessionID: sess.ID, ConvID: sess.ConvID, LastEventID: 7, Requests: 3,
		Model: "gpt-5", ReasoningEffort: "medium",
		LastCallInputTokens: 25114, OutputTokens: 300,
	}, sess.CreatedAt)
	require.NoError(t, err)
	require.True(t, saved)

	before, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	require.Empty(t, before.Model, "the restart case starts with a blank dashboard cell")

	// The home has no store at all, deliberately: the backfill must ride the
	// sweep tick BEFORE the store is opened, so a stored reading is restored
	// even where the store has become unreadable.
	sweepCopilotUsageHome(t.Context(), t.TempDir(), []*db.SessionRow{sess})

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "gpt-5", snap.Model, "the stored model must return without a new row")
	assert.Equal(t, "medium", snap.EffortLevel)
	assert.Equal(t, int64(25114), snap.TokensInput)
	assert.Equal(t, int64(300), snap.TokensOutput)

	// Once per session per daemon lifetime: later ticks must not write again.
	// Proven by making the row disagree and showing the sweep leaves it alone —
	// a second backfill would overwrite this.
	updated, err := db.UpdateContextSnapshotAndModelEffortForGeneration(
		sess.ID, sess.ConvID, sess.CreatedAt, 0, 25114, 300, 0, "set-by-someone-else", "")
	require.NoError(t, err)
	require.True(t, updated)

	sweepCopilotUsageHome(t.Context(), t.TempDir(), []*db.SessionRow{sess})
	sweepCopilotUsageHome(t.Context(), t.TempDir(), []*db.SessionRow{sess})

	snap, err = db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Equal(t, "set-by-someone-else", snap.Model,
		"the restart backfill runs once, not on every tick")
}

// TestCopilotUsageBackfillRespectsGenerationGuard keeps the restart path inside
// the same discipline as the live fold: a snapshot belonging to a PREVIOUS
// generation of this session id must not be restored onto the new
// conversation's row.
func TestCopilotUsageBackfillRespectsGenerationGuard(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	saved, err := db.SaveCopilotUsageSnapshot(db.CopilotUsageSnapshot{
		SessionID: sess.ID, ConvID: sess.ConvID, LastEventID: 7,
		Model: "gpt-5", ReasoningEffort: "medium", LastCallInputTokens: 25114,
	}, sess.CreatedAt)
	require.NoError(t, err)
	require.True(t, saved)

	// The pane was recreated: same session id, a NEW conversation, so the
	// stored snapshot describes a dead one.
	recreated := *sess
	recreated.ConvID = "conv-2"
	require.NoError(t, db.SaveSession(&recreated))

	sweepCopilotUsageHome(t.Context(), t.TempDir(), []*db.SessionRow{&recreated})

	snap, err := db.GetContextSnapshot(recreated.ID)
	require.NoError(t, err)
	assert.Empty(t, snap.Model, "a previous conversation's model must not be restored")
	assert.Zero(t, snap.TokensInput)
}

// TestCopilotUsageBackfillIgnoresEmptySnapshot: a session whose snapshot row
// exists but has folded nothing has no reading to restore, and must not write.
func TestCopilotUsageBackfillIgnoresEmptySnapshot(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	sess := copilotUsageSession(t, "s-copilot", "conv-1")
	saved, err := db.SaveCopilotUsageSnapshot(
		db.CopilotUsageSnapshot{SessionID: sess.ID, ConvID: sess.ConvID}, sess.CreatedAt)
	require.NoError(t, err)
	require.True(t, saved)

	sweepCopilotUsageHome(t.Context(), t.TempDir(), []*db.SessionRow{sess})

	snap, err := db.GetContextSnapshot(sess.ID)
	require.NoError(t, err)
	assert.Empty(t, snap.Model)
	assert.Zero(t, snap.TokensInput)
}

// TestCopilotUsageReadFailureLogsErrorOnce pins the log level this bug hid
// behind.
//
// A sweep read that fails is not a curiosity: it means live usage is not being
// recorded at all, which is exactly what happened on every real host while the
// only trace of it was a Debug line. It must reach an operator at Error — and,
// because the sweep ticks every 2 seconds against a condition that does not
// change, exactly once per (home, reason) per interval.
func TestCopilotUsageReadFailureLogsErrorOnce(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	home := "/nonexistent/copilot-home"
	failure := errors.New("scan call: converting driver.Value type float64 to a int64")
	for range 5 {
		logCopilotUsageHomeFailure(home, "read",
			"copilot-usage: sweep read failed; live usage is not being recorded", failure)
	}
	assert.Equal(t, 1, strings.Count(logs.String(), "sweep read failed"),
		"a permanent failure reports once, not every tick")
	assert.Contains(t, logs.String(), "level=ERROR")

	// A DIFFERENT failure mode must still be audible while the first is
	// suppressed — that is what keying the limiter by reason buys.
	logCopilotUsageHomeFailure(home, "open",
		"copilot-usage: session store unusable; live usage degrades to the durable event log", failure)
	assert.Contains(t, logs.String(), "session store unusable")
}

// TestCopilotUsageAbsentStoreStaysQuiet is the other half of the operator's
// rule: a host that simply does not run Copilot must not produce error lines.
// The distinguishing test is the presence of the file, not the kind of failure.
func TestCopilotUsageAbsentStoreStaysQuiet(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	// A real, empty home: no session-store.db, which is what every host without
	// Copilot looks like.
	store, ok := copilotUsageStoreFor(t.TempDir())
	require.False(t, ok)
	require.Nil(t, store)
	assert.NotContains(t, logs.String(), "level=ERROR")
	assert.NotContains(t, logs.String(), "level=WARN")
}
