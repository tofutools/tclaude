package agent

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// seedGroupWithMember creates a group with one enrolled conversation in it and
// returns both, so an explain test evaluates against a real roster.
func seedGroupWithMember(t *testing.T) (groupID int64, convID string) {
	t.Helper()
	convID = "conv-explain"
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	groupID, err = db.CreateAgentGroup("tclaude", "")
	require.NoError(t, err)
	require.NoError(t, db.AddAgentGroupMember(&db.AgentGroupMember{
		GroupID: groupID, ConvID: convID, Role: "worker",
	}))
	return groupID, convID
}

func seedOrder(t *testing.T, mut ...func(*db.StandingOrder)) *db.StandingOrder {
	t.Helper()
	o := &db.StandingOrder{
		Name:             "pr-early",
		TargetKind:       db.StandingTargetGroup,
		GroupID:          1,
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
	stored, err := db.GetStandingOrder(id)
	require.NoError(t, err)
	return stored
}

func TestOrdersLsEmptyAndPopulated(t *testing.T) {
	setupTestDB(t)

	var out, errOut bytes.Buffer
	require.Equal(t, rcOK, runOrdersLs(&out, &errOut))
	assert.Contains(t, out.String(), "(no standing orders)")

	seedOrder(t)
	out.Reset()
	require.Equal(t, rcOK, runOrdersLs(&out, &errOut))
	assert.Contains(t, out.String(), "pr-early")
	assert.Contains(t, out.String(), "session.start")
	assert.Contains(t, out.String(), "same-continuation")
}

func TestOrdersShowRendersCapabilityMatrix(t *testing.T) {
	setupTestDB(t)
	o := seedOrder(t, func(o *db.StandingOrder) { o.CooldownSeconds = 90 })

	var out, errOut bytes.Buffer
	require.Equal(t, rcOK, runOrdersShow(&out, &errOut, o.Name))

	got := out.String()
	assert.Contains(t, got, "pr-early")
	assert.Contains(t, got, "Push the PR early")
	assert.Contains(t, got, "Author:    operator")
	assert.Contains(t, got, "Cooldown:  1m30s per stable recipient agent")
	// The matrix is the point: an operator must be able to see that this
	// order does not reach OpenCode at all before they rely on it.
	assert.Contains(t, got, harness.DefaultName)
	assert.Contains(t, got, harness.OpenCodeName)
	assert.Contains(t, got, "unsupported")
	assert.Contains(t, got, "(none recorded)")
}

func TestOrdersShowUnknownOrder(t *testing.T) {
	setupTestDB(t)

	var out, errOut bytes.Buffer
	assert.Equal(t, rcNotFound, runOrdersShow(&out, &errOut, "nope"))
	assert.Contains(t, errOut.String(), "no standing order")
}

// A numeric selector is tried as a NAME first, so an order someone called
// "12" stays addressable.
func TestOrdersResolveByNameBeforeID(t *testing.T) {
	setupTestDB(t)
	first := seedOrder(t)
	numeric := seedOrder(t, func(o *db.StandingOrder) { o.Name = "12" })

	var errOut bytes.Buffer
	got, rc := resolveOrder(&errOut, "12")
	require.Equal(t, rcOK, rc)
	assert.Equal(t, numeric.ID, got.ID, "the name must win over the id")

	got, rc = resolveOrder(&errOut, first.Name)
	require.Equal(t, rcOK, rc)
	assert.Equal(t, first.ID, got.ID)
}

func TestOrdersExplainMatchAndNoMatch(t *testing.T) {
	setupTestDB(t)
	groupID, convID := seedGroupWithMember(t)
	seedOrder(t, func(o *db.StandingOrder) { o.GroupID = groupID })

	var out, errOut bytes.Buffer
	rc := runOrdersExplain(&out, &errOut, &ordersExplainParams{
		Event: db.StandingTriggerSessionStart, Source: db.StandingSourceCompact,
		Conv: convID, Harness: harness.DefaultName,
	})
	require.Equal(t, rcOK, rc)
	got := out.String()
	assert.Contains(t, got, "dry run")
	assert.Contains(t, got, db.StandingOutcomeDelivered)
	assert.Contains(t, got, "Text that would be delivered")

	// A source the order did not select is a clean non-match, and explain
	// must say so rather than staying silent.
	out.Reset()
	rc = runOrdersExplain(&out, &errOut, &ordersExplainParams{
		Event: db.StandingTriggerSessionStart, Source: db.StandingSourceResume,
		Conv: convID, Harness: harness.DefaultName,
	})
	require.Equal(t, rcOK, rc)
	assert.Contains(t, out.String(), db.StandingOutcomeNoMatch)
	assert.Contains(t, out.String(), "Nothing would be delivered")
}

// The four states the ticket asked explain to distinguish, on one surface.
func TestOrdersExplainDistinguishesUnsupportedTimingFromNoMatch(t *testing.T) {
	setupTestDB(t)
	groupID, convID := seedGroupWithMember(t)
	seedOrder(t, func(o *db.StandingOrder) { o.GroupID = groupID })

	var out, errOut bytes.Buffer
	require.Equal(t, rcOK, runOrdersExplain(&out, &errOut, &ordersExplainParams{
		Event: db.StandingTriggerSessionStart, Source: db.StandingSourceCompact,
		Conv: convID, Harness: harness.OpenCodeName,
	}))

	got := out.String()
	assert.Contains(t, got, db.StandingOutcomeUnsupportedTiming)
	assert.NotContains(t, got, db.StandingOutcomeNoMatch,
		"an undeliverable order must not be reported as a non-match")
	assert.Contains(t, got, "Nothing would be delivered")
}

func TestOrdersExplainReportsTrimmedPayload(t *testing.T) {
	setupTestDB(t)
	groupID, convID := seedGroupWithMember(t)
	// A trigger that DOES read the tool payload, so trimming is decisive.
	seedOrder(t, func(o *db.StandingOrder) {
		o.GroupID = groupID
		o.TriggerEvent = db.StandingTriggerToolBefore
		o.TriggerSources = nil
		o.MatchField = db.StandingMatchFieldToolInput
		o.MatchRegex = "deploy"
	})

	var out, errOut bytes.Buffer
	require.Equal(t, rcOK, runOrdersExplain(&out, &errOut, &ordersExplainParams{
		Event: db.StandingTriggerToolBefore, Source: "", Conv: convID,
		Harness: harness.DefaultName, Trimmed: true,
	}))
	assert.Contains(t, out.String(), "payload trimmed",
		"the simulated condition must be echoed so the output is self-describing")
	assert.Contains(t, out.String(), db.StandingOutcomeNotEvaluatedTrimmed)
}

func TestOrdersExplainSuppliesRegexMatcherInputs(t *testing.T) {
	setupTestDB(t)
	groupID, convID := seedGroupWithMember(t)
	seedOrder(t, func(o *db.StandingOrder) {
		o.GroupID = groupID
		o.TriggerEvent = db.StandingTriggerUserPrompt
		o.TriggerSources = nil
		o.MatchField = db.StandingMatchFieldPrompt
		o.MatchRegex = `(?i)\bdeploy\b`
	})

	var out, errOut bytes.Buffer
	require.Equal(t, rcOK, runOrdersExplain(&out, &errOut, &ordersExplainParams{
		Event: db.StandingTriggerUserPrompt, Conv: convID,
		Harness: harness.DefaultName, Prompt: "Please DEPLOY now",
	}))
	assert.Contains(t, out.String(), db.StandingOutcomeDelivered)

	out.Reset()
	require.Equal(t, rcOK, runOrdersExplain(&out, &errOut, &ordersExplainParams{
		Event: db.StandingTriggerUserPrompt, Conv: convID,
		Harness: harness.DefaultName, Prompt: "Run tests",
	}))
	assert.Contains(t, out.String(), db.StandingOutcomeNoMatch)
}

func TestOrdersExplainNoOrders(t *testing.T) {
	setupTestDB(t)
	_, convID := seedGroupWithMember(t)

	var out, errOut bytes.Buffer
	require.Equal(t, rcOK, runOrdersExplain(&out, &errOut, &ordersExplainParams{
		Event: db.StandingTriggerSessionStart, Source: db.StandingSourceStartup,
		Conv: convID, Harness: harness.DefaultName,
	}))
	assert.Contains(t, out.String(), "(no standing orders)")
}
