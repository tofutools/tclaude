package standingorders

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestCapabilitySameContinuationOnHookHarnesses(t *testing.T) {
	for _, h := range []string{harness.DefaultName, harness.CodexName} {
		c := CapabilityFor(db.StandingTimingSameContinuation, db.StandingTriggerSessionStart, h)
		assert.Equal(t, StatusSupported, c.Status, h)
		assert.Equal(t, db.StandingTransportHookContext, c.Transport, h)
	}
}

// OpenCode's SSE projection has no response channel, so a same-continuation
// order is reported unsupported rather than quietly downgraded to a message.
func TestCapabilitySameContinuationUnsupportedOnOpenCode(t *testing.T) {
	c := CapabilityFor(db.StandingTimingSameContinuation, db.StandingTriggerSessionStart, harness.OpenCodeName)

	assert.Equal(t, StatusUnsupported, c.Status)
	assert.False(t, c.Supported())
	assert.Equal(t, db.StandingTransportNone, c.Transport)
	assert.Contains(t, c.Detail, "next-turn", "the detail must say how to fix it")
}

func TestCapabilityNextTurnReachesEveryHarness(t *testing.T) {
	for _, h := range KnownHarnesses {
		c := CapabilityFor(db.StandingTimingNextTurn, db.StandingTriggerSessionStart, h)
		assert.True(t, c.Supported(), h)
	}
}

// Getting the stronger channel than you asked for is not a degradation.
func TestCapabilityNextTurnOnHookHarnessIsNotDegraded(t *testing.T) {
	c := CapabilityFor(db.StandingTimingNextTurn, db.StandingTriggerSessionStart, harness.DefaultName)

	assert.Equal(t, StatusSupported, c.Status)
	assert.Equal(t, db.StandingTransportHookContext, c.Transport)
}

// An unknown harness is never assumed capable — guessing upward would promise
// a timing guarantee tclaude has never tested there.
func TestCapabilityUnknownHarnessIsMessageOnly(t *testing.T) {
	assert.Equal(t, StatusUnsupported,
		CapabilityFor(db.StandingTimingSameContinuation, db.StandingTriggerSessionStart, "somethingelse").Status)

	c := CapabilityFor(db.StandingTimingNextTurn, db.StandingTriggerSessionStart, "somethingelse")
	assert.Equal(t, db.StandingTransportMessage, c.Transport)
}

func TestCapabilityUnknownTriggerAndTiming(t *testing.T) {
	assert.Equal(t, StatusUnsupported,
		CapabilityFor(db.StandingTimingNextTurn, "tool.before", harness.DefaultName).Status)
	assert.Equal(t, StatusUnsupported,
		CapabilityFor("whenever", db.StandingTriggerSessionStart, harness.DefaultName).Status)
}

// The rolled-up cell must report the worst case: the failure this feature has
// to avoid is an operator believing guidance reached agents it never reached.
func TestRolledUpCapabilityReportsWorstCase(t *testing.T) {
	worst := RolledUpCapability(db.StandingTimingSameContinuation, db.StandingTriggerSessionStart)
	assert.Equal(t, StatusUnsupported, worst.Status)
	assert.NotEmpty(t, worst.Detail)

	fine := RolledUpCapability(db.StandingTimingNextTurn, db.StandingTriggerSessionStart)
	assert.Equal(t, StatusSupported, fine.Status)
}

func TestCapabilityByHarnessCoversEveryKnownHarness(t *testing.T) {
	byH := CapabilityByHarness(db.StandingTimingSameContinuation, db.StandingTriggerSessionStart)

	assert.Len(t, byH, len(KnownHarnesses))
	assert.Equal(t, StatusSupported, byH[harness.DefaultName].Status)
	assert.Equal(t, StatusUnsupported, byH[harness.OpenCodeName].Status)
}

func TestOutcomeIsProblem(t *testing.T) {
	assert.False(t, OutcomeIsProblem(db.StandingOutcomeDelivered))
	assert.False(t, OutcomeIsProblem(db.StandingOutcomeNoMatch))
	for _, bad := range []string{
		db.StandingOutcomeUnsupportedTiming,
		db.StandingOutcomeNotEvaluatedTrimmed,
		db.StandingOutcomeDegradedTransport,
		db.StandingOutcomeSuppressedCadence,
	} {
		assert.True(t, OutcomeIsProblem(bad), bad)
	}
}
