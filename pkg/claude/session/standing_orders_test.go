package session

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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

func dispatch(t *testing.T, input HookCallbackInput) string {
	t.Helper()
	var buf bytes.Buffer
	require.NoError(t, DispatchHookEvent(context.Background(), input, "sess-1", LocalHookAmbient(), &buf))
	return buf.String()
}

func additionalContext(t *testing.T, out string) string {
	t.Helper()
	var doc struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &doc), "output was %q", out)
	assert.Equal(t, "SessionStart", doc.HookSpecificOutput.HookEventName)
	return doc.HookSpecificOutput.AdditionalContext
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
	require.NoError(t, db.SetStandingOrderEnabled(id, false))

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

	require.NoError(t, db.UpdateStandingOrderText(id, "Push the PR early, then request a cold review."))

	got := additionalContext(t, dispatch(t, sessionStart(db.StandingSourceCompact)))
	assert.Contains(t, got, "cold review", "an edited order must reach the agents the edit was for")
	assert.Contains(t, got, "pr-early@2")
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
