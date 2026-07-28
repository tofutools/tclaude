package statusbar

import (
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usageapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// --- the host-touching half of one statusline render ---
//
// TCL-754's second half. A `tclaude-layer` agent runs behind a mount
// namespace where ~/.tclaude/data is hidden and read-only, so the eleven
// writes and four reads a statusline render performs cannot reach the real
// database. This file is the seam: everything host-touching that a render
// implies, expressed once, so the direct path and the brokered path run
// the SAME code rather than two implementations that drift.
//
// The split that makes it work:
//
//   - renderDerived is pure. Given the payload and the resolved pin, it
//     produces every number the bar renders AND every number the writes
//     record. Both callers derive it identically.
//   - applyRenderWrites plus applyCostWrite are the whole write set, in
//     the order the inline code performed it, with the usage read between
//     them where it has always been. Nothing they write affects the bar.
//   - renderFacts is the whole read set. Everything the bar needs from the
//     database and nothing else.
//
// Under brokering the daemon calls the same two functions with the same
// inputs, which is what makes "brokered" a transport difference rather
// than a behaviour difference.

// renderDerived holds the values derived from one payload that both the
// rendered bar and the recorded writes need. Deriving them once is what
// keeps the stored context percentage equal to the displayed one.
type renderDerived struct {
	// EnvPinnedWindow is the pin OBSERVED in this pane's environment, in
	// tokens. Zero when unset or unparseable. This is the value that may
	// be written back to the row; the resolved one below must not be.
	EnvPinnedWindow int64

	// EffectiveWindow is the window compaction actually fires at.
	EffectiveWindow int64

	// CtxPct is the context percentage re-based onto EffectiveWindow.
	CtxPct int

	// RawWindow is the model's REAL context window, as the payload
	// reported it. The stored snapshot keeps this one, never the
	// re-based one — the relaunch layer reads it back to re-derive a 1M
	// model's [1m] suffix.
	RawWindow int64

	// TokensIn / TokensOut are the payload's token counts.
	TokensIn, TokensOut int64

	// HasContextData records whether the payload carried any context
	// block at all. Claude Code emits renders whose context_window is
	// empty (before a turn's first API response) and writing those
	// all-zero values would clobber a good snapshot.
	HasContextData bool
}

// deriveRender computes one render's numbers from its payload and the
// already-resolved auto-compaction pin.
//
// It takes the resolved pin rather than resolving it, because resolution
// is a database read whose two callers reach the database very
// differently — and because the environment-observed half must stay
// distinguishable from the row-read half all the way to the write, so a
// window READ from the row is never written straight back to it.
func deriveRender(input StatusLineInput, observedWindow, resolvedWindow int64) renderDerived {
	d := renderDerived{EnvPinnedWindow: observedWindow}

	cw := input.ContextWindow
	d.HasContextData = cw.UsedPercentage != nil || cw.TotalInputTokens != nil ||
		cw.TotalOutputTokens != nil || cw.ContextWindowSize != nil

	rawCtxPct := 0.0
	if cw.UsedPercentage != nil {
		rawCtxPct = *cw.UsedPercentage
	}
	if cw.ContextWindowSize != nil {
		d.RawWindow = *cw.ContextWindowSize
	}
	if cw.TotalInputTokens != nil {
		d.TokensIn = *cw.TotalInputTokens
	}
	if cw.TotalOutputTokens != nil {
		d.TokensOut = *cw.TotalOutputTokens
	}

	d.EffectiveWindow = harness.EffectiveContextWindow(d.RawWindow, resolvedWindow)
	d.CtxPct = int(harness.RebaseContextPercentage(rawCtxPct, d.RawWindow, d.EffectiveWindow))
	return d
}

// observedPinnedWindow parses the auto-compaction pin a pane exports in
// its own environment, in tokens, or zero when there is none.
//
// It routes the value through the SAME parser the spawn boundary uses, so
// this pane's bar is governed by a value the rest of the feature would
// accept: it applies the 10k–10M bounds and understands the `450k`
// spelling. Reading the raw integer instead would let a typo'd `=500` in
// the operator's shell re-base every percentage against a 500-token
// window — clamping the bar to 100%, storing that, firing the context
// nudge on every render, and (because the value is recorded durably by
// the caller) re-injecting itself on every later resume. An unparseable
// value is ignored, leaving Claude Code's own default threshold in
// charge, which is the same fail-soft direction
// durableRelaunchConfigForConv takes.
//
// It is a named function rather than an inline parse because the brokered
// path parses the same string on the far side of a socket, and the two
// must not drift: the daemon decides from it whether the pin is written
// back to the row.
func observedPinnedWindow(envValue string) int64 {
	parsed, err := harness.ParseAutoCompactWindow(envValue)
	if err != nil {
		return 0
	}
	return harness.AutoCompactWindowTokens(parsed)
}

// hasSubscriptionLimits reports whether the payload itself carried rate
// limit buckets. It is knowable before any database read, which is what
// lets the brokered path decide in advance whether it needs the usage
// cache at all — the fallback read is the one read that can make a
// network call, and asking for it speculatively on every render would be
// a real cost.
func hasSubscriptionLimits(input StatusLineInput) bool {
	rl := input.RateLimits
	if rl == nil {
		return false
	}
	return rl.FiveHour != nil || rl.SevenDay != nil ||
		(rl.SevenDaySonnet != nil && rl.SevenDaySonnet.UsedPercentage > 0)
}

// renderWrites is everything one render records. It is a value rather
// than a sequence of calls so the brokered path can carry it across a
// socket unchanged.
type renderWrites struct {
	Input   StatusLineInput
	Payload []byte
	Git     *GitSnapshot
	Derived renderDerived

	// Owned is the session row this render is allowed to write, already
	// through the attribution gate. Empty means no per-session write.
	Owned string

	// WorkspaceConv is the conversation id the workspace snapshot is
	// keyed by. Empty means no workspace write.
	WorkspaceConv string
}

// applyRenderWrites performs every host-touching write one statusline
// render implies EXCEPT the cost column, in the order the statusbar has
// always performed them. See applyCostWrite for why cost is separate.
//
// It reports whether every write it attempted succeeded. Locally that
// answer is ignored, because the very next render — a fraction of a
// second later — retries anyway. Brokered it is load-bearing: the change
// gate means a caller that believes a write landed will not re-send it
// until the payload changes again, so a failure the daemon swallowed
// would be a snapshot lost until the agent's next token tick. The gate
// removes the direct path's automatic retry, so the failure has to travel
// back instead.
//
// Every failure is still a warning and never an error: a statusline that
// exits non-zero is a visible defect in the agent's pane, and no
// telemetry write is worth that.
func applyRenderWrites(w renderWrites) (ok bool) {
	ok = true
	if w.WorkspaceConv != "" {
		ws := db.AgentWorkspace{
			ConvID:    w.WorkspaceConv,
			Cwd:       w.Input.Workspace.CurrentDir,
			UpdatedAt: time.Now(),
		}
		if w.Git != nil {
			ws.Branch = w.Git.Branch
			ws.RepoURL = w.Git.RepoURL
			ws.DefaultBranch = w.Git.DefaultBranch
			ws.PRNumber = w.Git.PRNumber
			ws.PRURL = w.Git.PRURL
			ws.PRState = w.Git.PRState
			// AgentWorkspace.UpdatedAt is also the freshness clock for the
			// published git/PR snapshot. A render may reuse the 15-second git
			// cache, so retain its actual fetch time instead of making stale PR
			// state look newer merely because the statusline rendered again.
			if !w.Git.FetchedAt.IsZero() {
				ws.UpdatedAt = w.Git.FetchedAt
			}
		}
		if err := db.UpsertAgentWorkspace(ws); err != nil {
			slog.Warn("status-bar: failed to upsert agent_workspace", "error", err, "module", "hooks")
			ok = false
		}
	}

	if w.Owned != "" && w.Derived.HasContextData {
		// The stored window stays the model's REAL one; the stored
		// PERCENTAGE is the re-based one. See renderDerived.
		if err := db.UpdateContextSnapshot(w.Owned, float64(w.Derived.CtxPct),
			w.Derived.TokensIn, w.Derived.TokensOut, w.Derived.RawWindow); err != nil {
			slog.Warn("status-bar: failed to update context snapshot", "error", err, "module", "hooks")
			ok = false
		}
	}

	if w.Owned != "" {
		if err := db.UpdateSessionModel(w.Owned, w.Input.Model.DisplayName); err != nil {
			slog.Warn("status-bar: failed to update session model", "error", err, "module", "hooks")
			ok = false
		}
		// Gated on the environment-OBSERVED value rather than the resolved
		// one so a window read from the row is not written straight back.
		if w.Derived.EnvPinnedWindow > 0 {
			if err := db.UpdateSessionAutoCompactWindow(w.Owned,
				strconv.FormatInt(w.Derived.EnvPinnedWindow, 10)); err != nil {
				slog.Warn("status-bar: failed to update session auto-compact window", "error", err, "module", "hooks")
				ok = false
			}
		}
		if err := db.UpdateSessionModelID(w.Owned, w.Input.Model.ID); err != nil {
			slog.Warn("status-bar: failed to update session model id", "error", err, "module", "hooks")
			ok = false
		}
		if err := db.UpdateSessionEffort(w.Owned, w.Input.Effort.Level); err != nil {
			slog.Warn("status-bar: failed to update session effort", "error", err, "module", "hooks")
			ok = false
		}
	}

	// Persist the VERBATIM statusline payload (latest snapshot, one
	// column, overwritten each render) so the full rate-limit picture can
	// be inspected off the database — including buckets StatusLineInput
	// does not name yet (Go drops unknown JSON keys). Gated on rate_limits
	// being present: those renders carry the data we're after, and
	// skipping the empty start-of-turn renders keeps a hollow payload from
	// clobbering a good snapshot.
	if w.Input.RateLimits != nil && w.Owned != "" {
		if err := db.UpdateStatuslineSnapshot(w.Owned, string(w.Payload)); err != nil {
			slog.Warn("status-bar: failed to update statusline snapshot", "error", err, "module", "hooks")
			ok = false
		}
	}
	return ok
}

// applyCostWrite records the session's cost, real or hypothetical.
//
// It is separate from the rest of the write set, and runs after them,
// because it is the ONE write whose target column depends on the
// usage-cache read: a render carrying no rate-limit buckets of its own
// only learns it is on a subscription plan from the fallback. The inline
// code had exactly this shape — every other write, then the usage read,
// then cost — and preserving it matters beyond tidiness: folding cost in
// with the others would put the usage read (which on the direct path can
// make a network call) in front of all eight writes, so a render killed
// by the harness's statusline timeout would record nothing at all instead
// of everything but its cost.
func applyCostWrite(w renderWrites, hasLimits bool) (ok bool) {
	if w.Owned == "" || w.Input.Cost.TotalCostUSD <= 0 {
		return true
	}
	if hasLimits {
		// Subscription plan: the figure is what this WOULD have cost
		// per-token, kept for the dashboard's WHAT-IF view.
		if err := db.UpdateSessionVirtualCost(w.Owned, w.Input.Cost.TotalCostUSD); err != nil {
			slog.Warn("status-bar: failed to update session virtual cost", "error", err, "module", "hooks")
			return false
		}
		return true
	}
	if err := db.UpdateSessionCost(w.Owned, w.Input.Cost.TotalCostUSD); err != nil {
		slog.Warn("status-bar: failed to update session cost", "error", err, "module", "hooks")
		return false
	}
	return true
}

// updateUsageCacheFromRender publishes this render's rate-limit buckets
// into the shared usage cache, so other sessions and the dashboard see
// fresh figures without an API call.
//
// It is separate from applyRenderWrites, and called BEFORE the usage
// read, because that is the order the inline code had: a render that
// carries a rate_limits block writes the cache, and only then does the
// fallback read it. Folding it in with the other writes would invert
// that, and a render with a present-but-empty block would then render
// against a cache one write staler than it used to.
func updateUsageCacheFromRender(input StatusLineInput) {
	rl := input.RateLimits
	if rl == nil {
		return
	}
	var fh, sd, sds *usageapi.CachedBucket
	if rl.FiveHour != nil {
		fh = &usageapi.CachedBucket{Pct: rl.FiveHour.UsedPercentage, ResetsAt: time.Unix(rl.FiveHour.ResetsAt, 0)}
	}
	if rl.SevenDay != nil {
		sd = &usageapi.CachedBucket{Pct: rl.SevenDay.UsedPercentage, ResetsAt: time.Unix(rl.SevenDay.ResetsAt, 0)}
	}
	if rl.SevenDaySonnet != nil {
		sds = &usageapi.CachedBucket{Pct: rl.SevenDaySonnet.UsedPercentage, ResetsAt: time.Unix(rl.SevenDaySonnet.ResetsAt, 0)}
	}
	usageapi.UpdateFromStatusLine(fh, sd, sds)
}

// temporarySandboxOff reports the reversible sandbox override for the
// agent behind a conversation generation. It is the boolean behind the
// ⚠ SB-OFF badge; the badge text itself is rendered pane-side so the
// brokered response carries a fact rather than terminal escapes.
func temporarySandboxOff(convID string) bool {
	agentID, err := db.AgentIDForConv(strings.TrimSpace(convID))
	if err != nil || agentID == "" {
		return false
	}
	_, active, err := db.TemporarySandboxModeForAgent(agentID)
	return err == nil && active
}
