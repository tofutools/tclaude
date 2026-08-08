package agentd

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Re-establishing a Copilot API channel that agentd lost by restarting.
//
// # What is broken without this
//
// Handles live in a process-memory registry, so an agentd restart empties it
// while every `copilot --ui-server` pane keeps running, keeps listening and
// keeps holding its conversation. Since TCL-1058 a Copilot conversation that
// took the API drive does NOT fall back to keystrokes when its channel is
// missing — it HOLDS, deliberately, because a silent fallback would re-open the
// injection sink for exactly the agent whose channel just became unverifiable.
// Holding is right; the consequence was not. Those agents stopped receiving
// mail until somebody relaunched them.
//
// # The trigger is a startup reconcile, not a disconnect
//
// There is no drop to observe. The new daemon comes up with an empty registry:
// nothing fired, nothing was missed, the state simply does not exist. So the
// question is not "what died" but "what should be here and is not", answered
// from what survived a restart — the port records, and the panes themselves.
//
// # Zero mutating RPCs, and that property is the design
//
// A reconnect issues no `session.create`, no `session.resume`, no
// `session.setForeground` — one read, and only to establish that a session by
// the conversation's id is drivable. That is not caution for its own sake:
//
//   - `session.create` at an id that is COLD (on disk, not in the server's
//     registry) starts it FRESH — measured in TCL-1056, and it is how a resumed
//     conversation gets destroyed while the launch looks healthy. A reconnect
//     cannot tell the cold case from the live one in advance, so it must not
//     make a call whose meaning depends on which one it is in.
//   - `session.resume` would work (measured), and costs one appended event plus
//     an options re-apply that reloads MCP servers and re-emits the system
//     prompt. Harmless, but it is churn in a conversation the daemon is meant to
//     be rejoining rather than changing.
//   - `session.setForeground` would move what the human is looking at. At launch
//     that is the point; on a reconnect there is nothing new to show.
//
// Measured on Copilot CLI 1.0.78: the server's session registry is
// PROCESS-GLOBAL rather than per-connection, so a second connection drives a
// session the first one created, and closing the first disposes nothing — a
// turn in flight survives the disconnect and finishes, and the reconnected
// subscription receives the whole assistant stream. The controls that make that
// an answer rather than a permissive default: the same probe against the pane's
// own startup session answers `Session not found`, and so does the same probe
// against a cold server holding the session only on disk.
//
// A future edit MUST NOT add a second RPC here without re-arguing that
// property. The moment this path can mutate, "reconnect" stops being a strictly
// safe operation and becomes one that has to be right about which case it is in.
//
// # What this deliberately does not recover
//
// A conversation whose session is NOT in the server's registry — a launch whose
// bootstrap never completed, so nothing was ever created over RPC. The probe
// says `Session not found` and the reconcile refuses. `session.resume` was
// measured to be able to adopt even the pane's own startup session, which is a
// real route back for that case, but it is a MUTATING call on a conversation
// this daemon has never owned and it belongs to its own ticket rather than
// riding along here.

// copilotAPIReconcileConcurrency bounds how many conversations are being
// re-established at once.
//
// It bounds the WORK, not the goroutines — one goroutine per candidate is
// launched immediately and then queues for a slot. What it is protecting
// against is a burst of concurrent /proc walks at the moment the daemon is
// busiest, since each candidate can spend up to the port wait's whole ceiling
// polling. The work is almost entirely waiting, so this is about not stampeding
// the process table rather than about CPU.
const copilotAPIReconcileConcurrency = 4

// copilotAPIReconcileOutcome names how the sweep finished with one candidate.
//
// It exists so that "this candidate did not end up connected" is not something
// the call site can say. That phrasing is the natural one, and it is wrong,
// because it is true of two exits that mean opposite things: a candidate whose
// bounded attempt RAN and failed is known to have no channel coming, while a
// candidate that never got a slot was never looked at. Recording an observation
// for the second reports an agent as deaf on the evidence that the sweep ran out
// of time — a confident wrong diagnosis, on the surface an operator reaches for
// when debugging, which is the pairing this series keeps producing.
//
// A comment asking the next author not to do that would be prose binding a call
// site, which does not hold. Every exit therefore names itself and one function
// decides what the name entitles.
type copilotAPIReconcileOutcome int

const (
	// copilotAPIReconcileUnexamined: the candidate never got a slot. Nothing was
	// attempted and nothing is known about it.
	copilotAPIReconcileUnexamined copilotAPIReconcileOutcome = iota
	// copilotAPIReconcileChannelUnavailable: the attempt ran and could not
	// re-establish the channel. Bounded, over, and not retried in this daemon's
	// life — the same structural property the bootstrap's failure path has.
	copilotAPIReconcileChannelUnavailable
	// copilotAPIReconcileReconnected: a handle was adopted, or a launch's
	// bootstrap got there first. Either way the conversation has a channel and
	// there is nothing to observe.
	copilotAPIReconcileReconnected
)

// reconcileOutcome records what the sweep learned about one candidate, if
// anything.
//
// The only outcome that yields an observation is
// [copilotAPIReconcileChannelUnavailable]. Keeping that decision here rather
// than at the exits means adding a new exit does not silently add a new
// recording site — a new outcome has to be named and answered in this switch.
func reconcileOutcome(convID string, generation uint64, outcome copilotAPIReconcileOutcome) {
	switch outcome {
	case copilotAPIReconcileChannelUnavailable:
		copilotAPISessions.NoteChannelFailed(convID, generation)
	case copilotAPIReconcileUnexamined, copilotAPIReconcileReconnected:
		// Nothing. Un-examined is an absence of evidence rather than evidence of
		// absence, and a reconnected conversation has a live handle, which
		// outranks any observation anyway.
	}
}

// reconnectCopilotAPISession re-establishes one conversation's channel.
//
// The port comes from [verifiedCopilotAPIPort] and from nowhere else, exactly as
// the bootstrap's does — this is the second site in the daemon entitled to dial
// the endpoint, and it is entitled for the same reason: it goes through the
// accessor that proves the listening socket belongs to the agent's pane subtree
// before it returns a number. `--ui-server` has no authentication, so that proof
// is the whole access-control story and a path that read the record directly
// would be the hole rather than a shortcut.
//
// Ownership is re-proved AFTER the connection exists, for the reason described
// on [copilotAPISession.StillOwned]. A restart is the moment that matters most:
// the daemon was away for an unknown length of time, so a port it once recorded
// is more likely than at any other moment to have been reused by something else.
//
// The session id is the CONVERSATION id, and the probe is what establishes that
// rather than an assumption. The bootstrap opens its session under the
// conversation's own id (and refuses a reply that echoes a different one), so
// that is the only id a reconnect could be looking for — and a successful read
// naming it IS the evidence that such a session exists and is drivable. A
// `Session not found` is a refusal, never a reason to create one.
func reconnectCopilotAPISession(ctx context.Context, convID string) (*copilotAPISession, error) {
	port, panePID, err := verifiedCopilotAPIPort(ctx, convID)
	if err != nil {
		return nil, err
	}

	address := "127.0.0.1:" + strconv.Itoa(port)
	client, err := copilotapi.DialRetry(ctx, address, nil)
	if err != nil {
		return nil, fmt.Errorf(
			"reconnect to the Copilot API for %s at %s: %w", convID, address, err)
	}
	handle := &copilotAPISession{
		ConvID: convID, SessionID: convID, Port: port, PanePID: panePID, Client: client,
	}
	closeOnFailure := func(err error) (*copilotAPISession, error) {
		_ = client.Close()
		return nil, err
	}

	if !handle.StillOwned() {
		return closeOnFailure(fmt.Errorf(
			"copilot API port %d for %s stopped being owned by the agent's pane subtree "+
				"between the ownership proof and the connection: refusing to drive it. This "+
				"endpoint has no authentication, so a listener that cannot be shown to be the "+
				"agent's cannot be told apart from another agent's. Relaunch the agent to "+
				"allocate a new port", port, convID))
	}

	// THE ONE CALL. It is a read, and it is here to answer "is a session by this
	// conversation's id drivable on this connection" — the only question a
	// reconnect has. Anything added beside it must re-argue the zero-mutating-RPC
	// property this whole path rests on; see the file header.
	if _, err := client.IsProcessing(ctx, convID); err != nil {
		return closeOnFailure(fmt.Errorf(
			"the Copilot server for %s has no drivable session under that conversation's id: "+
				"%w. Refusing to create or resume one here — a reconnect cannot tell a session "+
				"the server still holds from one that only exists on disk, and `session.create` "+
				"at a cold id starts it FRESH, which would discard the agent's conversation. "+
				"The pane is untouched and still usable; relaunch the agent to get its channel "+
				"back", convID, err))
	}
	return handle, nil
}

// reconcileCopilotAPISessions re-establishes every channel this daemon should
// have and does not.
//
// Candidates are conversations with a port record AND a live Copilot session
// row. The three halves answer different questions: the record says a launch
// was once given an endpoint here, the row says something is running under the
// conversation NOW, and the harness says the thing running is a Copilot pane
// rather than a successor launched on another harness over the same
// conversation.
//
// The liveness half is stricter than it looks, and it is what keeps this sweep
// cheap. [session.LiveSessionForConv] requires the row's tmux session to be
// alive, so a conversation whose pane exited is not a candidate at all — it
// never reaches the port wait, and the reaper's release grace is therefore not a
// cost this sweep pays. See the grace note below for what the two sweeps CAN
// disagree about.
//
// Conversations this daemon already holds a handle for are skipped, so running
// this twice costs one map lookup per candidate rather than a second connection.
//
// # What is deliberately NOT filtered
//
// The launch's recorded API posture. A conversation relaunched on send-keys
// keeps its predecessor's port record and stays a candidate, costing one bounded
// port wait per restart. Filtering on the posture would remove that, and would
// also remove the one case worth most: a conversation genuinely running the API
// drive whose posture was never recorded is exactly the one whose mail is being
// typed into its pane today, and it is the one a reconnect most needs to find.
// A wasted wait is cheap; skipping a mute agent is not.
//
// # The reaper's release grace
//
// The port record is released by the reaper, but only for a conversation with
// no live launch AND a record older than the reaper's spawn grace. The two
// sweeps therefore disagree only in narrow ways:
//
//   - A record already released before this runs simply is not a candidate.
//     Nothing to reconcile, which is correct — its pane is gone.
//   - A record still inside the grace window for a pane that has already exited
//     is likewise not a candidate: the liveness filter above drops it before the
//     port wait, so the grace costs this sweep nothing.
//   - A pane that exits BETWEEN the liveness filter and the port wait is a
//     candidate, and it fails at the wait with "no live pane process was found".
//     Bounded and named.
//   - A reaper release racing a reconnect that has already read the port cannot
//     produce an unsafe send: ownership is proved against the PANE, not against
//     the record, so a released record cannot make a foreign listener look like
//     the agent's. The worst case is a handle adopted for a conversation the
//     reaper has just retired, whose connection then dies with the process it is
//     attached to — which every reader of the registry already treats as gone.
func reconcileCopilotAPISessions(ctx context.Context) {
	convIDs, err := copilotAPIConversationsWithARecordedPort()
	if err != nil {
		slog.Error("copilot API reconnect: could not list conversations with a recorded port; "+
			"already-running API-driven agents stay held until they relaunch", "error", err)
		return
	}
	var candidates []string
	for _, convID := range convIDs {
		if copilotAPISessions.Handle(convID) != nil {
			continue
		}
		live := session.LiveSessionForConv(convID)
		if live == nil || live.Harness != harness.CopilotName {
			continue
		}
		if !copilotAPIPostureRecorded(convID) {
			// NOT a filter — see "What is deliberately NOT filtered" above. This
			// candidate is reconnected like any other, and reconnecting it is the
			// single highest-value thing this sweep does: with no recorded posture
			// its mail routes to KEYSTROKES under TCL-1058's durable arm, so
			// adopting a handle does not merely observe the conversation, it closes
			// an open injection sink for it.
			//
			// Logged because TCL-1059 closed every path that mints a conv id, so an
			// occurrence here is either a genuinely legacy conversation or a launch
			// path nobody has taught to record the posture — and the second is a
			// regression with no other signal at all.
			slog.Warn("copilot API reconnect: this conversation has a port record but no "+
				"recorded drive posture, so until it is reconnected its messages route as "+
				"keystrokes; if it is not a legacy conversation, a launch path has stopped "+
				"recording the posture", "conv_id", convID)
		}
		candidates = append(candidates, convID)
	}
	if len(candidates) == 0 {
		return
	}
	slog.Info("copilot API reconnect: re-establishing channels for already-running agents",
		"conversations", len(candidates))

	slots := make(chan struct{}, copilotAPIReconcileConcurrency)
	var waiting sync.WaitGroup
	for _, convID := range candidates {
		waiting.Go(func() {
			// Latched BEFORE any work, for the same reason the bootstrap is handed
			// its generation rather than reading one: this goroutine can finish a
			// port wait's whole budget from now, and a launch arriving in between
			// makes anything it concluded a statement about a launch nobody is
			// asking after. Zero is the ordinary value here — a restart empties the
			// registry — and it is a usable identity rather than a missing one,
			// because a launch landing mid-sweep moves it off zero.
			generation := copilotAPISessions.CurrentLaunch(convID)

			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				// Never silently. A candidate that never got a slot is an agent
				// that is still mute, and the operator's only other evidence is
				// the absence of a success line for it — which is indistinguishable
				// from a conversation that was never a candidate.
				//
				// And note what this exit is NOT: it is UNEXAMINED, not known-deaf.
				// Nothing was attempted, so there is no observation to record, and
				// recording one here would report an agent as deaf on the evidence
				// that we ran out of time looking at it. That is why the outcome is
				// a named value below rather than "did not end up connected" — the
				// phrasing that collapses these two is the natural one to write.
				reconcileOutcome(convID, generation, copilotAPIReconcileUnexamined)
				slog.Warn("copilot API reconnect: gave up before this conversation got a turn; "+
					"it stays held rather than being typed into, and a daemon restart is the "+
					"next thing that would retry it",
					"conv_id", convID, "error", ctx.Err())
				return
			}
			defer func() { <-slots }()

			handle, err := reconnectCopilotAPISessionFn(ctx, convID)
			if err != nil {
				// Warn rather than error: an agent that cannot be rejoined is
				// still fully usable through its pane, and the commonest cause
				// on this path is the ordinary one — a conversation whose pane
				// exited while the daemon was down.
				//
				// This is the sweep's version of the bootstrap's failure path, and
				// it carries the same structural property: a bounded attempt ran and
				// is over, and nothing re-runs it in this daemon's life. That is
				// what entitles it to record the observation.
				reconcileOutcome(convID, generation, copilotAPIReconcileChannelUnavailable)
				slog.Warn("copilot API reconnect: could not re-establish the channel; the agent "+
					"is still usable by typing into its pane, and its messages stay held rather "+
					"than being typed in", "conv_id", convID, "error", err)
				return
			}
			// AdoptIfAbsent, not Adopt. The candidate check ran at the top of the
			// sweep and this can land a port wait's whole budget later, with the
			// daemon serving spawns throughout — so a launch's bootstrap may have
			// established the channel in the meantime, and a replace would close
			// its connection while it is still running its remaining hard steps.
			// The launch is the newer truth about the conversation; a reconnect is
			// catching up on the older one, so the reconnect stands down.
			if !copilotAPISessions.AdoptIfAbsent(handle) {
				_ = handle.Client.Close()
				reconcileOutcome(convID, generation, copilotAPIReconcileReconnected)
				slog.Info("copilot API reconnect: a launch established this conversation's "+
					"channel first; leaving it alone", "conv_id", convID)
				return
			}
			// Same order as the bootstrap, and for the same reason: a consumer
			// attached to a handle that was never adopted would be reading for a
			// conversation the registry says is not connected. It needs nothing
			// else from this path — its first act on any connection is a full
			// authoritative read, so there is no gap to reason about.
			startCopilotAPIStateConsumer(handle)
			reconcileOutcome(convID, generation, copilotAPIReconcileReconnected)
			slog.Info("copilot API session re-established after an agentd restart",
				"conv_id", convID, "session_id", handle.SessionID, "port", handle.Port)
		})
	}
	waiting.Wait()
}

// reconnectCopilotAPISessionFn is the seam tests swap. It exists for the same
// reason the bootstrap's does: the reconcile's guard clauses (already-held
// handle, no live session row) are otherwise unobservable, because a guard that
// failed to hold starts work that goes on to fail slowly against a port nothing
// answers on — indistinguishable, for as long as any test would wait, from the
// guard holding.
var reconnectCopilotAPISessionFn = reconnectCopilotAPISession

// startCopilotAPIReconnect runs the reconcile once, in the background, at
// daemon startup.
//
// Background because a reconcile can spend up to the port wait's ceiling per
// conversation and the daemon must be answering requests long before that.
//
// Bounded by BOTH a deadline and the daemon's stop channel, and both are needed.
// The deadline stops a sweep that has outlived its usefulness from holding
// connections open for launches that have moved on; the stop channel is what
// keeps a shutdown from racing an in-flight reconnect into adopting a handle
// whose state consumer is then refused for stopping — an open connection with
// nobody reading it, for as long as the process lives.
//
// Indirected through a variable so tests can silence it, exactly as the
// bootstrap kick-off is. Flow tests run against a simulated tmux with no Copilot
// process anywhere, so a real reconcile would poll for listeners that cannot
// appear and would still be running against a torn-down database after the test
// returned. See agentd's TestMain.
var startCopilotAPIReconnect = runCopilotAPIReconnect

// SetCopilotAPIReconnectForTest swaps the startup reconcile and returns a
// restore function. TestMain installs a binary-wide no-op.
func SetCopilotAPIReconnectForTest(fn func(stop <-chan struct{})) func() {
	previous := startCopilotAPIReconnect
	startCopilotAPIReconnect = fn
	return func() { startCopilotAPIReconnect = previous }
}

func runCopilotAPIReconnect(stop <-chan struct{}) {
	go func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), copilotAPIBootstrapTimeout()+5*time.Minute)
		defer cancel()
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		reconcileCopilotAPISessions(ctx)
	}()
}
