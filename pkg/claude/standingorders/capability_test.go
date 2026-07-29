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
		CapabilityFor(db.StandingTimingNextTurn, "unknown.event", harness.DefaultName).Status)
	assert.Equal(t, StatusUnsupported,
		CapabilityFor("whenever", db.StandingTriggerSessionStart, harness.DefaultName).Status)
}

func TestCapabilityActionTriggersByHarness(t *testing.T) {
	for _, event := range []string{
		db.StandingTriggerUserPrompt,
		db.StandingTriggerToolBefore,
		db.StandingTriggerToolAfter,
	} {
		for _, harnessName := range []string{harness.DefaultName, harness.CodexName} {
			c := CapabilityFor(db.StandingTimingSameContinuation, event, harnessName)
			assert.Equal(t, StatusSupported, c.Status)
			assert.Equal(t, db.StandingTransportHookContext, c.Transport)
		}
	}

	prompt := CapabilityFor(
		db.StandingTimingNextTurn, db.StandingTriggerUserPrompt, harness.OpenCodeName)
	assert.Equal(t, StatusUnsupported, prompt.Status)
	assert.Equal(t, db.StandingTransportNone, prompt.Transport)
	assert.Contains(t, prompt.Detail, "normalized prompt text")

	for _, event := range []string{
		db.StandingTriggerToolBefore,
		db.StandingTriggerToolAfter,
	} {
		next := CapabilityFor(db.StandingTimingNextTurn, event, harness.OpenCodeName)
		assert.Equal(t, StatusSupported, next.Status)
		assert.Equal(t, db.StandingTransportMessage, next.Transport)
		assert.Contains(t, next.Detail, "self-trigger")

		same := CapabilityFor(db.StandingTimingSameContinuation, event, harness.OpenCodeName)
		assert.Equal(t, StatusUnsupported, same.Status)
		assert.Equal(t, db.StandingTransportNone, same.Transport)
	}
}

// The rolled-up cell must report the worst case: the failure this feature has
// to avoid is an operator believing guidance reached agents it never reached.
func TestPlatformCapabilityReportsWorstCase(t *testing.T) {
	worst := PlatformCapability(db.StandingTimingSameContinuation, db.StandingTriggerSessionStart)
	assert.Equal(t, StatusUnsupported, worst.Status)
	assert.NotEmpty(t, worst.Detail)

	fine := PlatformCapability(db.StandingTimingNextTurn, db.StandingTriggerSessionStart)
	assert.Equal(t, StatusSupported, fine.Status)
}

func TestCapabilityByHarnessCoversEveryKnownHarness(t *testing.T) {
	byH := CapabilityByHarness(db.StandingTimingSameContinuation, db.StandingTriggerSessionStart)

	assert.Len(t, byH, len(KnownHarnesses))
	assert.Equal(t, StatusSupported, byH[harness.DefaultName].Status)
	assert.Equal(t, StatusUnsupported, byH[harness.OpenCodeName].Status)
}

func TestOutcomeIsProblem(t *testing.T) {
	// Healthy states. Rate-control suppression is the order behaving exactly
	// as authored, so flagging it would mark a correctly working order as a
	// problem until its delivery window opens.
	for _, ok := range []string{
		db.StandingOutcomeDelivered,
		db.StandingOutcomeNoMatch,
		db.StandingOutcomeSuppressedCadence,
		db.StandingOutcomeSuppressedCooldown,
	} {
		assert.False(t, OutcomeIsProblem(ok), ok)
	}
	for _, bad := range []string{
		db.StandingOutcomeUnsupportedTiming,
		db.StandingOutcomeNotEvaluatedTrimmed,
		db.StandingOutcomeNotEvaluatedBusy,
		db.StandingOutcomeDegradedTransport,
		db.StandingOutcomeTransportUnimplemented,
		db.StandingOutcomeDeliveryFailed,
	} {
		assert.True(t, OutcomeIsProblem(bad), bad)
	}
}

// The distinction this pair of functions exists to preserve: a single-agent
// order is not "unsupported" merely because OpenCode exists somewhere.
func TestReduceCapabilityUsesOnlyReachableHarnesses(t *testing.T) {
	timing, event := db.StandingTimingSameContinuation, db.StandingTriggerSessionStart

	reachable := ReduceCapability(timing, event, []string{harness.DefaultName})
	assert.Equal(t, StatusSupported, reachable.Status,
		"a Claude-only target must not inherit OpenCode's limitation")

	assert.Equal(t, StatusUnsupported, PlatformCapability(timing, event).Status,
		"the platform-wide answer is still the worst case")
}

func TestReduceCapabilityMixedGroupTakesWorstCase(t *testing.T) {
	c := ReduceCapability(db.StandingTimingSameContinuation, db.StandingTriggerSessionStart,
		[]string{harness.DefaultName, harness.CodexName, harness.OpenCodeName})

	assert.Equal(t, StatusUnsupported, c.Status)
	assert.NotEmpty(t, c.Detail)
}

// Nothing reachable means nothing delivered — never "supported".
func TestReduceCapabilityEmptyListIsUnsupported(t *testing.T) {
	c := ReduceCapability(db.StandingTimingNextTurn, db.StandingTriggerSessionStart, nil)

	assert.Equal(t, StatusUnsupported, c.Status)
	assert.Equal(t, db.StandingTransportNone, c.Transport)
}
