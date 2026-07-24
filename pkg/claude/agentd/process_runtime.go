package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

const (
	processRunFallbackInterval = time.Minute
	processRunListDefault      = 20
	// Evidence payloads may each reach 256 KiB. Keep the public page at a
	// deliberate 4 MiB worst-case payload instead of exposing the store's
	// larger transition/read bound as a wire contract.
	processRunEventListMax     = 16
	processRunEventListDefault = processRunEventListMax
	processRunMaxClaims        = db.MaxProcessRunReadPage
	maxProcessRunRequestBytes  = db.MaxProcessRunParamsBytes + db.MaxProcessRunAuthorizationsBytes + 16<<10
	// processRunConcurrency is the fixed number of external programs one run
	// may have in flight. Ready branches past it simply stay ready: there is no
	// durable queue, and the next completion plans the next one.
	processRunConcurrency = 4
	// processRunDecideAttempts bounds the alternation between routing a
	// decision to a live owner and taking the claim ourselves. Each pass loses
	// only if the claim changed hands in between, which cannot repeat
	// indefinitely under a real workload; the bound exists so a pathological
	// hand-off storm fails closed instead of spinning.
	processRunDecideAttempts = 8
)

var (
	errProcessRunClaimed     = errors.New("process run is already being driven")
	errProcessRunCapacity    = errors.New("process runtime is at its active-run limit")
	errProcessRuntimeStopped = errors.New("process runtime is shutting down")
	processProgramPerform    = executor.Perform
	processRuns              = newProcessRunManager()
)

// processRunDecisionRequest is the entire live-claim request seam: one
// authenticated decision handed to the run's single state owner, answered
// exactly once. It is deliberately not a general mailbox — nothing else is
// routed this way, and the owner is the only reader.
type processRunDecisionRequest struct {
	actor string
	input executor.DecisionInput
	// reply is buffered, so the owner answers without ever blocking on an HTTP
	// caller that gave up. The owner MUST send exactly one answer for every
	// request it receives, before it exits.
	reply chan error
}

// processRunProgramResult is one worker's report back to the owner. The worker
// produces it and touches nothing else.
type processRunProgramResult struct {
	dispatch *executor.Dispatch
	result   executor.Result
	err      error
}

type processRunClaim struct {
	// run is owned exclusively by the driver goroutine. No handler and no
	// worker may read or mutate it.
	run    *executor.Run
	ctx    context.Context
	cancel context.CancelFunc
	// workerCtx is cancelled to stop sibling programs best-effort once a branch
	// has failed, without tearing down the owner itself. Shutdown cancels the
	// parent ctx instead and reaches the same workers.
	workerCtx     context.Context
	cancelWorkers context.CancelFunc
	decisions     chan *processRunDecisionRequest
	// gone is closed once this claim can no longer accept anything, after the
	// manager has already dropped it. A sender that observes it can safely fall
	// back to the ordinary load/claim path.
	gone     chan struct{}
	released sync.Once

	// mu guards only the presentation set below. It is written by the owner and
	// read by HTTP readers; it is not run state.
	mu        sync.Mutex
	executing map[string]struct{}
}

// processRunManager owns only short-lived in-process claims. SQLite remains
// authoritative for every checkpoint and command transition; a claim retains
// the one prepared executor.Run only while that run is actively advancing.
type processRunManager struct {
	mu          sync.Mutex
	claims      map[string]*processRunClaim
	sweepCursor string
	stopped     bool
	wg          sync.WaitGroup
}

func (c *processRunClaim) markExecuting(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.executing[nodeID] = struct{}{}
}

func (c *processRunClaim) clearExecuting(nodeID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.executing, nodeID)
}

func (c *processRunClaim) executingNodes() map[string]struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	nodes := make(map[string]struct{}, len(c.executing))
	for nodeID := range c.executing {
		nodes[nodeID] = struct{}{}
	}
	return nodes
}

// submitDecision hands one decision to this claim's owner and waits for its
// answer. delivered is false only when the claim died before accepting it, in
// which case the caller retries through the ordinary path.
func (c *processRunClaim) submitDecision(actor string, input executor.DecisionInput) (err error, delivered bool) {
	request := &processRunDecisionRequest{actor: actor, input: input, reply: make(chan error, 1)}
	select {
	case c.decisions <- request:
	case <-c.gone:
		return nil, false
	}
	// The owner answers every accepted request before it exits, so the reply
	// always arrives. Selecting on gone as well keeps a broken owner from
	// hanging an HTTP handler forever, and draining the buffered reply first
	// keeps a committed verdict from being reported as refused.
	select {
	case err := <-request.reply:
		return err, true
	case <-c.gone:
		select {
		case err := <-request.reply:
			return err, true
		default:
			return errProcessRunClaimed, true
		}
	}
}

type processRunStartMode int

const (
	processRunResume processRunStartMode = iota
	processRunReissue
	processRunRecordOutcome
	processRunDecide
)

type processRunStart struct {
	mode processRunStartMode
	// selector names which outbox entry a reconciliation action means, by node
	// id. It may be empty only while exactly one command is reconcilable.
	selector string
	actor    string
	outcome  executor.RecordedOutcome
	decision executor.DecisionInput
}

type processRunView struct {
	ID                    string            `json:"id"`
	TemplateRef           string            `json:"templateRef"`
	Params                map[string]string `json:"params"`
	ProgramAuthorizations []string          `json:"programAuthorizations"`
	Status                engine.RunStatus  `json:"status"`
	StateVersion          int64             `json:"stateVersion"`
	Checkpoint            engine.Checkpoint `json:"checkpoint"`
	// Action is a coarse one-word summary for humans and scripts. Commands and
	// AwaitingDecisions are the addressable truth: a run under bounded
	// concurrency can hold several of each at once.
	Action         string `json:"action"`
	NeedsReconcile bool   `json:"needsReconcile"`
	// AwaitingDecision repeats the first entry of AwaitingDecisions so existing
	// single-decision clients keep working.
	AwaitingDecision  *processRunDecisionView  `json:"awaitingDecision,omitempty"`
	AwaitingDecisions []processRunDecisionView `json:"awaitingDecisions"`
	Commands          []processRunCommandView  `json:"commands"`
	CreatedAt         time.Time                `json:"createdAt"`
	UpdatedAt         time.Time                `json:"updatedAt"`
}

// processRunDecisionView presents one awaited decision with its selectable
// verdicts derived from the prepared definition; the durable checkpoint keeps
// only the obligation identity.
type processRunDecisionView struct {
	NodeID   string   `json:"nodeId"`
	Verdicts []string `json:"verdicts"`
}

// processRunCommandView presents one durable outbox entry. State is executing
// while this daemon still accounts for the program, and needs_reconcile once it
// does not — the two are indistinguishable in SQLite alone, which is exactly
// why a cold load reports every entry as the latter.
type processRunCommandView struct {
	CommandID string `json:"commandId"`
	NodeID    string `json:"nodeId"`
	Profile   string `json:"profile,omitempty"`
	Program   string `json:"program"`
	State     string `json:"state"`
}

const (
	processCommandExecuting      = "executing"
	processCommandNeedsReconcile = "needs_reconcile"
)

type processRunSummaryView struct {
	ID           string           `json:"id"`
	TemplateRef  string           `json:"templateRef"`
	Status       engine.RunStatus `json:"status"`
	StateVersion int64            `json:"stateVersion"`
	CreatedAt    time.Time        `json:"createdAt"`
	UpdatedAt    time.Time        `json:"updatedAt"`
}

type processRunEventView struct {
	Sequence   int64           `json:"sequence"`
	OccurredAt time.Time       `json:"occurredAt"`
	NodeID     string          `json:"nodeId,omitempty"`
	Kind       string          `json:"kind"`
	Payload    json.RawMessage `json:"payload"`
	Actor      string          `json:"actor,omitempty"`
}

type processRunCreateRequest struct {
	ID                       string            `json:"id,omitempty"`
	TemplateID               string            `json:"templateId"`
	Params                   map[string]string `json:"params,omitempty"`
	AuthorizeProgramProfiles []string          `json:"authorizeProgramProfiles"`
}

type processRunOutcomeRequest struct {
	NodeID   string                `json:"nodeId,omitempty"`
	Outcome  engine.ProgramOutcome `json:"outcome"`
	ExitCode int                   `json:"exitCode"`
	Error    string                `json:"error,omitempty"`
	Note     string                `json:"note,omitempty"`
}

// processRunReissueRequest carries only the selector: which outstanding command
// the operator means. It stays optional so the single-command case needs no
// ceremony.
type processRunReissueRequest struct {
	NodeID string `json:"nodeId,omitempty"`
}

type processRunDecideRequest struct {
	NodeID   string `json:"nodeId"`
	Verdict  string `json:"verdict"`
	Evidence string `json:"evidence,omitempty"`
}

func newProcessRunManager() *processRunManager {
	return &processRunManager{claims: make(map[string]*processRunClaim)}
}

func (m *processRunManager) claim(runID string) (*processRunClaim, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return nil, false, errProcessRuntimeStopped
	}
	if _, exists := m.claims[runID]; exists {
		return nil, false, nil
	}
	if len(m.claims) >= processRunMaxClaims {
		return nil, false, errProcessRunCapacity
	}
	ctx, cancel := context.WithCancel(context.Background())
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	claim := &processRunClaim{
		ctx: ctx, cancel: cancel,
		workerCtx: workerCtx, cancelWorkers: cancelWorkers,
		decisions: make(chan *processRunDecisionRequest),
		gone:      make(chan struct{}),
		executing: make(map[string]struct{}),
	}
	m.claims[runID] = claim
	m.wg.Add(1)
	return claim, true, nil
}

func (m *processRunManager) release(runID string, claim *processRunClaim) {
	claim.released.Do(func() {
		m.mu.Lock()
		if m.claims[runID] == claim {
			delete(m.claims, runID)
		}
		m.mu.Unlock()
		claim.cancelWorkers()
		claim.cancel()
		// Closed only after the claim is unreachable, so a decision sender that
		// observes it is guaranteed to find the run claimable on its retry.
		close(claim.gone)
		m.wg.Done()
	})
}

// liveClaim returns the current owner of a run, if any.
func (m *processRunManager) liveClaim(runID string) (*processRunClaim, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stopped {
		return nil, false, errProcessRuntimeStopped
	}
	claim, ok := m.claims[runID]
	return claim, ok, nil
}

// executingCommands reports which of a run's durable commands this daemon is
// still running a program for, and whether the run is claimed at all.
func (m *processRunManager) executingCommands(runID string) (map[string]struct{}, bool) {
	m.mu.Lock()
	claim, ok := m.claims[runID]
	m.mu.Unlock()
	if !ok {
		return nil, false
	}
	return claim.executingNodes(), true
}

func (m *processRunManager) begin(runID string, start processRunStart) (bool, error) {
	return m.beginRun(runID, nil, start)
}

func (m *processRunManager) beginPrepared(run *executor.Run, start processRunStart) (bool, error) {
	if run == nil || run.ID() == "" {
		return false, executor.ErrInvalidRun
	}
	return m.beginRun(run.ID(), run, start)
}

func (m *processRunManager) beginRun(runID string, prepared *executor.Run, start processRunStart) (bool, error) {
	claim, acquired, err := m.claim(runID)
	if err != nil || !acquired {
		if err == nil && !acquired && start.mode != processRunResume {
			return false, errProcessRunClaimed
		}
		return false, err
	}
	release := true
	defer func() {
		if release {
			m.release(runID, claim)
		}
	}()

	run := prepared
	if run == nil {
		run, err = executor.LoadRun(runID)
		if err != nil {
			return false, err
		}
	}
	if claim.ctx.Err() != nil {
		return false, errProcessRuntimeStopped
	}

	var dispatches []*executor.Dispatch
	// committed records that this start already made a durable change. A
	// follow-on planning refusal must not then be reported as if that change
	// had failed.
	committed := false
	var fillErr error
	switch start.mode {
	case processRunResume:
		if run.Action().Kind == executor.ActionNeedsReconcile {
			return false, executor.ErrNeedsReconcile
		}
		dispatches, err = fillProcessRun(run, processRunConcurrency)
	case processRunReissue:
		var reissued *executor.Dispatch
		if reissued, err = executor.Reissue(run, start.actor, start.selector); err == nil {
			committed = true
			dispatches = append(dispatches, reissued)
			var more []*executor.Dispatch
			more, fillErr = fillProcessRun(run, processRunConcurrency-1)
			dispatches = append(dispatches, more...)
		}
	case processRunRecordOutcome:
		if err = executor.RecordOutcome(run, start.actor, start.selector, start.outcome); err == nil {
			committed = true
			dispatches, fillErr = fillProcessRun(run, processRunConcurrency)
		}
	case processRunDecide:
		if err = executor.RecordDecision(run, start.actor, start.decision); err == nil {
			committed = true
			// The verdict itself committed atomically above. The sweep reaches
			// this run again even when a sibling branch is still awaiting its
			// own decision, because the runnable predicate admits a run whose
			// ready nodes outnumber its awaited obligations.
			dispatches, fillErr = fillProcessRun(run, processRunConcurrency)
		}
	default:
		err = fmt.Errorf("unknown process run start mode")
	}
	if err != nil {
		return false, err
	}
	if fillErr != nil && (!committed || !errors.Is(fillErr, executor.ErrNeedsReconcile)) {
		return false, fillErr
	}
	if len(dispatches) == 0 {
		if fillErr != nil {
			// Committed, but another branch still needs a human before anything
			// can be planned. The action succeeded; nothing started.
			return false, nil
		}
		switch run.Action().Kind {
		case executor.ActionNeedsReconcile:
			return false, executor.ErrNeedsReconcile
		case executor.ActionAwaitDecision, executor.ActionTerminal:
			return false, nil
		default:
			return false, fmt.Errorf("process run did not become dispatchable, decision-blocked, or terminal")
		}
	}

	claim.run = run
	release = false
	go m.drive(runID, claim, dispatches)
	return true, nil
}

// fillProcessRun plans durable commands until capacity is used up or no ready
// task remains. Each one commits before its program is allowed to start; the
// ready branches it did not reach are simply left ready.
func fillProcessRun(run *executor.Run, capacity int) ([]*executor.Dispatch, error) {
	var dispatches []*executor.Dispatch
	for range capacity {
		dispatch, err := executor.Prepare(run)
		if err != nil {
			return dispatches, err
		}
		if dispatch == nil {
			return dispatches, nil
		}
		dispatches = append(dispatches, dispatch)
	}
	return dispatches, nil
}

// drive is the run's single state owner. It is the ONLY goroutine that touches
// the executor.Run, the checkpoint, or the store for this run: workers execute
// immutable dispatch values and report back here, and decisions arrive here as
// typed requests rather than as a second writer.
//
// It exits when nothing is in flight — a run parked purely on humans is not
// claimed, so the ordinary load/claim path serves it.
func (m *processRunManager) drive(runID string, claim *processRunClaim, dispatches []*executor.Dispatch) {
	defer m.release(runID, claim)
	// Capacity covers every worker this owner can ever have in flight, so a
	// worker's single send never blocks and no worker can outlive the loop.
	results := make(chan processRunProgramResult, processRunConcurrency)
	inFlight := 0
	start := func(pending []*executor.Dispatch) {
		for _, dispatch := range pending {
			// Authorize the command this dispatch actually executes, not the one
			// a run-level view happens to surface first: the outbox is plural, so
			// the two only coincide while a single command is in flight.
			command := dispatch.Command()
			authorization, ok := claim.run.AuthorizationFor(command.Program.Profile)
			if !ok {
				// Creation refuses this shape, but an old/future malformed row
				// must still fail closed. Dropping the live permission makes the
				// durable command explicitly reconcilable on the next load.
				slog.Warn("process runtime: persisted program authorization missing",
					"run", runID, "node", command.NodeID, "profile", command.Program.Profile)
				executor.Abandon(claim.run, dispatch)
				continue
			}
			if err := executor.Claim(claim.run, dispatch, authorization); err != nil {
				slog.Warn("process runtime: dispatch permission is not live",
					"run", runID, "node", command.NodeID, "error", err)
				executor.Abandon(claim.run, dispatch)
				continue
			}
			claim.markExecuting(command.NodeID)
			inFlight++
			go func() {
				result, err := processProgramPerform(claim.workerCtx, dispatch)
				results <- processRunProgramResult{dispatch: dispatch, result: result, err: err}
			}()
		}
	}
	start(dispatches)

	for inFlight > 0 {
		var pending []*executor.Dispatch
		select {
		case result := <-results:
			inFlight--
			claim.clearExecuting(result.dispatch.Command().NodeID)
			if next := m.observeProcessRunResult(runID, claim, result); next != nil {
				pending = append(pending, next)
			}
		case request := <-claim.decisions:
			// The owner is the single writer, so a verdict racing a program
			// result is serialized here rather than by a lock: whichever the
			// select takes first commits first, and the loser sees the state the
			// winner left behind.
			request.reply <- executor.RecordDecision(claim.run, request.actor, request.input)
		}
		more, err := fillProcessRun(claim.run, processRunConcurrency-inFlight-len(pending))
		if err != nil && !errors.Is(err, executor.ErrNeedsReconcile) {
			slog.Warn("process runtime: planning stopped", "run", runID, "error", err)
		}
		start(append(pending, more...))
	}
}

// observeProcessRunResult commits one worker's outcome and returns the single
// command that observation atomically planned, if any.
func (m *processRunManager) observeProcessRunResult(runID string, claim *processRunClaim, result processRunProgramResult) *executor.Dispatch {
	if result.err != nil {
		// No observation exists, and the program may well have run. Dropping the
		// permission leaves the durable command outstanding and explicitly
		// reconcilable — the honest cold-load shape — rather than guessing.
		slog.Warn("process runtime: program produced no observation", "run", runID,
			"node", result.dispatch.Command().NodeID, "error", result.err)
		executor.Abandon(claim.run, result.dispatch)
		return nil
	}
	if result.result.Observation.Outcome == engine.ProgramFailed {
		// Best-effort sibling cancellation. The killed programs still come back
		// as ordinary observations, so nothing is lost or assumed.
		claim.cancelWorkers()
	}
	next, err := executor.Observe(claim.run, result.dispatch, result.result)
	if err != nil {
		// The worker is gone either way, so this branch can no longer be
		// accounted for in memory. Dropping the permission is what stops the
		// owner from planning past an outcome nobody committed.
		slog.Warn("process runtime: observation not durable", "run", runID,
			"node", result.dispatch.Command().NodeID, "error", err)
		executor.Abandon(claim.run, result.dispatch)
		return nil
	}
	return next
}

// decide routes one authenticated verdict to whoever currently owns the run:
// the live claim's owner while programs are executing, or a fresh claim of our
// own when nothing is driving it. A run whose siblings are mid-program is
// exactly the case that used to be refused for as long as those programs ran.
// started reports whether the run is being actively driven afterwards, which
// for the live-owner path it already was.
func (m *processRunManager) decide(runID, actor string, input executor.DecisionInput) (started bool, err error) {
	for range processRunDecideAttempts {
		claim, live, err := m.liveClaim(runID)
		if err != nil {
			return false, err
		}
		if live {
			if err, delivered := claim.submitDecision(actor, input); delivered {
				return err == nil, err
			}
			// The claim was released between the lookup and the send, so the run
			// is ours to take on the next pass.
			continue
		}
		started, err := m.begin(runID, processRunStart{mode: processRunDecide, actor: actor, decision: input})
		if !errors.Is(err, errProcessRunClaimed) {
			return started, err
		}
	}
	return false, errProcessRunClaimed
}

func (m *processRunManager) shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.stopped = true
	claims := make([]*processRunClaim, 0, len(m.claims))
	for _, claim := range m.claims {
		claims = append(claims, claim)
	}
	m.mu.Unlock()
	for _, claim := range claims {
		claim.cancel()
	}
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func startProcessRunRuntime(stop <-chan struct{}) {
	go func() {
		sweepProcessRuns()
		ticker := time.NewTicker(processRunFallbackInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				sweepProcessRuns()
			}
		}
	}()
}

// sweepProcessRuns loads at most one ID-only store page. SQLite excludes
// terminal and outstanding-command rows before the reconstruction boundary, so
// the periodic fallback never materializes aggregate snapshots/checkpoints or
// repeatedly prepares definitions awaiting human reconciliation.
func sweepProcessRuns() {
	if !processRoutesEnabled() {
		return
	}
	processRuns.mu.Lock()
	after := processRuns.sweepCursor
	processRuns.mu.Unlock()
	runIDs, next, err := db.ListRunnableProcessRunIDs(after, db.MaxProcessRunReadPage)
	if err != nil {
		slog.Warn("process runtime: active-run sweep failed", "error", err)
		return
	}
	processRuns.mu.Lock()
	processRuns.sweepCursor = next
	processRuns.mu.Unlock()
	for _, runID := range runIDs {
		if _, err := processRuns.begin(runID, processRunStart{mode: processRunResume}); err != nil &&
			!errors.Is(err, executor.ErrNeedsReconcile) && !errors.Is(err, errProcessRuntimeStopped) {
			slog.Warn("process runtime: active run did not start", "run", runID, "error", err)
		}
	}
}

func handleProcessRuns(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		handleProcessRunList(w, r)
	case http.MethodPost:
		handleProcessRunCreate(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method", "method not allowed")
	}
}

func handleProcessRunList(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessRunsRead); !ok {
		return
	}
	limit := processRunListDefault
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > db.MaxProcessRunReadPage {
			writeError(w, http.StatusBadRequest, "process_run_limit", fmt.Sprintf("limit must be 1..%d", db.MaxProcessRunReadPage))
			return
		}
		limit = parsed
	}
	after := strings.TrimSpace(r.URL.Query().Get("after"))
	runs, err := db.ListProcessRunSummaries(after, limit)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	views := make([]processRunSummaryView, 0, len(runs))
	for i := range runs {
		views = append(views, processRunSummaryView{
			ID: runs[i].ID, TemplateRef: runs[i].TemplateRef,
			Status: engine.RunStatus(runs[i].Status), StateVersion: runs[i].StateVersion,
			CreatedAt: runs[i].CreatedAt, UpdatedAt: runs[i].UpdatedAt,
		})
	}
	next := ""
	if len(runs) == limit {
		next = runs[len(runs)-1].ID
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"runs": views, "next": next})
}

func handleProcessRunCreate(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	var request processRunCreateRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	setAuditDetail(r, processRunCreateAuditDetail(request))
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	run, err := createProcessRun(r.Context(), request, string(actor))
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	if _, err := processRuns.beginPrepared(run, processRunStart{mode: processRunResume}); err != nil {
		// Persistence already succeeded, so creation must remain a successful,
		// idempotent-looking response. SQLite keeps the runnable checkpoint for
		// a later bounded sweep; reporting failure here invites a retry that
		// creates a second generated run with duplicate effects.
		slog.Warn("process runtime: created run deferred", "run", run.ID(), "error", err)
	}
	view, err := loadProcessRunView(run.ID())
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusCreated, view)
}

func processRunCreateAuditDetail(request processRunCreateRequest) string {
	profiles, _ := json.Marshal(request.AuthorizeProgramProfiles)
	return "template: " + strings.TrimSpace(request.TemplateID) + ", authorized profiles: " + string(profiles)
}

func handleProcessRun(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessRunsRead); !ok {
		return
	}
	view, err := loadProcessRunView(r.PathValue("id"))
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusOK, view)
}

func handleProcessRunEvents(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessRunsRead); !ok {
		return
	}
	after := int64(0)
	if raw := strings.TrimSpace(r.URL.Query().Get("after")); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, "process_run_after", "after must be a non-negative sequence")
			return
		}
		after = parsed
	}
	limit := processRunEventListDefault
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 || parsed > processRunEventListMax {
			writeError(w, http.StatusBadRequest, "process_run_limit",
				fmt.Sprintf("limit must be 1..%d", processRunEventListMax))
			return
		}
		limit = parsed
	}
	runID := r.PathValue("id")
	events, err := db.ListProcessRunEvents(runID, after, limit+1)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	if len(events) == 0 {
		// The evidence reader intentionally returns an empty page for both an
		// existing run and an unknown run. Preserve that store contract while
		// giving the HTTP surface the same stable 404 as the nearby run read.
		if _, err := db.GetProcessRun(runID); err != nil {
			writeProcessRuntimeError(w, err)
			return
		}
	}
	hasMore := len(events) > limit
	if hasMore {
		events = events[:limit]
	}
	views := make([]processRunEventView, 0, len(events))
	for i := range events {
		views = append(views, processRunEventView{
			Sequence: events[i].Sequence, OccurredAt: events[i].OccurredAt,
			NodeID: events[i].NodeID, Kind: events[i].Kind,
			Payload: events[i].PayloadJSON, Actor: events[i].Actor,
		})
	}
	var next int64
	if hasMore {
		next = events[len(events)-1].Sequence
	}
	writeProcessJSON(w, http.StatusOK, map[string]any{"events": views, "next": next})
}

func handleProcessRunResume(w http.ResponseWriter, r *http.Request) {
	if _, ok := requirePermission(w, r, PermProcessRunsManage); !ok {
		return
	}
	if err := decodeEmptyProcessRuntimeRequest(w, r); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	runID := r.PathValue("id")
	started, err := processRuns.begin(runID, processRunStart{mode: processRunResume})
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusAccepted, map[string]any{"started": started, "run": view})
}

func handleProcessRunReissue(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	var request processRunReissueRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID := r.PathValue("id")
	selector := strings.TrimSpace(request.NodeID)
	setAuditDetail(r, "run: "+runID+", command node: "+stripControl(selector))
	started, err := processRuns.begin(runID, processRunStart{
		mode: processRunReissue, actor: string(actor), selector: selector,
	})
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusAccepted, map[string]any{"started": started, "run": view})
}

func handleProcessRunRecordOutcome(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	var request processRunOutcomeRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_outcome", err.Error())
		return
	}
	if request.Outcome != engine.ProgramSucceeded && request.Outcome != engine.ProgramFailed {
		writeError(w, http.StatusUnprocessableEntity, "process_run_outcome", "outcome must be succeeded or failed")
		return
	}
	if len(request.Error) > executor.MaxProgramErrorBytes || !utf8.ValidString(request.Error) || len(request.Note) > db.MaxProcessRunEventActor*16 || !utf8.ValidString(request.Note) {
		writeError(w, http.StatusUnprocessableEntity, "process_run_outcome", "error or note exceeds its bounded UTF-8 limit")
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID := r.PathValue("id")
	selector := strings.TrimSpace(request.NodeID)
	setAuditDetail(r, "run: "+runID+", command node: "+stripControl(selector)+
		", outcome: "+stripControl(string(request.Outcome)))
	started, err := processRuns.begin(runID, processRunStart{
		mode: processRunRecordOutcome, actor: string(actor), selector: selector,
		outcome: executor.RecordedOutcome{Outcome: request.Outcome, ExitCode: request.ExitCode, Error: request.Error, Note: request.Note},
	})
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusAccepted, map[string]any{"started": started, "run": view})
}

// handleProcessRunDecide records one manual decision verdict. The input is
// bound to the awaited decision node and the durable state-version CAS, so
// duplicate, stale, wrong-node, and concurrent attempts are refused; the actor
// is always the authenticated caller, never request content.
//
// It succeeds while sibling branches are mid-program: the verdict is routed to
// the run's live owner instead of being refused because the run is claimed.
func handleProcessRunDecide(w http.ResponseWriter, r *http.Request) {
	caller, ok := requirePermission(w, r, PermProcessRunsManage)
	if !ok {
		return
	}
	var request processRunDecideRequest
	if err := decodeProcessRuntimeRequest(w, r, &request); err != nil {
		writeError(w, http.StatusBadRequest, "process_run_request", err.Error())
		return
	}
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		writeError(w, http.StatusForbidden, "process_run_actor", err.Error())
		return
	}
	runID := r.PathValue("id")
	// The detail records the attempt (including refused input), so the raw
	// request fields are neutralized before they reach the audit row.
	setAuditDetail(r, "run: "+runID+", decision node: "+stripControl(request.NodeID)+
		", verdict: "+stripControl(request.Verdict))
	started, err := processRuns.decide(runID, string(actor),
		executor.DecisionInput{NodeID: request.NodeID, Verdict: request.Verdict, Evidence: request.Evidence})
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	view, err := loadProcessRunView(runID)
	if err != nil {
		writeProcessRuntimeError(w, err)
		return
	}
	writeProcessJSON(w, http.StatusAccepted, map[string]any{"started": started, "run": view})
}

// stripControl drops control characters from untrusted request text before it
// is embedded in a human-facing audit detail line.
func stripControl(value string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, value)
}

func createProcessRun(ctx context.Context, request processRunCreateRequest, actor string) (*executor.Run, error) {
	templateID := strings.TrimSpace(request.TemplateID)
	if templateID == "" {
		return nil, fmt.Errorf("%w: templateId is required", db.ErrProcessRunInvalid)
	}
	fs, err := store.NewFS(processStoreRoot())
	if err != nil {
		return nil, err
	}
	head, err := fs.GetTemplateHead(ctx, templateID)
	if err != nil {
		return nil, err
	}
	tmpl, err := fs.GetTemplate(ctx, head.Ref)
	if err != nil {
		return nil, err
	}
	params := request.Params
	if params == nil {
		params = map[string]string{}
	}
	definition, err := engine.Prepare(tmpl, params)
	if err != nil {
		return nil, err
	}
	authorizations, err := normalizeProcessRunAuthorizations(request.AuthorizeProgramProfiles)
	if err != nil {
		return nil, err
	}
	if missing := missingProcessProgramAuthorizations(tmpl, authorizations); len(missing) > 0 {
		return nil, &processProgramAuthorizationError{Profiles: missing}
	}
	runID := strings.TrimSpace(request.ID)
	if runID == "" {
		runID = db.NewProcessRunID()
	}
	checkpoint, err := engine.Initialize(runID, definition)
	if err != nil {
		return nil, err
	}
	snapshot, err := model.CanonicalSemanticJSON(tmpl)
	if err != nil {
		return nil, err
	}
	paramsJSON, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	authorizationsJSON, err := json.Marshal(authorizations)
	if err != nil {
		return nil, err
	}
	checkpointJSON, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, err
	}
	payload, err := json.Marshal(struct {
		TemplateRef           string   `json:"templateRef"`
		ProgramAuthorizations []string `json:"programAuthorizations"`
	}{TemplateRef: head.Ref, ProgramAuthorizations: authorizations})
	if err != nil {
		return nil, err
	}
	record := &db.ProcessRun{
		ID: runID, TemplateRef: head.Ref, TemplateSnapshotJSON: snapshot,
		ParamsJSON: paramsJSON, ProgramAuthorizationsJSON: authorizationsJSON,
		Status: string(checkpoint.Status), StateVersion: db.InitialProcessRunStateVersion,
		CheckpointJSON: checkpointJSON,
	}
	err = db.CreateProcessRun(db.ProcessRunCreate{
		ID: record.ID, TemplateRef: record.TemplateRef, TemplateSnapshotJSON: record.TemplateSnapshotJSON,
		ParamsJSON: record.ParamsJSON, ProgramAuthorizationsJSON: record.ProgramAuthorizationsJSON,
		Status: record.Status, CheckpointJSON: record.CheckpointJSON,
		InitialEvents: []db.ProcessRunEvent{{
			Sequence: 1, OccurredAt: time.Now().UTC(), Kind: "run_created",
			PayloadJSON: payload, Actor: actor,
		}},
	})
	if err != nil {
		return nil, err
	}
	return executor.LoadPreparedRun(record, definition)
}

type processProgramAuthorizationError struct{ Profiles []string }

func (e *processProgramAuthorizationError) Error() string {
	profiles := make([]string, len(e.Profiles))
	for i, profile := range e.Profiles {
		if profile == "" {
			profiles[i] = "<empty>"
		} else {
			profiles[i] = profile
		}
	}
	return "program profiles require explicit authorization: " + strings.Join(profiles, ", ")
}

func normalizeProcessRunAuthorizations(profiles []string) ([]string, error) {
	if len(profiles) > db.MaxProcessRunAuthorizationProfiles {
		return nil, fmt.Errorf("%w: too many program authorization profiles", db.ErrProcessRunInvalid)
	}
	normalized := append([]string{}, profiles...)
	slices.Sort(normalized)
	for i, profile := range normalized {
		if len(profile) > db.MaxProcessRunAuthorizationProfile || !utf8.ValidString(profile) {
			return nil, fmt.Errorf("%w: invalid program authorization profile", db.ErrProcessRunInvalid)
		}
		if i > 0 && profile == normalized[i-1] {
			return nil, fmt.Errorf("%w: duplicate program authorization profile %q", db.ErrProcessRunInvalid, profile)
		}
	}
	return normalized, nil
}

func missingProcessProgramAuthorizations(tmpl *model.Template, authorized []string) []string {
	allowed := make(map[string]struct{}, len(authorized))
	for _, profile := range authorized {
		allowed[profile] = struct{}{}
	}
	missingSet := make(map[string]struct{})
	for _, node := range tmpl.Nodes {
		if node.Type == model.NodeTypeTask && node.Performer != nil && node.Performer.Kind == model.PerformerProgram {
			if _, ok := allowed[node.Performer.Profile]; !ok {
				missingSet[node.Performer.Profile] = struct{}{}
			}
		}
	}
	missing := make([]string, 0, len(missingSet))
	for profile := range missingSet {
		missing = append(missing, profile)
	}
	slices.Sort(missing)
	return missing
}

func loadProcessRunView(runID string) (processRunView, error) {
	record, err := db.GetProcessRun(strings.TrimSpace(runID))
	if err != nil {
		return processRunView{}, err
	}
	return processRunViewOf(record)
}

func processRunViewOf(record *db.ProcessRun) (processRunView, error) {
	var checkpoint engine.Checkpoint
	if err := record.DecodeCheckpoint(&checkpoint); err != nil {
		return processRunView{}, err
	}
	var params map[string]string
	if err := record.DecodeParams(&params); err != nil {
		return processRunView{}, err
	}
	var authorizations []string
	if err := record.DecodeProgramAuthorizations(&authorizations); err != nil {
		return processRunView{}, err
	}
	// SQLite alone cannot tell an executing command from an abandoned one — that
	// is precisely why a cold load calls every outstanding command ambiguous.
	// The live claim is the only thing that knows which ones it is still
	// running.
	executing, claimed := processRuns.executingCommands(record.ID)
	commands := make([]processRunCommandView, 0, len(checkpoint.Commands))
	needsReconcile := false
	anyExecuting := false
	for i := range checkpoint.Commands {
		command := checkpoint.Commands[i]
		state := processCommandNeedsReconcile
		if _, live := executing[command.NodeID]; live {
			state, anyExecuting = processCommandExecuting, true
		} else {
			needsReconcile = true
		}
		commands = append(commands, processRunCommandView{
			CommandID: command.ID, NodeID: command.NodeID,
			Profile: command.Program.Profile, Program: command.Program.Run, State: state,
		})
	}

	awaitingDecisions := make([]processRunDecisionView, 0, len(checkpoint.AwaitingDecisions))
	if len(checkpoint.AwaitingDecisions) > 0 {
		definition, err := prepareProcessRunDefinition(record)
		if err != nil {
			return processRunView{}, err
		}
		for _, obligation := range checkpoint.AwaitingDecisions {
			verdicts, ok := definition.DecisionVerdicts(obligation.NodeID)
			if !ok {
				return processRunView{}, fmt.Errorf("%w: awaited node %q is not a prepared decision",
					executor.ErrInvalidRun, obligation.NodeID)
			}
			awaitingDecisions = append(awaitingDecisions,
				processRunDecisionView{NodeID: obligation.NodeID, Verdicts: verdicts})
		}
	}
	var awaiting *processRunDecisionView
	if len(awaitingDecisions) > 0 {
		first := awaitingDecisions[0]
		awaiting = &first
	}

	// A coarse summary in operator-relevance order, not a state machine: an
	// ambiguous command needs a human before anything else, live work outranks
	// a parked branch, and the plural fields above stay addressable regardless.
	action := "runnable"
	switch {
	case needsReconcile:
		action = processCommandNeedsReconcile
	case anyExecuting:
		action = processCommandExecuting
	case awaiting != nil:
		action = "awaiting_decision"
	case checkpoint.Status != engine.RunRunning:
		action = "terminal"
	case claimed:
		action = "driving"
	}
	return processRunView{
		ID: record.ID, TemplateRef: record.TemplateRef, Params: params,
		ProgramAuthorizations: authorizations, Status: checkpoint.Status,
		StateVersion: record.StateVersion, Checkpoint: checkpoint,
		Action: action, NeedsReconcile: needsReconcile,
		AwaitingDecision: awaiting, AwaitingDecisions: awaitingDecisions, Commands: commands,
		CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt,
	}, nil
}

// prepareProcessRunDefinition prepares the run's pinned snapshot once for one
// read. The checkpoint deliberately stores only obligation identities, so the
// verdict vocabulary has to come from the definition.
func prepareProcessRunDefinition(record *db.ProcessRun) (*engine.Definition, error) {
	var tmpl model.Template
	if err := strictjson.Decode(record.TemplateSnapshotJSON, &tmpl); err != nil {
		return nil, fmt.Errorf("%w: decode template snapshot: %v", executor.ErrInvalidRun, err)
	}
	var params map[string]string
	if err := record.DecodeParams(&params); err != nil {
		return nil, fmt.Errorf("%w: %v", executor.ErrInvalidRun, err)
	}
	definition, err := engine.Prepare(&tmpl, params)
	if err != nil {
		return nil, fmt.Errorf("%w: prepare definition: %v", executor.ErrInvalidRun, err)
	}
	return definition, nil
}

func decodeProcessRuntimeRequest(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, maxProcessRunRequestBytes)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if err := strictjson.Decode(data, dst); err != nil {
		return err
	}
	return nil
}

func decodeEmptyProcessRuntimeRequest(w http.ResponseWriter, r *http.Request) error {
	var request struct{}
	return decodeProcessRuntimeRequest(w, r, &request)
}

func writeProcessRuntimeError(w http.ResponseWriter, err error) {
	var authorizationErr *processProgramAuthorizationError
	var eligibilityErr *engine.EligibilityError
	switch {
	case errors.As(err, &authorizationErr), errors.Is(err, executor.ErrUnauthorized):
		writeError(w, http.StatusForbidden, "process_program_unauthorized", err.Error())
	case errors.Is(err, db.ErrProcessRunNotFound), errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "process_run_not_found", err.Error())
	case errors.Is(err, db.ErrProcessRunExists):
		writeError(w, http.StatusConflict, "process_run_exists", err.Error())
	case errors.Is(err, errProcessRunClaimed):
		writeError(w, http.StatusConflict, "process_run_claimed", err.Error())
	case errors.Is(err, errProcessRunCapacity):
		writeError(w, http.StatusServiceUnavailable, "process_runtime_capacity", err.Error())
	case errors.Is(err, executor.ErrNeedsReconcile):
		writeError(w, http.StatusConflict, "process_run_needs_reconcile", err.Error())
	case errors.Is(err, executor.ErrAmbiguousReconcile):
		writeError(w, http.StatusConflict, "process_run_reconcile_ambiguous", err.Error())
	case errors.Is(err, executor.ErrNoReconciliation):
		writeError(w, http.StatusConflict, "process_run_not_reconcilable", err.Error())
	case errors.Is(err, engine.ErrStaleDecision):
		writeError(w, http.StatusConflict, "process_decision_stale", err.Error())
	case errors.Is(err, db.ErrProcessRunVersionConflict):
		writeError(w, http.StatusConflict, "process_run_conflict", err.Error())
	case errors.Is(err, engine.ErrInvalidDecisionVerdict):
		writeError(w, http.StatusUnprocessableEntity, "process_decision_verdict", err.Error())
	case errors.Is(err, executor.ErrInvalidDecisionInput):
		writeError(w, http.StatusUnprocessableEntity, "process_decision_invalid", err.Error())
	case errors.As(err, &eligibilityErr), errors.Is(err, engine.ErrTemplateIneligible),
		errors.Is(err, engine.ErrInvalidCheckpoint), errors.Is(err, engine.ErrInvalidTransition),
		errors.Is(err, executor.ErrInvalidRun), errors.Is(err, db.ErrProcessRunInvalid),
		errors.Is(err, db.ErrProcessRunCorrupt):
		writeError(w, http.StatusUnprocessableEntity, "process_run_invalid", err.Error())
	case errors.Is(err, errProcessRuntimeStopped):
		writeError(w, http.StatusServiceUnavailable, "process_runtime_stopping", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "process_runtime", err.Error())
	}
}
