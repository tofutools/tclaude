package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/hookevents"
)

// standingOrderFixture wires up a real session row, a real group with the
// session's conv as a member, and one enabled order targeting that group. It
// returns the group id so a test can vary the order's target.
func standingOrderFixture(t *testing.T, harnessName string) int64 {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()

	require.NoError(t, SaveSessionState(&SessionState{
		ID:      "sess-1",
		ConvID:  "conv-1",
		Status:  StatusIdle,
		Harness: harnessName,
	}))

	_, _, err := db.EnsureAgentForConv("conv-1", "test")
	require.NoError(t, err)

	groupID, err := db.CreateAgentGroup("tclaude", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: groupID, ConvID: "conv-1", Role: "worker",
	}))
	return groupID
}

func insertOrder(t *testing.T, groupID int64, mut ...func(*db.StandingOrder)) int64 {
	t.Helper()
	o := &db.StandingOrder{
		Name:             "pr-early",
		TargetKind:       db.StandingTargetGroup,
		GroupID:          groupID,
		Summary:          "Push the PR early; cold review may follow.",
		TriggerEvent:     db.StandingTriggerSessionStart,
		TriggerSources:   []string{db.StandingSourceCompact, db.StandingSourceStartup},
		Timing:           db.StandingTimingSameContinuation,
		Cadence:          db.StandingCadenceAlways,
		Enabled:          true,
		OperatorAuthored: true,
	}
	for _, m := range mut {
		m(o)
	}
	id, err := db.InsertStandingOrder(o)
	require.NoError(t, err)
	return id
}

func sessionStart(source string) HookCallbackInput {
	return HookCallbackInput{
		HookEventName: "SessionStart",
		ConvID:        "conv-1",
		Source:        source,
	}
}

// observe runs the observation-only path and immediately drops the delivery
// lock it hands back. These tests exercise what the path DECIDES; the caller's
// send window is not what they are about, and holding the lock across the rest
// of a test would make a second call in the same test wait out the timeout.
func observe(input HookCallbackInput) []PendingStandingMessage {
	pending, release := ObserveStandingOrders(input, "sess-1")
	release()
	return pending
}

func dispatch(t *testing.T, input HookCallbackInput) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, DispatchHookEvent(context.Background(), input, "sess-1", LocalHookAmbient(), &buf))
	return buf.String()
}

func additionalContext(t *testing.T, out string) string {
	return additionalContextForEvent(t, out, "SessionStart")
}

func additionalContextForEvent(t *testing.T, out, eventName string) string {
	t.Helper()
	var doc struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc), "output was %q", out)
	assert.Equal(t, eventName, doc.HookSpecificOutput.HookEventName)
	return doc.HookSpecificOutput.AdditionalContext
}

func TestStandingOrdersEmptyTableIsCompletelyInert(t *testing.T) {
	standingOrderFixture(t, harness.DefaultName)

	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceCompact)),
		"an opted-out installation must not emit hook context")
	assert.Empty(t, observe(sessionStart(db.StandingSourceCompact)),
		"an opted-out installation must not queue a next-turn message")
	for _, input := range []HookCallbackInput{
		{HookEventName: "UserPromptSubmit", ConvID: "conv-1", Prompt: "deploy"},
		{HookEventName: "PreToolUse", ConvID: "conv-1", ToolName: "Bash"},
		{HookEventName: "PostToolUse", ConvID: "conv-1", ToolName: "Bash"},
	} {
		assert.Empty(t, dispatch(t, input),
			"an opted-out installation must not emit action-trigger context")
		assert.Empty(t, observe(input),
			"an opted-out installation must not queue action-trigger messages")
	}
}

// The headline path: a compaction boundary re-states the order in the same
// continuation, which is the whole reason this feature exists.
func TestStandingOrderDeliveredOnCompactBoundary(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID)

	got := additionalContext(t, dispatch(t, sessionStart(db.StandingSourceCompact)))

	assert.Contains(t, got, "Push the PR early")
	assert.Contains(t, got, "pr-early@1", "the order name and revision must travel with the text")
	assert.Contains(t, got, "authored by operator", "provenance belongs in the text, not just metadata")

	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	require.NotNil(t, latest, "a delivery must be recorded")
	assert.Equal(t, db.StandingOutcomeDelivered, latest.Outcome)
	assert.Equal(t, db.StandingTransportHookContext, latest.Transport)
	assert.Equal(t, harness.DefaultName, latest.Harness)
	assert.NotEmpty(t, latest.TargetAgent, "the ledger must carry the stable recipient")
}

func TestStandingOrderPromptRegexDeliversOnlyOnMatch(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerUserPrompt
		o.TriggerSources = nil
		o.MatchField = db.StandingMatchFieldPrompt
		o.MatchRegex = `(?i)\bdeploy\b`
	})

	input := HookCallbackInput{
		HookEventName: "UserPromptSubmit",
		ConvID:        "conv-1",
		Prompt:        "Run the tests first",
	}
	assert.Empty(t, dispatch(t, input))
	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	assert.Nil(t, latest, "clean matcher misses are intentionally not ledger noise")

	input.Prompt = "Please DEPLOY after the tests"
	got := additionalContextForEvent(t, dispatch(t, input), "UserPromptSubmit")
	assert.Contains(t, got, "Push the PR early")
}

func TestStandingOrderToolInputRegexUsesCompactJSON(t *testing.T) {
	groupID := standingOrderFixture(t, harness.CodexName)
	insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerToolBefore
		o.TriggerSources = nil
		o.MatchField = db.StandingMatchFieldToolInput
		o.MatchRegex = `"command":"git push"`
	})

	input := HookCallbackInput{
		HookEventName: "PreToolUse",
		ConvID:        "conv-1",
		ToolName:      "shell",
		ToolInput:     json.RawMessage(`{ "command": "git push", "timeout": 30 }`),
	}
	got := additionalContextForEvent(t, dispatch(t, input), "PreToolUse")
	assert.Contains(t, got, "Push the PR early")
}

func TestStandingOrderOriginSuppressesEveryDeliveryPath(t *testing.T) {
	groupID := standingOrderFixture(t, harness.OpenCodeName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerToolBefore
		o.TriggerSources = nil
		o.Timing = db.StandingTimingNextTurn
	})
	input := HookCallbackInput{
		HookEventName:       "PreToolUse",
		ConvID:              "conv-1",
		ToolName:            "Bash",
		ToolInput:           json.RawMessage(`{"command":"go test ./..."}`),
		StandingOrderOrigin: true,
	}

	assert.Empty(t, dispatch(t, input),
		"an internal reminder turn must not receive hook-context orders")
	assert.Empty(t, observe(input),
		"an internal reminder turn must not queue another reminder")
	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	assert.Nil(t, latest,
		"origin suppression is not a cadence outcome and must not consume delivery state")

	input.StandingOrderOrigin = false
	assert.Len(t, observe(input), 1,
		"the same tool event remains deliverable in an ordinary user turn")
}

func TestStandingOrderToolNameNormalizesCodexShellToBash(t *testing.T) {
	groupID := standingOrderFixture(t, harness.CodexName)
	insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerToolBefore
		o.TriggerSources = nil
		o.MatchField = db.StandingMatchFieldToolName
		o.MatchRegex = `^Bash$`
	})

	got := additionalContextForEvent(t, dispatch(t, HookCallbackInput{
		HookEventName: "PreToolUse",
		ConvID:        "conv-1",
		ToolName:      "shell",
	}), "PreToolUse")
	assert.Contains(t, got, "Push the PR early")
}

func TestStandingOrderTrimmedToolInputRecordsUnevaluable(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerToolBefore
		o.TriggerSources = nil
		o.MatchField = db.StandingMatchFieldToolInput
		o.MatchRegex = "deploy"
	})

	out := dispatch(t, HookCallbackInput{
		HookEventName:  "PreToolUse",
		ConvID:         "conv-1",
		ToolName:       "Bash",
		PayloadTrimmed: true,
	})
	assert.Empty(t, out)

	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeNotEvaluatedTrimmed, latest.Outcome)
}

func TestStandingOrderOpenCodeToolTriggerQueuesNextTurn(t *testing.T) {
	groupID := standingOrderFixture(t, harness.OpenCodeName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerToolBefore
		o.TriggerSources = nil
		o.MatchField = db.StandingMatchFieldToolName
		o.MatchRegex = `(?i)^bash$`
		o.Timing = db.StandingTimingNextTurn
	})

	pending := observe(HookCallbackInput{
		HookEventName: "PreToolUse",
		ConvID:        "conv-1",
		ToolName:      "Bash",
	})
	require.Len(t, pending, 1)
	assert.Equal(t, id, pending[0].OrderID)

	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	assert.Nil(t, latest,
		"the observation path must not claim delivery before the message is queued")

	RecordStandingMessageDelivery(pending[0], nil)
	latest, err = db.LatestStandingDelivery(id)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeDelivered, latest.Outcome)
	assert.Equal(t, db.StandingTransportMessage, latest.Transport)
}

func TestStandingOrderDebounceDefersAndMovesTrailingEdge(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Timing = db.StandingTimingNextTurn
		o.DebounceSeconds = 5
	})
	agentID, err := db.AgentIDForConv("conv-1")
	require.NoError(t, err)
	require.NotEmpty(t, agentID)

	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceCompact)),
		"debounce must not block the hook or emit inline context")
	first, err := db.GetDueStandingDebounce(id, agentID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, "conv-1", first.TargetConv)
	assert.Equal(t, harness.DefaultName, first.Harness)

	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceCompact)))
	second, err := db.GetDueStandingDebounce(id, agentID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.False(t, second.DueAt.Before(first.DueAt),
		"a later match moves the trailing edge later")
	assert.Equal(t, first.MaxDueAt, second.MaxDueAt,
		"continuous matches cannot postpone delivery forever")

	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	assert.Nil(t, latest,
		"individual burst events do not create ledger noise or claim delivery")
}

func TestStandingOrderLongDebounceWindowRemainsSchedulable(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Timing = db.StandingTimingNextTurn
		o.DebounceSeconds = 2 * 60 * 60
	})
	agentID, err := db.AgentIDForConv("conv-1")
	require.NoError(t, err)

	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceCompact)))
	pending, err := db.GetDueStandingDebounce(
		id, agentID, time.Now().Add(3*time.Hour))
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, pending.DueAt, pending.MaxDueAt,
		"the starvation bound can never precede an authored quiet window")
}

func TestStandingOrderSourceFilterSkipsUnselectedBoundary(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID)

	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceResume)),
		"a source the order did not select must write nothing at all")

	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	assert.Nil(t, latest, "a clean no-match must not fill the ledger")
}

// An empty source is a cold start; normalizing it keeps the operator's
// "startup" filter meaning one thing.
func TestStandingOrderEmptySourceTreatedAsStartup(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	insertOrder(t, groupID)

	got := additionalContext(t, dispatch(t, sessionStart("")))
	assert.Contains(t, got, "Push the PR early")
}

func TestStandingOrderNotDeliveredToNonMember(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	insertOrder(t, groupID, func(o *db.StandingOrder) { o.GroupID = groupID + 999 })

	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceCompact)))
}

func TestStandingOrderRoleFilter(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	insertOrder(t, groupID, func(o *db.StandingOrder) { o.TargetRole = "reviewer" })

	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceCompact)),
		"the member holds role 'worker', so a reviewer-filtered order must not reach it")
}

// An order requiring same-continuation on a harness that has no such channel
// delivers NOTHING, and says so in the ledger rather than downgrading quietly.
//
// SCOPE OF THIS TEST: it drives DispatchHookEvent, which proves the evaluator
// and the capability model. The real OpenCode path does not reach
// DispatchHookEvent — its SSE projector calls ApplyHook — so the integration
// itself is covered separately by TestObserveStandingOrdersSplitsByRequiredTiming
// below, which exercises the observation-only entry point OpenCode actually
// uses. Both are needed: this one pins the decision, that one pins the wiring.
func TestStandingOrderUnsupportedTimingIsRecordedNotDowngraded(t *testing.T) {
	groupID := standingOrderFixture(t, harness.OpenCodeName)
	id := insertOrder(t, groupID)

	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceCompact)))

	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	require.NotNil(t, latest, "an undeliverable order must still be explained")
	assert.Equal(t, db.StandingOutcomeUnsupportedTiming, latest.Outcome)
	assert.Equal(t, db.StandingTransportNone, latest.Transport)
}

func TestStandingOrderDisabledDeliversNothing(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID)
	order, err := db.GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, order)
	require.NoError(t, db.SetStandingOrderEnabled(
		id, false, order.RowVersion))

	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceCompact)))
}

// Cadence must survive the round trip through the ledger: the second boundary
// in the same conversation is suppressed, and editing the text re-arms it.
func TestStandingOrderCadenceOncePerGenerationAndRevisionRearm(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Cadence = db.StandingCadenceOncePerGeneration
	})

	assert.Contains(t, dispatch(t, sessionStart(db.StandingSourceCompact)), "Push the PR early")
	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceCompact)),
		"a second boundary in the same generation must be suppressed")

	order, err := db.GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, order)
	order.Summary = "Push the PR early, then request a cold review."
	require.NoError(t, db.UpdateStandingOrder(id, order.RowVersion, order))

	got := additionalContext(t, dispatch(t, sessionStart(db.StandingSourceCompact)))
	assert.Contains(t, got, "cold review", "an edited order must reach the agents the edit was for")
	assert.Contains(t, got, "pr-early@2")
}

func TestStandingOrderCooldownSuppressesRapidBoundaryForStableAgent(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.CooldownSeconds = 60
	})

	assert.Contains(t, dispatch(t, sessionStart(db.StandingSourceCompact)), "Push the PR early")
	assert.Empty(t, dispatch(t, sessionStart(db.StandingSourceCompact)),
		"a boundary inside the minimum interval must not repeat the order")

	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeSuppressedCooldown, latest.Outcome)
	assert.NotEmpty(t, latest.TargetAgent)
}

// Subagent inheritance is deliberately out of v1: an in-harness subagent
// shares the main conv-id, so without this guard every short-lived Explore
// agent would be handed orders it will never act on.
func TestStandingOrderSkipsInHarnessSubagents(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	insertOrder(t, groupID)

	input := sessionStart(db.StandingSourceStartup)
	input.AgentID = "sub-1"

	assert.Empty(t, dispatch(t, input))
}

// Events with no standing-order trigger must not change what the hook writes.
func TestStandingOrderIgnoresUntriggeredEvents(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	insertOrder(t, groupID)

	out := dispatch(t, HookCallbackInput{
		HookEventName: "PreToolUse",
		ConvID:        "conv-1",
		ToolName:      "Bash",
	})
	assert.Empty(t, out)
}

// Multiple matching orders arrive as one document, in a stable order.
func TestStandingOrderMultipleOrdersRenderTogether(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	insertOrder(t, groupID)
	insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Name = "worktrees"
		o.Summary = "Use a git worktree for feature work."
	})

	got := additionalContext(t, dispatch(t, sessionStart(db.StandingSourceStartup)))

	assert.Contains(t, got, "Push the PR early")
	assert.Contains(t, got, "Use a git worktree")
	assert.Less(t, indexOf(got, "Push the PR early"), indexOf(got, "Use a git worktree"),
		"orders render in insertion order so the document is stable")
}

func indexOf(haystack, needle string) int {
	return bytes.Index([]byte(haystack), []byte(needle))
}

// The observation-only path (OpenCode) must never silently do nothing. An
// order requiring same-continuation is recorded as unsupported and returns no
// pending message; one asking for next-turn comes back for the caller to send.
func TestObserveStandingOrdersSplitsByRequiredTiming(t *testing.T) {
	groupID := standingOrderFixture(t, harness.OpenCodeName)
	sameID := insertOrder(t, groupID)

	pending := observe(sessionStart(db.StandingSourceCompact))
	assert.Empty(t, pending, "a same-continuation order is not satisfiable by a message")

	latest, err := db.LatestStandingDelivery(sameID)
	require.NoError(t, err)
	require.NotNil(t, latest, "the degradation must be visible, not silent")
	assert.Equal(t, db.StandingOutcomeUnsupportedTiming, latest.Outcome)
}

func TestObserveStandingOrdersReturnsNextTurnOrdersForMessageDelivery(t *testing.T) {
	groupID := standingOrderFixture(t, harness.OpenCodeName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Timing = db.StandingTimingNextTurn
	})

	pending := observe(sessionStart(db.StandingSourceCompact))
	require.Len(t, pending, 1)
	assert.Equal(t, "pr-early", pending[0].Name)
	assert.Contains(t, pending[0].Body, "Push the PR early")
	assert.Equal(t, "conv-1", pending[0].TargetConv)

	// Nothing is recorded until the caller reports the send outcome, so a
	// crash between the two cannot leave a delivery claimed that never went.
	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	assert.Nil(t, latest)

	RecordStandingMessageDelivery(pending[0], nil)
	latest, err = db.LatestStandingDelivery(id)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeDelivered, latest.Outcome)
	assert.Equal(t, db.StandingTransportMessage, latest.Transport)
}

func TestHarnessHookSelectorUsesInlineContextWhenThatExactEventSupportsIt(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerHookEvent
		o.TriggerSources = nil
		o.HookSelectors = []hookevents.Selector{{
			Harness: hookevents.HarnessClaude,
			Event:   "PreToolUse",
		}}
	})

	got := additionalContextForEvent(t, dispatch(t, HookCallbackInput{
		HookEventName: "PreToolUse",
		ConvID:        "conv-1",
		ToolName:      "Bash",
	}), "PreToolUse")
	assert.Contains(t, got, "Push the PR early")
}

func TestHarnessHookSelectorQueuesDirectCallbackWhenEventHasNoInlineContext(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	orderID := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerHookEvent
		o.TriggerSources = nil
		o.Timing = db.StandingTimingNextTurn
		o.HookSelectors = []hookevents.Selector{{
			Harness: hookevents.HarnessClaude,
			Event:   "PostCompact",
		}}
	})

	assert.Empty(t, dispatch(t, HookCallbackInput{
		HookEventName: "PostCompact",
		ConvID:        "conv-1",
	}), "message transport must not emit an unsupported hook response")

	messages, err := db.ListAgentMessagesForConv("conv-1", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "[standing-order:pr-early]", messages[0].Subject)
	origin, err := db.AgentMessageStandingOrderOrigin(messages[0].ID)
	require.NoError(t, err)
	require.NotNil(t, origin)
	assert.Equal(t, orderID, origin.OrderID)

	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeDelivered, latest.Outcome)
	assert.Equal(t, db.StandingTransportMessage, latest.Transport)
}

func TestHarnessHookSelectorQueuesFromPreCompactGatePath(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	orderID := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerHookEvent
		o.TriggerSources = nil
		o.Timing = db.StandingTimingNextTurn
		o.HookSelectors = []hookevents.Selector{{
			Harness: hookevents.HarnessClaude,
			Event:   "PreCompact",
		}}
	})

	assert.Empty(t, dispatch(t, HookCallbackInput{
		HookEventName: "PreCompact",
		ConvID:        "conv-1",
		Trigger:       "manual",
	}))
	messages, err := db.ListAgentMessagesForConv("conv-1", 10)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "[standing-order:pr-early]", messages[0].Subject)
	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingTransportMessage, latest.Transport)
}

// A failed send must not satisfy the cadence, or the next boundary would treat
// an undelivered order as already done.
func TestRecordStandingMessageDeliveryFailureLeavesCadenceOpen(t *testing.T) {
	groupID := standingOrderFixture(t, harness.OpenCodeName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Timing = db.StandingTimingNextTurn
		o.Cadence = db.StandingCadenceOncePerGeneration
	})

	pending := observe(sessionStart(db.StandingSourceCompact))
	require.Len(t, pending, 1)
	RecordStandingMessageDelivery(pending[0], errors.New("queue full"))

	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, db.StandingOutcomeDeliveryFailed, latest.Outcome)

	already, err := db.StandingOrderDeliveredInEpoch(id, 1, "conv-1", "conv-1")
	require.NoError(t, err)
	assert.False(t, already, "a failed send must remain retryable")

	again := observe(sessionStart(db.StandingSourceCompact))
	assert.Len(t, again, 1, "the next boundary retries")
}

// A failed write must NOT burn a once-per-generation cadence slot. Recording
// before the write would mark the order delivered for a conversation that
// never saw the text, leaving it permanently silent with a ledger that
// disagrees — unrecoverable and invisible. Repeating a reminder is the far
// cheaper failure.
func TestStandingOrderFailedWriteDoesNotBurnCadence(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Cadence = db.StandingCadenceOncePerGeneration
	})

	err := DispatchHookEvent(context.Background(), sessionStart(db.StandingSourceCompact),
		"sess-1", LocalHookAmbient(), failingWriter{})
	require.Error(t, err, "a write failure must surface")

	already, err := db.StandingOrderDeliveredInEpoch(id, 1, "conv-1", "conv-1")
	require.NoError(t, err)
	assert.False(t, already, "an undelivered order must remain deliverable")

	// The next boundary really does retry, end to end.
	assert.Contains(t, dispatch(t, sessionStart(db.StandingSourceCompact)), "Push the PR early")
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// An agent-authored order must not be delivered as if the human wrote it.
func TestObserveStandingOrdersCarriesRealAuthorship(t *testing.T) {
	groupID := standingOrderFixture(t, harness.OpenCodeName)
	// The owner must be a real enrolled actor: the read path drops an order
	// whose owner cannot be resolved as live, exactly as cron's does.
	ownerAgent, _, err := db.EnsureAgentForConv("conv-author", "test")
	require.NoError(t, err)
	insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Timing = db.StandingTimingNextTurn
		o.OperatorAuthored = false
		o.OwnerAgent = ownerAgent
	})

	pending := observe(sessionStart(db.StandingSourceCompact))
	require.Len(t, pending, 1)
	assert.False(t, pending[0].OperatorAuthored,
		"an agent-authored order must not be stamped operator-authored")
	assert.Equal(t, "conv-author", pending[0].OwnerConv,
		"the message must be attributed to the authoring agent's live generation")
	assert.Contains(t, pending[0].Body, "authored by agent "+ownerAgent)
}

func TestObserveStandingOrdersOperatorOrderStaysOperatorAuthored(t *testing.T) {
	groupID := standingOrderFixture(t, harness.OpenCodeName)
	insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Timing = db.StandingTimingNextTurn
		o.OperatorAuthored = true
	})

	pending := observe(sessionStart(db.StandingSourceCompact))
	require.Len(t, pending, 1)
	assert.True(t, pending[0].OperatorAuthored)
}

// An event naming a conversation the caller was NOT resolved as must be
// refused outright.
//
// applyHook returns nil both when it applies an event and when its
// foreign-process guard drops one, so a payload naming somebody else's conv
// still reaches the standing-order path. Trusting it would leak the orders
// targeting that agent to whoever named it, and — worse — write a `delivered`
// ledger row that consumes the victim's once-per-generation slot, silencing
// the real agent behind a ledger claiming success.
func TestStandingOrderRefusesForeignConvInPayload(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	id := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Cadence = db.StandingCadenceOncePerGeneration
	})

	// A second session that is not in the target group, naming conv-1.
	require.NoError(t, SaveSessionState(&SessionState{
		ID: "sess-2", ConvID: "conv-2", Status: StatusIdle, Harness: harness.DefaultName,
	}))

	var buf bytes.Buffer
	require.NoError(t, DispatchHookEvent(context.Background(),
		sessionStart(db.StandingSourceStartup), "sess-2", LocalHookAmbient(), &buf))

	assert.Empty(t, buf.String(), "an order must not leak to an agent that merely named the conv")

	latest, err := db.LatestStandingDelivery(id)
	require.NoError(t, err)
	assert.Nil(t, latest, "a foreign event must not consume the victim's cadence slot")

	// The real agent still receives it.
	assert.Contains(t, dispatch(t, sessionStart(db.StandingSourceStartup)), "Push the PR early")
}

// Without a resolvable session row there is no authority for which
// conversation an event is about, so evaluation is refused rather than
// falling back to the payload.
func TestStandingOrderRefusesUnresolvableSession(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	insertOrder(t, groupID)

	var buf bytes.Buffer
	require.NoError(t, DispatchHookEvent(context.Background(),
		sessionStart(db.StandingSourceStartup), "", LocalHookAmbient(), &buf))
	assert.Empty(t, buf.String())
}

func TestStandingOrderGlobalRefusesRetiredRunningAgent(t *testing.T) {
	standingOrderFixture(t, harness.DefaultName)
	insertOrder(t, 0, func(o *db.StandingOrder) {
		o.TargetKind = db.StandingTargetGlobal
		o.GroupID = 0
	})

	retired, err := db.RetireAgentAuthorizationByConv(
		"conv-1", "human", "test keeps the session running")
	require.NoError(t, err)
	require.True(t, retired.Retired)

	var buf bytes.Buffer
	require.NoError(t, DispatchHookEvent(context.Background(),
		sessionStart(db.StandingSourceStartup), "sess-1", LocalHookAmbient(), &buf))
	assert.Empty(t, buf.String(),
		"a retired actor kept alive with shutdown=0 is outside global scope")
}

func TestStandingOrderGlobalRefusesStalePredecessorGeneration(t *testing.T) {
	standingOrderFixture(t, harness.DefaultName)
	orderID := insertOrder(t, 0, func(o *db.StandingOrder) {
		o.TargetKind = db.StandingTargetGlobal
		o.GroupID = 0
		o.Cadence = db.StandingCadenceOncePerGeneration
	})

	_, err := db.RotateAgentConv("conv-1", "conv-2", "test rotation")
	require.NoError(t, err)

	var buf bytes.Buffer
	require.NoError(t, DispatchHookEvent(context.Background(),
		sessionStart(db.StandingSourceStartup), "sess-1", LocalHookAmbient(), &buf))
	assert.Empty(t, buf.String(),
		"a still-running predecessor is not a current routing generation")

	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	assert.Nil(t, latest,
		"a stale generation must not consume the active agent's cadence slot")
}
