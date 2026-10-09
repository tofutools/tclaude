package session

import (
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/startupctx"
)

// reinjectionDedupeWindow collapses the several hooks one boundary can fire —
// Claude Code announces a compaction as both PostCompact and
// SessionStart(source=compact) — into a single queued message. It is short
// enough that two genuinely separate compactions are never merged: a
// compaction needs a context window's worth of new turns before it repeats.
const reinjectionDedupeWindow = 2 * time.Minute

// reinjectionBoundary maps a hook event onto the context boundary it
// announces, if any.
func reinjectionBoundary(input HookCallbackInput) (startupctx.Boundary, bool) {
	switch input.HookEventName {
	case "SessionStart":
		switch strings.ToLower(strings.TrimSpace(input.Source)) {
		case "compact":
			return startupctx.BoundaryCompact, true
		case "clear":
			return startupctx.BoundaryClear, true
		}
	case "PostCompact":
		return startupctx.BoundaryCompact, true
	}
	return "", false
}

// QueueContextReinjection queues the agent's startup context as an ordinary
// agent message after a compaction or /clear boundary, so the durable
// guidance tclaude briefed it with survives the harness summarizing (or
// clearing) its first turn.
//
// Every harness gets the same transport — a queued message, delivered as the
// next turn — rather than hook additionalContext, so the behaviour is uniform
// and an observation-only harness (OpenCode) is not a special case.
//
// It runs after the event has been applied, so a /clear rotation has already
// moved the actor onto the new conversation. Like the standing-order path,
// the recipient is the RESOLVED session row's conversation and the payload's
// conv-id must agree with it; a hook naming somebody else's conversation
// queues nothing. Failures are logged and swallowed: no reminder is worth
// disrupting a turn over.
func QueueContextReinjection(input HookCallbackInput, envSessionID string) {
	boundary, ok := reinjectionBoundary(input)
	if !ok || input.AgentID != "" {
		// AgentID marks an in-harness subagent sharing the main conv-id.
		return
	}
	state, err := loadStandingOrderSession(envSessionID)
	if err != nil || state == nil || state.Harness == ShellHarnessName {
		return
	}
	convID := strings.TrimSpace(state.ConvID)
	if convID == "" || convID != strings.TrimSpace(input.ConvID) {
		return
	}
	agentID, err := db.AgentIDForConv(convID)
	if err != nil || agentID == "" {
		return
	}
	r, err := startupctx.ComposeReinjection(agentID, boundary)
	if err != nil {
		slog.Warn("context re-injection: compose failed",
			"agent", agentID, "boundary", boundary, "error", err, "module", "hooks")
		return
	}
	if r.Body == "" {
		return
	}
	id, err := db.InsertReinjectedContextMessage(&db.AgentMessage{
		GroupID:      r.GroupID,
		ToConv:       convID,
		ToRecipients: []string{convID},
		Subject:      db.ReinjectedContextSubject,
		Body:         r.Body,
	}, reinjectionDedupeWindow)
	if err != nil {
		slog.Warn("context re-injection: queue failed",
			"agent", agentID, "boundary", boundary, "error", err, "module", "hooks")
		return
	}
	if id > 0 {
		slog.Info("context re-injection: queued startup context",
			"agent", agentID, "conv_id", convID, "boundary", boundary,
			"message_id", id, "module", "hooks")
	}
}
