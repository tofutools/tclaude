package session

import (
	"fmt"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// copilotAPIDriveIsDaemonOwned reports whether the daemon that ESTABLISHES the
// Copilot API channel resolved this launch and is holding the port it handed
// over.
//
// Both halves earn their place. `--managed-launch` is the codebase's declared
// "agentd resolved these launch parameters" boundary (NewParams.ManagedLaunch),
// set on both of agentd's argv builders next to `--copilot-api` itself. A port
// alone would not do: agentd ALLOCATES before it builds argv, so a port is a
// side effect of the daemon's involvement rather than a statement of it, and a
// human may type `--copilot-api-port` for a launch no daemon has ever heard of.
// The port half then covers the converse — an agentd-resolved launch that
// somehow arrived without a port has no number for anything to wait on.
//
// # What this is NOT
//
// It is not a permission. It does not decide whether the drive is ALLOWED (the
// folder-trust gate does that, and refuses for a different reason); it decides
// whether the machinery that makes the drive mean anything is present.
//
// It is not a statement about connectedness. It says the daemon resolved this
// launch, not that a channel exists or ever will — the bootstrap runs after the
// pane starts and may fail. That distinction is the one the seam has now been
// written wrong three times (see copilotAPIDriven's doc comment in agentd, and
// TCL-1080), so this predicate stays on the near side of it: it answers a
// question about THIS PROCESS'S CALLER, which is knowable here, rather than
// about a channel, which is not.
//
// On forgeability: `--managed-launch` is hidden argv, so a human can type it and
// claim to be the daemon. That does not decide anything here, because the
// codebase already extends this flag exactly this trust for a decision whose
// failure mode is PERMANENT ERASURE — relaunch_carryover.go skips the whole
// relaunch carryover on it, which is the TCL-730 harm. Defending one flag
// against a forgery accepted everywhere else buys nothing and leaves the next
// reader with two mechanisms answering one question.
func copilotAPIDriveIsDaemonOwned(managedLaunch bool, port int) bool {
	return managedLaunch && port > 0
}

// noteCarriedCopilotDrive records that the drive arrived by INFERENCE rather than
// by request, and which conversation it was inferred from.
//
// resolveCopilotAPIDriveForLaunch answers those two cases differently — an
// asserted drive is refused, an inferred one is disclosed and dropped — and the
// relaunch carryover is the only place that can still tell them apart. By the time
// runNew reads params.CopilotAPI, a flag the operator typed and a flag the
// carryover filled in are the same bool. Same reason explicitLaunchFields exists
// one layer up.
//
// Keyed off the CARRIED list rather than off params, so it cannot disagree with
// what was actually applied. A recorded false classifies as carryAppliedDefault,
// never reaches this list, and correctly sets no marker: there is no drive to have
// a policy about, and a marker set there would route an ordinary send-keys resume
// through the carried-drive arm.
func noteCarriedCopilotDrive(params *NewParams, carried []string, convID string) {
	if slices.Contains(carried, "--copilot-api") {
		params.copilotAPIDriveCarriedFrom = convID
	}
}

// copilotAPIPostureToRecord decides what this launch may ASSERT about the
// conversation's drive, as distinct from what it ran with.
//
// Normally they are the same and the launch asserts its own value, zero included:
// `session new` is the authoring surface for this field, and a relaunch that
// genuinely turned the drive off must be able to record the false.
//
// The exception is a launch that resolved the drive and then could not honour it
// — a carried drive with no daemon behind it, dropped by
// resolveCopilotAPIDriveForLaunch. Asserting that false would un-choose the drive
// FOR THE CONVERSATION on the authority of a launch that merely failed to provide
// it: the same inversion TCL-1076 stopped on the resume surfaces, arriving here
// from the opposite direction. Nil instead, so the projection's carry-forward
// keeps what the conversation chose (TCL-1059 made that carry unconditional,
// which is the only reason abstaining preserves anything here — see
// LaunchPosture).
//
// Dropping the drive is a fact about this launch. Un-choosing it would be a claim
// about the conversation, and this launch is not entitled to make it.
func copilotAPIPostureToRecord(params *NewParams) *bool {
	if params.copilotAPIDriveDropped {
		return nil
	}
	return &params.CopilotAPI
}

// copilotAPIDriveRequest is one launch's answer to "who is asking for the drive,
// and can anything provide it".
type copilotAPIDriveRequest struct {
	// Harness is the resolved harness for this launch. A harness with no
	// API-backed mode makes every field below irrelevant.
	Harness *harness.Harness
	// Drive is the resolved `--copilot-api` for this launch, after
	// harness.ResolveCopilotAPI and after the relaunch carryover has had its say.
	Drive bool
	// CarriedFromConv is the conversation the drive was CARRIED from by
	// applyRecordedLaunchPosture, empty when this launch asked for the drive
	// itself. That is the request-versus-inference distinction the policy turns
	// on, and it is recorded by the carryover rather than guessed at here: the
	// carryover is the only place that knows, and a launch cannot otherwise tell
	// a flag it was handed from a flag it was typed.
	CarriedFromConv string
	// ManagedLaunch / Port feed copilotAPIDriveIsDaemonOwned.
	ManagedLaunch bool
	Port          int
	// SendKeys is the deliberate override for the refusal below. Spelled exactly
	// as `tclaude conv resume --send-keys` (TCL-1076) so an operator who learns
	// the escape on one surface is not walled out on the other.
	SendKeys bool
}

// resolveCopilotAPIDriveForLaunch decides what a `tclaude session new` launch may
// do with the Copilot API drive, and returns the drive this launch should
// actually run with plus anything the operator must be told.
//
// # The defect this closes
//
// `session new` is the surface that HONOURS the drive: it resolves
// `--copilot-api` for real, allocates a port when none was supplied, and renders
// `copilot --ui-server --host 127.0.0.1 --port n`. All of that is correct under
// agentd, which is what it was built for — the daemon allocates the port before
// forking, creates the RPC session, holds the handle and proves ownership.
//
// Typed by a human with no daemon in the loop, the same argv produces a pane
// with an unauthenticated loopback JSON-RPC endpoint that nothing will ever
// dial. Nothing outside pkg/claude/agentd dials it, the port record is written
// only by agentd, reconcileCopilotAPISessions takes its candidates from that
// record and deliberately refuses to create or resume a session it never owned,
// and the pane's own startup session is not drivable. For a MANAGED
// conversation it compounds: the posture reads API, so agentd routes its mail
// over a channel no handle exists for, and since TCL-1058 delivery HOLDS rather
// than falling back to keystrokes. The agent goes deaf while its pane looks
// healthy.
//
// Note the direction. TCL-1076 fixed the surfaces that DROP the drive; this is
// the mirror-image gap in the surface that honours it, and it reaches the same
// end state from the opposite side.
//
// # The policy, and why it is not uniform
//
// Assert-and-refuse, infer-and-disclose:
//
//   - An EXPLICIT `--copilot-api` is the operator asserting something. Silently
//     downgrading an assertion is the failure TCL-1076 exists to stop, so it is
//     refused with the reason named.
//   - A CARRIED drive is tclaude inferring from a record. An inference that
//     turns out to be unserviceable should say so and proceed on the known-good
//     side — send-keys — rather than block a human to protect nothing.
//   - Except when the conversation is a live managed agent, where proceeding
//     produces the deafness above. That is refused, with both ways out named.
//
// The same axis as LaunchPosture's pointer fields one layer up: a value you
// resolved may be asserted, a value you merely inherited may not be acted on as
// though you had.
//
// # The record is left alone either way
//
// This function never writes. When it drops a carried drive the caller passes
// nil for LaunchPosture.CopilotAPI rather than false, so the conversation's
// choice survives a launch that could not honour it — the TCL-1076 rule that a
// surface which cannot produce the channel must not author the posture. Dropping
// the drive is a fact about THIS launch; un-choosing it would be a claim about
// the conversation.
//
// Silent for every launch that did not ask for the drive, including every
// non-Copilot harness: the recorded-drive read below is reached only when a
// drive is actually on the table.
func resolveCopilotAPIDriveForLaunch(req copilotAPIDriveRequest) (bool, string, error) {
	if !req.Drive {
		return false, "", nil
	}
	if req.Harness == nil || !req.Harness.SupportsCopilotAPI() {
		// harness.ResolveCopilotAPI has already refused an explicit opt-in for a
		// harness with no API mode, and a carry cannot survive one either. Being
		// defensive here rather than assuming it, since a wrong answer would refuse
		// a launch for a reason that does not apply to it.
		return req.Drive, "", nil
	}
	if copilotAPIDriveIsDaemonOwned(req.ManagedLaunch, req.Port) {
		return true, "", nil
	}
	if req.CarriedFromConv == "" {
		return false, "", fmt.Errorf(
			"copilot_api_drive_needs_agentd: --copilot-api binds an unauthenticated " +
				"loopback JSON-RPC endpoint, and only tclaude agentd allocates its port, " +
				"creates the session and holds the connection. Launched from here the pane " +
				"would come up with that endpoint listening and nothing driving it, which is " +
				"strictly worse than the send-keys default.\n" +
				"  drive it over the API: tclaude agent spawn ... --copilot-api\n" +
				"  launch it from here:   the same command without --copilot-api")
	}
	live, err := db.IsLiveAgentConv(req.CarriedFromConv)
	if err != nil {
		// Fail closed. An unreadable actor row is not evidence that nothing routes
		// this conversation, and proceeding on that assumption is what produces the
		// deaf agent.
		return false, "", fmt.Errorf(
			"resolve whether conversation %s is a live managed agent: %w",
			req.CarriedFromConv, err)
	}
	if live && !req.SendKeys {
		return false, "", fmt.Errorf(
			"copilot_api_drive_needs_agentd: conversation %s chose the Copilot API drive "+
				"and is a live managed agent, so agentd routes its messages over that "+
				"channel. This launch cannot create the channel — only agentd can — and "+
				"since TCL-1058 an API conversation with no provable channel HOLDS its mail "+
				"rather than typing it into the pane, so the agent would come up looking "+
				"healthy and hearing nothing.\n"+
				"  relaunch it on its own drive:   tclaude agent resume %s\n"+
				"  relaunch it on keystrokes anyway: add --send-keys (its mail keeps holding "+
				"until it is relaunched through agentd)",
			req.CarriedFromConv, req.CarriedFromConv)
	}
	notice := fmt.Sprintf(
		"Warning: conversation %s chose the Copilot API drive; this launch runs on tmux "+
			"send-keys instead, because only tclaude agentd can create that channel. The "+
			"conversation's recorded drive is left untouched, so relaunching it through "+
			"agentd picks the API back up.", req.CarriedFromConv)
	if live {
		notice += fmt.Sprintf(
			"\nThis is a live managed agent and you passed --send-keys: its mail will HOLD "+
				"rather than being typed into the pane until it is relaunched with "+
				"'tclaude agent resume %s'.", req.CarriedFromConv)
	}
	return false, notice, nil
}
