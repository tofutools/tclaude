package session

import (
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
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

// reinjectionTarget resolves the conversation a hook may queue for. Like the
// standing-order path, the recipient is the RESOLVED session row's
// conversation and the payload's conv-id must agree with it; a hook naming
// somebody else's conversation yields nothing.
func reinjectionTarget(input HookCallbackInput, envSessionID string) (*SessionState, string) {
	state, err := loadStandingOrderSession(envSessionID)
	if err != nil || state == nil || state.Harness == ShellHarnessName {
		return nil, ""
	}
	convID := strings.TrimSpace(state.ConvID)
	if convID == "" || convID != strings.TrimSpace(input.ConvID) {
		return nil, ""
	}
	return state, convID
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
// moved the actor onto the new conversation. Failures are logged and
// swallowed: no reminder is worth disrupting a turn over.
//
// A compaction announced while a subagent runs may be the subagent's own, and
// the hook cannot say (see harness.CompactionProof), so it is held and settled
// by the next main-thread hook instead. See holdCompaction.
func QueueContextReinjection(input HookCallbackInput, envSessionID string) {
	boundary, ok := reinjectionBoundary(input)
	if !ok {
		settleHeldReinjection(input, envSessionID)
		return
	}
	if input.AgentID != "" {
		// AgentID marks an in-harness subagent sharing the main conv-id.
		return
	}
	state, convID := reinjectionTarget(input, envSessionID)
	if state == nil {
		return
	}
	if boundary == startupctx.BoundaryCompact {
		if compactionHeld(input, envSessionID) {
			return
		}
		if input.Trigger == "manual" {
			// Only the main thread takes a /compact, and an idle prompt may
			// not produce a settling hook for a long time. This also covers
			// the SessionStart(compact) held just before it, which cannot say
			// it was manual, so that hook's orders are still owed.
			released, err := db.ReleaseHeldContextReinjection(state.ID)
			if err != nil {
				slog.Warn("context re-injection: release failed",
					"session_id", state.ID, "error", err, "module", "hooks")
			}
			if released {
				replayStandingOrders(input, envSessionID, compactSessionStart)
			}
		}
	}
	queueReinjection(convID, boundary)
}

// QueueCompactionReinjection queues the startup context after a compaction
// tclaude learned of outside a hook. Copilot announces its compactions only in
// the session event log, which the daemon follows.
func QueueCompactionReinjection(convID string) {
	queueReinjection(convID, startupctx.BoundaryCompact)
}

// QueueClearReinjection is QueueCompactionReinjection for a /clear.
func QueueClearReinjection(convID string) {
	queueReinjection(convID, startupctx.BoundaryClear)
}

// subagentsShareCompactionHooks reports whether a harness announces a
// subagent's compaction through the main thread's hooks.
func subagentsShareCompactionHooks(harnessName string) bool {
	h, err := harness.Resolve(harnessName)
	return err == nil && h.Compaction != nil
}

// holdCompaction holds a compaction hook's compaction if it may be a
// subagent's, and reports whether it is held. Holding from PreCompact, rather
// than from the hooks that follow, lets every effect of the compaction wait on
// the same answer: a compaction that is refused or fails leaves no boundary,
// and settles as if it had been a subagent's.
//
// A hold still standing covers this compaction too, so one replay accounts for
// everything skipped, unless the conversation rotated away from it: its
// transcript can no longer show the boundary. A /compact is only ever the main
// thread's.
func holdCompaction(input HookCallbackInput, envSessionID string) bool {
	if input.AgentID != "" || input.Trigger == "manual" || envSessionID == "" {
		return false
	}
	state, convID := reinjectionTarget(input, envSessionID)
	if state == nil {
		return false
	}
	if held, err := db.PeekHeldContextReinjection(state.ID); err == nil && held != nil {
		if held.ConvID == convID {
			return true
		}
		if _, err := db.ReleaseHeldContextReinjection(state.ID); err != nil {
			slog.Warn("context re-injection: release failed",
				"session_id", state.ID, "error", err, "module", "hooks")
		}
	}
	return holdReinjection(state, convID)
}

// compactionHeld reports whether a post-compaction hook's compaction is held.
// It holds again if need be: a background subagent's compaction does not block
// the main thread, whose hooks can settle the hold before these ones arrive.
func compactionHeld(input HookCallbackInput, envSessionID string) bool {
	boundary, ok := reinjectionBoundary(input)
	return ok && boundary == startupctx.BoundaryCompact && holdCompaction(input, envSessionID)
}

// holdReinjection defers an unattributed compaction and reports whether it
// did. With no subagent running the compaction can only be the main thread's.
// Anything stopping the proof from working declines the hold: a spurious
// reminder costs far less than an agent losing its briefing.
func holdReinjection(state *SessionState, convID string) bool {
	h, err := harness.Resolve(state.Harness)
	if err != nil || h.Compaction == nil || state.Subagents.LiveCount(time.Now()) == 0 {
		return false
	}
	info, err := os.Stat(h.Compaction.MainTranscriptPath(convID, state.Cwd))
	if err != nil {
		slog.Warn("context re-injection: main transcript unavailable, not holding",
			"session_id", state.ID, "error", err, "module", "hooks")
		return false
	}
	err = db.HoldContextReinjection(state.ID, db.HeldContextReinjection{
		ConvID: convID, TranscriptOffset: info.Size(),
	})
	if err != nil {
		slog.Warn("context re-injection: hold failed",
			"session_id", state.ID, "error", err, "module", "hooks")
		return false
	}
	return true
}

// settleHeldReinjection decides a held compaction once the main thread is
// demonstrably past it. The main thread is blocked while it compacts and its
// boundary lands as soon as the compaction hooks return, so at its next turn
// event a missing boundary means the compaction was a subagent's. A subagent's
// remaining compaction hooks may still follow; compactionHeld absorbs those.
func settleHeldReinjection(input HookCallbackInput, envSessionID string) {
	switch input.HookEventName {
	case "PreToolUse", "PostToolUse", "PostToolUseFailure", "Stop", "StopFailure":
		// Not UserPromptSubmit: a prompt is not proof the main thread is past
		// a compaction, and every turn it starts reaches one of these anyway.
	default:
		return
	}
	if input.AgentID != "" || envSessionID == "" {
		return
	}
	held, err := db.PeekHeldContextReinjection(envSessionID)
	if err != nil || held == nil {
		return
	}
	state, convID := reinjectionTarget(input, envSessionID)
	if state == nil {
		return
	}
	released, err := db.ReleaseHeldContextReinjection(state.ID)
	if err != nil || !released || convID != held.ConvID {
		return
	}
	h, err := harness.Resolve(state.Harness)
	if err != nil || h.Compaction == nil {
		return
	}
	compacted, err := h.Compaction.CompactedAfter(h.Compaction.MainTranscriptPath(convID, state.Cwd), held.TranscriptOffset)
	if err != nil {
		slog.Warn("context re-injection: main transcript unreadable, assuming main compaction",
			"session_id", state.ID, "error", err, "module", "hooks")
		compacted = true
	}
	if !compacted {
		slog.Info("context re-injection: skipped a subagent's compaction",
			"conv_id", convID, "module", "hooks")
		return
	}
	queueReinjection(convID, startupctx.BoundaryCompact)
	if err := db.ResetCompact(state.ID); err != nil {
		slog.Warn("context re-injection: post-compact reset failed",
			"session_id", state.ID, "error", err, "module", "hooks")
	}
	replayStandingOrders(input, envSessionID, compactPreCompact, compactSessionStart, compactPostCompact)
}

// queueReinjection composes and queues the message for one boundary.
func queueReinjection(convID string, boundary startupctx.Boundary) {
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
	subject := db.ReinjectedAfterCompactSubject
	if boundary == startupctx.BoundaryClear {
		subject = db.ReinjectedAfterClearSubject
	}
	id, err := db.InsertReinjectedContextMessage(&db.AgentMessage{
		GroupID:      r.GroupID,
		ToConv:       convID,
		ToRecipients: []string{convID},
		Subject:      subject,
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
