package conv

import (
	"fmt"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// What the plain-CLI resume surfaces owe a conversation that chose the Copilot
// API drive.
//
// # Why a non-daemon resume cannot simply honour it
//
// The recorded drive is a real choice and a resume reproducing recorded choices
// is the whole point of `pkg/claude/conv`. This one it cannot reproduce, and the
// reason is structural rather than a missing flag:
//
//   - The channel is CREATED by agentd (`bootstrapCopilotAPISession`: an RPC
//     `session.create`/`session.resume` plus `setForeground`), held in that
//     process's handle registry, and proved to belong to the agent's pane
//     subtree before every send. Nothing outside `pkg/claude/agentd` dials the
//     endpoint at all.
//   - The port is recorded only by agentd. A pane launched from here has no port
//     record, so it is not even a candidate for `reconcileCopilotAPISessions` —
//     and that reconcile deliberately refuses to `session.create` at a cold id,
//     because doing so starts the conversation FRESH and destroys it.
//   - The pane's own startup session is not drivable: every `session.*` call
//     against it answers "Session not found" (measured, TCL-1056).
//
// So rendering `--ui-server --host 127.0.0.1 --port N` from here would bind an
// unauthenticated loopback endpoint that nothing will ever dial, with no route
// back into the daemon's ownership. That is strictly worse than send-keys — new
// exposure, no drive — which is why this file gates and discloses rather than
// honouring. A future edit that wants to honour it must first give this surface
// a way to hand the channel to the daemon; the flag alone is not the missing
// piece.
//
// # Why a managed agent is REFUSED rather than warned
//
// Since TCL-1058 the recorded drive decides message ROUTING, and an API-driven
// conversation whose channel is missing HOLDS its mail on purpose — it must
// never fall back to typing into a pane whose channel just became unverifiable.
// For a daemon-managed agent that composes into a silently deaf agent: agentd
// keeps routing to a channel this pane does not have, the reconcile refuses to
// adopt it, and the pane meanwhile looks perfectly healthy. Announcing that
// state does not prevent it, and a better command exists one line away, so the
// default is a refusal that names it.
//
// An UNMANAGED conversation is the opposite case and it would be a mistake to
// treat it symmetrically: nothing routes it, so its recorded drive buys it
// nothing, and refusing would block a human from resuming their own conversation
// to protect nothing. It proceeds, loudly.
//
// # Why the record is left alone in every branch
//
// See LaunchPosture.CopilotAPI. These surfaces pass nil, so a resume performed
// here reports what it did to the human and nothing to the durable record. The
// drive stays owned by the surface that authored it.

// resumeCopilotDriveGate answers "may this plain-CLI resume launch, and what
// must the human be told" for one conversation.
//
// Returns ("", nil) for every conversation that did not choose the API drive —
// which is every Copilot conversation today unless someone opted in, and every
// conversation of every other harness. That inertness is deliberate: the
// send-keys path is the default and the known-good one, and this gate must not
// change a byte of it.
//
// allowSendKeys is the human's explicit "launch on keystrokes anyway"
// (`tclaude conv resume --send-keys`). It downgrades the managed refusal to the
// same disclosure the unmanaged case gets, and it exists because
// `tclaude agent resume` needs a running daemon: a refusal whose only way out
// required agentd would wall the human out of a pane at exactly the moment
// agentd is the thing that is broken.
//
// A READ failure is fatal rather than fail-open, matching resumeRemoteControl
// and the recorded-posture load in `session new -r`: proceeding on a value we
// could not read is how a posture gets silently downgraded, and refusing a
// resume is recoverable in a way an erased choice is not.
func resumeCopilotDriveGate(h *harness.Harness, convID string, allowSendKeys bool) (string, error) {
	if !h.SupportsCopilotAPI() {
		return "", nil
	}
	// Read what agentd's ROUTING reads, by the same merge and precedence
	// (db.CopilotAPIForConv → RecordedLaunchPostureForConv). A gate that
	// resolved the drive differently from the router would be comparing this
	// launch against a belief nobody holds.
	api, err := db.CopilotAPIForConv(convID)
	if err != nil {
		return "", fmt.Errorf(
			"load recorded Copilot drive for conversation %s: %w", convID, err)
	}
	if !api {
		return "", nil
	}
	managed, err := resumeConvIsManagedAgent(convID)
	if err != nil {
		return "", err
	}
	if managed && !allowSendKeys {
		return "", fmt.Errorf(
			"copilot_api_drive_unavailable_outside_agentd: conversation %s chose the "+
				"Copilot API drive, and this resume cannot reproduce it — the API channel "+
				"is created, held and ownership-proved by tclaude agentd, so a launch from "+
				"the plain CLI would bind an endpoint nothing drives.\n"+
				"This conversation is a managed agent, so agentd routes its messages over "+
				"that channel and deliberately does not fall back to keystrokes: a "+
				"send-keys pane would look healthy while the agent received no mail.\n"+
				"  resume it on its own drive:     tclaude agent resume %s\n"+
				"  resume it on keystrokes anyway: tclaude conv resume %s --send-keys",
			convID, convID, convID)
	}
	notice := fmt.Sprintf(
		"Warning: conversation %s chose the Copilot API drive; this resume runs on tmux "+
			"send-keys instead, because that channel can only be created by tclaude agentd. "+
			"The recorded drive is left untouched — this launch does not un-choose it.",
		convID)
	if managed {
		notice += fmt.Sprintf(
			"\nThis is a managed agent: agentd keeps routing its messages to the API "+
				"channel, so its mail will HOLD in the durable inbox rather than being "+
				"typed into this pane. Relaunch it with `tclaude agent resume %s` for a "+
				"pane on its own drive.", convID)
	}
	slog.Warn("resuming an API-driven Copilot conversation on send-keys",
		"conv_id", convID, "managed", managed, "send_keys_override", allowSendKeys)
	return notice, nil
}

// resumeConvIsManagedAgent reports whether the daemon has an actor for this
// conversation, i.e. whether anything routes messages to it.
//
// This is the discriminator between the refusal and the disclosure, so its
// failure direction matters: an unreadable actor row is treated as fatal rather
// than as "not an agent", because guessing "unmanaged" is the guess that lets a
// managed agent through the gate.
func resumeConvIsManagedAgent(convID string) (bool, error) {
	agent, err := db.GetAgentByConv(convID)
	if err != nil {
		return false, fmt.Errorf(
			"resolve whether conversation %s is a managed agent: %w", convID, err)
	}
	return agent != nil, nil
}
