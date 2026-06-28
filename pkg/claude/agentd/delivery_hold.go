package agentd

import "github.com/tofutools/tclaude/pkg/claude/session"

// deliveryOutcome reports what happened when the daemon tried to nudge a
// freshly-inserted agent message into the recipient's pane. It is the
// richer result behind nudgeIfAlive: callers that only care "did it land"
// use delivered(); the send handlers also surface held() so the sender's
// CLI can tell "queued because offline" apart from "held because the
// recipient is mid-question with a human."
type deliveryOutcome int

const (
	// outcomeQueued: the recipient has no alive tmux session right now, so
	// nothing was injected. The row sits in the inbox until a flush (the
	// recipient's next request, or the reaper backstop) finds it. This is
	// the historical "delivered == false" case.
	outcomeQueued deliveryOutcome = iota
	// outcomeDelivered: the nudge was injected and delivered_at stamped.
	outcomeDelivered
	// outcomeHeld: the recipient has an alive pane but is currently blocked
	// on a human — awaiting_input (an elicitation dialog) or
	// awaiting_permission (a permission prompt). CC's pane is capturing
	// keystrokes as the human's answer, so a tmux nudge typed in now would
	// be stolen by the dialog (and the real notification skipped). We
	// deliberately HOLD: the row stays undelivered (delivered_at empty) and
	// a later flush — once the recipient is back to working/idle — delivers
	// it. See isAwaitingHumanInput / deliverablePane.
	outcomeHeld
)

func (o deliveryOutcome) delivered() bool { return o == outcomeDelivered }
func (o deliveryOutcome) held() bool      { return o == outcomeHeld }

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
// no alive pane for nudgeIfAlive/flush to find in the first place.
func isAwaitingHumanInput(status string) bool {
	return status == session.StatusAwaitingInput ||
		status == session.StatusAwaitingPermission
}

// deliverablePane reports whether convID has an alive pane we may nudge into
// RIGHT NOW: it exists AND is not blocked on a human. It is the gate the async
// drains (flushAgent / flush) check BEFORE claiming a message — claiming stamps
// delivered_at, so claiming a message we then can't deliver would consume it.
// Returning false leaves the whole queue undelivered for a later drain.
//
// It folds the two "don't deliver yet" cases the async model must handle into
// one gate, because both want the same outcome (leave it queued, retry later):
//   - no alive session — the recipient is offline (the async send path can hit
//     this; the pre-async flush only ran when the target was online); and
//   - an alive pane blocked on a human (awaiting_input / awaiting_permission) —
//     the JOH-308 hold: a nudge typed in now would be captured by the open
//     dialog as the human's answer, and the real notification lost.
//
// It reuses pickAliveSession (the same most-recent-alive selector
// sendNudgeBracket uses), so the status it reads belongs to the pane a nudge
// would actually land in.
func deliverablePane(convID string) bool {
	sess := pickAliveSession(convID)
	return sess != nil && !isAwaitingHumanInput(sess.Status)
}
