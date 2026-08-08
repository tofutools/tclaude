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

// lookupCopilotAPIState returns the live reading for a conversation.
//
// False is the ordinary state for every send-keys agent, for an API agent whose
// connection has ended, and for one whose session has not run a turn yet —
// `session.metadata.contextInfo` legitimately answers null until then. All
// three mean the same thing to a caller: this source has nothing to say, use
// the ones that did.
func lookupCopilotAPIState(convID string) (copilotAPIStateReading, bool) {
	if convID == "" {
		return copilotAPIStateReading{}, false
	}
	copilotAPIStates.Lock()
	defer copilotAPIStates.Unlock()
	reading, found := copilotAPIStates.readings[convID]
	return reading, found
}

// copilotAPIStateConsumer is one conversation's event-triggered reader.
type copilotAPIStateConsumer struct {
	convID    string
	sessionID string
	client    *copilotapi.Client

	stop     chan struct{}
	stopOnce sync.Once

	// awaiting records that THIS consumer put the row into the
	// awaiting-permission state, so it only ever clears a state it set. A
	// consumer that cleared any awaiting state it happened to find would
	// stomp on awaiting_input, which comes from somewhere else entirely.
	awaiting bool

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
		client:    handle.Client,
		stop:      make(chan struct{}),
		warnedAt:  map[string]time.Time{},
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
	defer c.retire()

	subscription := c.client.Subscribe()
	defer subscription.Close()

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
				c.refresh()
				lastRefresh = time.Now()
				owed = false
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
			c.refresh()
			lastRefresh = time.Now()
		case <-settle.C:
			owed = false
			c.refresh()
			lastRefresh = time.Now()
		case <-backstop.C:
			c.refresh()
			lastRefresh = time.Now()
		}
	}
}

// retire tears down everything this consumer published, so a dead connection
// stops being a source the rest of the daemon defers to.
func (c *copilotAPIStateConsumer) retire() {
	dropCopilotAPIState(c.convID)
	copilotAPIStateConsumers.Lock()
	if copilotAPIStateConsumers.running[c.convID] == c {
		delete(copilotAPIStateConsumers.running, c.convID)
	}
	copilotAPIStateConsumers.Unlock()
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
func (c *copilotAPIStateConsumer) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), copilotAPIStateReadTimeout)
	defer cancel()

	info, err := c.client.ContextInfo(ctx, copilotapi.ContextInfoParams{SessionID: c.sessionID})
	if err != nil {
		c.warn("context", "copilot-api-state: context read failed", err)
		return
	}
	metrics, err := c.client.UsageMetrics(ctx, c.sessionID)
	if err != nil {
		c.warn("usage", "copilot-api-state: usage read failed", err)
		return
	}
	pending, err := c.client.PendingPermissionRequests(ctx, c.sessionID)
	pendingRead := err == nil
	if err != nil {
		// Not fatal to the refresh: the numbers above are already in hand and
		// are the more important half. Only the permission projection is lost,
		// and the next refresh retries it.
		c.warn("permissions", "copilot-api-state: pending-permission read failed", err)
	}

	// One row read serves both projections below, and both are guarded against
	// it being nil — an agent whose row has gone (retired, pruned) is not a
	// failure, it is a consumer that is about to stop.
	row := copilotAPIStateSessionRow(c.convID)
	if pendingRead {
		c.applyPermissions(ctx, row, len(pending) > 0)
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
		Model:            metrics.CurrentModel,
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
// Only sessions.model and sessions.effort_level are left alone. They stay owned
// by the usage sweep, which reads them out of Copilot's own store for both
// drives; taking them here would mean owning `effort_level`, which this reading
// has no source for at all.
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
	// tokens_output may only ever ADVANCE, matching the discipline the other
	// two writers already follow. The sources count different things and either
	// may legitimately be ahead — this one counts the session Copilot is
	// reporting on now, while the durable log's shutdown total survives a
	// resume — and writing the lower figure would stick rather than blink,
	// because a source that already mirrors the higher value never issues a
	// corrective write.
	output := max(reading.OutputTokens, stored.TokensOutput)

	if pct == stored.ContextPct && reading.TotalTokens == stored.TokensInput &&
		output == stored.TokensOutput && reading.PromptTokenLimit == stored.ContextWindowSize {
		return
	}
	// Generation-guarded for the same reason every other writer of these
	// columns is: this reading describes the conversation that was live when
	// the read began, and a session pruned and recreated in between must not
	// inherit it.
	if _, err := db.UpdateContextSnapshotForGeneration(
		row.ID, row.ConvID, row.CreatedAt,
		pct, reading.TotalTokens, output, reading.PromptTokenLimit,
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
// Only transitions this consumer is entitled to make are made. It enters the
// awaiting state from working/idle, and it leaves only a state it entered
// itself, so an awaiting_input coming from elsewhere is never cleared here.
func (c *copilotAPIStateConsumer) applyPermissions(
	ctx context.Context, row *db.SessionRow, waiting bool,
) {
	if !waiting && !c.awaiting {
		return
	}
	if row == nil {
		return
	}
	if waiting {
		if row.Status == session.StatusAwaitingPermission {
			c.awaiting = true
			return
		}
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
			c.awaiting = true
			slog.Debug("copilot-api-state: agent is waiting on a permission prompt",
				"conv_id", c.convID, "session_id", row.ID, "module", "agentd")
		}
		return
	}

	// The prompt is resolved. What follows it is not knowable from its absence
	// — the human may have approved it, in which case the turn is running
	// again, or declined it, in which case the turn is over — so the successor
	// state is READ rather than assumed.
	c.awaiting = false
	if row.Status != session.StatusAwaitingPermission ||
		row.StatusDetail != copilotAPIStatePermissionDetail {
		// A hook has already moved the row on, which is the common case: the
		// approved tool call fires PostToolUse a moment later. Nothing to undo.
		return
	}
	processing, err := c.client.IsProcessing(ctx, c.sessionID)
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
