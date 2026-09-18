package agentd

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// harness_ratelimit.go is the daemon's read-only rate-limit gate: given a
// harness, is its subscription usage already above the ceiling the operator
// configured in the top-level `ratelimit` block, and when does the offending
// window reset?
//
// It is the daemon-side sibling of ratelimit.WaitForRateLimit, which the task
// runner calls before starting a turn. Two differences matter. The task runner
// runs inside one Claude Code session and can block its own loop until the
// window resets; a daemon worker only decides whether to start NEW work, so
// this returns the hold rather than sleeping on it. And the task runner reads
// the Claude usage cache directly, while a daemon worker may be about to spawn
// any harness, so the reading comes from whichever cache belongs to the harness
// that will actually spend the subscription.
//
// Every read is local: the same SQLite caches the dashboard's top-bar readout
// uses. No network call is made to decide a hold, so a gate check costs one
// query and a missing, stale, or unparseable cache fails OPEN — the daemon
// keeps working rather than stalling on figures it cannot obtain.
//
// "Fails open" covers figures that cannot be obtained or read, not figures
// whose timestamps look odd. In particular a reading stamped in the FUTURE —
// the shape a backwards clock step leaves behind, since the same host writes
// and reads these caches — is still used. Its percentages are real, and a hold
// needs a percentage over the ceiling AND a reset still ahead, so the worst a
// skewed stamp costs is a hold that outlives the true reset by roughly the
// skew. Discarding the reading instead would open the spend gate on an
// exhausted subscription, which is the failure this whole gate exists to
// prevent, and would leave the gate disagreeing with the dashboard readouts,
// which judge the same caches on elapsed age alone.

// harnessUsageWindow is one harness's rolling subscription window reduced to
// what a rate-limit decision needs: how much of it is spent, when it resets,
// and which configured ceiling governs it.
type harnessUsageWindow struct {
	name     string
	pct      float64
	resetsAt time.Time
	// long marks the multi-day windows — Claude's 7-day buckets, Codex's
	// weekly, Copilot's monthly premium-request quota — which are measured
	// against ratelimit.seven_day_percent_max_used. The short windows are
	// measured against five_hour_percent_max_used. The two configured
	// ceilings are deliberately read as "the short window" and "the long
	// window" rather than as exact durations, because that is the only
	// mapping that gates a harness whose long window is not seven days.
	long bool
}

// rateLimitHold is a harness usage window sitting above its configured
// ceiling, plus the reset that ends the hold. A nil *rateLimitHold means "not
// held" — no configured ceiling, no usable reading, or nothing over the line.
type rateLimitHold struct {
	Harness   string
	Window    string
	Pct       float64
	Threshold float64
	ResetsAt  time.Time
}

// LogAttrs renders the hold for a structured log line or an audit detail.
func (h *rateLimitHold) LogAttrs() []any {
	if h == nil {
		return nil
	}
	return []any{"harness", h.Harness, "window", h.Window,
		"pct", h.Pct, "max_pct", h.Threshold, "resets_at", h.ResetsAt}
}

// rateLimitPolicy is the operator's configured ceilings plus the staleness
// grace the readings are judged by.
type rateLimitPolicy struct {
	ceiling *config.RateLimitConfig
	// idleTimeout is config.ResolvedUsageIdleTimeout, so a reading the
	// dashboard readout has already stopped trusting cannot hold work back
	// either.
	idleTimeout time.Duration
}

// loadRateLimitPolicy returns the configured ceilings, or nil when the
// operator configured none. Callers check this BEFORE working out which
// harness they would spawn: with no ceiling there is nothing to gate, and a
// default install should not pay for a resolution whose answer it discards.
func loadRateLimitPolicy() *rateLimitPolicy {
	cfg, err := config.Load()
	if err != nil {
		slog.Debug("rate limit gate: config unreadable; not holding", "error", err)
		return nil
	}
	if cfg == nil || cfg.RateLimit == nil {
		return nil
	}
	return &rateLimitPolicy{ceiling: cfg.RateLimit, idleTimeout: cfg.ResolvedUsageIdleTimeout()}
}

// harnessRateLimitHold reports the usage window that should stop the daemon
// from starting new work on harnessName, or nil when nothing does.
//
// When several windows are over their ceiling the one resetting LAST wins:
// work may only start once every exceeded window has reset, so the latest
// reset is the honest answer to "how long is this held".
//
// A window whose reset has already elapsed is ignored rather than trusted. Its
// percentage describes a window that has since rolled over, so acting on it
// would hold work back on figures that no longer exist. A window with no reset
// timestamp at all is ignored for the same reason in reverse: there would be no
// point at which to resume, and an indefinite hold is worse than an ungated
// spawn the operator can see and stop.
func harnessRateLimitHold(policy *rateLimitPolicy, harnessName string, now time.Time) *rateLimitHold {
	if policy == nil || policy.ceiling == nil {
		return nil
	}
	var hold *rateLimitHold
	for _, w := range harnessUsageWindows(harnessName, policy.idleTimeout, now) {
		threshold := policy.ceiling.FiveHourPercentMaxUsed
		if w.long {
			threshold = policy.ceiling.SevenDayPercentMaxUsed
		}
		if w.pct <= threshold || !w.resetsAt.After(now) {
			continue
		}
		if hold != nil && !w.resetsAt.After(hold.ResetsAt) {
			continue
		}
		hold = &rateLimitHold{Harness: harnessName, Window: w.name,
			Pct: w.pct, Threshold: threshold, ResetsAt: w.resetsAt}
	}
	return hold
}

// harnessUsageWindows returns the last-known subscription windows for one
// harness, or nil when the harness has no account-wide rolling limit tclaude
// can observe.
//
// OpenCode is that case today: it runs against whatever provider keys the
// operator configured, so there is no single subscription whose percentage a
// gate could read. Its spawns are therefore never held, which is the correct
// degradation rather than a gap — there is no limit to respect.
//
// Each harness's staleness rule matches the one its dashboard readout uses, so
// a figure the top bar has already stopped trusting can never hold work back.
func harnessUsageWindows(harnessName string, idleTimeout time.Duration, now time.Time) []harnessUsageWindow {
	switch harnessName {
	case harness.DefaultName:
		return claudeUsageWindows(idleTimeout, now)
	case harness.CodexName:
		return codexUsageWindows(now)
	case harness.CopilotName:
		return copilotUsageWindows(idleTimeout, now)
	}
	return nil
}

// claudeUsageWindows reads the Claude subscription buckets from the usage
// cache Claude Code's statusline callback (and the opt-in Anthropic usage
// poll) maintains. The 7-day Sonnet bucket is included alongside the general
// one because it limits the account just as hard — the same reason
// ratelimit.WaitForRateLimit waits on it.
func claudeUsageWindows(idleTimeout time.Duration, now time.Time) []harnessUsageWindow {
	row, err := db.LoadUsageCache()
	if err != nil || row == nil {
		return nil
	}
	var cached usageapi.CachedUsage
	if err := json.Unmarshal(row.Data, &cached); err != nil {
		slog.Debug("rate limit gate: claude usage cache unparseable; not holding", "error", err)
		return nil
	}
	if idleTimeout <= 0 {
		idleTimeout = usageStaleAfter
	}
	if cached.FetchedAt.IsZero() || now.Sub(cached.FetchedAt) > idleTimeout {
		return nil
	}
	var out []harnessUsageWindow
	for _, b := range []struct {
		name   string
		bucket *usageapi.CachedBucket
		long   bool
	}{
		{"five_hour", cached.FiveHour, false},
		{"seven_day", cached.SevenDay, true},
		{"seven_day_sonnet", cached.SevenDaySonnet, true},
	} {
		if b.bucket == nil {
			continue
		}
		out = append(out, harnessUsageWindow{name: b.name, pct: b.bucket.Pct, resetsAt: b.bucket.ResetsAt, long: b.long})
	}
	return out
}

// codexUsageWindows reads the Codex windows off the cached rollout snapshot,
// under the same generous age cap the dashboard applies: the figures only
// advance while Codex runs, so a quiet week must not invalidate a weekly
// reading that is still entirely valid.
func codexUsageWindows(now time.Time) []harnessUsageWindow {
	row, err := db.LoadCodexUsageCache()
	if err != nil || row == nil {
		return nil
	}
	var u harness.CodexUsage
	if err := json.Unmarshal(row.Data, &u); err != nil {
		slog.Debug("rate limit gate: codex usage cache unparseable; not holding", "error", err)
		return nil
	}
	if u.Observed.IsZero() || now.Sub(u.Observed) > codexUsageMaxAge {
		return nil
	}
	var out []harnessUsageWindow
	if u.FiveHour != nil {
		out = append(out, harnessUsageWindow{name: "five_hour", pct: u.FiveHour.UsedPercent, resetsAt: u.FiveHour.ResetsAt})
	}
	if u.Weekly != nil {
		out = append(out, harnessUsageWindow{name: "weekly", pct: u.Weekly.UsedPercent, resetsAt: u.Weekly.ResetsAt, long: true})
	}
	return out
}

// copilotUsageWindows reads GitHub Copilot's monthly premium-request quota.
// It is a month long rather than a week, so it is governed by the long-window
// ceiling; an operator who sets that ceiling low enough to trip on a monthly
// quota is asking for exactly this hold, and the alternative — spending a
// finite paid allowance with no gate at all — is the worse reading of the
// configuration.
func copilotUsageWindows(idleTimeout time.Duration, now time.Time) []harnessUsageWindow {
	// The quota lives in the retained subscription-usage history rather than a
	// single-row cache, and the dashboard's combined loader is the only reader
	// of its latest row. Borrowing it keeps one definition of "the current
	// monthly window" at the cost of loading two cache rows nobody here reads.
	_, _, row, _, err := db.LoadDashboardUsageCaches()
	if err != nil || row == nil || row.ObservedAt.IsZero() {
		return nil
	}
	if idleTimeout <= 0 {
		idleTimeout = usageStaleAfter
	}
	if now.Sub(row.ObservedAt) > idleTimeout {
		return nil
	}
	// The stored resets_at is deliberately NOT trusted: older Copilot samples
	// recorded the CLI account snapshot's raw timestamp_utc mislabeled as
	// resetDate, which reads as an already-elapsed window and would silently
	// drop the quota from the gate on a legacy row. The allowance boundary is
	// documented independently — the first day of the next calendar month at
	// 00:00 UTC — so it is derived from the observation, exactly as the
	// dashboard readout (collectCopilotUsageSnapshot) and the usage history
	// series both do.
	resetsAt := copilotMonthlyResetAt(row.ObservedAt)
	return []harnessUsageWindow{{name: "monthly", pct: row.UsedPercent, resetsAt: resetsAt, long: true}}
}
