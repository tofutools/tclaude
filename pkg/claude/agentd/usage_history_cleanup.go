package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Usage history grows slowly (at most one provider snapshot per 15 minutes),
// so a daily sweep is ample. It fires immediately at startup as well, which
// catches up after agentd has been offline.
const subscriptionUsageCleanupInterval = 24 * time.Hour

func startSubscriptionUsageCleanup(stop <-chan struct{}) {
	go func() {
		runSubscriptionUsageCleanup(time.Now())
		t := time.NewTicker(subscriptionUsageCleanupInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				runSubscriptionUsageCleanup(now)
			}
		}
	}()
}

func runSubscriptionUsageCleanup(now time.Time) {
	cutoff := now.Add(-db.DefaultSubscriptionUsageRetention)
	n, err := db.PruneSubscriptionUsageHistory(cutoff)
	if err != nil {
		slog.Warn("subscription usage cleanup: prune failed", "error", err, "cutoff", cutoff)
		return
	}
	if n > 0 {
		slog.Info("subscription usage cleanup: pruned old samples",
			"count", n, "older_than", cutoff.Format(time.RFC3339))
	}
}
