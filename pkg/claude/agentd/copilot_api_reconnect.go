package agentd

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
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

// copilotAPIReconcileConcurrency bounds how many conversations are
// re-established at once.
//
// Each one can spend up to the port wait's full ceiling polling /proc, and a
// host with many retired-but-recorded conversations would otherwise start a
// goroutine per row at the exact moment the daemon is busiest. The work is
// almost entirely waiting, so this is about not stampeding the process table
// rather than about CPU.
const copilotAPIReconcileConcurrency = 4

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
// Candidates are conversations with a port record AND a live session row. Both
// halves are load-bearing and neither is the same question: the record says a
// launch was once given an endpoint for this conversation, and the row says
// something is running under it now. Without the row, a retired conversation
// whose record has not yet been released would cost a full port wait to
// discover it is dead — see the reaper's grace note below.
//
// Conversations this daemon already holds a handle for are skipped, so running
// this twice costs one map lookup per candidate rather than a second connection.
//
// # The reaper's release grace
//
// The port record is released by the reaper, but only for a conversation with
// no live launch AND a record older than the reaper's spawn grace. The two
// sweeps therefore disagree only in one direction and only briefly:
//
//   - A record already released before this runs simply is not a candidate.
//     Nothing to reconcile, which is correct — its pane is gone.
//   - A record still inside the grace window for a pane that has already exited
//     IS a candidate, and it fails at the port wait with "no live pane process
//     was found". Bounded, named, and self-correcting on the reaper's next tick.
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
		if session.LiveSessionForConv(convID) == nil {
			continue
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
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-slots }()

			handle, err := reconnectCopilotAPISessionFn(ctx, convID)
			if err != nil {
				// Warn rather than error: an agent that cannot be rejoined is
				// still fully usable through its pane, and the commonest cause
				// on this path is the ordinary one — a conversation whose pane
				// exited while the daemon was down.
				slog.Warn("copilot API reconnect: could not re-establish the channel; the agent "+
					"is still usable through its pane, and its messages stay held rather than "+
					"being typed in", "conv_id", convID, "error", err)
				return
			}
			copilotAPISessions.Adopt(handle)
			// Same order as the bootstrap, and for the same reason: a consumer
			// attached to a handle that was never adopted would be reading for a
			// conversation the registry says is not connected. It needs nothing
			// else from this path — its first act on any connection is a full
			// authoritative read, so there is no gap to reason about.
			startCopilotAPIStateConsumer(handle)
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
// Bounded by its own context rather than the daemon's stop channel alone: this
// is a one-shot sweep, and one that outlived its usefulness would be holding
// connections open for launches that have moved on.
//
// Indirected through a variable so tests can silence it, exactly as the
// bootstrap kick-off is. Flow tests run against a simulated tmux with no Copilot
// process anywhere, so a real reconcile would poll for listeners that cannot
// appear and would still be running against a torn-down database after the test
// returned. See agentd's TestMain.
var startCopilotAPIReconnect = runCopilotAPIReconnect

// SetCopilotAPIReconnectForTest swaps the startup reconcile and returns a
// restore function. TestMain installs a binary-wide no-op.
func SetCopilotAPIReconnectForTest(fn func()) func() {
	previous := startCopilotAPIReconnect
	startCopilotAPIReconnect = fn
	return func() { startCopilotAPIReconnect = previous }
}

func runCopilotAPIReconnect() {
	go func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), copilotAPIBootstrapTimeout+5*time.Minute)
		defer cancel()
		reconcileCopilotAPISessions(ctx)
	}()
}
