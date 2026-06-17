package agentd

import (
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Codex subscription-usage readout for the dashboard top bar (JOH-214). The Claude side
// (usage.go) polls the Anthropic usage API into a SQLite cache; Codex has no
// such endpoint wired in, so the figures are lifted off the rollout files
// Codex writes locally (harness.LatestCodexUsage). The poller below keeps a
// last-known snapshot in memory so the 2-second /api/snapshot tick never has
// to walk ~/.codex/sessions itself, and the snapshot collector applies the
// same staleness cap the Claude readout uses.

// codexUsageMaxAge bounds how far back a rollout snapshot may be observed and
// still feed the readout. Unlike the Claude usage cap (usageStaleAfter, 30
// min) — which is safe only because a network poller keeps that cache fresh
// regardless of activity — Codex usage comes from rollouts that update ONLY
// when Codex runs. A 30-min cap would make the Codex line vanish 30 min after
// the last Codex turn, hiding a weekly figure that is still entirely valid.
// So the cap is the weekly window length plus margin: a snapshot older than
// that can only describe windows that have themselves already reset (caught
// per-window by codexUsageWindowFor). Within that span, each window's own
// resets_at is the real expiry, so the readout persists across idle periods
// and each window drops out exactly when it resets.
const codexUsageMaxAge = 8 * 24 * time.Hour

var (
	codexUsageMu   sync.RWMutex
	codexUsageData *harness.CodexUsage // last successful scan; nil = none known
)

// codexDashboardUsage is the /api/snapshot view of the Codex account's
// subscription rate limits — a sibling of the Claude figures under
// dashboardUsage.Codex. Shapes match the Claude windows (usageWindow) so the
// dashboard renders both through one helper. Available is false / the field is
// omitted entirely when no usable Codex figures exist (Codex never run, no
// recent rollout, or data gone stale).
type codexDashboardUsage struct {
	Available bool         `json:"available"`
	FiveHour  *usageWindow `json:"five_hour,omitempty"`
	SevenDay  *usageWindow `json:"seven_day,omitempty"`
}

// startCodexUsagePoller refreshes the in-memory Codex usage snapshot on the
// same cadence as the Claude poller, until stop closes. The first refresh
// fires immediately so a freshly started daemon has data without waiting a
// full interval. Mirrors startUsagePoller.
func startCodexUsagePoller(stop <-chan struct{}) {
	go func() {
		refreshCodexUsage()
		t := time.NewTicker(usagePollInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				refreshCodexUsage()
			}
		}
	}()
}

// refreshCodexUsage scans recent Codex rollouts for the latest rate-limit
// snapshot and stores it for collectCodexUsageSnapshot to read. Only rollouts
// touched within codexUsageMaxAge are read (an older one can only describe
// already-reset windows), bounding the scan to sessions recent enough to still
// matter. A failed scan is expected when Codex isn't installed and never
// surfaces beyond a debug log; the snapshot just omits the Codex line.
func refreshCodexUsage() {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Debug("codex usage poller: home dir unresolved; skipping", "error", err)
		return
	}
	u, err := harness.LatestCodexUsage(home, time.Now().Add(-codexUsageMaxAge))
	if err != nil {
		slog.Debug("codex usage poller: scan failed; dashboard omits the codex line", "error", err)
		return
	}
	codexUsageMu.Lock()
	codexUsageData = u
	codexUsageMu.Unlock()
}

// collectCodexUsageSnapshot formats the last-known Codex rate limits for
// /api/snapshot, or returns nil — the graceful "no Codex line" state — when
// none are known, the snapshot predates codexUsageMaxAge, or every window has
// already reset. The real per-window expiry is each window's resets_at (see
// codexUsageWindowFor); the Observed-age check here is only a backstop for a
// window that arrived without a reset timestamp, so it can't linger forever.
func collectCodexUsageSnapshot() *codexDashboardUsage {
	codexUsageMu.RLock()
	u := codexUsageData
	codexUsageMu.RUnlock()
	if u == nil || u.Observed.IsZero() || time.Since(u.Observed) > codexUsageMaxAge {
		return nil
	}
	fh := codexUsageWindowFor(u.FiveHour)
	wk := codexUsageWindowFor(u.Weekly)
	// Same "show both bars or neither" rule as the Claude readout: when one
	// window is still open the other renders as a 0% bar rather than
	// vanishing, so the 5h and weekly bars never appear independently.
	fh, wk, ok := pairUsageWindows(fh, wk)
	if !ok {
		return nil
	}
	return &codexDashboardUsage{Available: true, FiveHour: fh, SevenDay: wk}
}

// codexUsageWindowFor converts a resolved Codex window into the wire shape,
// reusing the Claude readout's formatters so the two render identically. A
// window already past its reset carries stale figures (the snapshot predates
// the reset and Codex hasn't run since), so it is dropped rather than shown.
func codexUsageWindowFor(w *harness.CodexRateLimitWindow) *usageWindow {
	if w == nil {
		return nil
	}
	if !w.ResetsAt.IsZero() && time.Now().After(w.ResetsAt) {
		return nil
	}
	return &usageWindow{
		Pct:       w.UsedPercent,
		ResetsAt:  formatResetsAt(w.ResetsAt),
		Remaining: formatRemaining(w.ResetsAt),
	}
}
