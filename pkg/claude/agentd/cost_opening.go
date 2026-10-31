package agentd

import (
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

const (
	// costOpeningCheckInterval is how often the refresher asks whether the
	// stored opening is due. The check reads no ledger rows, and the opening
	// holds a day of margin, so a rollover never leaves it past the read limit.
	costOpeningCheckInterval = 10 * time.Minute
	// costOpeningMinRefreshGap bounds rebuilds when something keeps changing
	// old rows: readers stay exact on the full read in the meantime.
	costOpeningMinRefreshGap = 30 * time.Second
)

var costOpeningRefreshRequests = make(chan struct{}, 1)

// costOpeningRefresherRunning keeps requests from queueing where nothing
// drains them, such as a test driving the mux alone.
var costOpeningRefresherRunning atomic.Bool

// requestCostOpeningRefresh asks the refresher to rebuild soon. A reader that
// found the opening unusable has already fallen back to the exact read.
func requestCostOpeningRefresh() {
	if !costOpeningRefresherRunning.Load() {
		return
	}
	select {
	case costOpeningRefreshRequests <- struct{}{}:
	default:
	}
}

// startCostOpeningRefresher keeps db's stored cost opening current until stop
// closes: at startup, when the day moves on, and on request. This is where
// history is read, so that the poll and the default Costs view need not.
func startCostOpeningRefresher(stop <-chan struct{}) {
	costOpeningRefresherRunning.Store(true)
	go func() {
		defer costOpeningRefresherRunning.Store(false)
		// Always at startup: first use after an upgrade, days offline, and an
		// audit of what the previous run stored.
		refreshCostOpening(time.Now())
		last := time.Now()
		t := time.NewTicker(costOpeningCheckInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				logCostReadCounts()
			case <-costOpeningRefreshRequests:
				select {
				case <-stop:
					return
				case <-time.After(costOpeningMinRefreshGap - time.Since(last)):
				}
			}
			now := time.Now()
			due, err := db.CostOpeningDue(now)
			if err != nil {
				slog.Warn("cost opening: check failed", "error", err)
				continue
			}
			if due {
				refreshCostOpening(now)
				last = time.Now()
			}
		}
	}()
}

// logCostReadCounts is how a live install shows whether the read limit holds:
// full_history should stay near zero, and a steady count means something keeps
// marking the opening stale.
func logCostReadCounts() {
	counts := db.TakeCostReadCounts()
	if counts == (db.CostReadCounts{}) {
		return
	}
	logAt := slog.Info
	if counts.FullHistory > 0 {
		logAt = slog.Warn
	}
	logAt("cost reads since the last report", "from_opening", counts.FromOpening,
		"full_history", counts.FullHistory, "older_range", counts.OlderRange, "lifetime_rule", counts.LifetimeRule)
}

func refreshCostOpening(now time.Time) {
	if err := db.MarkCostConvsBilledElsewhere(""); err != nil {
		slog.Warn("cost opening: mark billed conversations", "error", err)
	}
	if err := db.RefreshCostOpening(now); err != nil {
		slog.Warn("cost opening: refresh failed; cost reads stay on the full history", "error", err)
		return
	}
	slog.Info("cost opening: rebuilt from the ledger")
}
