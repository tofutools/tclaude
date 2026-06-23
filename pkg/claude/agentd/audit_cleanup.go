package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// auditLogCleanupInterval is how often the audit-log retention sweep
// runs (JOH-268). Audit rows accumulate slowly (one per command), so an
// hourly sweep keeps the table bounded without busy-work. The first sweep
// fires immediately at startup so a restart catches up without waiting an
// hour.
const auditLogCleanupInterval = 1 * time.Hour

// startAuditLogCleanup runs the audit-log retention sweep in its own
// goroutine, deleting rows older than the configured retention window
// (config.AuditConfig; default DefaultAuditRetentionDays). A negative
// configured retention disables pruning ("keep forever"). Shares the
// daemon-wide stop channel. The config is re-read each tick, so changing
// the retention takes effect without a restart.
func startAuditLogCleanup(stop <-chan struct{}) {
	go func() {
		runAuditLogCleanup(time.Now())
		t := time.NewTicker(auditLogCleanupInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				runAuditLogCleanup(now)
			}
		}
	}()
}

func runAuditLogCleanup(now time.Time) {
	// A genuine config-load failure (corrupt / unreadable config.json —
	// NOT a missing file, which Load returns as defaults with no error)
	// must not fall back to the default retention and prune: the operator
	// may have configured a longer window or "keep forever", and deleting
	// against a guessed policy is unrecoverable. Skip this sweep instead.
	cfg, err := config.Load()
	if err != nil {
		slog.Warn("audit cleanup: skipping prune; config load failed", "error", err)
		return
	}
	days, prune := cfg.ResolvedAuditRetentionDays()
	if !prune {
		return // retention disabled — keep forever
	}
	cutoff := now.AddDate(0, 0, -days)
	n, err := db.PruneAuditLog(cutoff)
	if err != nil {
		slog.Warn("audit cleanup: prune failed", "error", err, "cutoff", cutoff)
		return
	}
	if n > 0 {
		slog.Info("audit cleanup: pruned old rows",
			"count", n, "older_than", cutoff.Format(time.RFC3339))
	}
}
