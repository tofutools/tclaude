package agentd

import (
	"encoding/json"
	"log/slog"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Codex subscription-usage readout for the dashboard top bar (JOH-214). The
// Claude side (usage.go) reads a SQLite cache populated by Claude Code's
// statusline callback and, only when opted in, the Anthropic usage API. Codex
// has no such endpoint wired in, so the figures are lifted off the rollout
// files Codex writes locally. Hook callbacks refresh the shared SQLite cache
// from Codex's transcript_path at turn boundaries; the poller below is a
// repair path so a missed hook or daemon restart still discovers a last-known
// snapshot without making the 2-second /api/snapshot tick walk
// ~/.codex/sessions.

// codexUsageMaxAge bounds how far back a rollout snapshot may be observed and
// still feed the readout. The Claude usage cap (config.ResolvedUsageIdleTimeout,
// default 3 days) measures staleness from FetchedAt, which Claude Code's
// statusline — or the opt-in network poller — advances independently of
// activity. Codex usage instead comes from rollouts that update ONLY when
// Codex runs, so its cap is measured from the last rollout write and must be
// generous: too short and the Codex line would vanish that long after the last
// Codex turn, hiding a weekly figure that is still entirely valid. So the cap
// is the weekly window length plus margin: a snapshot older than that can only
// describe windows that have themselves already reset (caught per-window by
// codexUsageWindowFor). Within that span, each window's own resets_at is the
// real expiry, so the readout persists across idle periods and each window
// drops out exactly when it resets.
const codexUsageMaxAge = 8 * 24 * time.Hour

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

// startCodexUsagePoller refreshes the Codex usage cache on the same cadence as
// the Claude poller, until stop closes. The first refresh is a broad startup
// repair scan; later ticks only consider live Codex session ids known to
// tclaude. Mirrors startUsagePoller.
func startCodexUsagePoller(stop <-chan struct{}) {
	go func() {
		refreshCodexUsage(true)
		t := time.NewTicker(usagePollInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				refreshCodexUsage(false)
			}
		}
	}()
}

// refreshCodexUsage scans Codex rollouts for the latest rate-limit snapshot
// and stores it in the SQLite cache collectCodexUsageSnapshot reads. The
// startup pass is broad; steady-state repair is narrowed to live Codex
// conversations known to tclaude. A failed scan is expected when Codex isn't
// installed and never surfaces beyond a debug log; the snapshot just omits the
// Codex line.
func refreshCodexUsage(broad bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Debug("codex usage poller: home dir unresolved; skipping", "error", err)
		return
	}
	since := time.Now().Add(-codexUsageMaxAge)
	var u *harness.CodexUsage
	if broad {
		u, err = harness.LatestCodexUsage(home, since)
	} else {
		u, err = harness.LatestCodexUsageForConvs(home, liveCodexConvIDs(), since)
	}
	if err != nil {
		slog.Debug("codex usage poller: scan failed; dashboard omits the codex line", "error", err)
		return
	}
	if u == nil || u.Observed.IsZero() {
		return
	}
	saveCodexUsageSnapshot(u, "poller")
}

func saveCodexUsageSnapshot(u *harness.CodexUsage, source string) {
	if u == nil || u.Observed.IsZero() {
		return
	}
	data, err := json.Marshal(u)
	if err != nil {
		slog.Debug("codex usage: marshal failed", "error", err)
		return
	}
	if _, err := db.SaveCodexUsageCacheIfNewer(data, u.Observed, source); err != nil {
		slog.Debug("codex usage: cache write failed", "error", err)
	}
}

func liveCodexConvIDs() []string {
	alive, err := session.LiveTmuxSessions()
	if err != nil {
		slog.Debug("codex usage poller: tmux liveness unavailable; skipping targeted repair", "error", err)
		return nil
	}
	rows, err := db.ListSessions()
	if err != nil {
		slog.Debug("codex usage poller: session list unavailable; skipping targeted repair", "error", err)
		return nil
	}
	seen := map[string]struct{}{}
	var ids []string
	for _, r := range rows {
		if r == nil || r.Harness != harness.CodexName || r.ConvID == "" || r.TmuxSession == "" {
			continue
		}
		if _, ok := alive[r.TmuxSession]; !ok {
			continue
		}
		if _, ok := seen[r.ConvID]; ok {
			continue
		}
		seen[r.ConvID] = struct{}{}
		ids = append(ids, r.ConvID)
	}
	return ids
}

// collectCodexUsageSnapshot formats the last-known Codex rate limits for
// /api/snapshot, or returns nil — the graceful "no Codex line" state — when
// none are known, the snapshot predates codexUsageMaxAge, or every window has
// already reset. The real per-window expiry is each window's resets_at (see
// codexUsageWindowFor); the Observed-age check here is only a backstop for a
// window that arrived without a reset timestamp, so it can't linger forever.
func collectCodexUsageSnapshot() *codexDashboardUsage {
	row, err := db.LoadCodexUsageCache()
	if err != nil || row == nil {
		return nil
	}
	var u harness.CodexUsage
	if err := json.Unmarshal(row.Data, &u); err != nil {
		return nil
	}
	if u.Observed.IsZero() || time.Since(u.Observed) > codexUsageMaxAge {
		return nil
	}
	fh := codexUsageWindowFor(u.FiveHour)
	wk := codexUsageWindowFor(u.Weekly)
	if fh == nil && wk == nil {
		return nil
	}
	// Preserve the distinction between a limit Codex did not report and one
	// that it reported but has since reset. The browser hides an unreported
	// window while retaining its alignment slot; a known reset still renders
	// as the familiar 0% bar.
	if u.FiveHour != nil && fh == nil {
		fh = &usageWindow{}
	}
	if u.Weekly != nil && wk == nil {
		wk = &usageWindow{}
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
