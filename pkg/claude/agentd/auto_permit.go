package agentd

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/paneinput"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// auto_permit.go answers the permission prompts a harness reserves for a human
// keystroke, for agents whose operator granted the matching slug.
//
// Claude Code's EnterWorktree safety check is the case it exists for: when the
// target worktree lives outside the directory Claude Code manages itself, the
// confirmation is a hardcoded gate that ignores allow-rules, the auto-mode
// classifier and PreToolUse hook approvals alike, so the agent stalls until
// someone presses a key.
//
// This runs in the DAEMON, off a hook event the daemon already receives (see
// handleWhoamiHook), and that placement is the design:
//
//   - A sandboxed agent is deliberately cut off from both tmux and the
//     database, so neither the decision nor the keystroke can happen inside the
//     pane — which is exactly where the agents most likely to want this run.
//   - The pane injection lock lives here, so a press cannot interleave with a
//     nudge or a message delivery mid-flight.
//   - The agent is identified from the daemon's own recorded pids, and the
//     grant read through the full permission resolver. The hook reports an
//     event; it claims nothing and is never refused for lacking a grant.

// autoPermitSettleDelay is how long the daemon waits before pressing. The
// harness is still blocked on the hook that reported the prompt, so the dialog
// is not on screen yet; the press has to land after the harness resumes and
// paints it. A var so tests need not sleep.
var autoPermitSettleDelay = 400 * time.Millisecond

// maybeAnswerAutoPermit answers a just-raised permission prompt when the
// operator granted this agent the slug for it. Called from the brokered-hook
// handler on a PermissionRequest event, alongside the other daemon-side
// reactions to specific events.
//
// resolvePermission is the ordinary resolver, and the slug is neither
// default-granted nor owner-implied — so "no grant" is the answer for every
// agent nobody consented for, and it is a silent no-op: the prompt simply waits
// for the human, as it does today.
func maybeAnswerAutoPermit(row *db.SessionRow, toolName string) {
	accept, known := session.AutoPermitAcceptForTool(toolName)
	if !known || row == nil || row.ConvID == "" || row.TmuxSession == "" {
		return
	}
	if resolvePermission(row.ConvID, accept.Slug) != permAllow {
		return
	}
	recordAutoPermitAnswer(row, toolName)
	go pressAutoPermitAccept(row.TmuxSession, row.ConvID, toolName, accept.Keys)
}

// pressAutoPermitAccept sends the accept keys once the dialog has had time to
// paint, under the pane injection lock every other keystroke path takes.
func pressAutoPermitAccept(tmuxSession, convID, toolName string, keys []string) {
	time.Sleep(autoPermitSettleDelay)
	target := tmuxSession + ":0.0"
	mu := paneInjectLock(injectLockKey(target))
	if err := acquirePaneInjectLock(mu); err != nil {
		slog.Warn("auto-permit: could not take the pane lock", "error", err,
			"tool", toolName, "conv", convID, "module", "agentd")
		return
	}
	defer mu.Unlock()
	if err := paneinput.SendKeys(target, paneinput.Options{
		Run:         runTmuxCommand,
		LockTimeout: paneInjectLockTimeout,
		LockID:      target,
	}, keys...); err != nil {
		slog.Warn("auto-permit: accept keystroke failed", "error", err,
			"tool", toolName, "tmux", tmuxSession, "conv", convID, "module", "agentd")
		return
	}
	slog.Info("auto-permit: answered a permission prompt on the operator's behalf",
		"tool", toolName, "conv", convID, "module", "agentd")
}

// recordAutoPermitAnswer writes the operator's record of what was approved for
// them: an ordinary audit row, in the same trail and dashboard tab as every
// other action taken against this agent. Written when the answer is DECIDED
// rather than after the keystroke, so a press that fails still leaves the
// decision visible (the failure itself is logged).
func recordAutoPermitAnswer(row *db.SessionRow, toolName string) {
	if _, err := db.InsertAuditLog(db.AuditLogEntry{
		At:          time.Now(),
		ActorKind:   db.AuditActorSystem,
		ActorLabel:  "tclaude",
		Verb:        "auto-permit.answer",
		TargetConv:  row.ConvID,
		TargetLabel: row.ConvID,
		Detail:      toolName,
		Status:      http.StatusOK,
		Source:      db.AuditSourceHook,
		SessionID:   row.ID,
		TmuxSession: row.TmuxSession,
	}); err != nil {
		slog.Warn("auto-permit: failed to record the answered prompt",
			"error", err, "tool", toolName, "conv", row.ConvID, "module", "agentd")
	}
}
