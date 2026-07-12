package agentd

import (
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/session"
)

// liveTmuxCacheTTL bounds how long a cached tmux-liveness snapshot is reused
// before the next caller re-probes. A single tick's parallel fan-out clusters
// its cache reads within tens of ms, so 200ms still coalesces a whole tick onto
// one probe, while keeping the worst-case staleness an *unsynchronized* consumer
// can observe small: a second dashboard client polling on its own tick phase, or
// a manual refresh right after a spawn/retire, sees at most 200ms of stale
// liveness (vs 500ms at a longer TTL). A pane spawned or killed mid-window may
// therefore be up to one TTL (~200ms) late in the alive set — comfortably within
// the 2s poll cadence. If a stalled straggler re-probes just past the window it
// costs one extra ~5ms fork, which is acceptable.
const liveTmuxCacheTTL = 200 * time.Millisecond

// tmuxSessionCache coalesces session.LiveTmuxSessions probes across the
// dashboard's parallel poll handlers behind a short TTL. Each 2s tick fires up
// to parallel requests — /api/snapshot plus the Groups tab's paginated lists
// — and each needs the live tmux set; uncached, each forks its own `tmux ls`
// (~5-15ms fork+exec on macOS, measured tmux_ls median 4.92ms). This cache
// serves a shared snapshot for the TTL and, because get() holds the lock across
// the probe, funnels concurrent cold misses onto ONE fork rather than racing
// into three.
//
// The returned map is SHARED across all concurrent callers and MUST be treated
// as read-only. The dashboard's liveness consumers (isConvOnlineInSessions,
// stateForConvInSessions) only ever read it via map lookup, so no defensive
// copy is made.
//
// Error semantics: a probe error is cached (together with its nil/empty map)
// for the TTL, so a wedged tmux server yields ONE failed fork per TTL rather
// than a fork-storm; the next call after expiry re-probes, so a transient
// failure self-heals within one TTL and never sticks permanently. Callers keep
// today's behavior of treating an error as an empty alive set (they discard the
// error), so nothing user-visible changes on the error path.
type tmuxSessionCache struct {
	ttl   time.Duration
	now   func() time.Time
	probe func() (map[string]struct{}, error)

	mu       sync.Mutex
	valid    bool
	expires  time.Time
	sessions map[string]struct{}
	err      error
}

func newTmuxSessionCache(ttl time.Duration, now func() time.Time, probe func() (map[string]struct{}, error)) *tmuxSessionCache {
	return &tmuxSessionCache{ttl: ttl, now: now, probe: probe}
}

// get returns the cached alive set while it is still within the TTL, otherwise
// re-probes. The lock is intentionally held across the probe so the tick's
// concurrent cold callers coalesce onto one fork: the first caller probes while
// the rest block, then each finds the just-populated entry on the double-check
// the held lock provides. A ttl of 0 makes every call re-probe (cache
// transparent) — used by tests to keep production freshness semantics.
func (c *tmuxSessionCache) get() (map[string]struct{}, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.now().Before(c.expires) {
		return c.sessions, c.err
	}
	c.sessions, c.err = c.probe()
	c.valid = true
	c.expires = c.now().Add(c.ttl)
	return c.sessions, c.err
}

// liveTmuxCache is the daemon-wide cache backing cachedLiveTmuxSessions.
var liveTmuxCache = newTmuxSessionCache(liveTmuxCacheTTL, time.Now, session.LiveTmuxSessions)

// cachedLiveTmuxSessions returns the live tmux session set through the
// short-TTL coalescing cache. The dashboard poll handlers use this instead of
// session.LiveTmuxSessions so a tick's parallel requests share one probe.
// Short-lived CLI processes must keep calling session.LiveTmuxSessions
// directly — the cache only pays off inside the long-lived polling daemon.
func cachedLiveTmuxSessions() (map[string]struct{}, error) {
	return liveTmuxCache.get()
}
