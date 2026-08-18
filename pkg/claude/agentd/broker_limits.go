package agentd

import (
	"log/slog"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// --- limits shared by the brokered endpoints (TCL-754) ---
//
// /v1/whoami/hook and /v1/whoami/statusline are the two ways an agent
// inside a mount namespace reaches the conversation database. Both are
// reachable by any agent the daemon can place, and one of them runs
// several times a second by design, so both need a ceiling.
//
// Two properties are deliberate:
//
//   - The rate limit is keyed on the RESOLVED SESSION ROW — the identity
//     the ancestry walk produced — and never on a connection, a pid or a
//     global bucket. Each agent gets its own allowance, so one agent in
//     excess cannot starve its peers. Keying on anything the caller
//     controls would let a misbehaving agent split itself across buckets;
//     keying globally would let it deny service to everyone else.
//   - All state is in memory. The limiter must not itself generate the
//     I/O it exists to bound, and resetting on daemon restart is fine:
//     these are DoS ceilings, not a quota anybody is owed an accounting
//     of.
//
// Enforcement is opt-in (config.BrokerConfig). Off — the default — the
// limiter still measures and logs, stating what it would have refused.
// That way the ceilings are validated against real traffic before they
// can cut a working agent off. Advanced shaping is deliberately out of
// scope; see TCL-763.

// brokerMaxBody bounds a brokered request body. Tool payloads and
// assistant messages are the large ones and should normally fit whole —
// this sits far above them, so reaching it means malformed or hostile.
const brokerMaxBody = 10 << 20 // 10 MiB

// brokerRatePerSecond is the per-agent ceiling. Legitimate traffic is a
// statusline re-rendering a few times a second plus tool-use hooks
// bursting on top; this leaves generous headroom over that.
const brokerRatePerSecond = 20

// brokerPreIdentityRatePerSecond is the coarse backstop before identity is
// final. Even an ordinary ancestry result is provisional until a claimed-row
// mismatch has had the chance to prove the current launch through tmux. It
// exists only so bounded body parsing and that proof cannot be hammered, so it
// is deliberately far larger than any real caller needs.
const brokerPreIdentityRatePerSecond = 2000

// brokerProofRatePerSecond is the hard ceiling on live-tmux identity proofs.
// Unlike the operator-controlled traffic ceiling, this is always enforced:
// each admitted attempt may spawn a tmux subprocess. The headroom is well
// above ordinary agent CLI and broker traffic.
const brokerProofRatePerSecond = 100

// brokerPreIdentityKey is the shared pre-proof bucket for callers with no
// provisional harness identity at all.
const brokerPreIdentityKey = "\x00unplaced"

// brokerPreIdentityKeyForRow isolates pre-proof work by the daemon-resolved
// provisional row without charging that row's final per-agent quota. The row
// may be corrected after the request claim is proved, but it is stable against
// caller process churn; the NUL-prefixed namespace cannot collide with a real
// session bucket.
func brokerPreIdentityKeyForRow(rowID string) string {
	if rowID == "" {
		return brokerPreIdentityKey
	}
	return brokerPreIdentityKey + ":row:" + rowID
}

const brokerProofKey = "\x00identity-proof"

func brokerProofKeyForRow(rowID string) string {
	if rowID == "" {
		return brokerProofKey
	}
	return brokerProofKey + ":row:" + rowID
}

// brokerLimiterPruneAfter is how long an idle bucket survives. Buckets
// are tiny, but an agent-id-keyed map that never forgets is a slow leak
// on a daemon that runs for weeks.
const brokerLimiterPruneAfter = 5 * time.Minute

// brokerLimitLogInterval throttles the excess log itself. A caller in
// excess is by definition making many requests a second, and a log line
// per request would turn a client-side problem into a daemon-side one.
const brokerLimitLogInterval = 10 * time.Second

// brokerBucket is one caller's fixed one-second window.
type brokerBucket struct {
	windowStart time.Time
	count       int
	// overCount accumulates rejections (real or shadow) between log
	// lines, so the throttled log can report the true magnitude rather
	// than "1".
	overCount int
	lastLogAt time.Time
	lastSeen  time.Time
}

type brokerLimiter struct {
	mu      sync.Mutex
	buckets map[string]*brokerBucket
	// now is swappable for tests; nil means time.Now.
	now func() time.Time
}

var defaultBrokerLimiter = &brokerLimiter{buckets: map[string]*brokerBucket{}}

func (l *brokerLimiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// observe records one request against key and reports whether it is over
// the ceiling, together with how many requests that caller has made in
// the current window.
//
// It always counts, whatever enforcement is configured — measuring is the
// point of shadow mode.
func (l *brokerLimiter) observe(key string, limit int) (over bool, count int) {
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[key]
	if b == nil {
		// lastSeen is stamped BEFORE pruning: a bucket whose lastSeen is
		// still the zero time reads as idle since the epoch, so pruning
		// with it unset deletes the entry that was just created.
		b = &brokerBucket{lastSeen: now}
		l.buckets[key] = b
		l.pruneLocked(now)
	}
	if now.Sub(b.windowStart) >= time.Second {
		b.windowStart = now
		b.count = 0
	}
	b.count++
	b.lastSeen = now
	return b.count > limit, b.count
}

// shouldLogExcess reports whether this excess should produce a log line
// now, and how many were suppressed since the last one.
func (l *brokerLimiter) shouldLogExcess(key string) (bool, int) {
	now := l.clock()
	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.buckets[key]
	if b == nil {
		return false, 0
	}
	b.overCount++
	if !b.lastLogAt.IsZero() && now.Sub(b.lastLogAt) < brokerLimitLogInterval {
		return false, 0
	}
	b.lastLogAt = now
	n := b.overCount
	b.overCount = 0
	return true, n
}

func (l *brokerLimiter) pruneLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.lastSeen) > brokerLimiterPruneAfter {
			delete(l.buckets, k)
		}
	}
}

// brokerEnforcementTTL bounds how stale the enforcement flag may be.
//
// The flag lives in the config FILE, and reading a file per request would
// make the limiter generate exactly the I/O it exists to bound — at a
// statusline's cadence, several disk reads a second per agent. Caching it
// costs a few seconds' delay when the operator flips the toggle, which is
// the right trade for a DoS backstop.
const brokerEnforcementTTL = 5 * time.Second

var brokerEnforcement struct {
	mu        sync.Mutex
	checkedAt time.Time
	enforced  bool
}

func brokerLimitsEnforced(now time.Time) bool {
	brokerEnforcement.mu.Lock()
	defer brokerEnforcement.mu.Unlock()
	if !brokerEnforcement.checkedAt.IsZero() && now.Sub(brokerEnforcement.checkedAt) < brokerEnforcementTTL {
		return brokerEnforcement.enforced
	}
	cfg, err := config.Load()
	if err != nil {
		// An unreadable config is not a licence to start refusing
		// traffic: fail towards the default, which is shadow mode.
		brokerEnforcement.enforced = false
	} else {
		brokerEnforcement.enforced = cfg.BrokerLimitsEnforced()
	}
	brokerEnforcement.checkedAt = now
	return brokerEnforcement.enforced
}

// brokerRateVerdict is what an endpoint does with a caller.
type brokerRateVerdict struct {
	// Reject is true only when the caller is over the ceiling AND
	// enforcement is configured on.
	Reject bool
}

// checkBrokerRate measures one request against a caller's ceiling and
// decides what to do about it.
//
// key must be the resolved session row id. Pass brokerPreIdentityKey for
// the coarse pre-identity stage.
func checkBrokerRate(endpoint, key string, limit int) brokerRateVerdict {
	over, count := defaultBrokerLimiter.observe(key, limit)
	if !over {
		return brokerRateVerdict{}
	}
	enforced := brokerLimitsEnforced(defaultBrokerLimiter.clock())
	if log, suppressed := defaultBrokerLimiter.shouldLogExcess(key); log {
		slog.Warn("broker: caller over the per-agent request ceiling",
			"endpoint", endpoint, "session", key,
			"requests_this_second", count, "limit_per_second", limit,
			"excess_since_last_log", suppressed,
			"action", enforcementWord(enforced), "module", "hooks")
	}
	return brokerRateVerdict{Reject: enforced}
}

// checkBrokerProofRate is independent of broker.enforce_limits because it
// bounds daemon work, not ordinary agent traffic. A shadow-only ceiling here
// would still allow an unplaceable caller to spawn unbounded tmux probes.
func checkBrokerProofRate(endpoint, key string) brokerRateVerdict {
	over, count := defaultBrokerLimiter.observe(key, brokerProofRatePerSecond)
	if !over {
		return brokerRateVerdict{}
	}
	if log, suppressed := defaultBrokerLimiter.shouldLogExcess(key); log {
		slog.Warn("broker: caller over the identity-proof request ceiling",
			"endpoint", endpoint, "proof_bucket", key,
			"requests_this_second", count, "limit_per_second", brokerProofRatePerSecond,
			"excess_since_last_log", suppressed,
			"action", "rejected", "module", "hooks")
	}
	return brokerRateVerdict{Reject: true}
}

// logBrokerBodyOverCap records an over-cap body.
//
// Unlike the rate ceiling, the size ceiling has no shadow mode, and that
// is a property of what it is rather than a policy choice: the daemon
// reads the body through a LimitReader, so a request past the cap has
// already been truncated by the time anyone could decide to allow it.
// There is nothing left to process, so the honest answer is a rejection —
// what this adds is that the rejection is never silent.
func logBrokerBodyOverCap(endpoint, key string, size int) {
	slog.Warn("broker: rejecting a request body over the size ceiling",
		"endpoint", endpoint, "session", key,
		"at_least_bytes", size, "limit_bytes", brokerMaxBody, "module", "hooks")
}

// enforcementWord makes a shadow-mode log line unambiguous about what
// actually happened, so nobody reads a measurement as an outage.
func enforcementWord(enforced bool) string {
	if enforced {
		return "rejected"
	}
	return "allowed (shadow mode: set broker.enforce_limits to reject)"
}
