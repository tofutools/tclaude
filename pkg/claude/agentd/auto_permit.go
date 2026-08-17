package agentd

import (
	"log/slog"
	"net/http"
	"sync"
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
// paints it.
//
// autoPermitMaxAge is the other end of that window: a press is only meaningful
// while the prompt it answers is still the thing on screen. The pane injection
// lock waits up to paneInjectLockTimeout (a minute) behind whatever else is
// injecting, so without a bound the accept key could land a minute late, into
// whatever the pane has moved on to. Past this age the press is abandoned and
// the prompt is simply left for the human — the outcome auto-permit was there
// to improve, never a keystroke aimed at the wrong thing.
const autoPermitMaxAge = 5 * time.Second

// autoPermitState holds what tests adjust and observe. Guarded because the
// press runs on its own goroutine: the settle wait is read there while a test's
// cleanup restores it, which is a race even when the values never conflict.
var autoPermitState = struct {
	mu     sync.Mutex
	settle time.Duration
	// finished, when a test installed one, receives once per press attempt
	// that ran to a decision. Without it a test can only sleep and hope; the
	// goroutine outliving its test is how a stretched settle wait ends up
	// pressing into the NEXT test's pane.
	finished chan struct{}
}{settle: 400 * time.Millisecond}

func autoPermitSettle() time.Duration {
	autoPermitState.mu.Lock()
	defer autoPermitState.mu.Unlock()
	return autoPermitState.settle
}

// autoPermitPressFinished signals a test waiter, if one is installed. Never
// blocks: the channel is buffered and a full one means the test already has
// more signals than it waited for.
func autoPermitPressFinished() {
	autoPermitState.mu.Lock()
	ch := autoPermitState.finished
	autoPermitState.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}

// SetAutoPermitSettleDelayForTest overrides the settle wait and returns a
// restore func for t.Cleanup.
func SetAutoPermitSettleDelayForTest(d time.Duration) func() {
	autoPermitState.mu.Lock()
	defer autoPermitState.mu.Unlock()
	prev := autoPermitState.settle
	autoPermitState.settle = d
	return func() {
		autoPermitState.mu.Lock()
		defer autoPermitState.mu.Unlock()
		autoPermitState.settle = prev
	}
}

// AutoPermitPressesForTest installs a signal fired whenever a press attempt
// finishes, however it ended. Returns the channel and a restore func for
// t.Cleanup. A test that stretches the settle wait MUST drain this before
// returning, so its goroutine cannot wake inside a later test.
func AutoPermitPressesForTest() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 8)
	autoPermitState.mu.Lock()
	defer autoPermitState.mu.Unlock()
	prev := autoPermitState.finished
	autoPermitState.finished = ch
	return ch, func() {
		autoPermitState.mu.Lock()
		defer autoPermitState.mu.Unlock()
		autoPermitState.finished = prev
	}
}

// autoPermitInFlight holds the convs with a press already pending, so a prompt
// reported twice is answered once. Two hook registrations for the same event (a
// settings.json entry plus a plugin's) deliver it twice; the second accept key
// would land after the dialog closed, in the composer.
var autoPermitInFlight sync.Map

// maybeAnswerAutoPermit answers a just-raised permission prompt when the
// operator granted this agent the slug for it. Called from the brokered-hook
// handler on a PermissionRequest event, alongside the other daemon-side
// reactions to specific events.
//
// resolvePermission is the ordinary resolver, and the slug is neither
// default-granted nor owner-implied — so "no grant" is the answer for every
// agent nobody consented for, and it is a silent no-op: the prompt simply waits
// for the human, as it does today.
//
// The session's harness must be the one the entry describes. A tool name only
// means anything inside the harness that defines it, and the accept keys are
// specific to how that harness draws its dialog — so a same-named prompt from
// another harness is not this condition and is never answered by it.
func maybeAnswerAutoPermit(row *db.SessionRow, toolName string) {
	accept, known := session.AutoPermitAcceptForTool(toolName)
	if !known || row == nil || row.ConvID == "" || row.TmuxSession == "" {
		return
	}
	if !session.AutoPermitHarnessMatches(row.Harness, accept) {
		return
	}
	if resolvePermission(row.ConvID, accept.Slug) != permAllow {
		return
	}
	if _, pending := autoPermitInFlight.LoadOrStore(row.ConvID, struct{}{}); pending {
		return
	}
	go pressAutoPermitAccept(row.ConvID, accept, toolName, time.Now())
}

// pressAutoPermitAccept sends the accept keys once the dialog has had time to
// paint, under the pane injection lock every other keystroke path takes.
//
// Nothing captured at hook time is trusted at press time. The grant is re-read,
// because the press is the act being authorized and consent withdrawn in
// between must land before the key does. The pane is re-resolved through
// aliveSessionForConv, the same way every other daemon injector does, because
// the row's session may have exited while this waited — and a tmux name is
// reusable, so a stale target can be a live pane belonging to someone else.
func pressAutoPermitAccept(convID string, accept session.AutoPermitAccept, toolName string, raisedAt time.Time) {
	defer autoPermitInFlight.Delete(convID)
	defer autoPermitPressFinished()

	time.Sleep(autoPermitSettle())
	if resolvePermission(convID, accept.Slug) != permAllow {
		slog.Info("auto-permit: consent was withdrawn before the keystroke; leaving the prompt",
			"tool", toolName, "conv", convID, "module", "agentd")
		return
	}
	row := aliveSessionForConv(convID)
	if row == nil {
		slog.Info("auto-permit: no live pane for the prompt; leaving it",
			"tool", toolName, "conv", convID, "module", "agentd")
		return
	}
	// Re-checked on the re-resolved row rather than carried over: this is the
	// session the key would actually go to.
	if !session.AutoPermitHarnessMatches(row.Harness, accept) {
		return
	}
	target := row.TmuxSession + ":0.0"
	mu := paneInjectLock(injectLockKey(target))
	if err := acquirePaneInjectLock(mu); err != nil {
		slog.Warn("auto-permit: could not take the pane lock", "error", err,
			"tool", toolName, "conv", convID, "module", "agentd")
		return
	}
	defer mu.Unlock()
	// Checked after the lock, not before: waiting behind another injector is
	// exactly how a press gets old.
	if age := time.Since(raisedAt); age > autoPermitMaxAge {
		slog.Warn("auto-permit: the prompt is too old to answer safely; leaving it",
			"age", age, "tool", toolName, "conv", convID, "module", "agentd")
		return
	}
	if err := paneinput.SendKeys(target, paneinput.Options{
		Run:         runTmuxCommand,
		LockTimeout: paneInjectLockTimeout,
		LockID:      target,
	}, accept.Keys...); err != nil {
		slog.Warn("auto-permit: accept keystroke failed", "error", err,
			"tool", toolName, "tmux", row.TmuxSession, "conv", convID, "module", "agentd")
		return
	}
	slog.Info("auto-permit: answered a permission prompt on the operator's behalf",
		"tool", toolName, "conv", convID, "module", "agentd")
	recordAutoPermitAnswer(row, toolName)
}

// recordAutoPermitAnswer writes the operator's record of what was approved for
// them: an ordinary audit row, in the same trail and dashboard tab as every
// other action taken against this agent.
//
// Written after the keystroke actually went out, so a decision that never
// became a press (consent withdrawn during the settle, a pane that exited) is a
// log line rather than a claim that something was approved. What it records is
// that the accept key was SENT for this prompt — the daemon does not read the
// pane back, so it cannot attest to what the harness then did with it.
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
