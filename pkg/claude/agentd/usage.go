package agentd

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
	// TotalCostUSD is the month-to-date API cost summed across every
	// session row in the DB (sessions.cost_usd — recorded only for
	// API/enterprise-priced sessions, so subscription accounts stay at
	// 0). Independent of Available: an API-billing account has cost but
	// no rolling windows, and the dashboard then shows this figure
	// instead of "usage: n/a". 0 means "no cost data" and renders
	// nothing.
	TotalCostUSD float64 `json:"total_cost_usd,omitempty"`
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
	out := dashboardUsage{TotalCostUSD: monthToDateCost()}
	cached := usageapi.Peek()
	if cached == nil || cached.FetchedAt.IsZero() || time.Since(cached.FetchedAt) > usageStaleAfter {
		return out
	}
	fh := usageWindowFor(cached.FiveHour)
	sd := usageWindowFor(cached.SevenDay)
	if fh == nil && sd == nil {
		return out
	}
	out.Available = true
	out.FiveHour = fh
	out.SevenDay = sd
	return out
}

// monthToDateCost sums the recorded API cost since the start of the
// current calendar month (local time), through the same
// session_cost_daily aggregation the Costs tab uses — so the top-bar
// headline always matches the tab's "this month" total. A read
// failure degrades to 0 — the dashboard simply shows no cost token —
// since this is a display-only figure on the same snapshot path that
// already tolerates missing subscription data.
func monthToDateCost() float64 {
	rows, err := db.AllCostDailyRows()
	if err != nil {
		slog.Debug("usage snapshot: read daily costs failed; omitting cost readout", "error", err)
		return 0
	}
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	return sumCostDeltas(costDeltasFromRows(rows), monthStart.Format(costDayKey), "")
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
