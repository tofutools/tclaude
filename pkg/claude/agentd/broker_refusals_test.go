package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRefusalRecorder(now *time.Time) *brokerRefusalRecorder {
	return newBrokerRefusalRecorder(func() time.Time { return *now })
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

// A refusal must reach the daemon log, not only the dashboard: the notice
// text promises "the daemon log has the caller pid". The throttle exists
// because a statusline renders several times a second, so a permanently
// refused agent would otherwise write a line per render. The first refusal
// of a run always logs; the next line reports how many were suppressed.
func TestBrokerRefusals_LogDecisionIsThrottledPerRun(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	first := r.recordClaimMismatch("spwn-resolved", "hook: failed live-pane proof")
	assert.True(t, first.Log, "the first refusal of a run is always logged")
	assert.Zero(t, first.Suppressed)
	assert.Equal(t, 1, first.Count)

	now = now.Add(time.Second)
	second := r.recordClaimMismatch("spwn-resolved", "hook: failed live-pane proof")
	assert.False(t, second.Log, "a refusal inside the log interval is counted, not logged")

	now = now.Add(brokerRefusalLogInterval)
	third := r.recordClaimMismatch("spwn-resolved", "hook: failed live-pane proof")
	assert.True(t, third.Log)
	assert.Equal(t, 1, third.Suppressed, "the next line reports what the throttle swallowed")
	assert.Equal(t, 3, third.Count)

	// Rows throttle independently: a second agent's first refusal is not
	// hidden behind the first agent's interval.
	other := r.recordClaimMismatch("spwn-other", "hook: failed live-pane proof")
	assert.True(t, other.Log)

	// So does the unplaceable counter.
	unplaced := r.recordUnplaceable("hook: caller could not be placed")
	assert.True(t, unplaced.Log)
	assert.False(t, r.recordUnplaceable("hook: caller could not be placed").Log)
}

// A run that expired out of the window starts a fresh throttle too: the
// operator fixed something, it broke again, and the first refusal of the
// new episode should be as loud as the first one ever was.
func TestBrokerRefusals_LogThrottleResetsWithTheRun(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	require.True(t, r.recordClaimMismatch("spwn-resolved", "x").Log)
	now = now.Add(brokerRefusalWindow + time.Second)
	d := r.recordClaimMismatch("spwn-resolved", "x")
	assert.True(t, d.Log)
	assert.Equal(t, 1, d.Count, "a new run counts from one")
	assert.Zero(t, d.Suppressed)
}

// Unplaceable refusals share one dashboard counter but NOT one log
// throttle: a chatty orphan's refused renders must not swallow the only
// refusal a different caller ever produces, and a line's counts must
// describe the caller it names. The key is the socket peer pid, a kernel
// fact, never a caller string.
func TestBrokerRefusals_UnplaceableLogThrottleIsPerCaller(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	r := newTestRefusalRecorder(&now)

	require.True(t, r.recordUnplaceableFor(100, "statusline: caller could not be placed").Log)
	for range 5 {
		now = now.Add(200 * time.Millisecond)
		assert.False(t, r.recordUnplaceableFor(100, "statusline: caller could not be placed").Log)
	}

	other := r.recordUnplaceableFor(200, "hook: caller could not be placed")
	assert.True(t, other.Log, "a different caller's first refusal is not hidden behind pid 100's interval")
	assert.Equal(t, 1, other.Count, "the line's count describes the caller it names")
	assert.Zero(t, other.Suppressed)

	total, unplaceable := r.counts()
	assert.Equal(t, 7, unplaceable, "the dashboard counter stays daemon-wide")
	assert.Equal(t, 7, total)
}

// The "omitted its claim" reason is wrong when the caller did send one and
// the daemon simply could not load the row to check it.
func TestBrokerClaimReason(t *testing.T) {
	const fallback = "hook: tclaude’s sandbox callback omitted its session claim"
	assert.Equal(t, fallback, brokerClaimReason(fallback, "", "loading claimed row failed: busy"),
		"no claim sent: the fallback is the truth")
	assert.Equal(t, fallback, brokerClaimReason(fallback, "spwn-x", ""),
		"claim sent but no proof detail: nothing better to say")
	assert.Equal(t, "hook: claimed session could not be loaded to check the claim",
		brokerClaimReason(fallback, "spwn-x", "loading claimed row failed: busy"))
	assert.Equal(t, "statusline: claimed session could not be loaded to check the claim",
		brokerClaimReason("statusline: caller could not be placed", "spwn-x", "err"))
}
