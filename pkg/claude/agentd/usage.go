package agentd

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
)

// usagePollInterval is how often agentd refreshes the subscription
// usage figures. The dashboard snapshot reads a cached blob (see
// collectUsageSnapshot); this poller's only job is to keep that blob
// fresh via usageapi.GetCached — which itself only hits the network
// when its own 5-minute cache is stale — so the readout stays current
// even when no Claude Code statusbar is running to populate it.
const usagePollInterval = 3 * time.Minute

// usageStaleAfter caps how old a cached usage reading may be before the
// dashboard treats it as unavailable. Comfortably larger than
// usagePollInterval so a healthy poller never trips it, yet small
// enough that a genuinely dead source (usage API down, no statusbar
// running) degrades to a muted "n/a" rather than showing figures from
// hours ago.
const usageStaleAfter = 30 * time.Minute

// dashboardUsage is the /api/snapshot view of the account's
// subscription usage limits — one readout for the whole dashboard, not
// one per agent. Available is false when no usable figures could be
// obtained (no cache, an API-billing account with no rolling windows,
// or data gone stale); the dashboard then renders a muted "usage: n/a"
// instead of a broken or error state.
type dashboardUsage struct {
	Available bool         `json:"available"`
	FiveHour  *usageWindow `json:"five_hour,omitempty"`
	SevenDay  *usageWindow `json:"seven_day,omitempty"`
}

// usageWindow is one rolling-limit bucket: percent consumed plus the
// time until it resets. Remaining is pre-formatted ("2h16m", "5d9h",
// "reset") so the dashboard renders it verbatim, mirroring the
// statusbar; ResetsAt is the raw RFC3339 timestamp for any client-side
// recomputation between snapshot polls.
type usageWindow struct {
	Pct       float64 `json:"pct"`
	ResetsAt  string  `json:"resets_at,omitempty"`
	Remaining string  `json:"remaining"`
}

// startUsagePoller refreshes the subscription-usage cache on a timer
// until stop closes (the daemon-wide quit channel). The first refresh
// fires immediately so a freshly started daemon has data without
// waiting a full interval.
func startUsagePoller(stop <-chan struct{}) {
	go func() {
		refreshUsage()
		t := time.NewTicker(usagePollInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				refreshUsage()
			}
		}
	}()
}

// refreshUsage calls usageapi.GetCached purely for its side effect:
// GetCached refreshes the SQLite usage_cache row via the Anthropic
// OAuth usage API, but only when its own 5-minute cache is stale, so
// API hits stay rare. The returned value is discarded — the snapshot
// handler reads the cache row directly via collectUsageSnapshot. A
// failed refresh is expected ("sometimes not available") and never
// surfaces beyond a debug log; the snapshot just reports available=false.
func refreshUsage() {
	if _, err := usageapi.GetCached(); err != nil {
		slog.Debug("usage poller: refresh failed; dashboard falls back to n/a", "error", err)
	}
}

// collectUsageSnapshot reads the last-known subscription usage figures
// from the SQLite cache and formats them for /api/snapshot. It never
// makes a network call (usageapi.Peek is a pure DB read), so the
// snapshot handler stays cheap. Returns Available=false — the graceful
// "n/a" state — when the cache is missing, carries no rolling-limit
// buckets (e.g. an API-billing account), or has gone stale.
func collectUsageSnapshot() dashboardUsage {
	cached := usageapi.Peek()
	if cached == nil || cached.FetchedAt.IsZero() || time.Since(cached.FetchedAt) > usageStaleAfter {
		return dashboardUsage{Available: false}
	}
	fh := usageWindowFor(cached.FiveHour)
	sd := usageWindowFor(cached.SevenDay)
	if fh == nil && sd == nil {
		return dashboardUsage{Available: false}
	}
	return dashboardUsage{Available: true, FiveHour: fh, SevenDay: sd}
}

// usageWindowFor converts a cached bucket into the wire shape, or nil
// when the bucket is absent.
func usageWindowFor(b *usageapi.CachedBucket) *usageWindow {
	if b == nil {
		return nil
	}
	return &usageWindow{
		Pct:       b.Pct,
		ResetsAt:  formatResetsAt(b.ResetsAt),
		Remaining: formatRemaining(b.ResetsAt),
	}
}

// formatResetsAt renders a reset timestamp as RFC3339, or "" when zero.
func formatResetsAt(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// formatRemaining renders the time until a limit window resets,
// mirroring the statusbar's resetTimer format ("4d11h", "2h30m",
// "45m") minus the ANSI colour codes. A window already past its reset
// renders "reset"; a zero timestamp renders "".
func formatRemaining(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := time.Until(t)
	if d <= 0 {
		return "reset"
	}
	days := int(d.Hours()) / 24
	h := int(d.Hours()) % 24
	m := int(d.Minutes()) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}
