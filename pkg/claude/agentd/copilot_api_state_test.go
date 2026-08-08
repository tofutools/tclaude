package agentd

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The payloads below are the SHAPES the live 1.0.78 server returns, copied from
// a recorded run rather than invented. That matters more here than it usually
// would: two of the three have a wrong-but-decodable sibling shape that yields
// zeros without any error, so a test written against a convenient shape would
// pass while the production decode reported an empty meter.
const (
	// modelMetrics entries are NESTED. Flatten them and every counter reads 0.
	copilotAPITestUsage = `{
		"totalPremiumRequestCost":0,"totalUserRequests":2,"totalNanoAiu":460235000,
		"currentModel":"gpt-5-mini","lastCallInputTokens":14152,"lastCallOutputTokens":210,
		"codeChanges":{"linesAdded":0,"linesRemoved":0,"filesModifiedCount":0},
		"modelMetrics":{"gpt-5-mini":{
			"requests":{"count":2,"cost":0},
			"usage":{"inputTokens":26956,"outputTokens":311,"cacheReadTokens":7808,
				"cacheWriteTokens":0,"reasoningTokens":64},
			"totalNanoAiu":460235000}}}`

	// The parts sum to totalTokens and mcpToolsTokens is ALREADY inside
	// toolDefinitionsTokens: 6911 + 599 + 9320 = 16830, with 1238 of the 9320
	// being MCP. Adding the MCP figure would double-count it.
	copilotAPITestContext = `{"contextInfo":{
		"modelName":"claude-sonnet-4.5","systemTokens":6911,"conversationTokens":599,
		"toolDefinitionsTokens":9320,"mcpToolsTokens":1238,"totalTokens":16830,
		"promptTokenLimit":128000,"compactionThreshold":102400,"limit":128000,
		"bufferTokens":6400}}`

	// contextInfo is null until the first turn completes. Not an error.
	copilotAPITestContextNull = `{"contextInfo":null}`

	copilotAPITestNoPermissions = `{"items":[]}`

	copilotAPITestPendingPermission = `{"items":[{
		"requestId":"aef407eb-1eee-4fdd-a808-b85833d1eb66",
		"request":{"kind":"commands","fullCommandText":"sleep 8; echo done",
			"intention":"Sleep for 8 seconds then print 'done'",
			"toolCallId":"call_Ts9tKutFtmZm4Y2pvEyDmgqZ"}}]}`
)

func resetCopilotAPIStateForTest() {
	stopCopilotAPIStateConsumers()
	copilotAPIStateConsumers.Lock()
	copilotAPIStateConsumers.running = nil
	copilotAPIStateConsumers.stopping = false
	copilotAPIStateConsumers.Unlock()
	copilotAPIStates.Lock()
	copilotAPIStates.readings = nil
	copilotAPIStates.Unlock()
}

// copilotAPIStateSession creates the session row a reading is written into.
func copilotAPIStateSession(t *testing.T, sessionID, convID, status string) *db.SessionRow {
	t.Helper()
	row := &db.SessionRow{
		ID: sessionID, ConvID: convID, TmuxSession: "pane-" + sessionID,
		Status: status, Harness: harness.CopilotName, CreatedAt: time.Now(),
	}
	require.NoError(t, db.SaveSession(row))
	stored, err := db.LoadSession(sessionID)
	require.NoError(t, err)
	require.NotNil(t, stored)
	return stored
}

// startTestConsumer wires a consumer to a fake server answering the canned
// payloads, and returns once its first refresh has landed.
//
// The handle is ADOPTED, and with a real port and pid, because every refresh
// re-proves ownership through copilotAPIDrive before it reads. Registering a
// handle whose ownership proof is genuine — this test process really does own
// the loopback port the fake server is listening on — keeps that gate exercised
// on the same terms production uses, rather than stubbed out of the way.
func startTestConsumer(
	t *testing.T, server *fakeCopilotServer, convID, sessionID string,
) *copilotAPIStateConsumer {
	t.Helper()
	client := dialFakeCopilot(t, server)
	handle := &copilotAPISession{
		ConvID: convID, SessionID: sessionID, Client: client,
		Port: server.port(), PanePID: os.Getpid(),
	}
	copilotAPISessions.Adopt(handle)
	t.Cleanup(func() { copilotAPISessions.Drop(convID) })
	startCopilotAPIStateConsumer(handle)
	t.Cleanup(resetCopilotAPIStateForTest)
	copilotAPIStateConsumers.Lock()
	consumer := copilotAPIStateConsumers.running[convID]
	copilotAPIStateConsumers.Unlock()
	require.NotNil(t, consumer)
	return consumer
}

// ---------------------------------------------------------------------------
// The reading
// ---------------------------------------------------------------------------

// The payoff of the whole API drive, stated as one assertion: the meter's
// denominator stops being a static per-model guess and becomes the limit
// Copilot reported for this session.
//
// The numerator changes meaning with it, deliberately. It is no longer the
// newest call's billed prompt but everything currently in the window as Copilot
// tokenizes it — which is the SAME quantity Copilot compares against its own
// compaction threshold, so the meter now predicts the event it exists to warn
// about.
func TestCopilotAPIStateWritesTheReportedWindow(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	row := copilotAPIStateSession(t, "s-api", "conv-api", session.StatusIdle)
	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContext)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestNoPermissions)
	startTestConsumer(t, server, "conv-api", "conv-api")

	assert.Eventually(t, func() bool {
		snapshot, err := db.GetContextSnapshot(row.ID)
		return err == nil && snapshot.TokensInput == 16830
	}, 5*time.Second, 20*time.Millisecond)

	snapshot, err := db.GetContextSnapshot(row.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(16830), snapshot.TokensInput,
		"the numerator is the whole current window, not the newest call's prompt")
	assert.Equal(t, int64(128000), snapshot.ContextWindowSize,
		"the window Copilot reported is recorded as the observed window")
	assert.InDelta(t, 100*16830.0/128000.0, snapshot.ContextPct, 0.0001)
	assert.Equal(t, int64(311), snapshot.TokensOutput,
		"output comes from the NESTED usage shape; a flattened decode reports 0")
}

// The reading is published for other readers to defer to, and it carries the
// model from USAGE. contextInfo.modelName is a different model entirely under
// auto mode — measured naming claude-sonnet-4.5 for turns that ran on
// gpt-5-mini — so sourcing it there would misattribute the spend while looking
// perfectly healthy.
func TestCopilotAPIStateTakesTheModelFromUsageNotContext(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	copilotAPIStateSession(t, "s-model", "conv-model", session.StatusIdle)
	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContext)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestNoPermissions)
	startTestConsumer(t, server, "conv-model", "conv-model")

	assert.Eventually(t, func() bool {
		reading, ok := lookupCopilotAPIState("conv-model")
		return ok && reading.Model != ""
	}, 5*time.Second, 20*time.Millisecond)

	reading, ok := lookupCopilotAPIState("conv-model")
	require.True(t, ok)
	assert.Equal(t, "gpt-5-mini", reading.Model)
	assert.NotEqual(t, "claude-sonnet-4.5", reading.Model,
		"the context breakdown's model name is not the model the turn ran on")
}

// "auto" is a MODE, not a model, and usage reports it verbatim until a call has
// resolved one — observed mid-turn against a live server. Publishing it would
// not fail loudly: harness.CopilotContextWindowDefault answers any non-empty
// unknown model with a generic 200000, so a session actually running a 128000
// model would be metered against a window it does not have, and the meter would
// look entirely ordinary while doing it.
func TestCopilotAPIReadingModelRejectsTheAutoSentinel(t *testing.T) {
	// The value the bug would have produced, so the regression is stated rather
	// than implied.
	assert.Equal(t, int64(200000), harness.CopilotContextWindowDefault("auto"),
		"this test exists because the static table answers 'auto' with a real-looking number")

	assert.Equal(t, "gpt-5-mini", copilotAPIReadingModel(copilotapi.UsageMetrics{
		CurrentModel: "gpt-5-mini",
	}))
	assert.Equal(t, "gpt-5-mini", copilotAPIReadingModel(copilotapi.UsageMetrics{
		CurrentModel: "auto",
		ModelMetrics: map[string]copilotapi.ModelMetric{"gpt-5-mini": {}},
	}), "auto mode that has billed one model has named it")
	assert.Empty(t, copilotAPIReadingModel(copilotapi.UsageMetrics{CurrentModel: "auto"}),
		"auto mode before any call has resolved nothing, and unknown must read as unknown")
	assert.Empty(t, copilotAPIReadingModel(copilotapi.UsageMetrics{
		CurrentModel: "auto",
		ModelMetrics: map[string]copilotapi.ModelMetric{"gpt-5-mini": {}, "claude-haiku-4.5": {}},
	}), "with several billed models there is no current one, and picking arbitrarily "+
		"would attribute the window of one to the usage of another")
}

// A session that has not run a turn reports a null contextInfo. Publishing a
// zeroed reading for it would be worse than publishing nothing twice over: it
// would render "not measured yet" as a measured 0%, AND it would make the other
// Copilot sources stand down in favour of a source with nothing to say.
func TestCopilotAPIStatePublishesNothingBeforeTheFirstTurn(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	row := copilotAPIStateSession(t, "s-fresh", "conv-fresh", session.StatusIdle)
	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContextNull)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestNoPermissions)
	startTestConsumer(t, server, "conv-fresh", "conv-fresh")

	// Wait for the refresh to have definitely happened, by watching the call
	// the refresh always makes first.
	assert.Eventually(t, func() bool {
		return server.callCount(copilotapi.MethodSessionContextInfo) > 0
	}, 5*time.Second, 20*time.Millisecond)

	_, ok := lookupCopilotAPIState("conv-fresh")
	assert.False(t, ok, "a null contextInfo is not a reading")
	snapshot, err := db.GetContextSnapshot(row.ID)
	require.NoError(t, err)
	assert.Zero(t, snapshot.ContextPct)
	assert.Zero(t, snapshot.TokensInput)
}

// ---------------------------------------------------------------------------
// One writer
// ---------------------------------------------------------------------------

// Two writers converging on one row is how the TCL-1048 follow-up bug happened.
// While an API reading exists it is the only writer of the context columns, and
// the sweep must carry the stored values through rather than recomputing them
// from its own weaker sources.
func TestCopilotUsageSweepStandsDownOnTheContextColumnsWhenTheAPIOwnsThem(t *testing.T) {
	setupTestDB(t)
	resetCopilotUsageStateForTest()
	resetCopilotAPIStateForTest()
	t.Cleanup(resetCopilotUsageStateForTest)

	row := copilotAPIStateSession(t, "s-own", "conv-own", session.StatusIdle)
	require.NoError(t, db.UpdateContextSnapshot(row.ID, 13.15, 16830, 311, 128000))

	publishCopilotAPIState("conv-own", copilotAPIStateReading{
		ObservedAt: time.Now(), TotalTokens: 16830, PromptTokenLimit: 128000,
		OutputTokens: 311, Model: "gpt-5-mini",
	})

	// The sweep's own view is a smaller prompt against an assumed window, which
	// is exactly the pair that would overwrite the reading.
	persistCopilotUsageContext(row, db.CopilotUsageSnapshot{
		SessionID: row.ID, ConvID: row.ConvID, LastCallInputTokens: 14152,
		OutputTokens: 311, Model: "gpt-5-mini", ReasoningEffort: "low",
	})

	snapshot, err := db.GetContextSnapshot(row.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(16830), snapshot.TokensInput,
		"the sweep must not replace the API reading's occupancy")
	assert.InDelta(t, 13.15, snapshot.ContextPct, 0.0001,
		"nor recompute the percentage against its assumed window")
	assert.Equal(t, int64(128000), snapshot.ContextWindowSize)
	assert.Equal(t, "low", snapshot.EffortLevel,
		"model and effort stay the sweep's to own — the API reading has no source for effort")
}

// The durable follower contributes nothing while the API reading exists, and
// must not write at all. Merging it in would put a third source into a
// reconciliation that exists to referee two.
func TestCopilotDurableFollowerStandsDownWhenTheAPIOwnsTheRow(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	row := copilotAPIStateSession(t, "s-follow", "conv-follow", session.StatusIdle)
	require.NoError(t, db.UpdateContextSnapshot(row.ID, 13.15, 16830, 311, 128000))
	publishCopilotAPIState("conv-follow", copilotAPIStateReading{
		ObservedAt: time.Now(), TotalTokens: 16830, PromptTokenLimit: 128000,
		OutputTokens: 311, Model: "gpt-5-mini",
	})

	state := &copilotContextRefreshState{
		follower: &harness.CopilotTelemetryFollower{},
		convID:   row.ConvID, createdAt: row.CreatedAt,
	}
	persistCopilotContextSnapshot(row, state, harness.CopilotRuntimeSnapshot{
		Usage:      &harness.CopilotUsage{InputTokens: 999, OutputTokens: 1},
		HasContext: true,
		Context:    harness.CopilotContextTelemetry{CurrentTokens: 999, TokenLimit: 64000},
	})

	snapshot, err := db.GetContextSnapshot(row.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(16830), snapshot.TokensInput)
	assert.Equal(t, int64(128000), snapshot.ContextWindowSize)
	assert.InDelta(t, 13.15, snapshot.ContextPct, 0.0001)
}

// The stand-down lasts exactly as long as the connection does. A consumer whose
// connection ends must drop its reading, or the other two writers keep deferring
// to a source that has stopped answering and the meter freezes.
func TestCopilotAPIStateDropsItsReadingWhenTheConnectionEnds(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	copilotAPIStateSession(t, "s-end", "conv-end", session.StatusIdle)
	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContext)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestNoPermissions)
	startTestConsumer(t, server, "conv-end", "conv-end")

	require.Eventually(t, func() bool {
		_, ok := lookupCopilotAPIState("conv-end")
		return ok
	}, 5*time.Second, 20*time.Millisecond)

	server.close()
	assert.Eventually(t, func() bool {
		_, ok := lookupCopilotAPIState("conv-end")
		return !ok
	}, 5*time.Second, 20*time.Millisecond,
		"a reading is a statement about a connection, and must not outlive it")
}

// The stand-down predicate has to be "a reading that is CURRENT", not "a
// reading exists". A connection can stay open while the reads on it start
// failing — a session the server no longer knows, a renamed method — and in
// that state nothing ends the consumer. A reading with no expiry would sit in
// the registry with both fallback writers standing down behind it, freezing the
// row at its last good numbers with no writer left at all.
func TestCopilotAPIStateReadingExpires(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()
	t.Cleanup(resetCopilotAPIStateForTest)

	publishCopilotAPIState("conv-stale", copilotAPIStateReading{
		ObservedAt: time.Now(), TotalTokens: 16830, PromptTokenLimit: 128000,
	})
	_, ok := lookupCopilotAPIState("conv-stale")
	assert.True(t, ok, "a reading taken now is current")

	publishCopilotAPIState("conv-stale", copilotAPIStateReading{
		ObservedAt:  time.Now().Add(-copilotAPIStateFreshness - time.Second),
		TotalTokens: 16830, PromptTokenLimit: 128000,
	})
	_, ok = lookupCopilotAPIState("conv-stale")
	assert.False(t, ok,
		"a reading that has not been re-established must hand the row back to the "+
			"writers that can still speak for it")
}

// A relaunch replaces the consumer while the predecessor may still be parked in
// a read with a ten-second timeout. When it finally unwinds it must not delete
// the SUCCESSOR's reading, which would hand the row back to the weaker writers
// until the successor's next trigger.
func TestCopilotAPIStateRetirementSparesASuccessorsReading(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()
	t.Cleanup(resetCopilotAPIStateForTest)

	predecessor := &copilotAPIStateConsumer{convID: "conv-relaunch", stop: make(chan struct{})}
	successor := &copilotAPIStateConsumer{convID: "conv-relaunch", stop: make(chan struct{})}
	copilotAPIStateConsumers.Lock()
	copilotAPIStateConsumers.running = map[string]*copilotAPIStateConsumer{
		"conv-relaunch": successor,
	}
	copilotAPIStateConsumers.Unlock()
	publishCopilotAPIState("conv-relaunch", copilotAPIStateReading{
		ObservedAt: time.Now(), TotalTokens: 16830, PromptTokenLimit: 128000,
	})

	retireCopilotAPIStateConsumer(predecessor)

	_, ok := lookupCopilotAPIState("conv-relaunch")
	assert.True(t, ok, "the predecessor must not drop the successor's reading")

	retireCopilotAPIStateConsumer(successor)
	_, ok = lookupCopilotAPIState("conv-relaunch")
	assert.False(t, ok, "the registered consumer retiring does drop it")
}

// A reply with no promptTokenLimit must not erase a window an earlier reply (or
// a compaction the durable follower saw) already disclosed. The four columns
// are written together, and once this consumer is publishing the follower has
// stood down and cannot put it back.
func TestCopilotAPIStateKeepsAKnownWindowWhenTheReplyOmitsOne(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	row := copilotAPIStateSession(t, "s-window", "conv-window", session.StatusIdle)
	require.NoError(t, db.UpdateContextSnapshot(row.ID, 13.15, 16830, 311, 128000))

	persistCopilotAPIContext(row, copilotAPIStateReading{
		ObservedAt: time.Now(), TotalTokens: 17000, PromptTokenLimit: 0,
		OutputTokens: 311, Model: "gpt-5-mini",
	})

	snapshot, err := db.GetContextSnapshot(row.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(128000), snapshot.ContextWindowSize,
		"a missing limit carries the known one forward rather than writing zero")
	assert.Equal(t, int64(17000), snapshot.TokensInput)
}

// ---------------------------------------------------------------------------
// Permission
// ---------------------------------------------------------------------------

// The state tclaude could not see before. A Copilot agent blocked on a tool
// approval fires no hook at all — measured, the Stop hook never arrives while
// the prompt is open — so it sits at "working" for as long as the human takes,
// which is indistinguishable from an agent doing work.
func TestCopilotAPIStateProjectsAPendingPermissionPrompt(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	row := copilotAPIStateSession(t, "s-perm", "conv-perm", session.StatusWorking)
	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContext)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestPendingPermission)
	startTestConsumer(t, server, "conv-perm", "conv-perm")

	assert.Eventually(t, func() bool {
		stored, err := db.LoadSession(row.ID)
		return err == nil && stored != nil && stored.Status == session.StatusAwaitingPermission
	}, 5*time.Second, 20*time.Millisecond)

	stored, err := db.LoadSession(row.ID)
	require.NoError(t, err)
	assert.Equal(t, copilotAPIStatePermissionDetail, stored.StatusDetail,
		"the detail must name which surface is waiting")
}

// Clearing is not the mirror of setting: the prompt's absence says a decision
// was made, not which one. Approved means the turn is running again; declined
// means it is over. The successor is READ rather than assumed.
func TestCopilotAPIStateClearsAResolvedPromptFromTheServersAnswer(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		processing string
		want       string
	}{
		{"approved, the turn resumes", `{"processing":true}`, session.StatusWorking},
		{"declined, the turn is over", `{"processing":false}`, session.StatusIdle},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			setupTestDB(t)
			resetCopilotAPIStateForTest()

			row := copilotAPIStateSession(t, "s-clear", "conv-clear", session.StatusWorking)
			server := newFakeCopilotServer(t)
			server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContext)
			server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
			server.answer(copilotapi.MethodSessionPermissions, copilotAPITestPendingPermission)
			server.answer(copilotapi.MethodSessionIsProcessing, testCase.processing)
			startTestConsumer(t, server, "conv-clear", "conv-clear")

			require.Eventually(t, func() bool {
				stored, err := db.LoadSession(row.ID)
				return err == nil && stored != nil &&
					stored.Status == session.StatusAwaitingPermission
			}, 5*time.Second, 20*time.Millisecond)

			// The human answers, and an event announces it.
			server.answer(copilotapi.MethodSessionPermissions, copilotAPITestNoPermissions)
			server.push(copilotapi.MethodSessionEvent, `{"sessionId":"conv-clear",
				"event":{"type":"permission.completed","id":"e1"}}`)

			assert.Eventually(t, func() bool {
				stored, err := db.LoadSession(row.ID)
				return err == nil && stored != nil && stored.Status == testCase.want
			}, 5*time.Second, 20*time.Millisecond)
		})
	}
}

// Ownership of the awaiting state lives on the ROW, not in the consumer, and
// this is the case that decides it: a row left in awaiting_permission by a
// PREVIOUS consumer — an agentd restart is the reachable one — must still be
// cleared once the prompt is gone. An in-memory flag would start false and
// never clear it, and that is not a cosmetic wrong label: message delivery is
// held for an agent in an awaiting state, so it would silently stop receiving
// mail.
func TestCopilotAPIStateClearsAStrandedAwaitingRowFromAPreviousConsumer(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	row := copilotAPIStateSession(t, "s-strand", "conv-strand", session.StatusWorking)
	// The state a predecessor left behind, marker and all.
	set, err := db.SetSessionStatusIfUnchanged(row.ID, row.Status, row.UpdatedAt,
		session.StatusAwaitingPermission, copilotAPIStatePermissionDetail, time.Now())
	require.NoError(t, err)
	require.True(t, set)

	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContext)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestNoPermissions)
	server.answer(copilotapi.MethodSessionIsProcessing, `{"processing":false}`)
	startTestConsumer(t, server, "conv-strand", "conv-strand")

	assert.Eventually(t, func() bool {
		stored, err := db.LoadSession(row.ID)
		return err == nil && stored != nil && stored.Status == session.StatusIdle
	}, 5*time.Second, 20*time.Millisecond,
		"a fresh consumer must be able to clear a state it did not itself set")
}

// A failed clear must leave the marker in place so the next refresh retries.
// Dropping an in-memory flag before the clear succeeded would strand the row
// for the lifetime of the process.
func TestCopilotAPIStateRetriesAFailedClear(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	row := copilotAPIStateSession(t, "s-retry", "conv-retry", session.StatusWorking)
	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContext)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestPendingPermission)
	// The busy read the clear depends on answers with a shape that cannot be
	// decoded, standing in for any transient failure of it.
	server.answer(copilotapi.MethodSessionIsProcessing, `"not an object"`)
	startTestConsumer(t, server, "conv-retry", "conv-retry")

	require.Eventually(t, func() bool {
		stored, err := db.LoadSession(row.ID)
		return err == nil && stored != nil &&
			stored.Status == session.StatusAwaitingPermission
	}, 5*time.Second, 20*time.Millisecond)

	// The prompt resolves, but the clear cannot complete yet.
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestNoPermissions)
	server.push(copilotapi.MethodSessionEvent, `{"sessionId":"conv-retry",
		"event":{"type":"permission.completed","id":"e1"}}`)
	require.Eventually(t, func() bool {
		return server.callCount(copilotapi.MethodSessionIsProcessing) > 0
	}, 5*time.Second, 20*time.Millisecond)

	stored, err := db.LoadSession(row.ID)
	require.NoError(t, err)
	require.Equal(t, session.StatusAwaitingPermission, stored.Status,
		"a failed clear leaves the row as it was")

	// The read recovers, and the next refresh finishes the job.
	server.answer(copilotapi.MethodSessionIsProcessing, `{"processing":true}`)
	server.push(copilotapi.MethodSessionEvent, `{"sessionId":"conv-retry",
		"event":{"type":"assistant.turn_start","id":"e2"}}`)
	assert.Eventually(t, func() bool {
		stored, err := db.LoadSession(row.ID)
		return err == nil && stored != nil && stored.Status == session.StatusWorking
	}, 5*time.Second, 20*time.Millisecond,
		"the marker must survive a failed clear so the next refresh retries")
}

// The consumer only ever clears a state it set itself. An awaiting_input coming
// from somewhere else is not this file's to resolve, and clearing it would
// silently drop a genuine request for a human.
func TestCopilotAPIStateLeavesForeignAwaitingStatesAlone(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	row := copilotAPIStateSession(t, "s-foreign", "conv-foreign", session.StatusAwaitingInput)
	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContext)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestNoPermissions)
	startTestConsumer(t, server, "conv-foreign", "conv-foreign")

	require.Eventually(t, func() bool {
		return server.callCount(copilotapi.MethodSessionPermissions) > 0
	}, 5*time.Second, 20*time.Millisecond)

	stored, err := db.LoadSession(row.ID)
	require.NoError(t, err)
	assert.Equal(t, session.StatusAwaitingInput, stored.Status)
	assert.Zero(t, server.callCount(copilotapi.MethodSessionIsProcessing),
		"with nothing of its own to clear, the consumer must not even ask")
}

// ---------------------------------------------------------------------------
// Triggering
// ---------------------------------------------------------------------------

func TestCopilotAPIStateMarksDirty(t *testing.T) {
	consumer := &copilotAPIStateConsumer{convID: "conv-1", sessionID: "sess-1"}
	event := func(body string) copilotapi.Notification {
		return copilotapi.Notification{
			Method: copilotapi.MethodSessionEvent, Params: json.RawMessage(body),
		}
	}

	assert.True(t, consumer.marksDirty(event(
		`{"sessionId":"sess-1","event":{"type":"session.idle","id":"e1"}}`)))

	assert.False(t, consumer.marksDirty(event(
		`{"sessionId":"sess-1","event":{"type":"assistant.streaming_delta","id":"e2"}}`)),
		"a delta carries no state and fires tens of times a turn")

	// The distinction TCL-1052's cold review caught. A sub-agent's events are
	// its own; treating them as the root agent's would refresh on work that is
	// not this agent's turn.
	assert.False(t, consumer.marksDirty(event(
		`{"sessionId":"sess-1","event":{"type":"session.idle","id":"e3","agentId":"call_GW4"}}`)),
		"a sub-agent's idle is not the agent's idle")

	assert.False(t, consumer.marksDirty(event(
		`{"sessionId":"other","event":{"type":"session.idle","id":"e4"}}`)),
		"another session on the same server is not ours")

	// An event type this build has never seen must default to refreshing.
	// Copilot's vocabulary is open, and the failure mode of guessing "probably
	// irrelevant" is a display that silently stops updating.
	assert.True(t, consumer.marksDirty(event(
		`{"sessionId":"sess-1","event":{"type":"session.something_new","id":"e5"}}`)),
		"an unknown event type must trigger a read, not be assumed irrelevant")

	assert.True(t, consumer.marksDirty(copilotapi.Notification{
		Method: "some.future.notification", Params: json.RawMessage(`{}`),
	}), "a method this package does not model is a contract that has grown")
}

// A burst must not produce a read per event. This is the whole read-amplification
// argument, asserted rather than reasoned about.
func TestCopilotAPIStateCoalescesABurst(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	copilotAPIStateSession(t, "s-burst", "conv-burst", session.StatusIdle)
	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContext)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestNoPermissions)
	startTestConsumer(t, server, "conv-burst", "conv-burst")

	require.Eventually(t, func() bool {
		return server.callCount(copilotapi.MethodSessionContextInfo) > 0
	}, 5*time.Second, 20*time.Millisecond)
	before := server.callCount(copilotapi.MethodSessionContextInfo)

	for range 40 {
		server.push(copilotapi.MethodSessionEvent, `{"sessionId":"conv-burst",
			"event":{"type":"assistant.turn_start","id":"e"}}`)
	}
	// Long enough for the coalescing window to expire and its trailing refresh
	// to land, and for a per-event implementation to have made forty reads.
	time.Sleep(2 * copilotAPIStateWindow)

	after := server.callCount(copilotapi.MethodSessionContextInfo) - before
	assert.LessOrEqual(t, after, 3,
		"forty events in one window must collapse into the leading refresh plus "+
			"one trailing one, not forty reads")
	assert.GreaterOrEqual(t, after, 1,
		"the burst must still produce a refresh, or nothing would ever update")
}

// ---------------------------------------------------------------------------
// The denominator
// ---------------------------------------------------------------------------

// The precedence, and the one place it differs from the send-keys drive's:
// a limit Copilot REPORTED outranks tclaude's static per-model table. The
// operator's configured cap still wins outright, because that is intent rather
// than an estimate.
func TestCopilotAPIEffectiveContextWindowPrefersAReportedLimit(t *testing.T) {
	setupTestDB(t)

	reading := copilotAPIStateReading{PromptTokenLimit: 128000, Model: "gpt-5-mini"}
	assert.Equal(t, int64(128000), copilotAPIEffectiveContextWindow("conv-none", reading),
		"a reported limit beats the static assumption for the same model")

	assumed := harness.CopilotContextWindowDefault("gpt-5-mini")
	require.Positive(t, assumed, "this test needs a model the static table knows")
	assert.Equal(t, assumed,
		copilotAPIEffectiveContextWindow("conv-none",
			copilotAPIStateReading{Model: "gpt-5-mini"}),
		"with no reported limit the static table is still the fallback")
}

// ---------------------------------------------------------------------------
// The ownership gate
// ---------------------------------------------------------------------------

// Reading is not the harmless half of the ownership question, and this is the
// test that says so. TCL-1056's rule is that every byte reaching the endpoint
// comes from a listener the agent's pane can still be shown to own; a consumer
// that exempted itself because it "only reads" would copy a stranger's
// occupancy into this conversation's row, and could put the row into
// awaiting_permission because someone ELSE's agent is waiting on a human —
// which holds this agent's message delivery. A mute agent, arrived at by
// reading.
func TestCopilotAPIStateStopsReadingWhenOwnershipCannotBeProved(t *testing.T) {
	setupTestDB(t)
	resetCopilotAPIStateForTest()

	copilotAPIStateSession(t, "s-unproved", "conv-unproved", session.StatusIdle)
	server := newFakeCopilotServer(t)
	server.answer(copilotapi.MethodSessionContextInfo, copilotAPITestContext)
	server.answer(copilotapi.MethodSessionUsage, copilotAPITestUsage)
	server.answer(copilotapi.MethodSessionPermissions, copilotAPITestPendingPermission)
	startTestConsumer(t, server, "conv-unproved", "conv-unproved")

	require.Eventually(t, func() bool {
		_, ok := lookupCopilotAPIState("conv-unproved")
		return ok
	}, 5*time.Second, 20*time.Millisecond, "the consumer must work before it is broken")

	// A live, non-ancestor pid whose subtree genuinely excludes the listener:
	// the same way copilot_api_drive_test.go makes ownership unprovable, so the
	// two paths are broken by the same fact rather than by two mocks.
	stranger := exec.Command("sleep", "60")
	require.NoError(t, stranger.Start())
	t.Cleanup(func() {
		_ = stranger.Process.Kill()
		_ = stranger.Wait()
	})
	handle := copilotAPISessions.Handle("conv-unproved")
	require.NotNil(t, handle)
	handle.PanePID = stranger.Process.Pid

	before := server.callCount(copilotapi.MethodSessionContextInfo)
	for range 5 {
		server.push(copilotapi.MethodSessionEvent, `{"sessionId":"conv-unproved",
			"event":{"type":"assistant.turn_start","id":"e"}}`)
	}
	time.Sleep(3 * copilotAPIStateWindow)

	assert.Equal(t, before, server.callCount(copilotapi.MethodSessionContextInfo),
		"a consumer whose handle can no longer be shown to belong to the agent's "+
			"pane must send nothing, however many events arrive")

	// And the row is handed back rather than frozen: the published reading ages
	// out, so the pre-existing Copilot sources resume instead of standing down
	// behind a source that has stopped being allowed to read.
	copilotAPIStates.Lock()
	stale := copilotAPIStates.readings["conv-unproved"]
	stale.ObservedAt = time.Now().Add(-2 * copilotAPIStateFreshness)
	copilotAPIStates.readings["conv-unproved"] = stale
	copilotAPIStates.Unlock()
	_, ok := lookupCopilotAPIState("conv-unproved")
	assert.False(t, ok, "a reading that can no longer be refreshed must stop counting as one")
}
