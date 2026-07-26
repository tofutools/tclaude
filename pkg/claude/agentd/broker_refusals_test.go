package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRefusalRecorder(now *time.Time) *brokerRefusalRecorder {
	return &brokerRefusalRecorder{
		bySession: map[string]*brokerRefusal{},
		now:       func() time.Time { return *now },
	}
}

// The attribution rule is the whole security property of this recorder,
// so it is pinned rather than left to the call sites: what appears on an
// agent row comes from the row the DAEMON resolved, and there is no path
// by which a caller-supplied session id can put a mark on any row.
//
// The recorder cannot enforce that by itself — it only ever sees the
// string its caller passes — so what this pins is the shape that makes
// the rule checkable: one entry point that takes the resolved row, and
// one that takes no identifier at all. That the CALL SITES pass the right
// one is pinned end-to-end by the flow tests.
func TestBrokerRefusals_AttributeToTheResolvedRow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	r.recordClaimMismatch("spwn-resolved", "hook: claimed session id disagrees")

	got := r.forSession("spwn-resolved")
	require.NotNil(t, got, "the resolved row carries the refusal")
	assert.Equal(t, 1, got.Count)
	assert.Equal(t, now, got.First)

	total, unplaceable := r.counts()
	assert.Equal(t, 1, total, "an attributed refusal still counts towards the machine-level total")
	assert.Zero(t, unplaceable, "...but it is not an unplaceable one")
}

// An unplaceable caller has no trustworthy identifier at all, so it is
// counted and never attributed. A test is worth having because the
// tempting shortcut — falling back to the claimed id "just for the
// unplaceable case" — is exactly the spoof the design refuses.
func TestBrokerRefusals_UnplaceableIsCountOnly(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	r.recordUnplaceable("hook: caller could not be placed")
	r.recordUnplaceable("hook: caller could not be placed")

	total, unplaceable := r.counts()
	assert.Equal(t, 2, unplaceable)
	assert.Equal(t, 2, total)

	r.mu.Lock()
	attributed := len(r.bySession)
	r.mu.Unlock()
	assert.Zero(t, attributed, "an unplaceable refusal must not name any row")
}

// An empty resolved id must not silently become an attribution — it
// degrades to the counter instead.
func TestBrokerRefusals_EmptyResolvedIDFallsToTheCounter(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	r.recordClaimMismatch("", "hook: claimed session id disagrees")

	total, unplaceable := r.counts()
	assert.Equal(t, 1, unplaceable, "no row means no attribution, not a blank key")
	assert.Equal(t, 1, total)

	r.mu.Lock()
	_, blankKey := r.bySession[""]
	r.mu.Unlock()
	assert.False(t, blankKey, "a blank key would render as a badge on nothing")
}

// The total is what the operator sees when the badge lands somewhere they
// are not looking, so it has to be the sum of both kinds — a total that
// only counted the unplaceable ones would go quiet in exactly the
// pid-reuse case this feature exists for.
func TestBrokerRefusals_TotalCoversBothKinds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	r.recordClaimMismatch("spwn-a", "hook: claimed session id disagrees")
	r.recordClaimMismatch("spwn-a", "hook: claimed session id disagrees")
	r.recordClaimMismatch("spwn-b", "statusline: claimed session id disagrees")
	r.recordUnplaceable("hook: caller could not be placed")

	total, unplaceable := r.counts()
	assert.Equal(t, 4, total)
	assert.Equal(t, 1, unplaceable)
}

// The condition has to stop being shown once it stops happening, without
// needing a daemon restart — otherwise an operator who fixes it keeps
// seeing the badge and learns to ignore the badge.
func TestBrokerRefusals_ExpireOutOfTheWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	r.recordClaimMismatch("spwn-a", "hook: claimed session id disagrees")
	r.recordUnplaceable("hook: caller could not be placed")
	require.NotNil(t, r.forSession("spwn-a"))

	now = now.Add(brokerRefusalWindow + time.Minute)
	assert.Nil(t, r.forSession("spwn-a"), "a refusal that stopped must stop showing")

	total, unplaceable := r.counts()
	assert.Zero(t, total)
	assert.Zero(t, unplaceable)
}

// A run of refusals reports how long it has been going, so the operator
// can tell a momentary blip from a permanently starved agent.
func TestBrokerRefusals_RunReportsItsStart(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	start := now
	for range 5 {
		r.recordClaimMismatch("spwn-a", "hook: claimed session id disagrees")
		now = now.Add(time.Minute)
	}

	got := r.forSession("spwn-a")
	require.NotNil(t, got)
	assert.Equal(t, 5, got.Count)
	assert.Equal(t, start, got.First, "First must stay the start of the run, not the last event")

	// A fresh run after a quiet spell starts its own clock.
	now = now.Add(brokerRefusalWindow + time.Minute)
	r.recordClaimMismatch("spwn-a", "hook: claimed session id disagrees")
	got = r.forSession("spwn-a")
	require.NotNil(t, got)
	assert.Equal(t, 1, got.Count, "a new run does not inherit the old count")
	assert.Equal(t, now, got.First)
}

// forSession must not hand out the live record — the dashboard reads it
// on a poll while request handlers keep writing.
func TestBrokerRefusals_ForSessionReturnsACopy(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	r.recordClaimMismatch("spwn-a", "hook: claimed session id disagrees")
	got := r.forSession("spwn-a")
	require.NotNil(t, got)
	got.Count = 9999

	again := r.forSession("spwn-a")
	require.NotNil(t, again)
	assert.Equal(t, 1, again.Count, "a caller mutating its copy must not corrupt the recorder")
}

// Refusals are rare by construction, but a pathological caller must not
// grow the map without bound.
func TestBrokerRefusals_PruneExpiredEntries(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	r.recordClaimMismatch("spwn-old", "hook: claimed session id disagrees")
	now = now.Add(brokerRefusalWindow + time.Minute)

	// Pruning runs on a write cadence; drive enough writes to trip it.
	for range brokerRefusalPruneEvery {
		r.recordClaimMismatch("spwn-new", "hook: claimed session id disagrees")
	}

	r.mu.Lock()
	_, stillThere := r.bySession["spwn-old"]
	r.mu.Unlock()
	assert.False(t, stillThere, "an entry outside the window must not live forever")
}
