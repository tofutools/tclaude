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
// touched within the staleness window are read (older ones can't beat the
// staleness cap anyway), bounding the scan to recently-active sessions. A
// failed scan is expected when Codex isn't installed and never surfaces beyond
// a debug log; the snapshot just omits the Codex line.
func refreshCodexUsage() {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Debug("codex usage poller: home dir unresolved; skipping", "error", err)
		return
	}
	u, err := harness.LatestCodexUsage(home, time.Now().Add(-usageStaleAfter))
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
// none are known, the data has gone stale past usageStaleAfter, or every
// window has already reset. The staleness check is against the rollout event's
// own timestamp (Observed), so a long-idle Codex degrades to nothing rather
// than showing hours-old figures, exactly like the Claude readout.
func collectCodexUsageSnapshot() *codexDashboardUsage {
	codexUsageMu.RLock()
	u := codexUsageData
	codexUsageMu.RUnlock()
	if u == nil || u.Observed.IsZero() || time.Since(u.Observed) > usageStaleAfter {
		return nil
	}
	fh := codexUsageWindowFor(u.FiveHour)
	wk := codexUsageWindowFor(u.Weekly)
	if fh == nil && wk == nil {
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
