package agentd

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
)

// usagePollInterval is how often agentd checks whether it should refresh the
// Claude subscription usage figures. The Anthropic API refresh itself is
// opt-in (config usage.poll_anthropic_api); by default the dashboard relies on
// Claude Code's statusline callback plus the cached last-known reading.
const usagePollInterval = 3 * time.Minute

// usageStaleAfter is the fallback cap on how old a cached usage reading may
// be before the dashboard treats it as unavailable, used only for the
// defensive idleTimeout <= 0 branch in collectUsageSnapshot. The live
// snapshot path passes the configured grace instead —
// config.ResolvedUsageIdleTimeout, default config.DefaultUsageIdleTimeout
// (3 days).
//
// The cap can't be tiny: the Claude readout is fed by Claude Code's statusline
// callback (only while a session runs) and, when explicitly enabled, a periodic
// Anthropic usage-API poll. A failed poll keeps the cached figures but does NOT
// advance their FetchedAt clock (see usageapi.stampLastAttempt), so an
// aggressive cap would hide a perfectly good weekly reading the same night,
// leaving only Codex on the top bar — the "hiding too soon" the grace window
// fixes.
const usageStaleAfter = config.DefaultUsageIdleTimeout

var usageGetCached = usageapi.GetCached

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
	// TodayCostUSD is the API cost recorded so far on the current local
	// calendar day — the same delta walk as TotalCostUSD, windowed to
	// today. Always ≤ TotalCostUSD. The top bar shows it as a "(today)"
	// figure beside the "(mtd)" headline; 0 (nothing spent today, or a
	// subscription account) renders no today figure.
	TodayCostUSD float64 `json:"today_cost_usd,omitempty"`
	// Codex is the Codex account's subscription usage, lifted from Codex's
	// local rollout files (see codex_usage.go). nil — the field is omitted
	// — when Codex isn't installed or has no recent usage data, so the top
	// bar shows only the Claude readout. When present, the dashboard
	// renders a labelled "Claude" / "Codex" two-line readout. The fields
	// above stay Claude's (historical names), so an existing client that
	// doesn't know about Codex is unaffected.
	Codex *codexDashboardUsage `json:"codex,omitempty"`
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

// startUsagePoller conditionally refreshes the Claude subscription-usage cache
// on a timer until stop closes (the daemon-wide quit channel). The first check
// fires immediately; no Anthropic usage API call is made unless
// usage.poll_anthropic_api is true in config.json.
func startUsagePoller(stop <-chan struct{}) {
	go func() {
		maybeRefreshUsage()
		t := time.NewTicker(usagePollInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				maybeRefreshUsage()
			}
		}
	}()
}

// maybeRefreshUsage reloads the config and performs the Anthropic usage API
// refresh only when the user has explicitly opted in. Reloading here lets the
// dashboard Config tab enable/disable polling without restarting agentd.
func maybeRefreshUsage() {
	cfg, err := config.Load()
	if err != nil {
		slog.Debug("usage poller: config load failed; skipping Anthropic usage API refresh", "error", err)
		return
	}
	if !cfg.PollAnthropicUsageAPI() {
		return
	}
	refreshUsage()
}

// refreshUsage calls usageapi.GetCached purely for its side effect:
// GetCached refreshes the SQLite usage_cache row via the Anthropic
// OAuth usage API, but only when its own 5-minute cache is stale, so
// API hits stay rare. The returned value is discarded — the snapshot
// handler reads the cache row directly via collectUsageSnapshot. A
// failed refresh is expected ("sometimes not available") and never
// surfaces beyond a debug log; the snapshot just reports available=false.
func refreshUsage() {
	if _, err := usageGetCached(); err != nil {
		slog.Debug("usage poller: refresh failed; dashboard falls back to n/a", "error", err)
	}
}

// collectUsageSnapshot reads the last-known subscription usage figures
// from the SQLite cache and formats them for /api/snapshot. It never
// makes a network call (usageapi.Peek is a pure DB read), so the
// snapshot handler stays cheap. The 5h and 7d windows are surfaced as a
// pair or not at all (see pairUsageWindows): when either is live the
// other rides along, reset/absent reading as a 0% bar. Returns
// Available=false — the graceful "n/a" state — when the cache is missing,
// has gone stale, or carries no live rolling-limit window at all (an
// API-billing account, or a subscription account idle long enough that
// both windows have reset).
func collectUsageSnapshot(idleTimeout time.Duration) dashboardUsage {
	out := dashboardUsage{TotalCostUSD: monthToDateCost(), TodayCostUSD: todayCost(), Codex: collectCodexUsageSnapshot()}
	if idleTimeout <= 0 {
		idleTimeout = usageStaleAfter
	}
	cached := usageapi.Peek()
	if cached == nil || cached.FetchedAt.IsZero() || time.Since(cached.FetchedAt) > idleTimeout {
		return out
	}
	now := time.Now()
	fh := liveUsageWindow(cached.FiveHour, now)
	sd := liveUsageWindow(cached.SevenDay, now)
	fh, sd, ok := pairUsageWindows(fh, sd)
	if !ok {
		return out
	}
	out.Available = true
	out.FiveHour = fh
	out.SevenDay = sd
	return out
}

// liveUsageWindow returns the wire shape for a cached bucket while it still
// carries meaningful, current usage, or nil to drop it. A window is live
// when either:
//
//   - its reset lies in the future — the current rolling period, even at a
//     genuine 0% the account simply hasn't spent into yet (so a quiet-but-
//     current account shows "0%" instead of "n/a"); or
//   - it has NO reset timestamp but a nonzero percent — real usage whose
//     reset the source didn't report. The Anthropic weekly bucket in
//     particular often arrives with a percent but an empty resets_at; the
//     old "future reset required" gate discarded it, which is exactly why
//     the whole Claude readout vanished overnight even though the week still
//     had usage. This mirrors the Codex readout (codexUsageWindowFor), which
//     likewise only drops a window on a KNOWN, elapsed reset.
//
// A bucket whose reset is KNOWN and already elapsed is dropped (returns
// nil): its percent is now stale — the window reset — so the caller renders
// it as a 0% bar (see pairUsageWindows), e.g. a 5h window whose 5 hours have
// elapsed reads as 0 rather than its now-stale percentage. An absent bucket,
// or a reset-less one at 0%, is likewise dropped (nothing to say).
func liveUsageWindow(b *usageapi.CachedBucket, now time.Time) *usageWindow {
	if b == nil {
		return nil
	}
	if b.ResetsAt.After(now) {
		return usageWindowFor(b)
	}
	// No future reset. Keep it only when the reset is entirely absent yet
	// there's real usage to show; a known-but-elapsed reset means the window
	// genuinely reset and its percent is stale.
	if b.ResetsAt.IsZero() && b.Pct > 0 {
		return usageWindowFor(b)
	}
	return nil
}

// pairUsageWindows enforces the dashboard's "show both bars or neither"
// rule. Given the live 5h and 7d windows (nil meaning reset, absent, or
// zero), it returns the pair to render and whether anything is worth
// showing: when at least one window is live, both come back with a missing
// one zero-filled (a 0% bar) so the 5h and 7d bars never appear or vanish
// independently; when neither is live it reports ok=false and the caller
// degrades to the muted "usage: n/a" (or cost-only) state.
func pairUsageWindows(fh, sd *usageWindow) (outFh, outSd *usageWindow, ok bool) {
	if fh == nil && sd == nil {
		return nil, nil, false
	}
	return usageWindowOrZero(fh), usageWindowOrZero(sd), true
}

// usageWindowOrZero returns w, or a zeroed window (0%, no reset/remaining)
// when w is nil, so a reset or missing window renders as a 0% bar instead
// of disappearing.
func usageWindowOrZero(w *usageWindow) *usageWindow {
	if w != nil {
		return w
	}
	return &usageWindow{}
}

// monthToDateCost sums the recorded API cost since the start of the
// current calendar month (local time). The aggregation runs DB-side
// (SumCostSinceDay) because this sits on the 2s snapshot tick — but it
// is the closed form of the same delta walk the Costs tab performs, so
// the top-bar headline always matches the tab's "this month" total —
// TestCostDeltasFromRows and TestSumCostSinceDay pin both sides to one
// shared fixture. A read
// failure degrades to 0 — the dashboard simply shows no cost token —
// since this is a display-only figure on the same snapshot path that
// already tolerates missing subscription data.
func monthToDateCost() float64 {
	now := time.Now()
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	total, err := db.SumCostSinceDay(monthStart.Format(costDayKey))
	if err != nil {
		slog.Debug("usage snapshot: sum daily costs failed; omitting cost readout", "error", err)
		return 0
	}
	return total
}

// todayCost sums the API spend recorded so far on the current local
// calendar day — the same DB-side windowed delta as monthToDateCost,
// with the window opening at midnight today instead of the first of the
// month. It is always ≤ monthToDateCost and equals the Costs tab's
// today bar. Shares monthToDateCost's rationale: it rides the 2s
// snapshot tick, so the aggregation runs DB-side, and a read failure
// degrades to 0 (the top bar simply shows no "(today)" figure) since
// this is display-only.
func todayCost() float64 {
	total, err := db.SumCostSinceDay(time.Now().Format(costDayKey))
	if err != nil {
		slog.Debug("usage snapshot: sum today's costs failed; omitting today readout", "error", err)
		return 0
	}
	return total
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
