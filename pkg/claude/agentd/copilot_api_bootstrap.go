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
		// resume=false: this is the fresh-spawn boundary. A relaunch never
		// reaches executeSpawn — it forks `session new -r`, where the backstop
		// runs with the resume wording.
		h, p.CopilotAPI, p.TrustDir, false, p.Cwd, environment,
	); err != nil {
		return &spawnFailure{
			http.StatusUnprocessableEntity, "copilot_api_untrusted_launch_dir", err.Error()}
	}
	return nil
}

// copilotAPIBootstrapTimeout bounds the WHOLE bootstrap: waiting for a verified
// port, connecting, creating the session and foregrounding it. It is sized as
// the port wait's own ceiling plus a margin for the four RPCs, which each answer
// in milliseconds against a server that is by then up.
//
// One budget rather than one per step, because the steps are not independent: a
// launch that spends 55s reaching a verified listener is a loaded host, and
// giving it another full minute to finish four instant calls would only delay
// the report. The failure it produces still names the step it died on.
//
// Derived at each use rather than fixed at init, so it stays the port wait's
// ceiling plus a margin even when a test shortens that ceiling — a budget frozen
// at 90s around a 2s wait would silently stop being "the wait plus a margin".
func copilotAPIBootstrapTimeout() time.Duration {
	return copilotAPIStartupTimeout + 30*time.Second
}

// copilotAPIClientName identifies tclaude in Copilot's own telemetry and in
// `sessions.list` output, where it is the only thing distinguishing a session
// tclaude created from one the human started in the TUI.
const copilotAPIClientName = "tclaude"

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
// re-present. What partly replaces the credential is the connection itself: our
// socket stays open only as long as the process that accepted it lives, so it
// is a continuous liveness signal in a way a re-read of a port number is not.
//
// Be precise about what the conjunction proves, because the tempting statement
// is stronger than the truth. portowner matches the inode of a LISTENING socket;
// our own connection is a separate ESTABLISHED socket. So this establishes
// "my connection is still open" AND "someone in the agent's pane subtree is
// listening on this port now" — NOT "my connection was accepted by that
// listener". An interleaving that satisfies both while we talk to an impostor
// exists on paper: proof passes, the pane's copilot releases the port, an
// impostor binds it and accepts our dial, the impostor drops its listener, and
// something back inside the pane subtree binds the port again. It needs the
// pane's subtree to rebind a port copilot never rebinds, so it is not a
// practical attack — but it is the honest limit of this check, and the reason
// the wording here is "still consistent with being ours" rather than "proved".
//
// Closing it properly means matching the ESTABLISHED /proc/net/tcp entry for our
// own local/remote port pair against the pane subtree, which portowner does not
// expose today. Worth doing if this endpoint ever carries anything that matters
// more than a coding agent's prompts.
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
// # Creating and resuming are not the same call
//
// `session.create` at an id that already has history starts that id FRESH
// rather than attaching to it (measured in TCL-1056: `alreadyInUse:false`, an
// empty `getMessages`, the pane's timeline emptied, a model with no memory of
// its own last turn). That is exactly what a fresh launch wants and exactly what
// a RESUME must never do — a resumed pane is started `copilot --resume=<convID>`
// precisely because that conversation has history, and creating over it destroys
// the thing the resume existed to preserve while looking like a healthy launch.
//
// So the two are separate calls, chosen by [copilotAPILaunchKind], which is a
// fact the spawn facade knows for certain rather than something inferred here.
// This function did only the create for as long as every launch it saw was
// fresh — it was written before resume reached the drive — and it is worth
// naming that the code did not become wrong by changing, but by the set of
// situations it was called in widening underneath it.
//
// A failed resume is a HARD failure and never falls back to creating. The
// caller's degradation (the pane) is a pane still showing the fully resumed
// conversation, so what a failure costs is the channel; a fallback would cost
// the conversation.
//
// # One conversation, one id
//
// The session is created under the CONVERSATION's own id, not a fresh one, and
// that is the difference between owning the agent's conversation and owning a
// second one beside it.
//
// An API-drive launch still pins `--session-id <convID>`, so the pane's startup
// session IS the conversation: Copilot stores it at
// `$COPILOT_HOME/session-state/<convID>/`, which is the path tclaude's own
// conversation store, usage store, titles, transcript search and
// `--resume=<convID>` all resolve. A session created here under a fresh UUID
// would therefore be a SECOND conversation — drivable, foregrounded, and
// invisible to every one of those readers, while the id they do read stopped
// growing. `session.create` accepts the conv id (measured: it echoes it back),
// so there is no reason to accept that split.
//
// What creating at the conv id costs is the startup session's contents: the id
// is started FRESH rather than attached (measured: `alreadyInUse:false`, an
// empty `getMessages`, and a model with no memory of the pane's first turn).
// That is affordable only because an API-drive launch renders no `-i`, so there
// is nothing in the startup session to lose — see copilotSpawner's suppression
// of it, which exists for this reason and must not be undone on its own.
//
// Attaching instead is not on the table. The startup session is not in the RPC
// session registry: `session.getForeground` reports its id but every other
// `session.*` call against it fails "Session not found" — measured against a
// session pinned with `--session-id`, not merely against an anonymous one — and
// `sessions.open` with `{kind:"attach"}` reports `{"status":"resumed"}` for a
// session that stays just as undrivable.
//
// # The initial prompt
//
// Delivered here, over `session.send`, because the launch could not deliver it:
// see above. An empty prompt sends nothing, which is the ordinary case for a
// launch that carried no briefing.
//
// # No subscription
//
// Deliberately not subscribed here even though the package docs tell consumers
// to subscribe before creating. A subscription with nobody reading it fills its
// buffer, overruns and closes — so opening one on the caller's behalf would
// hand the event consumer (TCL-1057) a dead stream that looks like a live one.
func bootstrapCopilotAPISession(
	ctx context.Context, convID string, kind copilotAPILaunchKind, initialPrompt string,
) (*copilotAPISession, error) {
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

	// Re-proved AFTER the connection exists, because a proof taken before the
	// dial says only that the port was the agent's a moment ago, while one taken
	// after — with the connection already established and still open — says the
	// pane subtree still holds the listener we dialled at. That narrows the
	// window rather than eliminating it; see StillOwned for exactly what the
	// conjunction does and does not establish.
	if !handle.StillOwned() {
		return closeOnFailure(fmt.Errorf(
			"copilot API port %d for %s stopped being owned by the agent's pane subtree "+
				"between the ownership proof and the connection: refusing to drive it. This "+
				"endpoint has no authentication, so a listener that cannot be shown to be the "+
				"agent's cannot be told apart from another agent's. Relaunch the agent to "+
				"allocate a new port", port, convID))
	}

	info, err := openCopilotAPISession(ctx, client, convID, kind, workingDir)
	if err != nil {
		return closeOnFailure(err)
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

	// After foregrounding, so the human sees the briefing arrive in the session
	// it belongs to rather than watching a turn answer itself in a pane that is
	// still showing something else.
	//
	// A failure here is HARD, unlike most delivery failures. The launch already
	// dropped its `-i` on the promise that this would carry the prompt, so a
	// swallowed error would leave an agent idle and briefed by nobody — which on
	// every dashboard surface looks exactly like an agent that finished its work.
	if initialPrompt != "" {
		if _, err := client.Send(ctx, copilotapi.SendParams{
			SessionID: info.SessionID, Prompt: initialPrompt,
		}); err != nil {
			return closeOnFailure(fmt.Errorf(
				"deliver the launch prompt to Copilot session %s for %s: %w",
				info.SessionID, convID, err))
		}
	}
	return handle, nil
}

// openCopilotAPISession creates or resumes the session, according to what the
// launch actually was.
//
// The working directory is supplied on BOTH paths, and on the resume path it is
// the one deliberate difference from letting Copilot use its own persisted
// value. The server treats a supplied directory as authoritative, and the
// directory read here comes from the agent's LIVE session row — so an agent
// relaunched somewhere else resumes its conversation against where it is now
// rather than where it used to be, which is the same reason the create path
// reads it from the row.
func openCopilotAPISession(
	ctx context.Context,
	client *copilotapi.Client,
	convID string,
	kind copilotAPILaunchKind,
	workingDir string,
) (copilotapi.SessionInfo, error) {
	if kind == copilotAPILaunchResume {
		info, err := client.ResumeSession(ctx, copilotapi.ResumeSessionParams{
			SessionID:        convID,
			WorkingDirectory: workingDir,
			ClientName:       copilotAPIClientName,
			Streaming:        true,
		})
		if err != nil {
			// Named as the step it is, so the log distinguishes "the resume could
			// not be reached" from "the conversation was replaced" — which is what
			// falling back to a create would have produced, silently.
			return copilotapi.SessionInfo{}, fmt.Errorf(
				"resume the Copilot session for %s in %s: %w. Refusing to create a session "+
					"at that id instead: `session.create` starts an id FRESH, so it would "+
					"discard the history this resume exists to keep. The pane's own resumed "+
					"conversation is unaffected and still usable",
				convID, workingDir, err)
		}
		return info, nil
	}
	info, err := client.CreateSession(ctx, copilotapi.CreateSessionParams{
		SessionID:        convID,
		WorkingDirectory: workingDir,
		ClientName:       copilotAPIClientName,
		Streaming:        true,
	})
	if err != nil {
		return copilotapi.SessionInfo{}, fmt.Errorf(
			"create a Copilot session for %s in %s: %w", convID, workingDir, err)
	}
	return info, nil
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
// series keeps being bitten by.
//
// What that costs today, stated plainly rather than glossed: a handle is
// established ONLY at launch, from the spawn facades, so an agentd restart
// leaves every already-running API-driven agent with no handle and no way to
// acquire one short of relaunching. Those agents keep working — their panes are
// untouched — but tclaude reports them as not connected, which is TRUE of
// tclaude and misleading about the agent. There is deliberately no lazy
// re-bootstrap wired into the read paths: the first step of a bootstrap is a
// bounded wait of up to a minute, which is not something a dashboard snapshot
// tick may block on. Reconnection belongs with the component that owns the
// long-lived connection (TCL-1057) and is called out there.
// It also holds the OBSERVATION that a conversation's channel is not coming up
// (TCL-1089), which is a different kind of thing from a handle and is kept here
// deliberately. Its lifetime is the launch, and this registry is the only place
// in the daemon whose contents already have that lifetime: a launch supersedes
// it, a successful adopt outranks it, and a process restart drops it — which is
// correct rather than lossy, because at boot nobody yet knows whether a channel
// is adoptable and reconcileCopilotAPISessions is what finds out. Persisting it
// would assert at startup a fact that is not in evidence until the sweep reaches
// that conversation, and would route to send-keys an agent about to be
// reconnected. See copilotAPIChannelFailed for what may and may not be read
// from it.
type copilotAPISessionRegistry struct {
	mu       sync.Mutex
	sessions map[string]*copilotAPISession
	// launches counts launches per conversation, and is the identity a failure
	// observation is keyed to. Monotonic within the process, which is complete
	// rather than approximate: the goroutines that record an observation die
	// with the process, so there is no observer from an earlier daemon whose
	// count this would have to agree with.
	launches map[string]uint64
	// failed holds, per conversation, the launch generation whose channel was
	// observed not to be coming up. Stale entries are inert rather than wrong —
	// a reader compares against launches — but they are also cleared on the next
	// launch so the map does not grow a row per dead launch.
	failed map[string]uint64
}

// copilotAPISessions is the process-wide registry.
var copilotAPISessions = &copilotAPISessionRegistry{}

// NoteLaunch records that a new launch has begun for a conversation and returns
// the generation it owns.
//
// The returned value is the caller's launch identity, and it exists so a
// bootstrap that fails LATE cannot speak for a launch that has since replaced
// it. Relaunching is the operator's recovery action for an agent that has gone
// deaf, so the window in which a dying goroutine could libel its own successor
// is exactly the window in which someone is most likely to be relaunching.
//
// Recording a launch also drops any earlier failure observation, and it is worth
// being exact about what that delete is and is not doing, because the obvious
// reading is wrong. It is HYGIENE, not correctness: a superseded observation is
// already unreadable, because [copilotAPISessionRegistry.ChannelFailed] compares
// the stored generation against the current one and an old launch's entry can
// never match. Deleting it keeps the map from holding statements about launches
// nobody can ask after.
//
// Stated this way because a mutation pass found it: removing the delete left the
// whole suite green, including the test named for a relaunch clearing the
// observation. The generation compare is what enforces that property, and a
// reader who takes this line for the mechanism would think the compare was
// removable.
func (r *copilotAPISessionRegistry) NoteLaunch(convID string) uint64 {
	if convID == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.launches == nil {
		r.launches = map[string]uint64{}
	}
	r.launches[convID]++
	delete(r.failed, convID)
	return r.launches[convID]
}

// CurrentLaunch returns the generation a conversation's latest known launch
// owns, or zero when this process has not seen one.
//
// Zero is the ordinary answer after an agentd restart for every agent that was
// already running, and it is a usable identity rather than a missing one: the
// reconcile latches it before it starts work and presents it back, so a launch
// arriving mid-sweep moves the generation and the reconcile's observation is
// dropped. That is [copilotAPISessionRegistry.AdoptIfAbsent]'s rule — the launch
// is the newer truth, the older thing stands down — applied to the observation
// instead of to the handle.
func (r *copilotAPISessionRegistry) CurrentLaunch(convID string) uint64 {
	if convID == "" {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.launches[convID]
}

// NoteChannelFailed records that the launch identified by generation will not be
// getting an API channel, and reports whether the observation was taken.
//
// The comparison and the write are ONE critical section, which is what makes the
// relaunch race impossible rather than unlikely. A caller that read the current
// generation, compared it itself and then called a plain setter would leave a
// window between the two in which a relaunch lands — small, real, and precisely
// the shape of race this seam has been bitten by before.
//
// A false is not a failure to report. It means a newer launch owns this
// conversation now, so the caller's observation is about a launch nobody is
// asking after any more.
func (r *copilotAPISessionRegistry) NoteChannelFailed(convID string, generation uint64) bool {
	if convID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.launches[convID] != generation {
		return false
	}
	if r.failed == nil {
		r.failed = map[string]uint64{}
	}
	r.failed[convID] = generation
	return true
}

// ChannelFailed reports whether the conversation's CURRENT launch has been
// observed not to be getting a channel.
//
// A live handle answers false regardless of what was recorded, so an adopt
// self-corrects the observation without anything having to clear it. That
// matters for the case an error return does not distinguish: a bootstrap that
// died at setForeground or at the launch prompt had already created the session,
// so a later daemon's reconcile can legitimately adopt it.
func (r *copilotAPISessionRegistry) ChannelFailed(convID string) bool {
	if convID == "" {
		return false
	}
	if r.Handle(convID) != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	generation, observed := r.failed[convID]
	return observed && generation == r.launches[convID]
}

// ForgetLaunchesForTest clears the launch and failure bookkeeping. Tests share
// a process-wide registry, so one test's launch generation would otherwise be
// the next one's starting point.
func (r *copilotAPISessionRegistry) ForgetLaunchesForTest() {
	r.mu.Lock()
	defer r.mu.Unlock()
	clear(r.launches)
	clear(r.failed)
}

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

// AdoptIfAbsent records a handle only when the conversation has none, and
// reports whether it did.
//
// The difference from [copilotAPISessionRegistry.Adopt] is who is entitled to
// win, and it is not a detail. Adopt REPLACES, which is right for a launch: the
// launch is the new truth about the conversation and the predecessor's
// connection is to a pane that no longer exists. A reconnect is the opposite —
// it is catching up on state that already existed — so if a launch's bootstrap
// has adopted in the meantime, the launch wins and the reconnect stands down.
//
// Without this the reconcile can close a bootstrap's connection out from under
// it. The reconnect's candidate check happens once, at the top of the sweep,
// and its Adopt can land up to the port wait's whole budget later; the daemon
// is serving spawns throughout. A replace in that window leaves the bootstrap
// running its remaining HARD steps — setForeground, and the initial prompt —
// on a closed connection, so they fail, and the registry still looks healthy
// because the reconnect's own handle is in it. The visible result is an agent
// that was never given its briefing, which on every dashboard surface looks
// exactly like an agent that finished its work.
//
// The loser closes its own connection; this never closes a handle it did not
// take.
func (r *copilotAPISessionRegistry) AdoptIfAbsent(handle *copilotAPISession) bool {
	if handle == nil || handle.ConvID == "" {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessions == nil {
		r.sessions = map[string]*copilotAPISession{}
	}
	if existing, found := r.sessions[handle.ConvID]; found && existing != nil &&
		existing.Client != nil && existing != handle {
		select {
		case <-existing.Client.Done():
			// Dead, so it is not a claim anybody is relying on. Fall through and
			// take the slot rather than refusing in favour of a closed socket.
		default:
			return false
		}
	}
	r.sessions[handle.ConvID] = handle
	return true
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
	// A nil Client is not reachable from bootstrapCopilotAPISession, which only
	// ever adopts a handle around a live connection — but Adopt accepts one, and
	// a handle that cannot answer "is this connection alive" must read as not
	// connected rather than panic the caller. This runs on the dashboard's
	// snapshot path, where a panic is a blank dashboard.
	if handle == nil || handle.Client == nil {
		delete(r.sessions, convID)
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
//
// The launch kind reaches the callback as a plain `resume` bool because this
// seam crosses the package boundary and [copilotAPILaunchKind] deliberately does
// not — exporting an internal enum so a no-op stub can name it would be a worse
// trade. In-package tests that care about the kind itself assign
// startCopilotAPIBootstrap directly.
// The launch generation is likewise not surfaced to the callback: it is an
// identity for the compare-and-set inside the registry, and a stub that never
// records an observation has nothing to do with it.
func SetCopilotAPIBootstrapForTest(
	fn func(convID string, copilotAPI bool, resume bool, initialPrompt string),
) func() {
	previous := startCopilotAPIBootstrap
	startCopilotAPIBootstrap = func(
		convID string, copilotAPI bool, kind copilotAPILaunchKind, initialPrompt string,
		_ uint64,
	) {
		fn(convID, copilotAPI, kind == copilotAPILaunchResume, initialPrompt)
	}
	return func() { startCopilotAPIBootstrap = previous }
}

// bootstrapCopilotAPISessionFn is the seam the guard-clause test swaps, and it
// exists for that test alone. Observing "the guards held" from the outside is
// otherwise impossible in a useful way: a guard that failed to hold starts a
// goroutine that goes on to fail — slowly, on a timeout — and adopts nothing,
// which is indistinguishable from the guard holding for as long as any test
// would be willing to wait. Without the seam the assertion can only be "nothing
// was adopted", which stays true with both guards deleted.
var bootstrapCopilotAPISessionFn = bootstrapCopilotAPISession

func runCopilotAPIBootstrap(
	convID string, copilotAPI bool, kind copilotAPILaunchKind, initialPrompt string,
	generation uint64,
) {
	if !copilotAPI || convID == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), copilotAPIBootstrapTimeout())
		defer cancel()
		handle, err := bootstrapCopilotAPISessionFn(ctx, convID, kind, initialPrompt)
		if err != nil {
			// This is where the fact is known, and it is known here without any
			// timing judgement left in it. The whole bootstrap ran under one
			// budget — the port wait's ceiling plus a margin — so "slow" has
			// already been absorbed INSIDE the call that just returned. An error
			// out of it means this launch's bounded attempt is over, and nothing
			// re-runs it: startCopilotAPIBootstrap is called only from
			// completeCopilotAPILaunch, and reconcileCopilotAPISessions runs once,
			// at daemon startup. An observer outside this function would be
			// re-deriving, from a clock, a fact the failing call already held.
			//
			// Recorded against THIS launch's generation. A relaunch is what an
			// operator does about an agent that has gone deaf, so the window this
			// return sits at the end of is the window in which a successor is most
			// likely to exist — and the registry drops the observation rather than
			// letting it speak for that successor.
			recorded := copilotAPISessions.NoteChannelFailed(convID, generation)
			slog.Error("could not bring up the Copilot API session; this launch will not get "+
				"one, and until the agent is relaunched its mail is held rather than "+
				"delivered. It is still usable by typing into its pane",
				"conv_id", convID, "error", err, "observation_recorded", recorded)
			return
		}
		copilotAPISessions.Adopt(handle)
		// The event consumer starts with the handle and dies with it. Started
		// here rather than inside the bootstrap because the bootstrap's job
		// ends at "the channel is open": a consumer attached to a handle that
		// was never adopted would be reading for a conversation the registry
		// says is not connected.
		startCopilotAPIStateConsumer(handle)
		slog.Info("Copilot API session established",
			"conv_id", convID, "session_id", handle.SessionID, "port", handle.Port)
	}()
}
