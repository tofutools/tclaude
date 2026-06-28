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
	// it. See isAwaitingHumanInput / heldForHumanInput.
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

// heldForHumanInput reports whether convID has an alive pane that is
// currently blocked on a human, in which case queued mail must be held
// rather than nudged in. It reuses pickAliveSession (the same most-recent-
// alive selector nudgeIfAlive and sendNudgeBracket use), so the status it
// reads is the one belonging to the pane a nudge would actually land in.
//
// Returns false when there is no alive session (nothing to hold for — the
// offline-queue path handles that) or when the alive pane is working/idle.
func heldForHumanInput(convID string) bool {
	sess := pickAliveSession(convID)
	return sess != nil && isAwaitingHumanInput(sess.Status)
}
