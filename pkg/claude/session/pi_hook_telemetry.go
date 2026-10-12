package session

import (
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// PiContext is the context occupancy tclaude's pi extension attaches to every
// hook it sends: pi's own ctx.getContextUsage(). Tokens and Percent are null
// right after a compaction, until the next response measures the context
// again.
type PiContext struct {
	Tokens        *int64   `json:"tokens"`
	ContextWindow *int64   `json:"context_window"`
	Percent       *float64 `json:"percent"`
}

// PiUsage is what the pi session itself has spent so far, summed by the
// extension from the usage pi records. CostUSD is pi's price from its model
// catalog; under a subscription login it is only what the tokens would have
// cost.
type PiUsage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
	Calls        int64   `json:"calls"`
	Subscription bool    `json:"subscription"`
}

// persistPiHookTelemetry stores the context meter and cost a pi hook carries.
// pi has no status line, so the hook is the only writer: the percentage is
// re-based onto a pinned auto-compaction window, as the status line does for
// Claude Code, while the stored window stays the model's own. The extension
// itself compacts at the pinned window, so the meter and the compaction agree.
func persistPiHookTelemetry(state *SessionState, input HookCallbackInput, amb HookAmbient) {
	if state == nil || state.Harness != harness.PiName || state.ID == "" || input.AgentID != "" {
		return
	}
	if state.ConvID != "" && input.ConvID != "" && input.ConvID != state.ConvID {
		return
	}
	if c := input.PiContext; c != nil && c.ContextWindow != nil && *c.ContextWindow > 0 {
		window := *c.ContextWindow
		var tokens int64
		var pct float64
		if c.Tokens != nil && *c.Tokens > 0 {
			tokens = *c.Tokens
			pct = float64(tokens) / float64(window) * 100
			if c.Percent != nil && *c.Percent > 0 {
				pct = *c.Percent
			}
		}
		pinned := piPinnedCompactWindow(state.ID, amb)
		pct = harness.RebaseContextPercentage(pct, window, harness.EffectiveContextWindow(window, pinned))
		if err := db.UpdateContextSnapshot(state.ID, pct, tokens, 0, window); err != nil {
			slog.Warn("pi: failed to update context snapshot", "session_id", state.ID, "error", err, "module", "hooks")
		}
	}
	if u := input.PiUsage; u != nil && u.CostUSD > 0 {
		write := db.UpdateSessionCost
		if u.Subscription {
			write = db.UpdateSessionVirtualCost
		}
		if err := write(state.ID, u.CostUSD); err != nil {
			slog.Warn("pi: failed to update session cost", "session_id", state.ID, "error", err, "module", "hooks")
		}
	}
}

// piPinnedCompactWindow is the auto-compaction window in effect for a pi
// session: the one in the hook's environment, which is the one the extension
// compacts at, recorded on the session row the way the status line records
// Claude Code's so the dashboard re-bases its meter onto it; failing that, the
// row's own. Parsed, never raw, so an out-of-range value in the operator's
// shell governs nothing.
func piPinnedCompactWindow(sessionID string, amb HookAmbient) int64 {
	if parsed, err := harness.ParseAutoCompactWindow(amb.AutoCompactWindow); err == nil && parsed != "" {
		if err := db.UpdateSessionAutoCompactWindow(sessionID, parsed); err != nil {
			slog.Warn("pi: failed to record auto-compact window", "session_id", sessionID, "error", err, "module", "hooks")
		}
		return harness.AutoCompactWindowTokens(parsed)
	}
	stored, err := db.GetSessionAutoCompactWindow(sessionID)
	if err != nil {
		return 0
	}
	return harness.AutoCompactWindowTokens(stored)
}
