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
// Returns false when the channel never came up, which is a real state rather
// than a timeout to retry — the bootstrap logs why. Its caller then falls back
// to the pane, and that fallback is NOT the one copilotAPIDriven exists to
// prevent: a bootstrap that never completed leaves the pane's own startup
// session in the foreground, so the pane genuinely IS the agent's channel, and
// the alternative is an agent that is never told who it is. That is the same
// call the bootstrap already makes for itself — a failed channel is a loud log
// line and an agent still usable through its pane, not a failed spawn.
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
		if copilotAPIDriven(convID) {
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
	handle, err := copilotAPIDrive(convID)
	if err != nil {
		return err
	}
	go runCopilotAPICompaction(handle, convID, followUp)
	return nil
}

func runCopilotAPICompaction(handle *copilotAPISession, convID, followUp string) {
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

	if followUp == "" {
		return
	}
	if err := sendCopilotAPIMessage(convID, followUp); err != nil {
		slog.Warn("copilot API: compaction follow-up failed",
			"conv_id", convID, "session_id", handle.SessionID, "error", err)
	}
}
