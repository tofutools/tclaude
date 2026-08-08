package agentd

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Driving an API-connected Copilot agent.
//
// # What this file is for
//
// tclaude's default way of making a pane do something is tmux `send-keys`, and
// CLAUDE.md names in-pane slash-command delivery an injection sink for exactly
// that reason: an agent-to-agent message and a lifecycle command travel the
// same keystroke stream as the human's typing, so the two can only be kept
// apart by gating the text. For a Copilot agent launched with the API drive
// there is a typed channel instead, and a typed call has no keystroke stream to
// escape from. This file is that channel's verbs.
//
// # It is not a fallback pair
//
// An API-connected conversation NEVER falls back to send-keys when a call
// fails. Falling back would silently return exactly the agent that opted out of
// the keystroke path to it, one failure at a time, and the caller would have no
// way to tell which channel actually carried the text. Every verb here returns
// an error the caller reports or retries, the same rule the managed OpenCode
// path follows.
//
// # It is not a second way to reach the port
//
// Nothing here dials anything. Every verb runs on the connection the bootstrap
// established and the registry holds, which is the only connection in the
// daemon whose far end was proved to belong to the agent's pane. See
// copilot_api_bootstrap.go, and the structural test that pins it.

// copilotAPIDriveTimeout bounds an ordinary drive call.
//
// These are local RPCs against a server in the same host's memory: the ones
// measured here answer in single-digit milliseconds. The budget is generous
// anyway because the failure it guards is a wedged server, and a caller
// blocking forever on one is worse than a caller reporting a slow channel.
// Compaction is not an ordinary call and does not use this; see
// copilotAPICompactTimeout.
const copilotAPIDriveTimeout = 20 * time.Second

// copilotAPICompactTimeout bounds a compaction.
//
// `session.history.compact` runs a summarization turn on the model and returns
// only when the new history is in place, so it is bounded like a model turn
// rather than like an RPC.
const copilotAPICompactTimeout = 5 * time.Minute

// copilotAPINameMaxRunes is the server's own limit on a session name.
// Enforcing it here turns a doomed request into a specific local message
// instead of a generic RPC failure at the far end.
const copilotAPINameMaxRunes = 100

// copilotAPIDriven reports whether a conversation's deliveries BELONG to the
// API channel. It is the routing question, and it is answered by the launch's
// durable opt-in — NOT by whether a connection happens to exist right now.
//
// Keeping it separate from "may I send on it right now" is load-bearing, and
// collapsing the two is the mistake this seam has now been written with twice,
// each time one level further out. A predicate that answers false because
// something about the channel is wrong is indistinguishable, at the call site,
// from one that answers false because the conversation never took the drive —
// and a caller written as `if driven { api } else { keystrokes }` routes both
// into the pane. That is not a graceful degradation; it is the injection sink
// re-opening precisely for the agent whose channel is in trouble, which is the
// worst possible moment for it.
//
// The first version re-proved port ownership here, so a live-but-unprovable
// handle fell through to keystrokes. The second answered "does a handle exist",
// which fixed that case and left three more, all reachable and none of them
// exotic:
//
//   - The bootstrap window. The bootstrap is a background goroutine whose first
//     step waits up to a minute for the pane to bind its port. Every delivery in
//     that minute predates the handle.
//   - A bootstrap that failed. There is then no handle, ever, for the life of
//     the launch.
//   - An agentd restart. Handles live in memory only, so every
//     already-running API-driven agent has none and cannot acquire one short of
//     relaunching (see the registry's own note). That state is durable.
//
// In all three the launch DID opt out of keystrokes, so the honest answer is to
// hold the delivery and report the channel unavailable — the caller retries, or
// the operator sees a named failure — rather than quietly typing it in.
//
// # Why not the port record
//
// TCL-1054's rule is that nothing may reach the endpoint from a port the
// verified accessor did not return, and TCL-1056's is that CONNECTEDNESS is
// derived from the connection. Neither is violated here: this reads the
// launch's recorded posture (the relaunch profile), not a port, and it is not
// claiming anything about a connection. "Which channel does this belong to" and
// "is that channel up" are the two questions, and this is the first one.
//
// The second one is [copilotAPIConnected], and it is named there rather than
// left to be spelled out at each call site because leaving it unnamed is what
// produced the third instance of this confusion: a caller wanting "is it up"
// found this predicate adjacent, documented and compiling. See that function.
//
// # Cost
//
// Two small SQLite reads per delivery decision, on paths that are already doing
// database work and a tmux probe. Deliberately NOT used by the dashboard's
// snapshot tick, which has its own cheaper intent read.
func copilotAPIDriven(convID string) bool {
	if copilotAPISessions.Handle(convID) != nil {
		// The fast, unambiguous answer. A live handle exists only for a launch
		// that took the drive, so this needs no database read.
		return true
	}
	return harnessForConv(convID).Name == harness.CopilotName &&
		copilotLaunchIntentForConv(convID).API
}

// copilotAPIConnected reports whether a conversation's API channel is UP right
// now. It is answered by the live handle and by nothing else.
//
// # Why this exists as a name
//
// It is the third question this seam asks, and until TCL-1080 it was the only
// one without a name here. [copilotAPIDriven] answers "which channel do this
// conversation's deliveries BELONG to"; [copilotAPIDrive] answers "may I send
// on it for THIS call"; this answers "has it come up YET". The registry has
// always been able to answer it — this delegates to
// [copilotAPISessionRegistry.Connected], which is still the implementation and
// still the place the reasoning about the port record lives — but a method on
// a registry in another file is not part of the vocabulary a caller reads when
// it opens this one.
//
// That is not a stylistic point. The spawn's post-init wait wanted this
// question, found `copilotAPIDriven` adjacent and documented, and looped on it;
// `copilotAPIDriven` is true from the durable posture that
// completeCopilotAPILaunch writes BEFORE it starts the bootstrap, so the wait
// returned on its first iteration and every API-drive spawn lost its rename and
// its welcome. That was the third instance of this conflation, and the second
// where the code and a comment warning against it shipped in the same commit —
// so the fix is a predicate to reach for, not another warning. Prose cannot
// bind a call site; a name the call site can find might.
//
// # What may NOT be derived from it
//
//   - NOT a routing decision. A false here means "not up yet" or "not up any
//     more", never "this conversation is on send-keys". Writing
//     `if connected { api } else { keystrokes }` is the injection sink
//     re-opening for exactly the agent whose channel is in trouble — which is
//     the whole reason [copilotAPIDriven] is a separate predicate. Route on
//     that one.
//   - NOT permission to send. Connectedness is true at one instant; the
//     ownership re-proof that stands in for this endpoint's missing
//     authentication is per call and lives in [copilotAPIDrive]. A caller that
//     checks this and then sends has re-derived the single-predicate seam
//     TCL-1058 removed.
//
// The one caller entitled to act on a false is [waitForCopilotAPISession],
// and only because it acts on "false for the bootstrap's WHOLE budget", which
// is a different fact — see the argument there.
func copilotAPIConnected(convID string) bool {
	return copilotAPISessions.Connected(convID)
}

// copilotAPIChannelFailed reports whether this conversation's CURRENT launch has
// been observed not to be getting an API channel at all.
//
// # It is an OBSERVATION, and that is the whole of its meaning
//
// Every other entry in this vocabulary READS a fact somebody else wrote. This
// one is written by the daemon, from something it watched happen, and it is the
// first thing in this seam that bears on routing without being a record of
// anybody's intent. So be exact about what it states:
//
//   - It says THIS LAUNCH's channel is not coming up. It does NOT say the agent
//     is on send-keys, and it must never be stored, displayed or reasoned about
//     as the agent's drive. The agent chose the API drive and still has; a
//     bootstrap that lost a race with a loaded host has not un-chosen anything,
//     and the next relaunch must try the API drive again. Writing this fact into
//     the agent's relaunch profile would turn a per-agent operator toggle into
//     one decided by weather. TCL-1082's manual un-choose is a DECISION, with the
//     opposite lifetime and the opposite behaviour on relaunch and on reconnect;
//     it is a different record and the two must not be merged.
//   - It is keyed to a launch generation, not to a conversation. A bootstrap can
//     fail up to its whole budget after it started, and relaunching is what an
//     operator does about a deaf agent — so an observation that spoke for the
//     conversation would libel the healthy launch that replaced it. The
//     comparison lives inside the registry's lock; see NoteChannelFailed.
//   - It is in memory, and it must stay there. Persisting it would assert at
//     daemon startup a fact that is not yet in evidence: at that moment nobody
//     knows which channels are adoptable, and reconcileCopilotAPISessions is
//     what finds out. A durable "failed" would route to send-keys an agent that
//     the sweep is seconds away from reconnecting — the silent degradation the
//     hold rule exists to prevent, reintroduced by the fix for it.
//
// # What a FALSE does not mean
//
// Not "the channel is fine". The ordinary false is "nothing has been observed
// about this conversation yet", which covers the whole bootstrap window, every
// already-running agent between an agentd restart and the sweep reaching it, and
// every candidate the sweep ran out of time to examine. A caller that reads a
// false as health has read an absent measurement as a good one. Callers wanting
// "is it up" want [copilotAPIConnected]; callers wanting "which channel does
// this belong to" want [copilotAPIDriven].
//
// # Why it does not route, and why nothing revokes
//
// It has a read-only consumer and nothing else, and that is now a settled
// decision rather than a deferral. THE DAEMON NEVER REVOKES A CONVERSATION'S API
// POSTURE. No code path demotes an agent to send-keys because its channel failed;
// [TestNoAutomaticRevokeOfTheCopilotAPIPosture] is what keeps that true.
//
// The reason is that the API drive is a CONSTRAINT, not an optimisation. An
// operator who turns it on is saying "do not put bytes in this agent's pane", so
// a revoke IS the injection sink re-opening, and a revoke the daemon performs by
// itself withdraws the opt-out by weather rather than by the person who chose it.
// The drive is also still unverified in real use, which sharpens it: a silent
// demote would make the drive's failures unobservable at exactly the moment they
// are most informative, destroying the evidence this phase exists to collect. A
// held agent is a loud, cheap, recoverable failure; a demoted one looks healthy
// while doing the thing its operator opted out of, and only the first kind gets
// fixed.
//
// The remedy is the operator's, and it already exists: relaunch the agent
// (POST /api/agents/{id}/restart, which the dashboard's own control posts to).
// That retries the API drive, because the relaunch profile still says API — which
// is the point. A demote would spend the agent's posture permanently to fix one
// launch, trading a recoverable state for an unrecoverable one to avoid an action
// the operator can already take.
//
// THAT REMEDY IS A DEPENDENCY OF THIS DECISION, not a convenience beside it. The
// position is not "holding mail forever is acceptable"; it is "holding is
// acceptable BECAUSE the operator can already fix it in one click". The restart
// control requires the agent to be idle, which does not bite for a deaf agent —
// it has received nothing, so it is idle almost by construction. If that ever
// changes, or restart otherwise stops being reachable for an agent in this state,
// this decision has lost its foundation and needs re-making rather than
// inheriting.
//
// THIS CALL IS PHASE-DEPENDENT, AND A LATER READER SHOULD KNOW IT RATHER THAN
// INFER IT. "Constraint, no automatic revoke" is right while the drive is
// unverified. Once it is verified and in ordinary use the balance may genuinely
// shift: a mature drive that occasionally fails to come up is much closer to an
// optimisation, and holding mail forever is a harsher default then than it is
// now. So this is a decision to REVISIT AT VERIFICATION, not a permanent property
// of the design — a reader who finds an unexplained hold and "fixes" it would be
// reasoning correctly from a premise that had expired.
//
// # The distinction whoever revisits it must inherit
//
// "Revoke" carries two different changes, and keeping them apart is most of the
// problem:
//
//   - Route THIS LAUNCH's mail to keystrokes. Lifetime: the launch.
//   - This AGENT is no longer an API agent. Lifetime: the agent.
//
// The second is the only one the database can express, and the first is the only
// one this observation's evidence supports. A conversation-scoped write does not
// bridge them: [copilotLaunchIntentForConv] reads the agent relaunch profile
// first and consults the conversation fallback only for fields the profile left
// nil, and relaunchProfileForSpawn freezes CopilotAPI non-nil for every Copilot
// launch — so a revoke written to the conversation fallback is INERT for every
// spawned agent, which is precisely the population that has this problem.
func copilotAPIChannelFailed(convID string) bool {
	return copilotAPISessions.ChannelFailed(convID)
}

// copilotAPIDrive returns the handle to send on, or an error saying why not.
//
// The ownership re-proof is the point of routing every verb through here.
// `--ui-server` has no authentication, so what stands in for a credential is
// the conjunction StillOwned checks: our connection is still open AND the
// agent's pane subtree still holds the listener we dialled. It is two small
// kernel table reads, and the bootstrap's own proof is one-shot — re-asking
// before each send is what the comment on StillOwned means by "callers should
// re-ask before acting on anything that matters".
//
// A handle that fails the re-proof is left in the registry rather than dropped.
// The two halves it reads can both fail transiently (a /proc walk racing a
// re-exec), and dropping would close a working connection and report the agent
// as disconnected until it relaunched — turning a moment's uncertainty into a
// durable false statement. Refusing this one call costs a retry.
func copilotAPIDrive(convID string) (*copilotAPISession, error) {
	handle := copilotAPISessions.Handle(convID)
	if handle == nil {
		return nil, fmt.Errorf(
			"conversation %s is not connected to a Copilot API session", convID)
	}
	if !handle.StillOwned() {
		return nil, fmt.Errorf(
			"the Copilot API port %d for %s can no longer be shown to belong to the agent's "+
				"pane subtree: refusing to drive it. This endpoint has no authentication, so "+
				"an unverified listener cannot be told apart from another agent's",
			handle.Port, convID)
	}
	return handle, nil
}

// awaitCopilotAPISession waits for an API-driven launch's channel to come up,
// and reports whether it did.
//
// This exists for the spawn's post-init delivery, and the reason is a
// correctness bug rather than a preference. An API-drive launch renders no
// `-i`, and the bootstrap does not attach to the pane's startup session — it
// CREATES a session under the conversation id and foregrounds it, which starts
// the id fresh (measured in TCL-1056: `alreadyInUse:false`, an empty
// getMessages). So anything typed into the pane between "the pane is alive" and
// "the bootstrap has foregrounded our session" goes into a session that is
// about to be replaced, and is simply lost. For a spawn that is the agent's
// rename and its entire welcome — its identity, its group, and the pointer to
// the briefing waiting in its inbox. An agent that lost that looks, on every
// dashboard surface, exactly like one that read it and had nothing to say.
//
// The budget is the bootstrap's own whole budget: waiting longer than the
// bootstrap can possibly take proves nothing, and waiting less would abandon a
// healthy launch on a loaded host.
//
// # What it waits ON
//
// [copilotAPIConnected], and deliberately not [copilotAPIDriven]. The routing
// predicate is true from the durable posture that completeCopilotAPILaunch
// writes BEFORE it starts the bootstrap, so a wait on it returns on its first
// iteration and this function does nothing at all — which is what it did until
// TCL-1080, and it cost every API-drive spawn its rename and its welcome.
//
// Connectedness is also the STRONGER of the two in exactly the way this caller
// needs. For a launch — the only situation this wait runs in — the handle comes
// from runCopilotAPIBootstrap, and only after the whole bootstrap succeeded:
// session created, foregrounded, and the launch prompt delivered. So a true
// here is not "a port answered" but "the session the post-init delivery is
// about to land in is the one the pane is showing".
//
// Be precise about the scope of that, because it is a property of THIS caller's
// situation and not of the registry. reconcileCopilotAPISessions (TCL-1074)
// also adopts, through AdoptIfAbsent, and it deliberately does NOT foreground —
// it reconnects to a session that is already the pane's. Nobody should derive a
// general "connected implies foregrounded" from this paragraph. It holds here
// because the reconcile takes its candidate list once at daemon start, over
// conversations that already existed, so a spawn happening now cannot be in it.
//
// Returns false when the channel never came up, which is a real state rather
// than a timeout to retry — the bootstrap logs why. Its caller then falls back
// to the pane, and that fallback is NOT the one copilotAPIDriven exists to
// prevent: a bootstrap that never completed leaves the pane's own startup
// session in the foreground, so the pane genuinely IS the agent's channel, and
// the alternative is an agent that is never told who it is. That is the same
// call the bootstrap already makes for itself — a failed channel is a loud log
// line and an agent still usable through its pane, not a failed spawn.
//
// The fallback is safe against the obvious race — falling back to the pane a
// moment before the bootstrap foregrounds a fresh session over it — and safe by
// construction rather than by margin. Both deadlines are
// copilotAPIBootstrapTimeout(); the bootstrap's starts at
// completeCopilotAPILaunch, inside the spawn facade, and this one starts later,
// after runSpawnPostInit has waited for the pane to come alive. So when this
// wait expires the bootstrap's own context is already cancelled and it can no
// longer create or foreground anything.
// [TestCopilotDrive_ThePostInitWaitStartsAfterTheBootstrapDoes] pins the start
// order and [TestTheSpawnPostInitWaitGivesUpAtTheBootstrapsBudget] pins that
// this wait uses that budget, since together they are what make the fallback a
// decision rather than a gamble.
//
// Indirected through a variable for the same reason the bootstrap kick-off is:
// flow tests install a no-op bootstrap binary-wide, so no handle can ever
// appear, and a spawn that waited the full budget for one would spend a minute
// and a half waiting for something the test itself disabled. See agentd's
// TestMain.
var awaitCopilotAPISession = waitForCopilotAPISession

// SetCopilotAPIPostInitWaitForTest swaps the post-init wait and returns a
// restore function. TestMain installs a binary-wide "it never came up", which
// is the truthful answer when the bootstrap is stubbed out.
func SetCopilotAPIPostInitWaitForTest(fn func(convID string) bool) func() {
	previous := awaitCopilotAPISession
	awaitCopilotAPISession = fn
	return func() { awaitCopilotAPISession = previous }
}

func waitForCopilotAPISession(convID string) bool {
	deadline := time.Now().Add(copilotAPIBootstrapTimeout())
	for {
		if copilotAPIConnected(convID) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(copilotAPIPollInterval)
	}
}

// sendCopilotAPIMessage delivers one user message to an API-connected agent.
//
// The delivery mode is deliberately the server default, `enqueue`. Measured
// against Copilot CLI 1.0.78: an enqueued message lands in the queue behind the
// turn in flight and behind anything already queued — including what the human
// typed into the pane — while `immediate` lands in a separate steering lane
// that runs FIRST once the current turn unwinds. Neither interrupts a running
// turn, so `immediate` buys no promptness; it only lets an agent-to-agent
// message overtake the human's own queued input, which is a reordering nobody
// asked for. See copilotapi.SendModeEnqueue.
//
// This is also what makes the API path strictly better than the keystrokes it
// replaces: text typed into a pane mid-turn lands in whatever the TUI's input
// box happens to be, while an enqueued message is one whole user turn or
// nothing.
func sendCopilotAPIMessage(convID, text string) error {
	handle, err := copilotAPIDrive(convID)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), copilotAPIDriveTimeout)
	defer cancel()

	messageID, err := handle.Client.Send(ctx, copilotapi.SendParams{
		SessionID: handle.SessionID, Prompt: text,
	})
	if err != nil {
		return fmt.Errorf("deliver a message to Copilot session %s for %s: %w",
			handle.SessionID, convID, err)
	}
	slog.Info("copilot API: message delivered over session.send",
		"conv_id", convID, "session_id", handle.SessionID, "message_id", messageID)
	return nil
}

// renameCopilotAPISession renames an API-connected agent's session.
//
// `session.name.set` is not merely the nearest RPC to `/rename`; it is the same
// write. Measured: it sets `name` and `user_named: true` in the session's
// workspace.yaml, which is the exact file copilotConvStore.Title reads. So this
// changes what tclaude reports without tclaude ever writing Copilot's state
// behind the CLI's back — the reason copilotConvStore.SetTitle refuses to
// exist.
//
// The length check is the server's own 1–100 rule, applied here so an
// over-long title fails with a sentence naming the limit rather than as a
// generic RPC error. The caller's charset gate is unchanged and still runs: it
// belongs to the send-keys path, which is still the default for Copilot, and
// this ticket removes an injection sink for API-mode agents rather than
// relaxing a gate for everyone else.
func renameCopilotAPISession(convID, title string) error {
	handle, err := copilotAPIDrive(convID)
	if err != nil {
		return err
	}
	trimmed := strings.TrimSpace(title)
	if trimmed == "" {
		return fmt.Errorf("copilot rejects an empty session name for %s", convID)
	}
	if count := len([]rune(trimmed)); count > copilotAPINameMaxRunes {
		return fmt.Errorf(
			"copilot rejects a session name longer than %d characters, and %s's is %d",
			copilotAPINameMaxRunes, convID, count)
	}
	ctx, cancel := context.WithTimeout(context.Background(), copilotAPIDriveTimeout)
	defer cancel()

	if err := handle.Client.SetSessionName(ctx, handle.SessionID, trimmed); err != nil {
		return fmt.Errorf("rename Copilot session %s for %s: %w", handle.SessionID, convID, err)
	}
	slog.Info("copilot API: session renamed over session.name.set",
		"conv_id", convID, "session_id", handle.SessionID, "title", trimmed)
	return nil
}

// compactCopilotAPISession compacts an API-connected agent's history, then
// submits followUp when one was asked for.
//
// # Why it runs in the background
//
// Unlike every other verb here, compaction is a model turn: it returns when the
// new history is in place, which is seconds to minutes. The daemon's own
// clients time out in ten seconds, and the send-keys path this replaces has
// always been fire-and-forget, so a request path must not wait for it. What the
// caller gets back is "requested", which is exactly as much as `send-keys
// /compact` ever established, and the outcome is logged.
//
// # The follow-up is genuinely ordered here
//
// On send-keys the follow-up bytes queue in the pty and may land in a textarea
// that is still busy; on OpenCode the pair is refused outright because its
// command publication does not acknowledge completion. `session.history.
// compact` resolves only once compaction is done, so submitting the follow-up
// after it returns is ordered by construction — the one place this transport is
// better than a promise rather than merely safer.
//
// # "Nothing to compact" is not a failure
//
// The server refuses a session with no history worth summarizing, and for an
// agent that has barely started that is the correct answer rather than a fault.
// It is logged as the ordinary outcome it is, and the follow-up is still
// submitted: the caller asked for it after compaction, not after a successful
// compaction, and dropping it would leave an agent waiting for a prompt that
// silently never came.
//
// # Concurrent compactions are not deduplicated
//
// Two overlapping requests start two goroutines, each running its own
// summarization turn and each submitting its own follow-up. Stated rather than
// fixed: send-keys had exactly the same shape (two `/compact` submissions), so
// this is not a regression, and the server serialises the actual work. Worth
// revisiting only alongside a general "one lifecycle operation in flight per
// conversation" rule, which is not this ticket's to invent.
func compactCopilotAPISession(convID, followUp string) error {
	// Proved here so the caller gets a synchronous error for a conversation that
	// cannot be driven at all. This proof does NOT travel with the handle — see
	// runCopilotAPICompaction, which proves again on the goroutine that sends.
	if _, err := copilotAPIDrive(convID); err != nil {
		return err
	}
	go runCopilotAPICompaction(convID, followUp)
	return nil
}

// runCopilotAPICompaction re-proves ownership and then compacts.
//
// It takes a conv id rather than the handle its caller already proved, and that
// is the whole point. `--ui-server` has no authentication, so what stands in for
// a credential is the proof that the listener still belongs to the agent's pane
// subtree — and a proof taken on one goroutine says nothing about the moment a
// different goroutine sends. This was the one send in the package whose proof
// did not share a call stack with it (TCL-1075), which is exactly the gap the
// dialling guard was widely believed to cover and never did.
func runCopilotAPICompaction(convID, followUp string) {
	handle, err := copilotAPIDrive(convID)
	if err != nil {
		// Refused rather than sent. The port may now belong to a different
		// process, and this endpoint has no way to tell us apart from it.
		slog.Warn("copilot API: refusing to compact on a connection whose ownership "+
			"could no longer be proved", "conv_id", convID, "error", err)
		// The follow-up still goes out, and it goes through sendCopilotAPIMessage
		// rather than over a handle from here. DO NOT "simplify" this into a
		// direct send using a handle captured above: the reason we are in this
		// branch is that we would not send Compact to that port, and the
		// follow-up is different bytes to the same stranger. sendCopilotAPIMessage
		// re-asks on its own account, so it either reaches a port we can still
		// prove is ours or refuses for the same reason this did.
		deliverCopilotAPICompactionFollowUp(convID, "", followUp)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), copilotAPICompactTimeout)
	defer cancel()

	result, err := handle.Client.Compact(ctx, copilotapi.CompactParams{
		SessionID: handle.SessionID, Trigger: copilotapi.CompactTriggerManual,
	})
	switch {
	case copilotapi.IsNothingToCompact(err):
		slog.Info("copilot API: nothing to compact",
			"conv_id", convID, "session_id", handle.SessionID)
	case err != nil:
		slog.Warn("copilot API: compaction failed",
			"conv_id", convID, "session_id", handle.SessionID, "error", err)
		// The follow-up is still submitted below. A compaction that failed
		// leaves the agent exactly where it was, which is a worse place to also
		// leave it un-prompted.
	default:
		// Not the whole result: SummaryContent is the generated summary and runs
		// to thousands of characters. TokensRemoved is signed on purpose — a
		// short session's summary can be larger than the history it replaced.
		slog.Info("copilot API: history compacted over session.history.compact",
			"conv_id", convID, "session_id", handle.SessionID,
			"tokens_removed", result.TokensRemoved,
			"messages_removed", result.MessagesRemoved)
	}

	deliverCopilotAPICompactionFollowUp(convID, handle.SessionID, followUp)
}

// deliverCopilotAPICompactionFollowUp submits the prompt the caller asked for
// after compaction, whatever happened to the compaction itself — including the
// case where it was refused because ownership could not be re-proved.
//
// sessionID is for the log line only and is empty when no handle was proved; the
// delivery deliberately does not take one, because the follow-up must not ride a
// handle this function was handed.
func deliverCopilotAPICompactionFollowUp(convID, sessionID, followUp string) {
	if followUp == "" {
		return
	}
	if err := sendCopilotAPIMessage(convID, followUp); err != nil {
		slog.Warn("copilot API: compaction follow-up failed",
			"conv_id", convID, "session_id", sessionID, "error", err)
	}
}
