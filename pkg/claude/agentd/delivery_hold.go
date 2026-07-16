package agentd

import "github.com/tofutools/tclaude/pkg/claude/session"

// isAwaitingHumanInput reports whether a session status means the pane is
// blocked on a human decision: an elicitation dialog (awaiting_input) or a
// permission prompt (awaiting_permission). These are exactly the "red,
// needs you" states the dashboard surfaces. In both the pane is reading
// keystrokes as the human's reply, so any tmux nudge must be held until the
// agent is back to working/idle — otherwise the injected text is captured
// as the answer and the mail notification is lost.
//
// StatusError is deliberately NOT held: it is a transient API/billing
// failure with the pane back at an ordinary prompt, where a nudge behaves
// like the idle case. StatusExited is irrelevant here — a dead session has
// no alive pane for the async flush to find in the first place.
func isAwaitingHumanInput(status string) bool {
	return status == session.StatusAwaitingInput ||
		status == session.StatusAwaitingPermission
}

// deliverablePane reports whether convID has an alive pane we may nudge into
// RIGHT NOW: it exists AND is not blocked on a human. It is the gate the async
// drains (flushAgent / flush) check before claiming a message. The durable
// claim is retryable now, but acquiring it while the pane is known unsafe
// would still create needless attempts and injected-dialog risk. Returning
// false leaves the whole queue untouched for a later drain.
//
// It folds the two "don't deliver yet" cases the async model must handle into
// one gate, because both want the same outcome (leave it queued, retry later):
//   - no alive session — the recipient is offline (the async send path can hit
//     this; the pre-async flush only ran when the target was online); and
//   - an alive pane blocked on a human (awaiting_input / awaiting_permission) —
//     the JOH-308 hold: a nudge typed in now would be captured by the open
//     dialog as the human's answer, and the real notification lost.
//
// It reuses pickNudgeSession (the same most-recent-alive, timeout-bounded
// selector sendNudgeBracket uses), so the status it reads belongs to the pane
// a nudge would actually land in.
func deliverablePane(convID string) bool {
	sess := pickNudgeSession(convID)
	return sess != nil && !isAwaitingHumanInput(sess.Status)
}
