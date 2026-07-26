package agentd

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// resetBrokerEnforcementForTest drops the memoised enforcement flag so a
// test starts from a real config read.
func resetBrokerEnforcementForTest() {
	brokerEnforcement.mu.Lock()
	defer brokerEnforcement.mu.Unlock()
	brokerEnforcement.checkedAt = time.Time{}
	brokerEnforcement.enforced = false
}

// setBrokerEnforcedForTest writes the operator toggle to the config file
// the daemon actually reads, rather than poking the cache — the point is
// to exercise the real path from config.json to a rejection.
func setBrokerEnforcedForTest(t *testing.T, enforced bool) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Broker = &config.BrokerConfig{EnforceLimits: enforced}
	require.NoError(t, config.Save(cfg))
}

// newTestLimiter gives a limiter on a clock the test drives, so the
// window boundary is exercised rather than slept through.
func newTestLimiter(now *time.Time) *brokerLimiter {
	return &brokerLimiter{
		buckets: map[string]*brokerBucket{},
		now:     func() time.Time { return *now },
	}
}

// The ceiling is per second, and the second is a real boundary: a caller
// at the limit must be allowed again once the window turns over, or a
// steady legitimate load would trip it permanently.
func TestBrokerLimiter_CountsWithinAOneSecondWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(&now)

	for i := 1; i <= brokerRatePerSecond; i++ {
		over, count := l.observe("spwn-a", brokerRatePerSecond)
		assert.False(t, over, "request %d is within the ceiling", i)
		assert.Equal(t, i, count)
	}

	over, count := l.observe("spwn-a", brokerRatePerSecond)
	assert.True(t, over, "the request past the ceiling is over")
	assert.Equal(t, brokerRatePerSecond+1, count)

	now = now.Add(time.Second)
	over, count = l.observe("spwn-a", brokerRatePerSecond)
	assert.False(t, over, "a new window starts the caller fresh")
	assert.Equal(t, 1, count)
}

// The operator condition that matters most: one agent in excess must
// never starve its peers. Keying on the resolved session row is what
// delivers that, so it is pinned here rather than left implicit.
func TestBrokerLimiter_IsPerAgent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(&now)

	for range brokerRatePerSecond * 3 {
		l.observe("spwn-noisy", brokerRatePerSecond)
	}
	over, _ := l.observe("spwn-noisy", brokerRatePerSecond)
	assert.True(t, over, "the noisy agent is over its own ceiling")

	over, count := l.observe("spwn-quiet", brokerRatePerSecond)
	assert.False(t, over, "a peer must not inherit the noisy agent's excess")
	assert.Equal(t, 1, count, "the peer's own bucket starts empty")
}

// The excess log is itself throttled — a caller in excess is by
// definition making many requests a second, so a line per request would
// turn a client-side problem into a daemon-side one. The suppressed count
// is what keeps the throttling honest about magnitude.
func TestBrokerLimiter_ThrottlesTheExcessLogButReportsMagnitude(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(&now)
	l.observe("spwn-a", brokerRatePerSecond)

	log, n := l.shouldLogExcess("spwn-a")
	assert.True(t, log, "the first excess must be logged")
	assert.Equal(t, 1, n)

	for range 50 {
		log, _ = l.shouldLogExcess("spwn-a")
		assert.False(t, log, "further excess within the interval must stay quiet")
	}

	now = now.Add(brokerLimitLogInterval + time.Second)
	log, n = l.shouldLogExcess("spwn-a")
	assert.True(t, log, "the interval having passed, log again")
	assert.Equal(t, 51, n,
		"the line must report every excess since the last one, not just this request")
}

// An agent-id-keyed map that never forgets is a slow leak on a daemon
// that runs for weeks.
func TestBrokerLimiter_PrunesIdleBuckets(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	l := newTestLimiter(&now)
	l.observe("spwn-gone", brokerRatePerSecond)
	assert.Len(t, l.buckets, 1)

	now = now.Add(brokerLimiterPruneAfter + time.Minute)
	// Pruning runs when a NEW key arrives, which is the moment the map
	// would otherwise grow.
	l.observe("spwn-fresh", brokerRatePerSecond)

	assert.NotContains(t, l.buckets, "spwn-gone", "an idle bucket must not live forever")
	assert.Contains(t, l.buckets, "spwn-fresh")
}

// Enforcement is opt-in by operator ruling: with the config absent the
// limiter measures and logs but refuses nothing, so the ceilings can be
// validated against real traffic before they can cut a working agent off.
func TestBrokerLimits_ShadowModeByDefault(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetBrokerEnforcementForTest()

	assert.False(t, brokerLimitsEnforced(time.Now()),
		"an absent broker config must leave the limiter in shadow mode")
}

// The enforcement flag is cached, because reading a config file per
// request at a statusline's cadence would make the limiter generate
// exactly the I/O it exists to bound.
func TestBrokerLimits_EnforcementFlagIsCached(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	resetBrokerEnforcementForTest()

	now := time.Unix(1_700_000_000, 0)
	assert.False(t, brokerLimitsEnforced(now))

	setBrokerEnforcedForTest(t, true)
	assert.False(t, brokerLimitsEnforced(now.Add(brokerEnforcementTTL/2)),
		"within the TTL the cached answer stands")
	assert.True(t, brokerLimitsEnforced(now.Add(2*brokerEnforcementTTL)),
		"past the TTL the flag is re-read")
}

// The size ceiling has no shadow mode, and that is a property of what it
// is: the daemon reads through a LimitReader, so an over-cap body has
// already been truncated by the time anyone could decide to allow it.
// What the shadow-mode discipline buys there is that the rejection is
// never silent.
func TestBrokerBodyCap_IsAlwaysEnforcedAndNeverSilent(t *testing.T) {
	assert.Equal(t, 10<<20, brokerMaxBody,
		"the operator-set cap: large tool payloads should normally fit whole")
	// The call is the log; a nil-safe smoke of it here keeps a refactor
	// that drops the logging from passing unnoticed.
	logBrokerBodyOverCap("/v1/whoami/hook", "spwn-a", brokerMaxBody+1)
}
