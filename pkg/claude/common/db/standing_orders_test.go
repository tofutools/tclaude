package db

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sampleOrder(name string) *StandingOrder {
	return &StandingOrder{
		Name:           name,
		TargetKind:     StandingTargetGroup,
		GroupID:        1,
		Summary:        "Push the PR early.",
		TriggerEvent:   StandingTriggerSessionStart,
		TriggerSources: []string{StandingSourceCompact, StandingSourceStartup},
		Timing:         StandingTimingSameContinuation,
		Cadence:        StandingCadenceAlways,
		Enabled:        true,
	}
}

func TestStandingOrder_InsertAndRead(t *testing.T) {
	setupTestDB(t)

	id, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)
	require.NotZero(t, id)

	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "pr-early", got.Name)
	assert.Equal(t, int64(1), got.Revision)
	assert.True(t, got.Enabled)
	// Sources round-trip canonically (sorted, de-duplicated).
	assert.Equal(t, []string{StandingSourceCompact, StandingSourceStartup}, got.TriggerSources)

	byName, err := GetStandingOrderByName("pr-early")
	require.NoError(t, err)
	require.NotNil(t, byName)
	assert.Equal(t, id, byName.ID)

	missing, err := GetStandingOrder(id + 999)
	require.NoError(t, err)
	assert.Nil(t, missing, "absent order should read as (nil, nil)")
}

func TestStandingOrder_NameIsUnique(t *testing.T) {
	setupTestDB(t)

	_, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)

	_, err = InsertStandingOrder(sampleOrder("pr-early"))
	assert.True(t, errors.Is(err, ErrStandingOrderNameTaken), "got %v", err)
}

func TestStandingOrder_ValidateRejectsBadInput(t *testing.T) {
	cases := map[string]func(*StandingOrder){
		"no name":          func(o *StandingOrder) { o.Name = "" },
		"no summary":       func(o *StandingOrder) { o.Summary = "" },
		"bad target kind":  func(o *StandingOrder) { o.TargetKind = "everyone" },
		"group without id": func(o *StandingOrder) { o.GroupID = 0 },
		"unknown trigger":  func(o *StandingOrder) { o.TriggerEvent = "tool.before" },
		"unknown source":   func(o *StandingOrder) { o.TriggerSources = []string{"whenever"} },
		"unknown timing":   func(o *StandingOrder) { o.Timing = "immediately" },
		"unknown cadence":  func(o *StandingOrder) { o.Cadence = "hourly" },
		"role on conv target": func(o *StandingOrder) {
			o.TargetKind = StandingTargetConv
			o.GroupID = 0
			o.TargetConv = "conv-a"
			o.TargetRole = "reviewer"
		},
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			o := sampleOrder("x")
			mut(o)
			err := o.Validate()
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrStandingOrderInvalid), "got %v", err)
		})
	}
}

// The cap exists because the text is re-injected at every boundary; an
// unbounded order would recreate the context bloat the feature is meant to fix.
func TestStandingOrder_SummaryLengthCapped(t *testing.T) {
	setupTestDB(t)
	o := sampleOrder("long")
	o.Summary = strings.Repeat("x", StandingSummaryMaxLen+1)

	_, err := InsertStandingOrder(o)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrStandingOrderInvalid))
}

// Editing the text must re-arm a once-per-generation order: leaving recipients
// pinned to "already delivered" would withhold the new wording from exactly
// the agents the edit was written for.
func TestStandingOrder_TextEditBumpsRevision(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)

	require.NoError(t, UpdateStandingOrderText(id, "Push the PR early, then trigger a cold review."))

	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Revision)
	assert.Contains(t, got.Summary, "cold review")
}

func TestStandingOrder_EnableDisableClearsReason(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)

	n, err := DisableGroupTargetStandingOrdersForRetire(1)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	got, _ := GetStandingOrder(id)
	assert.False(t, got.Enabled)
	assert.Equal(t, StandingDisabledReasonGroupRetired, got.DisabledReason)

	require.NoError(t, SetStandingOrderEnabled(id, true))
	got, _ = GetStandingOrder(id)
	assert.True(t, got.Enabled)
	assert.Empty(t, got.DisabledReason, "an explicit enable is a human choice, not an auto-pause")
}

// Only orders tclaude itself paused come back on a resume — one the human
// disabled by hand must stay disabled.
func TestStandingOrder_GroupResumeSkipsHandDisabledOrders(t *testing.T) {
	setupTestDB(t)
	autoID, err := InsertStandingOrder(sampleOrder("auto"))
	require.NoError(t, err)
	handID, err := InsertStandingOrder(sampleOrder("hand"))
	require.NoError(t, err)

	require.NoError(t, SetStandingOrderEnabled(handID, false))

	n, err := DisableGroupTargetStandingOrdersForRetire(1)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the still-enabled order is auto-paused")

	n, err = ReenableGroupRetiredStandingOrders(1)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	auto, _ := GetStandingOrder(autoID)
	hand, _ := GetStandingOrder(handID)
	assert.True(t, auto.Enabled)
	assert.False(t, hand.Enabled, "a hand-disabled order must not be silently re-enabled")
}

func TestStandingOrder_ListEnabledForEvent(t *testing.T) {
	setupTestDB(t)
	_, err := InsertStandingOrder(sampleOrder("on"))
	require.NoError(t, err)
	offID, err := InsertStandingOrder(sampleOrder("off"))
	require.NoError(t, err)
	require.NoError(t, SetStandingOrderEnabled(offID, false))

	got, err := ListEnabledStandingOrdersForEvent(StandingTriggerSessionStart)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "on", got[0].Name)

	none, err := ListEnabledStandingOrdersForEvent("tool.before")
	require.NoError(t, err)
	assert.Empty(t, none)
}

func TestStandingOrder_DeliveryLedgerAndCadence(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)

	already, err := StandingOrderDeliveredInEpoch(id, 1, "conv-a", "conv-a")
	require.NoError(t, err)
	assert.False(t, already)

	_, err = RecordStandingDelivery(&StandingDelivery{
		OrderID: id, OrderRevision: 1, TargetConv: "conv-a", Epoch: "conv-a",
		Outcome: StandingOutcomeDelivered, Transport: StandingTransportHookContext,
		Harness: "claude", Detail: "session.start(source=compact)",
	})
	require.NoError(t, err)

	already, err = StandingOrderDeliveredInEpoch(id, 1, "conv-a", "conv-a")
	require.NoError(t, err)
	assert.True(t, already)

	// A newer revision is a fresh cadence slot — that is what re-arms an
	// edited order.
	already, err = StandingOrderDeliveredInEpoch(id, 2, "conv-a", "conv-a")
	require.NoError(t, err)
	assert.False(t, already)

	latest, err := LatestStandingDelivery(id)
	require.NoError(t, err)
	require.NotNil(t, latest)
	assert.Equal(t, StandingOutcomeDelivered, latest.Outcome)
	assert.False(t, latest.CreatedAt.IsZero())
}

// An evaluation that never put text in front of the agent must not suppress
// the next attempt.
func TestStandingOrder_FailedOutcomeDoesNotSuppressCadence(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)

	_, err = RecordStandingDelivery(&StandingDelivery{
		OrderID: id, OrderRevision: 1, TargetConv: "conv-a", Epoch: "conv-a",
		Outcome: StandingOutcomeUnsupportedTiming, Transport: StandingTransportNone,
	})
	require.NoError(t, err)

	already, err := StandingOrderDeliveredInEpoch(id, 1, "conv-a", "conv-a")
	require.NoError(t, err)
	assert.False(t, already, "an undelivered order must remain deliverable")
}

func TestStandingOrder_DeleteRemovesLedger(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)
	_, err = RecordStandingDelivery(&StandingDelivery{
		OrderID: id, OrderRevision: 1, TargetConv: "conv-a",
		Outcome: StandingOutcomeDelivered,
	})
	require.NoError(t, err)

	require.NoError(t, DeleteStandingOrder(id))

	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	assert.Nil(t, got)

	recs, err := ListStandingDeliveries(id, 10)
	require.NoError(t, err)
	assert.Empty(t, recs)
}

func TestStandingOrder_TriggerLabelAndSourceMatching(t *testing.T) {
	o := sampleOrder("pr-early")
	require.NoError(t, o.Validate())
	assert.Equal(t, "session.start (compact, startup)", o.TriggerLabel())
	assert.True(t, o.MatchesSource(StandingSourceCompact))
	assert.False(t, o.MatchesSource(StandingSourceResume))

	o.TriggerSources = nil
	assert.Equal(t, "session.start (any source)", o.TriggerLabel())
	assert.True(t, o.MatchesSource(StandingSourceResume), "no sources means every source")
}

func TestNormalizeTriggerSources(t *testing.T) {
	got := NormalizeTriggerSources([]string{" Compact ", "startup", "compact", ""})
	assert.Equal(t, []string{"compact", "startup"}, got)
}
