package agentd

import (
	"log/slog"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// What a launch owes its conversation id.
//
// # The moment this file is about
//
// A launch and a conversation id do not become real at the same instant, and
// they do not always become real in the same order. Most launches PRESET the id
// (an enrolled spawn, a resume, a reincarnation), so it exists before the fork;
// an unenrolled clone or reincarnation lets the harness MINT one and discovers
// it afterwards from the session row. Either way there is exactly one moment per
// launch at which the daemon first holds both the launch and the id it belongs
// to, and everything durable that ties the two together has to happen there.
//
// # Why it is ONE function rather than three calls
//
// Because the failure being guarded against is somebody adding a fifth launch
// site and remembering two of the three. That is not hypothetical: the port
// record and the bootstrap were already paired by hand at four sites, with a
// comment at one of them explaining that pairing them was the point — and the
// posture write, which arrived later and matters more, was at none of them. A
// caller that can record a port without recording the posture will eventually do
// exactly that, so the seam is shaped so it cannot.
//
// [TestCopilotLaunchesRecordPortAndPostureTogether] enforces it structurally.
type copilotAPILaunchKind string

const (
	// copilotAPILaunchFresh is a launch whose conversation starts empty: a
	// spawn, a reincarnation's successor, a clone's sibling. The pane was
	// started with `--session-id` (or let the harness mint one) and has no
	// history behind it.
	copilotAPILaunchFresh copilotAPILaunchKind = "fresh"
	// copilotAPILaunchResume is a launch that continues an EXISTING
	// conversation: the pane was started with `--resume=<convID>` and the
	// conversation's history is already on disk.
	//
	// This distinction is threaded down from the spawn facade rather than
	// inferred at the bootstrap, and that is deliberate. It is a fact known for
	// certain at the top of the call stack — the facade IS the fresh/resume
	// choice — and anything the bootstrap could compute from the conversation's
	// on-disk state would be a proxy for it. The cost of getting it wrong is not
	// symmetric either: treating a resume as fresh destroys the conversation
	// (see bootstrapCopilotAPISession), while treating a fresh launch as a
	// resume merely fails to find a session.
	copilotAPILaunchResume copilotAPILaunchKind = "resume"
)

// completeCopilotAPILaunch records everything a launch owes convID and starts
// its API channel.
//
// Ordering is deliberate: the durable posture is written FIRST, because it is
// what decides how messages to this conversation are routed, and the bootstrap
// it precedes is what makes the conversation reachable at all.
//
// A no-op for every non-Copilot harness, and for a conv id that is not known
// yet — the callers that mint their id late call this again from the point they
// discover it.
//
// # The launch generation
//
// Taken FIRST, before anything else, and handed to the bootstrap rather than
// looked up by it. It is this launch's identity, and the reason it is a
// parameter rather than something the bootstrap reads for itself is that the
// bootstrap needs it at the moment it FAILS — which may be its whole budget
// later, by which time a relaunch may own the conversation. A value read at the
// failure path would be the successor's.
//
// It is also deliberately not derived from the port: a bootstrap that dies
// inside verifiedCopilotAPIPort never held one, so an identity taken from the
// verified port would be missing in exactly the case that needs it most. Passing
// it in leaves the tidier-looking refactor with nothing to take.
//
// Counted for every launch this function completes, not only API-driven ones,
// because what it marks is "a new launch owns this conversation now" — which is
// what supersedes an earlier launch's observation, and is just as true when the
// relaunch is on send-keys.
func completeCopilotAPILaunch(convID string, kind copilotAPILaunchKind, args clcommon.SpawnArgs) {
	generation := copilotAPISessions.NoteLaunch(convID, args.CopilotAPI)
	recordCopilotAPIPosture(convID, args)
	recordCopilotAPIPort(convID, args.CopilotAPIPort)
	startCopilotAPIBootstrap(convID, args.CopilotAPI, kind, args.InitialPrompt, generation)
}

// recordCopilotAPIPosture freezes which drive this launch took, against the
// conversation it launched.
//
// Written for a Copilot launch even when the drive is OFF. The value being
// recorded is not "is this agent on the API" but "which channel did this launch
// CHOOSE", and a recorded false is a different fact from no record at all:
// copilotAPIDriven may act on the first and must never invent the second. The
// same rule, and the same wording, as durableRelaunchConfig's own freeze.
//
// Best-effort in the same sense recordCopilotAPIPort is, and for the same
// reason: the pane is already starting by the time this runs, so returning an
// error would report a failed spawn for a launch that succeeded and send the
// caller into a rollback it should not do. Unlike the port record this failure
// is loud rather than merely discoverable — a conversation whose posture is
// missing is one whose messages route to keystrokes, so it is logged at error
// level with the value that was lost.
func recordCopilotAPIPosture(convID string, args clcommon.SpawnArgs) {
	if convID == "" || harnessOrDefault(args.Harness) != harness.CopilotName {
		return
	}
	// nil attribution: this records what the launch RESOLVED, which is not the
	// same as someone having chosen it — args.CopilotAPI is frozen non-nil for
	// every Copilot launch including false, and SpawnArgs carries no tier to name.
	// Claiming an explicit choice here would let a from-group snapshot carry an
	// observed default as a curated spec line (TCL-1090). Leaving the attribution
	// alone keeps this write saying only what it can support.
	if err := db.SetConversationCopilotAPI(
		convID, harness.CopilotName, args.Cwd, args.CopilotAPI, nil,
	); err != nil {
		slog.Error("failed to record the Copilot drive this launch took; until the "+
			"launched process records it, messages to this conversation route as send-keys",
			"conv_id", convID, "copilot_api", args.CopilotAPI, "error", err)
	}
}
