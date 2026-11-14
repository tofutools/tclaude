package agentd

import (
	"log/slog"
	"sync"
	"time"
)

// The spawn dialog's harness catalog is served from memory and rebuilt in the
// background, never on the request that reads it.
//
// buildHarnessCatalog walks every harness descriptor, and some descriptors
// reach the host: pi's model list runs `pi --list-models` (a Node process,
// ~300-400 ms) whenever its 5-minute cache expires, and OpenCode's did the
// same with `opencode models` before it moved to a background refresh. On the
// dashboard's 2-second /api/snapshot poll, an inline rebuild turns every such
// expiry into a stalled poll. Caching the whole catalog here, instead of
// chasing each descriptor, keeps the poll path cheap even when a future
// descriptor starts doing work on its read path again.
//
// Refresh is read-triggered, like OpenCode's model catalog: a read of a
// catalog older than harnessCatalogRefreshAfter serves the cached copy and
// starts one background rebuild. A dashboard that is open therefore sees a
// change (a harness opted in, a model list refreshed) one poll later, and a
// daemon with no dashboard open never rebuilds at all.
//
// Only the very first read, with nothing cached yet, builds inline; the daemon
// warms the cache at startup so that read is normally already served.

// harnessCatalogRefreshAfter is how old the cached catalog may be before a
// read starts a background rebuild.
const harnessCatalogRefreshAfter = time.Second

var harnessCatalogCache = struct {
	sync.Mutex
	// enabled is false only in tests that need every read to see a change
	// they just made (see DisableHarnessCatalogCacheForTest).
	enabled    bool
	value      []dashboardHarness
	at         time.Time
	refreshing bool
	build      func() []dashboardHarness
}{enabled: true, build: buildHarnessCatalog}

// cachedHarnessCatalog returns the last built harness catalog, starting a
// background rebuild when it is stale. The returned slice is shared: callers
// serialize it and must not modify it.
func cachedHarnessCatalog() []dashboardHarness {
	c := &harnessCatalogCache
	c.Lock()
	defer c.Unlock()
	if !c.enabled {
		return c.build()
	}
	if c.at.IsZero() {
		// Nothing to serve yet. Holding the lock makes concurrent first
		// readers wait for this one build instead of each starting their own.
		c.value = c.build()
		c.at = time.Now()
		return c.value
	}
	if !c.refreshing && time.Since(c.at) >= harnessCatalogRefreshAfter {
		c.refreshing = true
		go refreshHarnessCatalog(c.build)
	}
	return c.value
}

// refreshHarnessCatalog rebuilds the catalog and publishes it. It runs outside
// any HTTP handler, so a panic is recovered and logged rather than taking the
// daemon down; the previous catalog stays in place.
func refreshHarnessCatalog(build func() []dashboardHarness) {
	c := &harnessCatalogCache
	var fresh []dashboardHarness
	ok := false
	defer func() {
		if r := recover(); r != nil {
			slog.Error("harness catalog refresh panicked", "panic", r, "module", "agentd")
		}
		c.Lock()
		defer c.Unlock()
		c.refreshing = false
		// Stamped even after a panic, so a failing rebuild is retried at the
		// refresh interval rather than on every read.
		c.at = time.Now()
		if ok {
			c.value = fresh
		}
	}()
	fresh = build()
	ok = true
}

// warmHarnessCatalog builds the catalog once in the background at daemon
// start, so the first dashboard poll does not pay for it.
func warmHarnessCatalog() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("harness catalog warm-up panicked", "panic", r, "module", "agentd")
			}
		}()
		cachedHarnessCatalog()
	}()
}
