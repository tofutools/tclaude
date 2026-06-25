package agentd

import (
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/conv"
)

// retiredCleanupInterval is how often the long-horizon retired-agent
// cleanup sweep runs (JOH-269). Retired entities accrue slowly, and the
// retention window is ~1 year, so a frequent sweep is unnecessary — but
// a half-hourly cadence keeps a long-running daemon's exhaust bounded
// without the human having to restart. The first sweep fires immediately
// at startup so a daemon that was offline over a threshold boundary
// catches up without waiting the interval.
const retiredCleanupInterval = 30 * time.Minute

// startRetiredAgentCleanup runs the long-horizon retired-agent cleanup
// sweep in its own goroutine, permanently deleting agents/conversations
// that have been retired longer than the configured window
// (config.RetiredCleanupConfig). The feature is OPT-IN: an absent /
// disabled block keeps today's keep-retired-forever behaviour, so a fresh
// daemon never deletes anything. Shares the daemon-wide stop channel. The
// config is re-read each tick, so toggling the feature takes effect
// without a restart.
func startRetiredAgentCleanup(stop <-chan struct{}) {
	go func() {
		runRetiredAgentCleanup(time.Now())
		t := time.NewTicker(retiredCleanupInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case now := <-t.C:
				runRetiredAgentCleanup(now)
			}
		}
	}()
}

// runRetiredAgentCleanup performs one sweep: it deletes every retired
// enrollment whose retired_at is older than the configured window, via
// the same conv.DeleteConvByID teardown the dashboard / CLI delete paths
// use (DB purge across every agent_* table + the .jsonl + session-env).
//
// Safety:
//   - OPT-IN: returns immediately unless the human enabled the sweep.
//   - A genuine config-load failure (corrupt config.json — NOT a missing
//     file, which Load returns as defaults with no error) SKIPS the
//     sweep: deleting conversations against a guessed policy is
//     unrecoverable, so a broken config must never trigger a delete.
//   - The eligibility cut compares the PARSED retired_at (a time.Time)
//     against the cutoff in Go, never a SQL ORDER BY / comparison on the
//     raw RFC3339Nano text — variable fractional-second widths make that
//     string not sort as time. The retired set is bounded (this sweep is
//     what keeps it so), so iterating it in memory is cheap.
//   - A retired conv with a still-live tmux session is skipped, mirroring
//     handleAgentDelete's refuse-while-alive guard: we never race a live
//     pane's writes to its own .jsonl during teardown. (A year-retired
//     agent being online is near-impossible, but the guard is cheap.)
func runRetiredAgentCleanup(now time.Time) {
	cfg, err := config.Load()
	if err != nil {
		slog.Warn("retired cleanup: skipping sweep; config load failed", "error", err)
		return
	}
	enabled, afterDays := cfg.ResolvedRetiredCleanup()
	if !enabled {
		return // opt-in feature is off — keep retired entities forever
	}
	cutoff := now.AddDate(0, 0, -afterDays)

	retired, err := db.ListRetiredAgents()
	if err != nil {
		slog.Warn("retired cleanup: list retired agents failed", "error", err)
		return
	}

	var deleted int
	for _, e := range retired {
		// RetiredAt is the parsed time.Time; a zero value (a row that
		// somehow lost its stamp) is never old enough — skip it rather
		// than treat the zero time as "infinitely old" and delete it.
		if e.RetiredAt.IsZero() || !e.RetiredAt.Before(cutoff) {
			continue
		}
		if isConvOnline(e.ConvID) {
			slog.Info("retired cleanup: skipping still-online retired conv", "conv", e.ConvID)
			continue
		}
		// Re-read the row's state immediately before the irreversible
		// delete. ListRetiredAgents above is a one-shot snapshot; between it
		// and this delete a concurrent reinstate (the dashboard's
		// "reinstate" button → db.ReinstateAgent / PromoteAgent) could have
		// flipped this row retired→active. isConvOnline wouldn't catch a
		// just-reinstated-but-still-offline agent, so without this recheck
		// the sweep could permanently delete a freshly reinstated agent. On
		// any error (or any non-retired state) we skip — never delete on an
		// uncertain state. Cheap insurance on a no-undo path.
		if st, err := db.EnrollmentState(e.ConvID); err != nil || st != db.EnrollmentRetired {
			continue
		}
		// Log every deletion individually: the .jsonl is removed and there
		// is no undo, so the daemon log is the sole forensic record of what
		// was reaped (the aggregate line below is for at-a-glance volume).
		if _, err := conv.DeleteConvByID(e.ConvID); err != nil {
			slog.Warn("retired cleanup: delete failed", "conv", e.ConvID, "error", err)
			continue
		}
		slog.Info("retired cleanup: deleted long-retired conversation",
			"conv", e.ConvID, "retired_at", e.RetiredAt.Format(time.RFC3339))
		deleted++
	}
	if deleted > 0 {
		slog.Info("retired cleanup: deleted long-retired conversations",
			"count", deleted, "retired_before", cutoff.Format(time.RFC3339), "after_days", afterDays)
	}
}
