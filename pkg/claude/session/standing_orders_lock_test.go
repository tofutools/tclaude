package session

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestStandingOrderLockIsExclusivePerConv(t *testing.T) {
	standingOrderFixture(t, harness.DefaultName)

	release, acquired := lockStandingOrderDelivery(context.Background(), "conv-1")
	require.True(t, acquired)

	// A second holder for the SAME conversation is refused rather than let
	// through — that refusal is what stops a duplicate delivery. A short
	// deadline stands in for the production 3s one so the test does not wait
	// it out; the caller's context is what bounds the wait either way.
	busy, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, second := lockStandingOrderDelivery(busy, "conv-1")
	assert.False(t, second, "the same conversation cannot be delivered twice at once")

	// A different conversation is unaffected. The lock is per-conv precisely so
	// one busy agent does not stall every other agent's boundary.
	otherRelease, other := lockStandingOrderDelivery(context.Background(), "conv-2")
	assert.True(t, other, "a different conversation is not blocked")
	otherRelease()

	release()
	regained, ok := lockStandingOrderDelivery(context.Background(), "conv-1")
	assert.True(t, ok, "the lock is available again once released")
	regained()
}

// The regression the lock exists for: two SessionStart boundaries racing on one
// conversation — a compaction landing next to a resume — must not both deliver
// a once-per-generation order.
//
// Without serialization both evaluate before either records, both read "not
// delivered yet", and the operator's one-shot guidance arrives twice.
func TestStandingOrderConcurrentDispatchDeliversOnce(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	orderID := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Cadence = db.StandingCadenceOncePerGeneration
	})

	const racers = 4
	var wg sync.WaitGroup
	outputs := make([]string, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Release all four at once, so they contend on the cadence check
			// rather than trickling past it one at a time.
			<-start
			var buf bytes.Buffer
			// Assertions stay on the test goroutine: a failed require here
			// would Goexit a worker and hang the WaitGroup.
			errs[i] = DispatchHookEvent(context.Background(),
				sessionStart(db.StandingSourceCompact), "sess-1", LocalHookAmbient(), &buf)
			outputs[i] = buf.String()
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}

	delivered := 0
	for _, out := range outputs {
		if strings.Contains(out, "Push the PR early") {
			delivered++
		}
	}
	assert.Equal(t, 1, delivered,
		"a once-per-generation order reaches the model exactly once across concurrent boundaries")

	// And the ledger agrees. A second `delivered` row would mean the cadence
	// check had been satisfied twice, which is the same bug seen from the
	// durable side.
	deliveries, err := db.ListStandingDeliveries(orderID, 100)
	require.NoError(t, err)
	rows := 0
	for _, d := range deliveries {
		if d.Outcome == db.StandingOutcomeDelivered {
			rows++
		}
	}
	assert.Equal(t, 1, rows, "exactly one delivery is recorded")
}

// An always-cadence order is NOT suppressed by a lost lock. It has no
// read-modify-write to protect — it is authored to arrive on every boundary —
// so skipping it would drop guidance to prevent a race it cannot lose.
func TestStandingOrderAlwaysCadenceSurvivesLostLock(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	insertOrder(t, groupID)

	release, acquired := lockStandingOrderDelivery(context.Background(), "conv-1")
	require.True(t, acquired)
	defer release()

	// The lock is held elsewhere, so this dispatch cannot acquire it. The
	// always-cadence order still arrives.
	out := dispatch(t, sessionStart(db.StandingSourceCompact))
	assert.Contains(t, out, "Push the PR early",
		"an always-cadence order is delivered even when the delivery lock is unavailable")
}

// The mirror of the above: a once-per-generation order whose lock is held is
// deferred, and the deferral is RECORDED rather than dropped silently — an
// order that quietly did not arrive looks identical to one that did.
func TestStandingOrderOncePerGenerationDeferredWhenLockHeld(t *testing.T) {
	groupID := standingOrderFixture(t, harness.DefaultName)
	orderID := insertOrder(t, groupID, func(o *db.StandingOrder) {
		o.Cadence = db.StandingCadenceOncePerGeneration
	})

	release, acquired := lockStandingOrderDelivery(context.Background(), "conv-1")
	require.True(t, acquired)

	out := dispatch(t, sessionStart(db.StandingSourceCompact))
	assert.NotContains(t, out, "Push the PR early",
		"a cadence-gated order is skipped rather than delivered unserialized")

	latest, err := db.LatestStandingDelivery(orderID)
	require.NoError(t, err)
	require.NotNil(t, latest, "the skip is visible in the ledger")
	assert.Equal(t, db.StandingOutcomeNotEvaluatedBusy, latest.Outcome)

	// The cadence stayed open: once the lock is free, the next boundary
	// delivers. That is what makes skipping safe rather than lossy.
	release()
	out = dispatch(t, sessionStart(db.StandingSourceCompact))
	assert.Contains(t, out, "Push the PR early",
		"a deferred order is delivered at the next boundary")
}
