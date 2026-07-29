package standingorders

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// order builds a valid enabled group-target order, so each test can mutate the
// one field it is actually about.
func order(mut ...func(*db.StandingOrder)) *db.StandingOrder {
	o := &db.StandingOrder{
		ID:             1,
		Name:           "pr-early",
		Revision:       1,
		TargetKind:     db.StandingTargetGroup,
		GroupID:        7,
		Summary:        "Push the PR early.",
		TriggerEvent:   db.StandingTriggerSessionStart,
		TriggerSources: []string{db.StandingSourceCompact, db.StandingSourceStartup},
		Timing:         db.StandingTimingSameContinuation,
		Cadence:        db.StandingCadenceAlways,
		Enabled:        true,
	}
	for _, m := range mut {
		m(o)
	}
	return o
}

func event(mut ...func(*Event)) Event {
	ev := Event{
		Event:       db.StandingTriggerSessionStart,
		Source:      db.StandingSourceCompact,
		ConvID:      "conv-a",
		AgentID:     "agt_aaa",
		Harness:     harness.DefaultName,
		Memberships: []Membership{{GroupID: 7, Role: "worker"}},
	}
	for _, m := range mut {
		m(&ev)
	}
	return ev
}

func neverDelivered(int64, int64, string, string) (bool, error) { return false, nil }

func TestEvaluateDeliversOnMatchingBoundary(t *testing.T) {
	d := Evaluate(order(), event(), neverDelivered)

	assert.True(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeDelivered, d.Outcome)
	assert.Equal(t, db.StandingTransportHookContext, d.Capability.Transport)
	assert.True(t, d.ShouldRecord())
}

func TestEvaluateCorruptMatcherFailsClosed(t *testing.T) {
	tests := []struct {
		name  string
		field string
		regex string
	}{
		{name: "expression without field", regex: `.*`},
		{name: "field without expression", field: db.StandingMatchFieldCwd},
		{name: "unknown field matching empty", field: "unknown", regex: `^$`},
		{name: "field invalid for event", field: db.StandingMatchFieldPrompt, regex: `.*`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := Evaluate(order(func(o *db.StandingOrder) {
				o.MatchField = tt.field
				o.MatchRegex = tt.regex
			}), event(), neverDelivered)

			assert.False(t, d.Deliver)
			assert.Equal(t, db.StandingOutcomeNoMatch, d.Outcome)
			assert.Contains(t, d.Detail, "stored matcher is invalid")
		})
	}
}

func TestEvaluateDisabledShortCircuitsBeforeScope(t *testing.T) {
	o := order(func(o *db.StandingOrder) {
		o.Enabled = false
		o.DisabledReason = db.StandingDisabledReasonGroupRetired
	})
	// An agent that is NOT in the target group: the disabled check must win,
	// so the reason the operator sees is the one they can act on.
	d := Evaluate(o, event(func(e *Event) { e.Memberships = nil }), neverDelivered)

	assert.False(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeDisabled, d.Outcome)
	assert.Contains(t, d.Detail, db.StandingDisabledReasonGroupRetired)
	assert.False(t, d.ShouldRecord(), "a disabled order should not spam the ledger")
}

func TestEvaluateOutOfScopeForNonMember(t *testing.T) {
	d := Evaluate(order(), event(func(e *Event) {
		e.Memberships = []Membership{{GroupID: 99, Role: "worker"}}
	}), neverDelivered)

	assert.False(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeOutOfScope, d.Outcome)
	assert.False(t, d.ShouldRecord())
}

func TestEvaluateRoleFilterIsResolvedAgainstLiveRoster(t *testing.T) {
	o := order(func(o *db.StandingOrder) { o.TargetRole = "reviewer" })

	matched := Evaluate(o, event(func(e *Event) {
		e.Memberships = []Membership{{GroupID: 7, Role: "Reviewer"}}
	}), neverDelivered)
	assert.True(t, matched.Deliver, "role match should be case-insensitive")

	missed := Evaluate(o, event(), neverDelivered)
	assert.Equal(t, db.StandingOutcomeOutOfScope, missed.Outcome)
	assert.Contains(t, missed.Detail, "reviewer")
}

func TestEvaluateSingleTargetFollowsStableAgentKey(t *testing.T) {
	// The order was authored against an older generation. Matching on the
	// stable agent key is what keeps a reincarnated agent covered.
	o := order(func(o *db.StandingOrder) {
		o.TargetKind = db.StandingTargetConv
		o.GroupID = 0
		o.TargetAgent = "agt_aaa"
		o.TargetConv = "conv-OLD"
	})
	d := Evaluate(o, event(func(e *Event) { e.ConvID = "conv-NEW" }), neverDelivered)

	assert.True(t, d.Deliver, "a conv rotation must not strand the order")
}

func TestEvaluateSingleTargetNeverFallsBackToConversationIDOrEmptyIdentity(t *testing.T) {
	// Invalid or old data that lacks a stable target must fail closed. A
	// matching generation id is never a substitute for the persistent actor.
	o := order(func(o *db.StandingOrder) {
		o.TargetKind = db.StandingTargetConv
		o.GroupID = 0
		o.TargetAgent = ""
		o.TargetConv = "conv-a"
	})
	d := Evaluate(o, event(), neverDelivered)

	assert.False(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeOutOfScope, d.Outcome)
	assert.Contains(t, d.Detail, "stable agent")

	actorless := Evaluate(o, event(func(e *Event) { e.AgentID = "" }), neverDelivered)
	assert.False(t, actorless.Deliver, "two empty agent ids must not compare as a valid target")
	assert.Equal(t, db.StandingOutcomeOutOfScope, actorless.Outcome)
}

func TestEvaluateSourceFilter(t *testing.T) {
	d := Evaluate(order(), event(func(e *Event) { e.Source = db.StandingSourceResume }), neverDelivered)

	assert.False(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeNoMatch, d.Outcome)
	assert.False(t, d.ShouldRecord())
}

func TestEvaluateEmptySourcesMatchesEverySource(t *testing.T) {
	o := order(func(o *db.StandingOrder) { o.TriggerSources = nil })
	d := Evaluate(o, event(func(e *Event) { e.Source = db.StandingSourceResume }), neverDelivered)

	assert.True(t, d.Deliver)
}

// The distinction this feature most needs to preserve: a payload we could not
// read is not the same answer as a trigger that did not match.
func TestEvaluateTrimmedPayloadIsDistinctFromNoMatch(t *testing.T) {
	o := order(func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerToolBefore
		o.MatchField = db.StandingMatchFieldToolInput
		o.MatchRegex = "deploy"
	})
	ev := event(func(e *Event) {
		e.Event = db.StandingTriggerToolBefore
		e.PayloadTrimmed = true
	})

	d := Evaluate(o, ev, neverDelivered)

	assert.False(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeNotEvaluatedTrimmed, d.Outcome)
	assert.NotEqual(t, db.StandingOutcomeNoMatch, d.Outcome)
	assert.True(t, d.ShouldRecord(), "an unevaluatable trigger must reach the ledger")
}

func TestEvaluateTrimmedPayloadStillMatchesToolName(t *testing.T) {
	o := order(func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerToolBefore
		o.MatchField = db.StandingMatchFieldToolName
		o.MatchRegex = `(?i)^bash$`
	})
	ev := event(func(e *Event) {
		e.Event = db.StandingTriggerToolBefore
		e.ToolName = "Bash"
		e.PayloadTrimmed = true
	})

	d := Evaluate(o, ev, neverDelivered)
	assert.True(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeDelivered, d.Outcome)
}

func TestEvaluateTrimmedPromptIsUnevaluable(t *testing.T) {
	o := order(func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerUserPrompt
		o.MatchField = db.StandingMatchFieldPrompt
		o.MatchRegex = "deploy"
	})
	ev := event(func(e *Event) {
		e.Event = db.StandingTriggerUserPrompt
		e.Prompt = "truncated prefix"
		e.PayloadTrimmed = true
	})

	d := Evaluate(o, ev, neverDelivered)
	assert.False(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeNotEvaluatedTrimmed, d.Outcome)
}

func TestEvaluateRegexMatcherUsesNormalizedField(t *testing.T) {
	o := order(func(o *db.StandingOrder) {
		o.TriggerEvent = db.StandingTriggerUserPrompt
		o.MatchField = db.StandingMatchFieldPrompt
		o.MatchRegex = `(?i)\bdeploy\b`
	})

	matched := Evaluate(o, event(func(e *Event) {
		e.Event = db.StandingTriggerUserPrompt
		e.Prompt = "Please DEPLOY the service"
	}), neverDelivered)
	assert.True(t, matched.Deliver)

	missed := Evaluate(o, event(func(e *Event) {
		e.Event = db.StandingTriggerUserPrompt
		e.Prompt = "Run the tests"
	}), neverDelivered)
	assert.False(t, missed.Deliver)
	assert.Equal(t, db.StandingOutcomeNoMatch, missed.Outcome)
	assert.Contains(t, missed.Detail, db.StandingMatchFieldPrompt)
}

// SessionStart does not read the tool payload, so a trimmed event must not
// stop a boundary order from being delivered.
func TestEvaluateTrimmedPayloadIrrelevantToSessionStart(t *testing.T) {
	d := Evaluate(order(), event(func(e *Event) { e.PayloadTrimmed = true }), neverDelivered)

	assert.True(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeDelivered, d.Outcome)
}

func TestEvaluateUnsupportedTimingDeliversNothing(t *testing.T) {
	d := Evaluate(order(), event(func(e *Event) { e.Harness = harness.OpenCodeName }), neverDelivered)

	assert.False(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeUnsupportedTiming, d.Outcome)
	assert.Equal(t, db.StandingTransportNone, d.Capability.Transport)
	assert.True(t, d.ShouldRecord())
	assert.Contains(t, d.Detail, "next-turn",
		"the operator needs to be told how to make this order reach the harness")
}

func TestEvaluateNextTurnOrderReachesOpenCodeOnTheMessagePath(t *testing.T) {
	o := order(func(o *db.StandingOrder) { o.Timing = db.StandingTimingNextTurn })
	d := Evaluate(o, event(func(e *Event) { e.Harness = harness.OpenCodeName }), neverDelivered)

	assert.True(t, d.Deliver)
	assert.Equal(t, db.StandingTransportMessage, d.Capability.Transport)
}

func TestEvaluateDebouncedOrderSelectsMessageTransport(t *testing.T) {
	o := order(func(o *db.StandingOrder) {
		o.Timing = db.StandingTimingNextTurn
		o.DebounceSeconds = 5
	})
	d := Evaluate(o, event(func(e *Event) {
		e.Harness = harness.DefaultName
	}), neverDelivered)

	assert.True(t, d.Deliver)
	assert.Equal(t, db.StandingTransportMessage, d.Capability.Transport)
}

func TestEvaluateCadenceSuppressesSecondDeliveryInSameGeneration(t *testing.T) {
	o := order(func(o *db.StandingOrder) { o.Cadence = db.StandingCadenceOncePerGeneration })

	first := Evaluate(o, event(), neverDelivered)
	require.True(t, first.Deliver)
	assert.Equal(t, "conv-a", first.Epoch)

	second := Evaluate(o, event(), func(int64, int64, string, string) (bool, error) {
		return true, nil
	})
	assert.False(t, second.Deliver)
	assert.Equal(t, db.StandingOutcomeSuppressedCadence, second.Outcome)
}

// Capability is checked BEFORE cadence on purpose: an order that could never
// be delivered on this harness must not burn its once-per-generation slot and
// leave the conversation permanently silent.
func TestEvaluateUnsupportedTimingDoesNotConsultCadence(t *testing.T) {
	o := order(func(o *db.StandingOrder) { o.Cadence = db.StandingCadenceOncePerGeneration })
	consulted := false
	lookup := func(int64, int64, string, string) (bool, error) {
		consulted = true
		return false, nil
	}

	d := Evaluate(o, event(func(e *Event) { e.Harness = harness.OpenCodeName }), lookup)

	assert.Equal(t, db.StandingOutcomeUnsupportedTiming, d.Outcome)
	assert.False(t, consulted, "cadence state must not be consumed by an undeliverable order")
}

// The house style for anything on the agent's critical path: fail open. A
// ledger we cannot read is not a reason to withhold guidance.
func TestEvaluateCadenceLookupFailureDeliversAnyway(t *testing.T) {
	o := order(func(o *db.StandingOrder) { o.Cadence = db.StandingCadenceOncePerGeneration })
	d := Evaluate(o, event(), func(int64, int64, string, string) (bool, error) {
		return false, errors.New("database is locked")
	})

	assert.True(t, d.Deliver)
	assert.Contains(t, d.Detail, "database is locked")
}

func TestEvaluateCooldownUsesStableRecipientAndRevision(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 30, 0, time.UTC)
	o := order(func(o *db.StandingOrder) { o.CooldownSeconds = 60 })
	var gotOrder, gotRevision int64
	var gotAgent string
	d := Evaluate(o, event(func(e *Event) { e.OccurredAt = now }), neverDelivered,
		func(orderID, revision int64, targetAgent string) (time.Time, error) {
			gotOrder, gotRevision, gotAgent = orderID, revision, targetAgent
			return now.Add(-30 * time.Second), nil
		})

	assert.False(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeSuppressedCooldown, d.Outcome)
	assert.True(t, d.ShouldRecord(), "a cooldown suppression is a distinct ledger outcome")
	assert.Equal(t, o.ID, gotOrder)
	assert.Equal(t, o.Revision, gotRevision)
	assert.Equal(t, "agt_aaa", gotAgent)
	assert.Contains(t, d.Detail, "stable agent agt_aaa")
	assert.False(t, OutcomeIsProblem(d.Outcome), "a working rate control is not a fault")
}

func TestEvaluateCooldownExpiresAndFailuresFailOpen(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 2, 0, 0, time.UTC)
	o := order(func(o *db.StandingOrder) { o.CooldownSeconds = 60 })

	expired := Evaluate(o, event(func(e *Event) { e.OccurredAt = now }), neverDelivered,
		func(int64, int64, string) (time.Time, error) {
			return now.Add(-61 * time.Second), nil
		})
	assert.True(t, expired.Deliver)

	failed := Evaluate(o, event(func(e *Event) { e.OccurredAt = now }), neverDelivered,
		func(int64, int64, string) (time.Time, error) {
			return time.Time{}, errors.New("database is locked")
		})
	assert.True(t, failed.Deliver)
	assert.Contains(t, failed.Detail, "database is locked")
}

func TestEvaluateCooldownNeverUsesAnEmptySharedRecipientBucket(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	o := order(func(o *db.StandingOrder) { o.CooldownSeconds = 60 })
	consulted := false
	d := Evaluate(o, event(func(e *Event) {
		e.AgentID = ""
		e.OccurredAt = now
	}), neverDelivered, func(int64, int64, string) (time.Time, error) {
		consulted = true
		return now, nil
	})

	assert.False(t, d.Deliver)
	assert.Equal(t, db.StandingOutcomeOutOfScope, d.Outcome)
	assert.False(t, consulted)
}

func TestRenderContextCarriesProvenanceAndRevision(t *testing.T) {
	operatorOrder := order(func(o *db.StandingOrder) {
		o.OperatorAuthored = true
		o.Revision = 3
	})
	agentOrder := order(func(o *db.StandingOrder) {
		o.ID = 2
		o.Name = "cold-review"
		o.OwnerAgent = "agt_bbb"
		o.Summary = "Trigger a cold review after opening the PR."
	})

	text := RenderContext([]Decision{
		Evaluate(operatorOrder, event(), neverDelivered),
		Evaluate(agentOrder, event(), neverDelivered),
	})

	assert.Contains(t, text, "pr-early@3")
	assert.Contains(t, text, "authored by operator")
	assert.Contains(t, text, "authored by agent agt_bbb")
	assert.Contains(t, text, "Push the PR early.")
}

func TestRenderContextEmptyWhenNothingDelivers(t *testing.T) {
	d := Evaluate(order(), event(func(e *Event) { e.Memberships = nil }), neverDelivered)
	assert.Empty(t, RenderContext([]Decision{d}))
}

func TestEvaluateAllPreservesOrder(t *testing.T) {
	a := order(func(o *db.StandingOrder) { o.ID = 1; o.Name = "a" })
	b := order(func(o *db.StandingOrder) { o.ID = 2; o.Name = "b" })

	got := EvaluateAll([]*db.StandingOrder{a, b}, event(), neverDelivered)

	require.Len(t, got, 2)
	assert.Equal(t, "a", got[0].Order.Name)
	assert.Equal(t, "b", got[1].Order.Name)
}

// An order with neither the operator marker nor an owner is UNATTRIBUTED. Both
// columns default to that state, so inferring "operator" would let any order
// that failed to stamp an owner read to the model as a human instruction.
func TestAuthorLabelDoesNotInferOperatorFromEmptyOwner(t *testing.T) {
	assert.Equal(t, "an unattributed source", AuthorLabel(&db.StandingOrder{}))
	assert.Equal(t, "operator", AuthorLabel(&db.StandingOrder{OperatorAuthored: true}))
	assert.Equal(t, "agent agt_x", AuthorLabel(&db.StandingOrder{OwnerAgent: "agt_x"}))
}

func TestRenderContextUnattributedOrderSaysSo(t *testing.T) {
	o := order(func(o *db.StandingOrder) {
		o.OperatorAuthored = false
		o.OwnerAgent = ""
	})
	text := RenderContext([]Decision{Evaluate(o, event(), neverDelivered)})

	assert.Contains(t, text, "unattributed")
	assert.NotContains(t, text, "authored by operator")
	assert.NotContains(t, text, "tclaude agent orders",
		"model-visible context must not advertise a host-only DB command to sandboxed agents")
}

// The steady state of a once-per-generation order must not append a ledger row
// at every later boundary of the same conversation.
func TestSuppressedCadenceIsNotRecorded(t *testing.T) {
	o := order(func(o *db.StandingOrder) { o.Cadence = db.StandingCadenceOncePerGeneration })
	d := Evaluate(o, event(), func(int64, int64, string, string) (bool, error) {
		return true, nil
	})

	require.Equal(t, db.StandingOutcomeSuppressedCadence, d.Outcome)
	assert.False(t, d.ShouldRecord(), "the healthy steady state must not grow the ledger")
}

func TestNormalizeSourceSharedByEveryCaller(t *testing.T) {
	assert.Equal(t, db.StandingSourceStartup,
		NormalizeSource(db.StandingTriggerSessionStart, ""))
	assert.Equal(t, db.StandingSourceCompact,
		NormalizeSource(db.StandingTriggerSessionStart, "  Compact "))
	assert.Equal(t, "", NormalizeSource("tool.before", ""))
}
