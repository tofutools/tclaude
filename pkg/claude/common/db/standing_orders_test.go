package db

import (
	"errors"
	"strings"
	"testing"
	"time"

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

func setStandingOrderEnabledForTest(t *testing.T, id int64, enabled bool) {
	t.Helper()
	current, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, current)
	require.NoError(t, SetStandingOrderEnabled(
		id, enabled, current.RowVersion))
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
	assert.Equal(t, int64(1), got.RowVersion)
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
		"unknown trigger":  func(o *StandingOrder) { o.TriggerEvent = "no.such.event" },
		"unknown source":   func(o *StandingOrder) { o.TriggerSources = []string{"whenever"} },
		"source on prompt trigger": func(o *StandingOrder) {
			o.TriggerEvent = StandingTriggerUserPrompt
		},
		"matcher field without regex": func(o *StandingOrder) {
			o.MatchField = StandingMatchFieldCwd
		},
		"matcher regex without field": func(o *StandingOrder) {
			o.MatchRegex = "repo"
		},
		"matcher field invalid for event": func(o *StandingOrder) {
			o.MatchField = StandingMatchFieldPrompt
			o.MatchRegex = "deploy"
		},
		"invalid matcher regex": func(o *StandingOrder) {
			o.MatchField = StandingMatchFieldCwd
			o.MatchRegex = "(unterminated"
		},
		"excessive matcher regex": func(o *StandingOrder) {
			o.MatchField = StandingMatchFieldCwd
			o.MatchRegex = strings.Repeat("x", StandingMatchRegexMaxLen+1)
		},
		"unknown timing":    func(o *StandingOrder) { o.Timing = "immediately" },
		"unknown cadence":   func(o *StandingOrder) { o.Cadence = "hourly" },
		"negative cooldown": func(o *StandingOrder) { o.CooldownSeconds = -1 },
		"excessive cooldown": func(o *StandingOrder) {
			o.CooldownSeconds = StandingCooldownMaxSeconds + 1
		},
		"negative debounce": func(o *StandingOrder) {
			o.DebounceSeconds = -1
		},
		"excessive debounce": func(o *StandingOrder) {
			o.DebounceSeconds = StandingDebounceMaxSeconds + 1
		},
		"debounce on same continuation": func(o *StandingOrder) {
			o.DebounceSeconds = 5
		},
		"role on conv target": func(o *StandingOrder) {
			o.TargetKind = StandingTargetConv
			o.GroupID = 0
			o.TargetAgent = "agt_aaa"
			o.TargetRole = "reviewer"
		},
		"single target without stable agent": func(o *StandingOrder) {
			o.TargetKind = StandingTargetConv
			o.GroupID = 0
			o.TargetConv = "conv-a"
		},
		"single target with non-agent id": func(o *StandingOrder) {
			o.TargetKind = StandingTargetConv
			o.GroupID = 0
			o.TargetAgent = "conv-a"
		},
		"owner conv without stable agent": func(o *StandingOrder) {
			o.OwnerConv = "conv-owner"
		},
		"group with single target agent": func(o *StandingOrder) {
			o.TargetAgent = "agt_aaa"
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

func TestStandingOrder_SingleTargetFollowsStableAgentAcrossRotation(t *testing.T) {
	setupTestDB(t)

	agentID, _, err := EnsureAgentForConv("conv-old", "test")
	require.NoError(t, err)
	o := sampleOrder("stable-target")
	o.TargetKind = StandingTargetConv
	o.GroupID = 0
	o.TargetAgent = agentID

	id, err := InsertStandingOrder(o)
	require.NoError(t, err)

	before, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, before)
	assert.Equal(t, agentID, before.TargetAgent)
	assert.Equal(t, "conv-old", before.TargetConv)

	_, err = RotateAgentConv("conv-old", "conv-new", "reincarnate")
	require.NoError(t, err)

	after, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, agentID, after.TargetAgent, "the durable target never changes")
	assert.Equal(t, "conv-new", after.TargetConv, "routing follows the agent's current generation")
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

	current, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, current)
	current.Summary = "Push the PR early, then trigger a cold review."
	require.NoError(t, UpdateStandingOrder(id, current.RowVersion, current))

	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	assert.Equal(t, int64(2), got.Revision)
	assert.Equal(t, int64(2), got.RowVersion)
	assert.Contains(t, got.Summary, "cold review")
}

func TestStandingOrder_FullUpdateUsesRowVersionCAS(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("before"))
	require.NoError(t, err)
	before, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, before)

	replacement := sampleOrder("after")
	replacement.Summary = "Updated instruction."
	replacement.TriggerSources = []string{StandingSourceResume}
	replacement.MatchField = StandingMatchFieldCwd
	replacement.MatchRegex = `/deploy$`
	replacement.Timing = StandingTimingNextTurn
	replacement.Cadence = StandingCadenceOncePerGeneration
	replacement.CooldownSeconds = 90
	replacement.DebounceSeconds = 12
	replacement.Enabled = false
	replacement.DisabledReason = StandingDisabledReasonGroupRetired
	require.NoError(t, UpdateStandingOrder(id, before.RowVersion, replacement))

	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, int64(2), got.Revision)
	assert.Equal(t, int64(2), got.RowVersion)
	assert.Equal(t, "after", got.Name)
	assert.Equal(t, "Updated instruction.", got.Summary)
	assert.Equal(t, []string{StandingSourceResume}, got.TriggerSources)
	assert.Equal(t, StandingMatchFieldCwd, got.MatchField)
	assert.Equal(t, `/deploy$`, got.MatchRegex)
	assert.Equal(t, StandingTimingNextTurn, got.Timing)
	assert.Equal(t, StandingCadenceOncePerGeneration, got.Cadence)
	assert.Equal(t, int64(90), got.CooldownSeconds)
	assert.Equal(t, int64(12), got.DebounceSeconds)
	assert.False(t, got.Enabled)
	assert.Equal(t, StandingDisabledReasonGroupRetired, got.DisabledReason)

	stale := sampleOrder("stale-overwrite")
	err = UpdateStandingOrder(id, before.RowVersion, stale)
	assert.ErrorIs(t, err, ErrStandingOrderVersionConflict)
	got, _ = GetStandingOrder(id)
	assert.Equal(t, "after", got.Name, "a stale writer cannot overwrite the accepted edit")
}

func TestStandingOrder_AdministrativeEditDoesNotRearmDelivery(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("before"))
	require.NoError(t, err)
	before, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, before)

	replacement := *before
	replacement.Name = "renamed"
	replacement.GroupID = 2
	replacement.TargetRole = "reviewer"
	replacement.CooldownSeconds = 300
	require.NoError(t, UpdateStandingOrder(id, before.RowVersion, &replacement))

	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, before.Revision, got.Revision,
		"rename, retarget, and cooldown tuning preserve cadence/cooldown history")
	assert.Equal(t, before.RowVersion+1, got.RowVersion,
		"every accepted edit invalidates stale writers")
	assert.Equal(t, "renamed", got.Name)
	assert.Equal(t, int64(2), got.GroupID)
	assert.Equal(t, "reviewer", got.TargetRole)
	assert.Equal(t, int64(300), got.CooldownSeconds)
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
	assert.Equal(t, int64(1), got.Revision)
	assert.Equal(t, int64(2), got.RowVersion)

	setStandingOrderEnabledForTest(t, id, true)
	got, _ = GetStandingOrder(id)
	assert.True(t, got.Enabled)
	assert.Empty(t, got.DisabledReason, "an explicit enable is a human choice, not an auto-pause")
	assert.Equal(t, int64(2), got.Revision, "manual re-enable deliberately re-arms delivery")
	assert.Equal(t, int64(3), got.RowVersion)
}

func TestStandingOrder_EnableDisableNoOpIsIdempotent(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)
	before, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, before)

	require.NoError(t, SetStandingOrderEnabled(
		id, true, before.RowVersion))
	after, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, after)
	assert.Equal(t, before.Revision, after.Revision)
	assert.Equal(t, before.RowVersion, after.RowVersion)
	assert.Equal(t, before.UpdatedAt, after.UpdatedAt)
}

func TestStandingOrder_AutomaticLifecycleInvalidatesStaleWritersWithoutRearming(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)
	captured, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, captured)

	n, err := DisableGroupTargetStandingOrdersForRetire(1)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	retired, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, retired)
	assert.Equal(t, captured.Revision, retired.Revision,
		"automatic lifecycle changes remain neutral to the delivery cadence")
	assert.Equal(t, captured.RowVersion+1, retired.RowVersion,
		"automatic lifecycle changes invalidate stale writers")
	assert.NotEqual(t, captured.UpdatedAt, retired.UpdatedAt,
		"the audit timestamp still advances even though it is no longer the CAS token")

	replacement := sampleOrder("stale-edit")
	assert.ErrorIs(t,
		UpdateStandingOrder(id, captured.RowVersion, replacement),
		ErrStandingOrderVersionConflict)
	assert.ErrorIs(t,
		DeleteStandingOrder(id, captured.RowVersion),
		ErrStandingOrderVersionConflict)
}

// Only orders tclaude itself paused come back on a resume — one the human
// disabled by hand must stay disabled.
func TestStandingOrder_GroupResumeSkipsHandDisabledOrders(t *testing.T) {
	setupTestDB(t)
	autoID, err := InsertStandingOrder(sampleOrder("auto"))
	require.NoError(t, err)
	handID, err := InsertStandingOrder(sampleOrder("hand"))
	require.NoError(t, err)

	setStandingOrderEnabledForTest(t, handID, false)

	n, err := DisableGroupTargetStandingOrdersForRetire(1)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "only the still-enabled order is auto-paused")
	retiredAuto, err := GetStandingOrder(autoID)
	require.NoError(t, err)
	require.NotNil(t, retiredAuto)

	n, err = ReenableGroupRetiredStandingOrders(1)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	auto, _ := GetStandingOrder(autoID)
	hand, _ := GetStandingOrder(handID)
	assert.True(t, auto.Enabled)
	assert.Equal(t, retiredAuto.Revision, auto.Revision,
		"automatic resume does not re-arm delivery")
	assert.Equal(t, retiredAuto.RowVersion+1, auto.RowVersion,
		"automatic resume invalidates stale writers")
	assert.False(t, hand.Enabled, "a hand-disabled order must not be silently re-enabled")
}

func TestStandingOrder_ListEnabledForEvent(t *testing.T) {
	setupTestDB(t)
	_, err := InsertStandingOrder(sampleOrder("on"))
	require.NoError(t, err)
	offID, err := InsertStandingOrder(sampleOrder("off"))
	require.NoError(t, err)
	setStandingOrderEnabledForTest(t, offID, false)

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
		TargetAgent: "agt_aaa",
		Outcome:     StandingOutcomeDelivered, Transport: StandingTransportHookContext,
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
	assert.Equal(t, "agt_aaa", latest.TargetAgent)
	assert.False(t, latest.CreatedAt.IsZero())

	deliveredAt, err := LatestSuccessfulStandingDeliveryAt(id, 1, "agt_aaa")
	require.NoError(t, err)
	assert.WithinDuration(t, latest.CreatedAt, deliveredAt, time.Millisecond)

	otherAgent, err := LatestSuccessfulStandingDeliveryAt(id, 1, "agt_bbb")
	require.NoError(t, err)
	assert.True(t, otherAgent.IsZero(), "one stable recipient must not cool down another")

	_, err = LatestSuccessfulStandingDeliveryAt(id, 1, "")
	assert.ErrorIs(t, err, ErrStandingOrderInvalid,
		"an empty recipient must not become a shared cooldown bucket")
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

func TestStandingOrder_UnsupportedOutcomeIsDeduplicatedPerRecipientRevision(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)

	rec := &StandingDelivery{
		OrderID: id, OrderRevision: 1, TargetConv: "conv-a",
		TargetAgent: "agt_aaa", Outcome: StandingOutcomeUnsupportedTiming,
		Transport: StandingTransportNone, Harness: "opencode",
		Detail: "action hooks are observation-only",
	}
	_, err = RecordStandingDelivery(rec)
	require.NoError(t, err)
	_, err = RecordStandingDelivery(rec)
	require.NoError(t, err)

	got, err := ListStandingDeliveries(id, 10)
	require.NoError(t, err)
	assert.Len(t, got, 1,
		"unchanged high-frequency capability failures need one durable explanation")
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

	current, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, current)
	require.NoError(t, DeleteStandingOrder(id, current.RowVersion))

	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	assert.Nil(t, got)

	recs, err := ListStandingDeliveries(id, 10)
	require.NoError(t, err)
	assert.Empty(t, recs)
}

func TestStandingOrder_DeleteRowVersionRejectsStaleEditor(t *testing.T) {
	setupTestDB(t)
	id, err := InsertStandingOrder(sampleOrder("pr-early"))
	require.NoError(t, err)
	current, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, current)
	current.Summary = "New wording."
	require.NoError(t, UpdateStandingOrder(id, current.RowVersion, current))
	current, err = GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, current)

	assert.ErrorIs(t, DeleteStandingOrder(id, 1),
		ErrStandingOrderVersionConflict)
	got, err := GetStandingOrder(id)
	require.NoError(t, err)
	require.NotNil(t, got)

	require.NoError(t, DeleteStandingOrder(id, current.RowVersion))
	got, err = GetStandingOrder(id)
	require.NoError(t, err)
	assert.Nil(t, got)
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

	o.MatchField = StandingMatchFieldCwd
	o.MatchRegex = `(?i)/repo$`
	assert.Equal(t,
		`session.start (any source) where cwd matches /(?i)/repo$/`,
		o.TriggerLabel())

	prompt := sampleOrder("prompt")
	prompt.TriggerEvent = StandingTriggerUserPrompt
	prompt.TriggerSources = nil
	prompt.MatchField = StandingMatchFieldPrompt
	prompt.MatchRegex = `\bdeploy\b`
	require.NoError(t, prompt.Validate())
	assert.Equal(t,
		`user.prompt where prompt matches /\bdeploy\b/`,
		prompt.TriggerLabel())
}

func TestNormalizeTriggerSources(t *testing.T) {
	got := NormalizeTriggerSources([]string{" Compact ", "startup", "compact", ""})
	assert.Equal(t, []string{"compact", "startup"}, got)
}
