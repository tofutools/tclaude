package agentd

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/copilotapi"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The Copilot API state consumer: what an API-driven agent's numbers and
// waiting-for-a-human state are read from, and when.
//
// # Events are the trigger, reads are the truth
//
// The one design rule here, and the reason this file is short: NOTHING tclaude
// displays is derived from an event. An event only marks the session dirty; a
// refresh then asks the server what is true and writes that. There is no
// accumulator, no per-turn arithmetic, and no state carried across a
// connection.
//
// That is not fastidiousness — it is what makes the failure modes this series
// keeps hitting structurally impossible rather than merely guarded against.
// Three of them, concretely:
//
//   - The nested-modelMetrics trap cannot produce a plausible zero, because a
//     read that fails is an error rather than a counter that stopped moving.
//   - A dropped or overrun subscription cannot leave a stale figure on screen
//     indefinitely, because the next refresh re-reads everything from scratch.
//   - "Catch-up after a reconnect" has nothing to catch up ON. The consumer's
//     first act on any connection is the same read it always does.
//
// The last point is worth stating plainly because the ticket that asked for
// this expected `session.eventLog.tail`/`read` to carry the reconnect story.
// They cannot: `session.idle` and `assistant.idle` are both EPHEMERAL (measured
// against a live 1.0.78 server), so they never reach the persisted log, and the
// nearest durable event — `assistant.turn_end` — also fires mid-loop, so a
// consumer reading it would report an agent idle in the middle of a tool round
// trip. `session.metadata.isProcessing` answers the question outright.
//
// # What this owns, and what it deliberately does not
//
// It owns the CONTEXT reading (occupancy, the window, and the percentage
// between them) and the waiting-for-permission state. It does not own busy/idle
// and does not write it.
//
// That boundary is measured, not assumed. tclaude's own Copilot hooks were
// verified to fire for a session created over the RPC API — UserPromptSubmit
// and Stop both arrived, carrying the conversation's own id — so busy/idle
// already works on this drive, and a second writer would only introduce a race
// with the path the send-keys drive depends on.
//
// Permission is the opposite case, and it is a real hole rather than a
// refinement. copilot_hooks.go documents why tclaude cannot install Copilot's
// PermissionRequest hook, so today an agent blocked on a permission prompt
// emits no hook at all: measured, the Stop hook never fires while the prompt is
// open, and the session sits at "working" for as long as the human takes. That
// is indistinguishable, on every tclaude surface, from an agent doing work.
// `session.permissions.pendingRequests` is the server's own reconstruction of
// exactly that state, and it stays empty under `--allow-all-tools`, so mapping
// it cannot mislabel an unattended agent.
//
// # Read amplification
//
// One turn pushes on the order of a hundred and twenty events, most of them
// streaming deltas that carry no state at all. Two things keep the read cost
// flat. Noisy types do not mark the session dirty (see
// copilotAPIStateNoisyEvents, a DENY-list precisely so an event type Copilot
// adds tomorrow defaults to triggering a refresh rather than being missed), and
// what survives that is coalesced into at most one refresh per
// copilotAPIStateWindow.
//
// The cost is also per-agent rather than shared: every API-driven agent has its
// own copilot process and its own embedded server, so a fleet does not
// concentrate these reads anywhere. What a single Copilot server sees is three
// small local calls, at most once per window.

const (
	// copilotAPIStateWindow is the coalescing window: a burst of events
	// produces one refresh at the start and one after the burst settles.
	//
	// Sized against what it is refreshing rather than against the event rate. A
	// human reading the dashboard cannot perceive the difference between 100ms
	// and 750ms, while the dashboard's own poll is 2s — so anything below this
	// buys latency nobody can observe, at a cost that scales with the fleet.
	copilotAPIStateWindow = 750 * time.Millisecond

	// copilotAPIStateBackstop is the unconditional re-read, and it exists for
	// the case the trigger path cannot cover: an event that never arrives.
	//
	// A subscription that overruns is noticed and replaced, and a connection
	// that dies ends the consumer — but a display refreshed only on a trigger
	// has no mechanism to notice that it stopped receiving them. This bounds
	// "stale" at one interval instead of leaving it unbounded. Tens of seconds
	// rather than seconds, because it is a safety net and not the mechanism.
	copilotAPIStateBackstop = 30 * time.Second

	// copilotAPIStateFreshness is how long a published reading may go without
	// being re-established before it stops counting as one.
	//
	// The stand-down has to be "a reading that is CURRENT", not "a reading
	// exists", and the difference is a real failure rather than a hypothetical.
	// A connection can stay perfectly open while the reads on it start failing
	// — a session id the server no longer knows, a method a future release
	// renames — and in that state nothing ends the consumer, so a reading with
	// no expiry would sit in the registry for the lifetime of the connection
	// with both fallback writers standing down behind it. The row would freeze
	// at its last good numbers with no writer left, which is precisely the
	// stale-but-plausible reading this whole design exists to avoid.
	//
	// Three backstop intervals: long enough that an ordinary failed refresh or
	// two changes nothing, short enough that a persistent failure hands the row
	// back within a minute and a half.
	copilotAPIStateFreshness = 3 * copilotAPIStateBackstop

	// copilotAPIStateReadTimeout bounds one refresh's calls. Generous against
	// the sub-millisecond these answer in locally, because the cost of timing
	// out early is a blank meter and the cost of waiting is one goroutine.
	copilotAPIStateReadTimeout = 10 * time.Second

	// copilotAPIStateWarnInterval rate-limits the failure line. A refresh
	// failing usually means it will keep failing, and at one refresh per window
	// an unsuppressed line would be thousands an hour.
	copilotAPIStateWarnInterval = 5 * time.Minute

	// copilotAPIStatePermissionDetail is the status_detail written alongside
	// the awaiting-permission status, so the dashboard can say WHICH harness
	// surface is waiting rather than just that something is.
	copilotAPIStatePermissionDetail = "copilot permission"

	// copilotAPIAutoModel is Copilot's automatic-selection sentinel, which
	// UsageMetrics.CurrentModel reports verbatim for a session in auto mode
	// until a call has resolved one. It is a mode, not a model.
	copilotAPIAutoModel = "auto"
)

// copilotAPIStateNoisyEvents are the event types that never change any answer
// this file reads, so they do not mark the session dirty.
//
// A DENY-list rather than an allow-list, and the asymmetry is the point.
// Copilot's event vocabulary is open and grows between releases; an allow-list
// would silently stop triggering on a newly meaningful event, which is the
// quiet-staleness failure this whole design exists to avoid. A deny-list can
// only ever cost latency on a type that is genuinely pure noise, and every
// entry here was observed firing tens of times within a single turn while
// carrying nothing but a delta of text already accounted for elsewhere.
var copilotAPIStateNoisyEvents = map[string]bool{
	"assistant.streaming_delta":        true,
	"assistant.message_delta":          true,
	"assistant.reasoning_delta":        true,
	"assistant.tool_call_delta":        true,
	"tool.execution_partial_result":    true,
	"tool.execution_progress":          true,
	"session.background_tasks_changed": true,
}

// copilotAPIStateReading is one authoritative observation of a conversation's
// context window.
//
// Every field is a value the server reported in a single read, and the whole
// struct is replaced on each refresh rather than merged. A partial update would
// be the beginning of an accumulator.
type copilotAPIStateReading struct {
	// ObservedAt is when the read completed.
	ObservedAt time.Time
	// TotalTokens is the occupancy NUMERATOR: everything currently in the
	// window (system + conversation + tool definitions), as Copilot itself
	// tokenizes it. This is the same quantity Copilot compares against its own
	// compaction threshold, which is what makes it the right numerator for a
	// meter that is supposed to predict a compaction.
	TotalTokens int64
	// PromptTokenLimit is the DENOMINATOR Copilot reported. Before the API
	// drive tclaude had no source for this at all and fell back to a static
	// per-model assumption; this is the model's actual limit for this session.
	PromptTokenLimit int64
	// OutputTokens is the session's cumulative output across every model, from
	// the usage read.
	OutputTokens int64
	// Model is the model usage attributes the session's spend to. Read from
	// usage and NEVER from the context breakdown, whose ModelName was measured
	// naming a different model than the turn actually ran on.
	Model string
}

// copilotAPIStates holds the live reading per conversation.
//
// In memory and connection-scoped, for the same reason the handle registry is:
// a reading is a statement about a connection that exists. Persisting one would
// persist a claim that outlives the thing it describes, which is the exact
// shape of value this series keeps being bitten by.
var copilotAPIStates struct {
	sync.Mutex
	readings map[string]copilotAPIStateReading
}

// publishCopilotAPIState records a fresh reading for a conversation.
func publishCopilotAPIState(convID string, reading copilotAPIStateReading) {
	if convID == "" {
		return
	}
	copilotAPIStates.Lock()
	defer copilotAPIStates.Unlock()
	if copilotAPIStates.readings == nil {
		copilotAPIStates.readings = map[string]copilotAPIStateReading{}
	}
	copilotAPIStates.readings[convID] = reading
}

// dropCopilotAPIState forgets a conversation's reading.
//
// Called when the consumer stops, which is when the connection it was reading
// over has ended. Keeping the last reading would leave the other Copilot
// writers standing down in favour of a source that is no longer answering.
func dropCopilotAPIState(convID string) {
	copilotAPIStates.Lock()
	defer copilotAPIStates.Unlock()
	delete(copilotAPIStates.readings, convID)
}

// retireCopilotAPIStateConsumer removes a consumer and, only if it was still
// the registered one, the reading it published.
//
// The conditional drop matters on a RELAUNCH. Adopt closes the predecessor's
// client and the successor registers immediately, but the predecessor's
// goroutine may still be parked in a read with a ten-second timeout — so it
// unwinds and runs this AFTER the successor has already published. An
// unconditional drop would delete the successor's reading, handing the row back
// to the weaker writers until the successor's next trigger.
func retireCopilotAPIStateConsumer(consumer *copilotAPIStateConsumer) {
	copilotAPIStateConsumers.Lock()
	current := copilotAPIStateConsumers.running[consumer.convID] == consumer
	if current {
		delete(copilotAPIStateConsumers.running, consumer.convID)
	}
	copilotAPIStateConsumers.Unlock()
	if current {
		dropCopilotAPIState(consumer.convID)
	}
}

// lookupCopilotAPIState returns the live reading for a conversation.
//
// False is the ordinary state for every send-keys agent, for an API agent whose
// connection has ended, for one whose session has not run a turn yet —
// `session.metadata.contextInfo` legitimately answers null until then — and for
// one whose reads have stopped succeeding for copilotAPIStateFreshness. All of
// them mean the same thing to a caller: this source has nothing to say now, use
// the ones that do.
func lookupCopilotAPIState(convID string) (copilotAPIStateReading, bool) {
	if convID == "" {
		return copilotAPIStateReading{}, false
	}
	copilotAPIStates.Lock()
	defer copilotAPIStates.Unlock()
	reading, found := copilotAPIStates.readings[convID]
	if !found || time.Since(reading.ObservedAt) > copilotAPIStateFreshness {
		return copilotAPIStateReading{}, false
	}
	return reading, true
}

// copilotAPIStateConsumer is one conversation's event-triggered reader.
type copilotAPIStateConsumer struct {
	convID    string
	sessionID string
	// handle is kept so each refresh can re-prove ownership through
	// copilotAPIDrive before reading, and so a consumer can recognise that the
	// conversation has been re-adopted by a newer launch than its own.
	handle *copilotAPISession
	// client is held ONLY for Subscribe and Done, and that is the rule rather
	// than a description of today's calls: both are non-transmitting — Subscribe
	// allocates a channel and registers it under the client's mutex, Done hands
	// back an already-closed channel — so neither sends a byte to an endpoint
	// whose ownership nobody re-proved.
	//
	// That is WHY this field may exist outside the drive layer. Anything that
	// transmits must go through copilotAPIDrive instead, which re-proves
	// ownership first; adding a third method here would quietly convert a field
	// that cannot reach the endpoint into one that can.
	client *copilotapi.Client

	stop     chan struct{}
	stopOnce sync.Once

	// lastPermissionRead is when the pending-permission set was last read
	// successfully. It bounds how long this consumer's awaiting marker may
	// outlive its ability to clear it; see releaseStrandedPermission.
	lastPermissionRead time.Time

	// warnedAt rate-limits the refresh-failure line, keyed by reason so a new
	// failure mode stays audible while an old one is suppressed.
	warnedAt map[string]time.Time
}

// copilotAPIStateConsumers tracks the running consumers so a replaced handle
// does not leave two goroutines reading for one conversation.
var copilotAPIStateConsumers struct {
	sync.Mutex
	running  map[string]*copilotAPIStateConsumer
	stopping bool
}

// startCopilotAPIStateConsumer begins consuming for a freshly adopted handle,
// replacing any consumer already running for the conversation.
//
// Replacement rather than refusal, matching the handle registry it follows: a
// conversation legitimately outlives several launches, and the predecessor is
// reading over a connection to a pane that no longer exists.
func startCopilotAPIStateConsumer(handle *copilotAPISession) {
	if handle == nil || handle.ConvID == "" || handle.Client == nil || handle.SessionID == "" {
		return
	}
	consumer := &copilotAPIStateConsumer{
		convID:    handle.ConvID,
		sessionID: handle.SessionID,
		handle:    handle,
		client:    handle.Client,
		stop:      make(chan struct{}),
		warnedAt:  map[string]time.Time{},
		// Counts from birth, so a consumer that never manages a single
		// permission read still releases a marker inherited from a predecessor
		// rather than sitting behind it forever.
		lastPermissionRead: time.Now(),
	}
	copilotAPIStateConsumers.Lock()
	if copilotAPIStateConsumers.stopping {
		copilotAPIStateConsumers.Unlock()
		return
	}
	if copilotAPIStateConsumers.running == nil {
		copilotAPIStateConsumers.running = map[string]*copilotAPIStateConsumer{}
	}
	previous := copilotAPIStateConsumers.running[handle.ConvID]
	copilotAPIStateConsumers.running[handle.ConvID] = consumer
	copilotAPIStateConsumers.Unlock()
	if previous != nil {
		previous.halt()
	}
	go consumer.run()
}

// stopCopilotAPIStateConsumers ends every consumer, for daemon shutdown.
func stopCopilotAPIStateConsumers() {
	copilotAPIStateConsumers.Lock()
	copilotAPIStateConsumers.stopping = true
	running := copilotAPIStateConsumers.running
	copilotAPIStateConsumers.running = nil
	copilotAPIStateConsumers.Unlock()
	for _, consumer := range running {
		consumer.halt()
	}
}

func (c *copilotAPIStateConsumer) halt() {
	c.stopOnce.Do(func() { close(c.stop) })
}

// run is the consumer's whole loop.
//
// The subscription is opened BEFORE the first refresh, so an event that lands
// during that read is not lost between the two — it finds the subscription
// already in place and marks the session dirty for the next window.
func (c *copilotAPIStateConsumer) run() {
	defer retireCopilotAPIStateConsumer(c)

	subscription := c.client.Subscribe()
	// Wrapped in a closure rather than `defer subscription.Close()`, which would
	// bind the subscription this consumer STARTED with. The overrun path
	// replaces it, and the replacement is the one that needs closing.
	defer func() { subscription.Close() }()

	// A consumer halted between being registered and reaching this line has
	// nothing to say, and a daemon shutting down does not need three RPCs and a
	// row write on the way out.
	select {
	case <-c.stop:
		return
	default:
	}

	// The connection is new (or newly re-established). Establish truth before
	// waiting for anything to happen on it: an agent that is mid-turn when the
	// consumer starts would otherwise show nothing until its next event.
	c.refresh()
	lastRefresh := time.Now()

	backstop := time.NewTicker(copilotAPIStateBackstop)
	defer backstop.Stop()
	// Stopped, and armed only while a refresh is owed. A running timer with
	// nothing to do would refresh on a session nothing has happened to.
	settle := time.NewTimer(time.Hour)
	if !settle.Stop() {
		<-settle.C
	}
	defer settle.Stop()
	owed := false
	// Every path that refreshes for a reason OTHER than the settle timer goes
	// through this, so an owed trailing refresh is cancelled rather than firing
	// again a fraction of a second later against a session nothing has happened
	// to in between.
	refreshNow := func() {
		if owed {
			owed = false
			if !settle.Stop() {
				<-settle.C
			}
		}
		c.refresh()
		lastRefresh = time.Now()
	}

	for {
		select {
		case <-c.stop:
			return
		case <-c.client.Done():
			return
		case notification, open := <-subscription.C():
			if !open {
				// An overrun is the subscriber's own fault and is recoverable:
				// re-subscribe and re-read, which is exactly what the client's
				// documentation asks a consumer to do. Any other end means the
				// connection is going away and so is this consumer.
				if !errors.Is(subscription.Err(), copilotapi.ErrSubscriptionOverrun) {
					return
				}
				slog.Debug("copilot-api-state: subscription overran; re-subscribing",
					"conv_id", c.convID, "module", "agentd")
				subscription.Close()
				subscription = c.client.Subscribe()
				refreshNow()
				continue
			}
			if !c.marksDirty(notification) {
				continue
			}
			if owed {
				continue
			}
			if remaining := copilotAPIStateWindow - time.Since(lastRefresh); remaining > 0 {
				owed = true
				settle.Reset(remaining)
				continue
			}
			refreshNow()
		case <-settle.C:
			// The timer has already fired, so the channel is drained and owed
			// must be cleared WITHOUT trying to stop it.
			owed = false
			c.refresh()
			lastRefresh = time.Now()
		case <-backstop.C:
			refreshNow()
		}
	}
}

// marksDirty decides whether a notification means anything might have changed.
//
// Sub-agent events are excluded by their AgentID, which is the distinction
// TCL-1052's cold review caught: a sub-agent's events are its own, and a
// consumer treating them as the root agent's would refresh (and, for anything
// deriving status from them, report) on work that is not the agent's turn.
// Lifecycle notifications are included — a session being foregrounded or
// updated is cheap to re-read and rare.
func (c *copilotAPIStateConsumer) marksDirty(notification copilotapi.Notification) bool {
	switch notification.Method {
	case copilotapi.MethodSessionEvent:
		event, err := notification.SessionEvent()
		if err != nil {
			// An event this client cannot decode is still evidence that
			// something happened. Refreshing on it costs one window and is
			// strictly better than assuming it was irrelevant.
			return true
		}
		if event.SessionID != c.sessionID {
			return false
		}
		if event.Event.AgentID != "" {
			return false
		}
		return !copilotAPIStateNoisyEvents[event.Event.Type]
	case copilotapi.MethodSessionLifecycle:
		lifecycle, err := notification.Lifecycle()
		if err != nil {
			return true
		}
		return lifecycle.SessionID == c.sessionID
	default:
		// A method this package does not model. It is a contract that has
		// grown, and the honest response is to re-read rather than to guess.
		return true
	}
}

// refresh does the reads and applies them.
//
// The permission projection runs FIRST and independently of the two number
// reads. It used to sit below them, which quietly made "a human is waiting"
// conditional on the context and usage reads both succeeding — so a session id
// the server had stopped recognising would take the meter and the
// waiting-for-a-human state down together, and the second is the one an
// operator needs most.
//
// # Every byte leaves through the same gate as every other verb
//
// Reading is not the harmless half of the ownership question. A refresh that
// reached a listener the agent's pane no longer owns would copy a STRANGER's
// occupancy into this conversation's row, and could put the row into
// awaiting_permission because someone else's agent is waiting on a human. An
// awaiting row holds message delivery, so that failure is a mute agent arrived
// at purely by reading — which is why this re-proves through copilotAPIDrive
// rather than trusting the proof the bootstrap made once.
//
// A refusal is ordinary and transient (a /proc walk racing a re-exec): read
// nothing, try again on the next trigger. A refusal that persists ages the
// reading out of copilotAPIStateFreshness and the pre-existing Copilot sources
// resume, so losing this source degrades rather than freezes.
func (c *copilotAPIStateConsumer) refresh() {
	handle, err := copilotAPIDrive(c.convID)
	if err != nil {
		c.warn("ownership", "copilot-api-state: refusing to read", err)
		// Refusing to read must not also mean refusing to let go. This is the
		// one thing the gate would otherwise freeze; see the function below.
		c.releaseStrandedPermission(nil)
		return
	}
	if handle != c.handle {
		// The conversation has been re-adopted by a newer launch, and the
		// consumer for THAT handle is already running. Reading on someone
		// else's handle would be this consumer reporting on a session it was
		// not started for.
		c.halt()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), copilotAPIStateReadTimeout)
	defer cancel()

	// One row read serves both projections, and both are guarded against it
	// being nil — an agent whose row has gone (retired, pruned) is not a
	// failure, it is a consumer that is about to stop.
	row := copilotAPIStateSessionRow(c.convID)

	if pending, err := handle.Client.PendingPermissionRequests(ctx, c.sessionID); err != nil {
		c.warn("permissions", "copilot-api-state: pending-permission read failed", err)
		c.releaseStrandedPermission(row)
	} else {
		c.lastPermissionRead = time.Now()
		c.applyPermissions(ctx, handle, row, len(pending) > 0)
	}

	info, err := handle.Client.ContextInfo(ctx, copilotapi.ContextInfoParams{SessionID: c.sessionID})
	if err != nil {
		c.warn("context", "copilot-api-state: context read failed", err)
		return
	}
	metrics, err := handle.Client.UsageMetrics(ctx, c.sessionID)
	if err != nil {
		c.warn("usage", "copilot-api-state: usage read failed", err)
		return
	}
	if info == nil {
		// Normal before the first turn completes: the session has not cached a
		// system prompt or tool metadata yet. Publishing nothing leaves the
		// pre-existing Copilot sources fully in charge, which is right — they
		// have no reading either, and inventing a zero would render an empty
		// meter as a measured 0%.
		return
	}
	reading := copilotAPIStateReading{
		ObservedAt:       time.Now(),
		TotalTokens:      int64(info.TotalTokens),
		PromptTokenLimit: int64(info.PromptTokenLimit),
		OutputTokens:     copilotAPIOutputTokens(metrics),
		Model:            copilotAPIReadingModel(metrics),
	}
	// Published BEFORE the write, because publishing is what makes the other
	// two Copilot context writers stand down. In the other order a sweep tick
	// landing in between would recompute the row from its own weaker sources
	// and briefly overwrite the reading that is about to be written.
	publishCopilotAPIState(c.convID, reading)
	persistCopilotAPIContext(row, reading)
}

// persistCopilotAPIContext writes the reading into the harness-agnostic context
// columns the dashboard already renders.
//
// This is the ONE Copilot context writer while a reading exists —
// copilot_usage_poller.go and copilot_context_refresh.go both defer to it — so
// there is nothing here to reconcile against another source and no precedence
// to keep in step. That is the whole reason the stand-down is expressed as
// "does a reading exist" rather than as a merge: two writers converging on one
// row is how the TCL-1048 follow-up bug happened, and the cheapest way not to
// have it again is not to have two writers.
//
// sessions.model and sessions.effort_level are left alone. They stay owned by
// the usage sweep, which reads them out of Copilot's own store for both drives;
// taking them here would mean owning `effort_level`, which this reading has no
// source for at all. tokens_output is shared rather than owned: both writers
// only ever advance it, so whichever has seen more of the session wins and they
// cannot disagree.
func persistCopilotAPIContext(row *db.SessionRow, reading copilotAPIStateReading) {
	if row == nil {
		return
	}
	stored, err := db.GetContextSnapshot(row.ID)
	if err != nil {
		slog.Warn("copilot-api-state: failed to read context window; skipping context write",
			"session_id", row.ID, "error", err, "module", "agentd")
		return
	}
	window := copilotAPIEffectiveContextWindow(row.ConvID, reading)
	pct := copilotContextPct(reading.TotalTokens, window)
	// A reply that carried no limit CARRIES THE STORED ONE FORWARD rather than
	// writing zero. UpdateContextSnapshotForGeneration writes all four columns
	// together, so passing the raw figure would erase a real window an earlier
	// reply (or a compaction the durable follower saw) had disclosed — and once
	// this consumer is publishing, the follower has stood down and cannot put
	// it back. Same rule the durable follower states for the same columns.
	observedWindow := reading.PromptTokenLimit
	if observedWindow <= 0 {
		observedWindow = stored.ContextWindowSize
	}
	// tokens_output may only ever ADVANCE, matching the discipline the other
	// two writers already follow. The sources count different things and either
	// may legitimately be ahead — this one counts the session Copilot is
	// reporting on now, while the durable log's shutdown total survives a
	// resume — and writing the lower figure would stick rather than blink,
	// because a source that already mirrors the higher value never issues a
	// corrective write.
	output := max(reading.OutputTokens, stored.TokensOutput)

	if pct == stored.ContextPct && reading.TotalTokens == stored.TokensInput &&
		output == stored.TokensOutput && observedWindow == stored.ContextWindowSize {
		return
	}
	// Generation-guarded for the same reason every other writer of these
	// columns is: this reading describes the conversation that was live when
	// the read began, and a session pruned and recreated in between must not
	// inherit it.
	if _, err := db.UpdateContextSnapshotForGeneration(
		row.ID, row.ConvID, row.CreatedAt,
		pct, reading.TotalTokens, output, observedWindow,
	); err != nil {
		slog.Warn("copilot-api-state: failed to persist context snapshot",
			"session_id", row.ID, "error", err, "module", "agentd")
	}
}

// copilotAPIEffectiveContextWindow resolves the denominator for an API-driven
// agent.
//
// It differs from copilotEffectiveContextWindow in exactly one place, and the
// difference is the point of this drive: a limit COPILOT REPORTED outranks the
// static per-model assumption tclaude keeps for the drives that cannot ask.
// The operator's own configured cap still wins outright, because that is
// intent rather than an estimate — the settled TCL-1048 precedence — and the
// static table remains as the last resort for a session whose reply carried no
// limit.
func copilotAPIEffectiveContextWindow(convID string, reading copilotAPIStateReading) int64 {
	if window := copilotConfiguredContextWindowMax(convID); window > 0 {
		return window
	}
	if reading.PromptTokenLimit > 0 {
		return reading.PromptTokenLimit
	}
	return harness.CopilotContextWindowDefault(strings.TrimSpace(reading.Model))
}

// copilotAPIReadingModel resolves the model a session's spend is attributed to,
// or "" when nothing has resolved one yet.
//
// UsageMetrics.CurrentModel is the SELECTED model, and under auto mode that is
// the literal string "auto" until a call has resolved one — measured against a
// live server, mid-turn. "auto" is not a model, and publishing it as one is not
// harmless: harness.CopilotContextWindowDefault answers any non-empty unknown
// model with a generic 200k, so a session actually running a 128k model would
// be metered against a window it does not have. That figure would look
// completely ordinary on the dashboard, which is what makes it worth spending a
// function on.
//
// ModelMetrics is keyed by the REAL model ids Copilot billed, so a session that
// has completed a call names its model there. One key is the ordinary case;
// with several — a model change part way through a session — there is no
// "current" among them, so none is claimed. Empty means unknown here exactly as
// it does everywhere else in this file, and the caller falls back to the
// reported limit rather than to a guess.
func copilotAPIReadingModel(metrics copilotapi.UsageMetrics) string {
	if model := strings.TrimSpace(metrics.CurrentModel); model != "" &&
		model != copilotAPIAutoModel {
		return model
	}
	if len(metrics.ModelMetrics) != 1 {
		return ""
	}
	for model := range metrics.ModelMetrics {
		return strings.TrimSpace(model)
	}
	return ""
}

// copilotAPIOutputTokens sums the session's output across every model it used.
//
// Summed across models rather than taken from a session-level field because
// there is no session-level output total in the reply — and read from the
// NESTED Usage, which is the trap this series has already been bitten by: a
// flattened struct decodes the same payload without error and reports zero.
func copilotAPIOutputTokens(metrics copilotapi.UsageMetrics) int64 {
	var total int64
	for _, model := range metrics.ModelMetrics {
		total += int64(model.Usage.OutputTokens)
	}
	return total
}

// applyPermissions projects "a human is being waited on" onto the session row.
//
// Written with a compare-and-swap against the row this consumer just read, so a
// hook that fires concurrently always wins — the same discipline the daemon's
// own background reconcile uses. That is what keeps this from racing the hook
// path that owns busy/idle on both Copilot drives.
//
// Ownership of the awaiting state is read off the ROW, not remembered in the
// consumer: the pair (status = awaiting_permission, detail =
// copilotAPIStatePermissionDetail) is the marker, and only a row carrying it is
// ever cleared. An in-memory flag was the obvious alternative and is worse in
// both directions. It cannot clear a state a PREDECESSOR set — after an agentd
// restart nothing would ever take the row out of awaiting_permission, and that
// is not a cosmetic wrong label: message delivery is held for an agent in an
// awaiting state, so the agent would silently stop receiving mail. And a flag
// dropped before a clear that then failed would strand the row the same way for
// the lifetime of the process. Reading the marker back makes both cases
// self-correcting on the next refresh, which happens at least every
// copilotAPIStateBackstop.
//
// It never touches an awaiting state it did not set — an awaiting_input from
// elsewhere carries a different detail and is left exactly as found.
func (c *copilotAPIStateConsumer) applyPermissions(
	ctx context.Context, handle *copilotAPISession, row *db.SessionRow, waiting bool,
) {
	if row == nil {
		return
	}
	ours := row.Status == session.StatusAwaitingPermission &&
		row.StatusDetail == copilotAPIStatePermissionDetail

	if waiting {
		if ours {
			return
		}
		// Entered only from the states the hook path leaves behind. Anything
		// else — exited, error, another awaiting — belongs to someone else.
		if row.Status != session.StatusWorking && row.Status != session.StatusIdle &&
			row.Status != session.StatusMainAgentIdle {
			return
		}
		set, err := db.SetSessionStatusIfUnchanged(row.ID, row.Status, row.UpdatedAt,
			session.StatusAwaitingPermission, copilotAPIStatePermissionDetail, time.Now())
		if err != nil {
			c.warn("status", "copilot-api-state: failed to record a pending permission prompt", err)
			return
		}
		if set {
			slog.Debug("copilot-api-state: agent is waiting on a permission prompt",
				"conv_id", c.convID, "session_id", row.ID, "module", "agentd")
		}
		return
	}

	if !ours {
		// Nothing of this consumer's to clear. The common case by far, and the
		// reason the read below is not made on every refresh.
		return
	}
	// The prompt is resolved. What follows it is not knowable from its absence
	// — the human may have approved it, in which case the turn is running
	// again, or declined it, in which case the turn is over — so the successor
	// state is READ rather than assumed.
	//
	// A failure here leaves the row in the awaiting state AND leaves the marker
	// on it, so the next refresh tries again rather than giving up.
	processing, err := handle.Client.IsProcessing(ctx, c.sessionID)
	if err != nil {
		c.warn("processing", "copilot-api-state: busy read failed while clearing a permission prompt", err)
		return
	}
	next := session.StatusIdle
	if processing {
		next = session.StatusWorking
	}
	if _, err := db.SetSessionStatusIfUnchanged(row.ID, row.Status, row.UpdatedAt,
		next, "", time.Now()); err != nil {
		c.warn("status", "copilot-api-state: failed to clear a resolved permission prompt", err)
	}
}

// releaseStrandedPermission takes this consumer's awaiting marker off the row
// once it can no longer be shown to be true.
//
// The awaiting projection is the one thing this consumer owns that does NOT
// self-correct when its reads stop working, and the asymmetry is easy to miss.
// A context reading ages out of copilotAPIStateFreshness and the pre-existing
// Copilot sources resume, so the meter degrades to stale-and-labelled. The
// awaiting marker has no such expiry: by design the only thing that removes it
// is a later successful refresh BY THIS CONSUMER, so a consumer that has
// stopped being able to read — an ownership proof that keeps failing, a session
// id the server no longer knows — leaves the row in awaiting_permission with
// nothing left that will ever clear it.
//
// That is not a wrong label. An awaiting row HOLDS message delivery, so the
// agent goes mute. It is the same end state the ownership gate exists to
// prevent, reached from the opposite direction: not a stranger's prompt copied
// in, but this agent's own resolved prompt that can no longer be cleared.
//
// Released to working rather than idle. The successor state is genuinely
// unknown here — that is the whole problem — and of the two, idle is the
// dangerous guess: it says the agent is available. Working holds no delivery
// and is repainted by the hook path that owns busy/idle at the next turn end,
// so this hands the row back rather than asserting anything about it. No RPC is
// involved, which matters because the usual reason to be here is that RPC is
// exactly what has stopped working.
func (c *copilotAPIStateConsumer) releaseStrandedPermission(row *db.SessionRow) {
	if time.Since(c.lastPermissionRead) <= copilotAPIStateFreshness {
		// Ordinary transient failure. Leaving the marker is right: the next
		// refresh clears it properly, from a read rather than from a guess.
		return
	}
	if row == nil {
		row = copilotAPIStateSessionRow(c.convID)
	}
	if row == nil {
		return
	}
	// Only ever this consumer's own marker, on exactly the terms applyPermissions
	// uses. An awaiting_input from elsewhere carries a different detail.
	if row.Status != session.StatusAwaitingPermission ||
		row.StatusDetail != copilotAPIStatePermissionDetail {
		return
	}
	set, err := db.SetSessionStatusIfUnchanged(row.ID, row.Status, row.UpdatedAt,
		session.StatusWorking, "", time.Now())
	if err != nil {
		c.warn("release", "copilot-api-state: failed to release a permission marker "+
			"this consumer can no longer clear", err)
		return
	}
	if set {
		slog.Warn("copilot-api-state: released a permission marker after reads stopped "+
			"succeeding; message delivery for this agent was being held",
			"conv_id", c.convID, "session_id", row.ID, "module", "agentd")
	}
}

// copilotAPIStateSessionRow returns the row a conversation's readings belong
// to: its newest row that has not exited.
//
// Newest-not-exited rather than "the live one", because deciding liveness means
// asking tmux, and this runs on an event-triggered path rather than on the
// daemon's own cached liveness tick. A retained predecessor row is excluded by
// its exited status, and a row whose pane has died without being marked yet is
// harmless: the writes below are all compare-and-swaps or generation-guarded.
func copilotAPIStateSessionRow(convID string) *db.SessionRow {
	rows, err := db.FindSessionsByConvID(convID)
	if err != nil {
		return nil
	}
	for _, row := range rows {
		if row == nil || row.Harness != harness.CopilotName ||
			row.Status == session.StatusExited {
			continue
		}
		return row
	}
	return nil
}

func (c *copilotAPIStateConsumer) warn(reason, message string, err error) {
	now := time.Now()
	if last, seen := c.warnedAt[reason]; seen && now.Sub(last) < copilotAPIStateWarnInterval {
		return
	}
	c.warnedAt[reason] = now
	slog.Warn(message, "conv_id", c.convID, "session_id", c.sessionID,
		"reason", reason, "error", err, "module", "agentd")
}
