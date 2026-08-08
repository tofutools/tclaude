package agentd

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/portowner"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// copilotAPIFolderTrustFailure refuses an API-driven Copilot spawn whose launch
// directory Copilot does not trust and this launch is not going to pre-trust.
//
// The whole argument lives in session.ValidateCopilotAPIFolderTrust; this is
// the spawn boundary's wrapper, so the operator gets a named reason from the
// spawn API instead of a `tclaude session new` that fails a moment later with
// the same sentence in a worse-shaped error. The backstop inside session.New
// still runs for relaunch, clone and the direct CLI path.
func copilotAPIFolderTrustFailure(p spawnParams) *spawnFailure {
	if !p.CopilotAPI {
		return nil
	}
	h, err := harness.Resolve(p.Harness)
	if err != nil {
		return &spawnFailure{http.StatusUnprocessableEntity, "invalid_harness", err.Error()}
	}
	var environment []sandboxpolicy.EnvironmentEntry
	if p.EffectiveSandbox != nil {
		environment = p.EffectiveSandbox.Effective.Environment
	}
	if err := session.ValidateCopilotAPIFolderTrust(
		h, p.CopilotAPI, p.TrustDir, p.Cwd, environment,
	); err != nil {
		return &spawnFailure{
			http.StatusUnprocessableEntity, "copilot_api_untrusted_launch_dir", err.Error()}
	}
	return nil
}

const (
	// copilotAPIBootstrapTimeout bounds the WHOLE bootstrap: waiting for a
	// verified port, connecting, creating the session and foregrounding it. It
	// is sized as the port wait's own ceiling plus a margin for the four RPCs,
	// which each answer in milliseconds against a server that is by then up.
	//
	// One budget rather than one per step, because the steps are not
	// independent: a launch that spends 55s reaching a verified listener is a
	// loaded host, and giving it another full minute to finish four instant
	// calls would only delay the report. The failure it produces still names
	// the step it died on.
	copilotAPIBootstrapTimeout = copilotAPIStartupTimeout + 30*time.Second

	// copilotAPIClientName identifies tclaude in Copilot's own telemetry and in
	// `sessions.list` output, where it is the only thing distinguishing a
	// session tclaude created from one the human started in the TUI.
	copilotAPIClientName = "tclaude"
)

// copilotAPISession is a live, tclaude-owned Copilot session: a connection that
// has been proved to belong to the agent's pane, and a session on it that the
// TUI is displaying.
//
// The handle IS the claim. Nothing in it is a record to be re-read later —
// every field describes the connection held in Client, and when that connection
// dies the whole handle stops meaning anything at once rather than degrading
// into a plausible-looking stale answer. That is why "is this agent
// API-connected" is answered from [copilotAPISessionRegistry.Connected] and
// never from the recorded port: the port record is a true statement about a
// launch, and a false one about now.
type copilotAPISession struct {
	// ConvID is the conversation this session belongs to.
	ConvID string
	// SessionID is the id tclaude chose and Copilot echoed back — the session
	// the TUI is foregrounding and the one every later `session.*` call names.
	SessionID string
	// Port and PanePID are the endpoint identity the connection was proved
	// against, kept so a later re-proof asks about the same subtree rather than
	// re-deriving one that may have moved on.
	Port    int
	PanePID int
	// Client is the live connection. Its owner closes it.
	Client *copilotapi.Client
}

// StillOwned re-establishes that the process on the other end of this
// connection is still the agent's.
//
// The port proof is one-shot and has a residual TOCTOU window (TCL-1054): it
// says the listening socket belonged to the pane's subtree at one instant, and
// `--ui-server` has no authentication (TCL-1055), so there is no credential to
// re-present. What replaces the credential is the connection itself. A listener
// cannot be taken over while it is held: for another process to bind this port,
// the pane's process must first release it, and releasing it kills the accepted
// connection this handle holds. So a handle whose Client is still open AND
// whose port is still owned by the same pane subtree is talking to the process
// that was proved, not merely to something that answers on the same number.
//
// Both halves are load-bearing and neither is sufficient. An open connection
// alone would be satisfied by a socket accepted from an impostor before the
// first proof was even taken; an ownership re-read alone would be satisfied by
// the agent rebinding after an impostor had already answered us.
//
// Callers should re-ask before acting on anything that matters, not once at
// adoption. It is two small kernel table reads.
func (s *copilotAPISession) StillOwned() bool {
	if s == nil || s.Client == nil {
		return false
	}
	select {
	case <-s.Client.Done():
		return false
	default:
	}
	return portowner.ProcessOwnsLoopbackPort(s.PanePID, s.Port)
}

// bootstrapCopilotAPISession takes an API-driven Copilot conversation from
// "the pane is running" to "tclaude owns a live session the human can see".
//
// The sequence, and why it is this one, is in the copilotapi package docs. The
// short version: the pane's own startup session is visible over RPC but not
// drivable, and the documented `sessions.open` create path returns a session
// that reports success and then cannot be foregrounded or named. `session.
// create` followed by `session.setForeground` is the only route to a session
// this client can drive, and foregrounding it is what puts it on the human's
// screen.
//
// # The port
//
// The address comes from [verifiedCopilotAPIPort] and from nowhere else. That
// is not a preference: `--ui-server` has no authentication, so the ownership
// proof is the entire access-control story, and a second path that read the
// port from the record would not be a shortcut but a hole. The re-proof after
// connecting is described on [copilotAPISession.StillOwned].
//
// # The working directory
//
// Read from the agent's LIVE session row rather than carried down from the
// spawn arguments. The session is being created for the directory the agent is
// running in now; spawn args describe what a launch was asked for, which is the
// same value right up until it is not (an empty --cwd, a relaunch elsewhere),
// and a session created against the wrong directory resolves the wrong
// workspace and repository context while looking entirely healthy.
//
// # No subscription
//
// Deliberately not subscribed here even though the package docs tell consumers
// to subscribe before creating. A subscription with nobody reading it fills its
// buffer, overruns and closes — so opening one on the caller's behalf would
// hand the event consumer (TCL-1057) a dead stream that looks like a live one.
// What this function guarantees instead is the precondition that makes the
// advice satisfiable: the returned client has never been used to send a prompt,
// so the consumer can subscribe before the first turn and miss nothing.
func bootstrapCopilotAPISession(ctx context.Context, convID string) (*copilotAPISession, error) {
	port, panePID, err := verifiedCopilotAPIPort(ctx, convID)
	if err != nil {
		return nil, err
	}
	workingDir, err := copilotAPIWorkingDir(convID)
	if err != nil {
		return nil, err
	}

	address := "127.0.0.1:" + strconv.Itoa(port)
	client, err := copilotapi.DialRetry(ctx, address, nil)
	if err != nil {
		return nil, fmt.Errorf("connect to the Copilot API for %s at %s: %w", convID, address, err)
	}
	handle := &copilotAPISession{
		ConvID: convID, Port: port, PanePID: panePID, Client: client,
	}
	closeOnFailure := func(err error) (*copilotAPISession, error) {
		_ = client.Close()
		return nil, err
	}

	// Re-proved AFTER the connection exists, which is the only ordering that
	// closes the window the first proof leaves open. A proof taken before the
	// dial says the port was the agent's a moment ago; taken after, with the
	// connection already established, it says the process still holding the
	// listener is the agent's — and this connection could only have been
	// accepted by whoever held it.
	if !handle.StillOwned() {
		return closeOnFailure(fmt.Errorf(
			"copilot API port %d for %s stopped being owned by the agent's pane subtree "+
				"between the ownership proof and the connection: refusing to drive it. This "+
				"endpoint has no authentication, so a listener that cannot be shown to be the "+
				"agent's cannot be told apart from another agent's. Relaunch the agent to "+
				"allocate a new port", port, convID))
	}

	info, err := client.CreateSession(ctx, copilotapi.CreateSessionParams{
		SessionID:        copilotapi.NewSessionID(),
		WorkingDirectory: workingDir,
		ClientName:       copilotAPIClientName,
		Streaming:        true,
	})
	if err != nil {
		return closeOnFailure(fmt.Errorf(
			"create a Copilot session for %s in %s: %w", convID, workingDir, err))
	}
	handle.SessionID = info.SessionID

	if err := client.SetForegroundSession(ctx, info.SessionID); err != nil {
		// Hard rather than degraded. A created-but-backgrounded session is
		// drivable over RPC and invisible in the pane, which is the same
		// working-and-unseen state the folder-trust gate exists to keep out —
		// and here it would be produced by tclaude itself.
		return closeOnFailure(fmt.Errorf(
			"foreground the Copilot session %s for %s: %w", info.SessionID, convID, err))
	}
	return handle, nil
}

// copilotAPIWorkingDir resolves the directory the agent is running in, from its
// live session row.
//
// A missing row is an error rather than a fallback to some default. There is no
// directory that is a safe guess here: a session created in the wrong one comes
// up healthy and resolves another repository's workspace context, which is a
// failure that surfaces much later as wrong answers rather than as a fault.
func copilotAPIWorkingDir(convID string) (string, error) {
	live := session.LiveSessionForConv(convID)
	if live == nil {
		return "", fmt.Errorf(
			"no live session for conversation %s: cannot tell which directory to create its "+
				"Copilot session in", convID)
	}
	if live.Cwd == "" {
		return "", fmt.Errorf(
			"the live session for conversation %s records no working directory: cannot create "+
				"its Copilot session without one", convID)
	}
	return live.Cwd, nil
}

// copilotAPISessionRegistry holds the live handles, keyed by conversation.
//
// In memory only, and that is the point rather than a limitation. A handle is a
// live connection; persisting one would persist a claim that is false the
// moment the process restarts, which is precisely the shape of value this
// series keeps being bitten by. An agentd restart therefore forgets every
// handle, and the next consumer bootstraps again through the full front door —
// verified port included — which is the correct behaviour and not a recovery
// gap.
type copilotAPISessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*copilotAPISession
}

// copilotAPISessions is the process-wide registry.
var copilotAPISessions = &copilotAPISessionRegistry{}

// Adopt records a handle, closing and replacing any earlier one for the same
// conversation.
//
// Replacement rather than refusal because a conversation legitimately outlives
// several launches (see the port record's own per-conversation lifetime), and
// the predecessor's connection is to a pane that no longer exists. Leaving it
// in place would leave "is this agent connected" answered by a dead socket.
func (r *copilotAPISessionRegistry) Adopt(handle *copilotAPISession) {
	if handle == nil || handle.ConvID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = map[string]*copilotAPISession{}
	}
	if previous, found := r.sessions[handle.ConvID]; found && previous != nil &&
		previous.Client != nil && previous != handle {
		_ = previous.Client.Close()
	}
	r.sessions[handle.ConvID] = handle
}

// Handle returns the live handle for a conversation, or nil.
//
// Nil for a handle whose connection has ended, and the dead entry is dropped on
// the way out: a caller asking for a handle is about to use it, so returning
// one that can only fail would be a slower way of saying no.
func (r *copilotAPISessionRegistry) Handle(convID string) *copilotAPISession {
	if convID == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	handle := r.sessions[convID]
	if handle == nil {
		return nil
	}
	select {
	case <-handle.Client.Done():
		delete(r.sessions, convID)
		return nil
	default:
	}
	return handle
}

// Connected answers "is this agent driven over the API right now".
//
// Derived from the connection, never from the recorded port. The record says a
// launch was given a number; it says nothing about whether anything is on the
// other end of it, nor about whose. This is the surface TCL-1051's proxy-value
// note predicted would be tempted by the record, and the temptation is real
// precisely because the record is a true value — about a different question.
func (r *copilotAPISessionRegistry) Connected(convID string) bool {
	return r.Handle(convID) != nil
}

// Drop closes and forgets a conversation's handle.
func (r *copilotAPISessionRegistry) Drop(convID string) {
	if convID == "" {
		return
	}
	r.mu.Lock()
	handle := r.sessions[convID]
	delete(r.sessions, convID)
	r.mu.Unlock()
	if handle != nil && handle.Client != nil {
		_ = handle.Client.Close()
	}
}

// startCopilotAPIBootstrap runs the bootstrap in the background for a launch
// that just took the API drive.
//
// Background because the bootstrap's first step is a bounded wait for the pane
// to bind its port — up to a minute on a loaded host — and a spawn call must
// not block on it. The spawn has already succeeded by the time this is reached;
// what is being established is the channel, and a channel that fails to come up
// is a loud log line and an agent that is still perfectly usable through its
// pane, not a failed spawn to roll back.
//
// convID may be empty for a launch that lets the harness mint its id. Those
// launches call this again from the point they discover it, exactly as they
// already do for the port record.
//
// Indirected through a variable so tests can silence it, the same way the spawn
// facades themselves are swapped. Flow tests spawn API-driven agents against a
// simulated tmux with no Copilot process anywhere, so the real bootstrap would
// spend a minute polling for a listener that cannot appear and would still be
// running against a torn-down database after the test returned. See
// agentd's TestMain.
var startCopilotAPIBootstrap = runCopilotAPIBootstrap

// SetCopilotAPIBootstrapForTest swaps the bootstrap kick-off and returns a
// restore function. TestMain installs a binary-wide no-op; a test that wants to
// observe the call swaps its own.
func SetCopilotAPIBootstrapForTest(fn func(convID string, copilotAPI bool)) func() {
	previous := startCopilotAPIBootstrap
	startCopilotAPIBootstrap = fn
	return func() { startCopilotAPIBootstrap = previous }
}

func runCopilotAPIBootstrap(convID string, copilotAPI bool) {
	if !copilotAPI || convID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), copilotAPIBootstrapTimeout)
		defer cancel()
		handle, err := bootstrapCopilotAPISession(ctx, convID)
		if err != nil {
			slog.Error("could not bring up the Copilot API session; the agent is still "+
				"usable through its pane",
				"conv_id", convID, "error", err)
			return
		}
		copilotAPISessions.Adopt(handle)
		slog.Info("Copilot API session established",
			"conv_id", convID, "session_id", handle.SessionID, "port", handle.Port)
	}()
}
