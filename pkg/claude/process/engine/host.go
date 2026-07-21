package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
)

const (
	DefaultLeaseTTL          = 30 * time.Second
	DefaultMaxConcurrentRuns = 4
)

type Host struct {
	Store             store.Store
	Executor          *processexec.Executor
	releaseStore      schema7ReleaseStore
	epochStore        *store.FS
	epochAdapters     map[model.PerformerKind]processexec.Adapter
	Holder            string
	LeaseTTL          time.Duration
	MaxConcurrentRuns int
	Now               func() time.Time
	heartbeatTimer    func(time.Duration) (<-chan time.Time, func())
	tickMu            sync.Mutex
}

// EnableExclusiveV7 is retained as a compatibility check for callers that
// construct the production filesystem host explicitly. Schema classification
// itself is always active for a capable store; schema 7 is reset-required and
// this method no longer enables migration or execution.
func (h *Host) EnableExclusiveV7() error {
	if h == nil || h.releaseStore == nil {
		return fmt.Errorf("schema-7 exclusive release requires the concrete filesystem store")
	}
	return nil
}

type schema7ReleaseStore interface {
	RunStateSchemaKind(context.Context, string) (store.RunSchemaKind, error)
}

type RunResult struct {
	RunID          string                `json:"runId"`
	Status         state.RunStatus       `json:"status,omitempty"`
	Waiting        string                `json:"waiting,omitempty"`
	Verification   *processverify.Report `json:"verification,omitempty"`
	LeaseContended bool                  `json:"leaseContended,omitempty"`
	Error          string                `json:"error,omitempty"`
}

func New(st store.Store, holder string, adapters map[model.PerformerKind]processexec.Adapter) *Host {
	limited := make(map[model.PerformerKind]processexec.Adapter, len(adapters))
	for kind, adapter := range adapters {
		// One slot per performer pool is deliberately conservative in v1. Runs
		// can still advance concurrently, but a burst of ready nodes cannot
		// accidentally spawn an unbounded number of actors or programs.
		base := &limitedAdapter{adapter: adapter, slots: make(chan struct{}, 1)}
		if deferred, ok := adapter.(processexec.DeferredAdapter); ok {
			wrapped := &limitedDeferredAdapter{limitedAdapter: base, deferred: deferred}
			if contact, contactOK := adapter.(processexec.ContactAdapter); contactOK {
				limited[kind] = &limitedDeferredContactAdapter{limitedDeferredAdapter: wrapped, contact: contact}
			} else {
				limited[kind] = wrapped
			}
			continue
		}
		limited[kind] = base
	}
	host := &Host{
		Store:             st,
		Executor:          processexec.New(st, limited),
		Holder:            holder,
		LeaseTTL:          DefaultLeaseTTL,
		MaxConcurrentRuns: DefaultMaxConcurrentRuns,
		Now:               time.Now,
		heartbeatTimer:    startHeartbeatTimer,
		epochAdapters:     limited,
	}
	if fs, ok := st.(*store.FS); ok {
		host.epochStore = fs
	}
	if release, ok := st.(schema7ReleaseStore); ok {
		host.releaseStore = release
	}
	return host
}

func startHeartbeatTimer(interval time.Duration) (<-chan time.Time, func()) {
	ticker := time.NewTicker(interval)
	return ticker.C, ticker.Stop
}

// SetHeartbeatTimerForTest controls heartbeat ticks without changing the
// production lease-renewal path. Install it before the host starts running.
func (h *Host) SetHeartbeatTimerForTest(start func(time.Duration) (<-chan time.Time, func())) func() {
	previous := h.heartbeatTimer
	h.heartbeatTimer = start
	return func() { h.heartbeatTimer = previous }
}

type limitedDeferredAdapter struct {
	*limitedAdapter
	deferred processexec.DeferredAdapter
}

func (a *limitedDeferredAdapter) Dispatch(ctx context.Context, request processexec.Request) (processexec.DispatchResult, error) {
	if err := a.acquire(ctx); err != nil {
		return processexec.DispatchResult{}, err
	}
	defer a.release()
	return a.deferred.Dispatch(ctx, request)
}

func (a *limitedDeferredAdapter) ReconcileDeferred(ctx context.Context, request processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	if err := a.acquire(ctx); err != nil {
		return processexec.Observation{}, processexec.DeferredMissing, err
	}
	defer a.release()
	return a.deferred.ReconcileDeferred(ctx, request)
}

type limitedDeferredContactAdapter struct {
	*limitedDeferredAdapter
	contact processexec.ContactAdapter
}

func (a *limitedDeferredContactAdapter) Contact(ctx context.Context, request processexec.Request, escalation bool) error {
	if err := a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	return a.contact.Contact(ctx, request, escalation)
}

func (a *limitedDeferredContactAdapter) Activity(ctx context.Context, request processexec.Request, since time.Time) (processexec.Activity, error) {
	if err := a.acquire(ctx); err != nil {
		return processexec.Activity{}, err
	}
	defer a.release()
	return a.contact.Activity(ctx, request, since)
}

type limitedAdapter struct {
	adapter processexec.Adapter
	slots   chan struct{}
}

func (a *limitedAdapter) Validate(request processexec.Request) error {
	return a.adapter.Validate(request)
}

func (a *limitedAdapter) Perform(ctx context.Context, request processexec.Request) (processexec.Observation, error) {
	if err := a.acquire(ctx); err != nil {
		return processexec.Observation{}, err
	}
	defer a.release()
	return a.adapter.Perform(ctx, request)
}

func (a *limitedAdapter) Reconcile(ctx context.Context, request processexec.Request) (processexec.Observation, bool, error) {
	reconciler, ok := a.adapter.(processexec.ReconcileAdapter)
	if !ok {
		return processexec.Observation{}, false, nil
	}
	if err := a.acquire(ctx); err != nil {
		return processexec.Observation{}, false, err
	}
	defer a.release()
	return reconciler.Reconcile(ctx, request)
}

func (a *limitedAdapter) acquire(ctx context.Context) error {
	select {
	case a.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *limitedAdapter) release() {
	<-a.slots
}

// Tick scans every run once. Per-run failures are returned as results and do
// not stop unrelated runs from advancing.
func (h *Host) Tick(ctx context.Context) ([]RunResult, error) {
	if h == nil || h.Store == nil || h.Executor == nil {
		return nil, fmt.Errorf("process engine store and executor are required")
	}
	if strings.TrimSpace(h.Holder) == "" {
		return nil, fmt.Errorf("process engine lease holder is required")
	}
	h.tickMu.Lock()
	defer h.tickMu.Unlock()
	h.Executor.Now = h.Now
	runs, err := h.Store.ListRuns(ctx)
	if err != nil {
		return nil, err
	}
	if len(runs) == 0 {
		return nil, nil
	}

	limit := h.MaxConcurrentRuns
	if limit <= 0 {
		limit = DefaultMaxConcurrentRuns
	}
	jobs := make(chan store.RunRecord)
	results := make(chan RunResult, len(runs))
	var wg sync.WaitGroup
	workers := min(limit, len(runs))
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for run := range jobs {
				results <- h.tickRun(ctx, run.ID)
			}
		}()
	}
	for _, run := range runs {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			close(results)
			return nil, ctx.Err()
		case jobs <- run:
		}
	}
	close(jobs)
	wg.Wait()
	close(results)

	out := make([]RunResult, 0, len(runs))
	for result := range results {
		out = append(out, result)
	}
	slices.SortFunc(out, func(a, b RunResult) int { return strings.Compare(a.RunID, b.RunID) })
	return out, nil
}

func (h *Host) tickRun(ctx context.Context, runID string) RunResult {
	result := RunResult{RunID: runID}
	if h.releaseStore != nil {
		kind, err := h.releaseStore.RunStateSchemaKind(ctx, runID)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		switch kind {
		case store.RunSchemaResetRequired:
			result.Error = fmt.Sprintf("%v: process run %q", store.ErrRunResetRequired, runID)
			return result
		case store.RunSchemaEpochV8:
			return h.tickEpochV8(ctx, runID)
		case store.RunSchemaLegacy:
		default:
			result.Error = fmt.Sprintf("process run %q has unsupported state schema", runID)
			return result
		}
	}
	checkpoint, err := h.Store.LoadRunState(ctx, runID)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Status = checkpoint.Status
	if isTerminal(checkpoint.Status) && !hasIssuedInternalCommand(checkpoint) {
		return result
	}
	ttl := h.LeaseTTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	if _, err := h.Store.AcquireRunLease(ctx, runID, h.Holder, ttl); err != nil {
		result.LeaseContended = errors.Is(err, store.ErrLeaseHeld)
		result.Error = err.Error()
		return result
	}
	runCtx, cancel, heartbeatErr, heartbeatDone := h.heartbeat(ctx, runID, ttl)
	defer func() {
		cancel()
		<-heartbeatDone
		_ = h.Store.ReleaseRunLease(context.WithoutCancel(ctx), runID, h.Holder)
	}()
	for round := 0; round < 1000; round++ {
		select {
		case err := <-heartbeatErr:
			result.Error = err.Error()
			return result
		default:
		}
		if err := runCtx.Err(); err != nil {
			result.Error = err.Error()
			return result
		}

		snapshot, err := h.Store.LoadRun(runCtx, runID)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.Status = snapshot.State.Status
		report := processverify.Snapshot(snapshot)
		if report.HasErrors() {
			result.Status = report.EffectiveStatus
			result.Verification = &report
			result.Waiting = firstDiagnostic(report)
			return result
		}

		progressed, waiting, err := h.resume(runCtx, snapshot)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		if waiting != "" {
			result.Waiting = waiting
			if latest, loadErr := h.Store.LoadRun(runCtx, runID); loadErr == nil {
				result.Status = latest.State.Status
				if latest.State.Pause != nil {
					result.Waiting = latest.State.Pause.Reason
				}
			}
			return result
		}
		if progressed {
			continue
		}

		if isTerminal(snapshot.State.Status) {
			return result
		}
		fired, err := h.fireDueTimers(runCtx, snapshot)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		if fired {
			continue
		}

		finished, err := h.Executor.Drive(runCtx, runID)
		if err != nil {
			var rateLimited *processexec.RateLimitError
			if errors.As(err, &rateLimited) {
				latest, loadErr := h.Store.LoadRun(runCtx, runID)
				if loadErr != nil {
					result.Error = loadErr.Error()
					return result
				}
				commandID := firstIssuedPerformer(latest.State)
				if commandID == "" {
					result.Error = err.Error()
					return result
				}
				reason := fmt.Sprintf("rate limited until %s", rateLimited.Until.UTC().Format(time.RFC3339))
				if pauseErr := h.pause(runCtx, latest, state.PauseState{Kind: state.PauseKindRateLimited, Reason: reason, CommandID: commandID, Until: rateLimited.Until.UTC()}); pauseErr != nil {
					result.Error = pauseErr.Error()
					return result
				}
				result.Status = state.RunStatusPaused
				result.Waiting = reason
				return result
			}
			result.Error = err.Error()
			return result
		}
		result.Status = finished.State.Status
		if finished.State.Pause != nil {
			result.Waiting = finished.State.Pause.Reason
		}
		return result
	}
	result.Error = fmt.Sprintf("process run %q exceeded engine tick rounds", runID)
	return result
}

func (h *Host) tickEpochV8(ctx context.Context, runID string) RunResult {
	result := RunResult{RunID: runID}
	if h.epochStore == nil {
		result.Error = "schema-8 execution requires the concrete filesystem store"
		return result
	}
	ttl := h.LeaseTTL
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}
	lease, err := h.epochStore.AcquireEngineLease(ctx, runID, h.Holder, ttl)
	if err != nil {
		result.LeaseContended = errors.Is(err, store.ErrLeaseHeld)
		result.Error = err.Error()
		return result
	}
	runCtx, cancel, heartbeatErr, heartbeatDone := h.epochHeartbeat(ctx, lease, ttl)
	defer func() {
		cancel()
		<-heartbeatDone
		_ = h.epochStore.ReleaseEngineLease(context.WithoutCancel(ctx), lease)
	}()
	if _, err := h.epochStore.EnsureEpochV8Runtime(runCtx, lease); err != nil {
		result.Error = err.Error()
		return result
	}
	executor := processexec.NewEpochV8(h.epochStore, lease, h.epochAdapters)
	executor.Now = h.Now
	checkpoint, err := executor.Drive(runCtx, runID)
	select {
	case heartbeatFailure := <-heartbeatErr:
		result.Error = heartbeatFailure.Error()
		return result
	default:
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	switch pathv1.CurrentRunStatus(checkpoint) {
	case "completed":
		result.Status = state.RunStatusCompleted
	case "failed":
		result.Status = state.RunStatusFailed
	case "canceled":
		result.Status = state.RunStatusCanceled
	default:
		result.Status = state.RunStatusRunning
		result.Waiting = "owner-epoch runtime is waiting"
	}
	return result
}

func (h *Host) epochHeartbeat(parent context.Context, lease store.EngineLease, ttl time.Duration) (context.Context, context.CancelFunc, <-chan error, <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	errs := make(chan error, 1)
	done := make(chan struct{})
	interval := ttl / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	go func() {
		defer close(done)
		startTimer := h.heartbeatTimer
		if startTimer == nil {
			startTimer = startHeartbeatTimer
		}
		ticks, stopTimer := startTimer(interval)
		defer stopTimer()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
				if _, err := h.epochStore.RenewEngineLease(ctx, lease, ttl); err != nil {
					select {
					case errs <- fmt.Errorf("process run %q lease heartbeat lost: %w", lease.RunID, err):
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel, errs, done
}

func exclusiveV7Eligible(tmpl *model.Template) bool {
	if tmpl == nil {
		return false
	}
	hasParallel := false
	for _, node := range tmpl.Nodes {
		if node.Type == model.NodeTypeParallel {
			hasParallel = true
		}
	}
	for nodeID, node := range tmpl.Nodes {
		if node.IsCompound() {
			return false
		}
		switch node.Type {
		case model.NodeTypeTask, model.NodeTypeDecision:
			if node.Performer == nil || node.Performer.Kind == model.PerformerProgram {
				return false
			}
			// A template whose eventual durable contact fields exceed the
			// schema-7 bounds must stay on v6: dispatch would otherwise create
			// external work whose contact schedule can never seal.
			if processexec.PreflightSchema7Contact(*node.Performer) != nil {
				return false
			}
			if node.Type == model.NodeTypeTask && len(node.Performer.ChoiceOutcomes) != 0 {
				return false
			}
		case model.NodeTypeEnd:
			if nodeID == tmpl.Start {
				return false
			}
			result := strings.ToLower(strings.TrimSpace(node.Result))
			switch result {
			case "", "pass", "passed", "success", "succeeded", "complete", "completed", "done", "ok",
				"fail", "failed", "failure", "error":
			default:
				return false
			}
		case model.NodeTypeWait:
			if !exclusiveV7WaitEligible(node.Wait) {
				return false
			}
		case model.NodeTypeStart:
		case model.NodeTypeParallel:
			if len(node.Next) < 2 {
				return false
			}
		default:
			return false
		}
	}
	if hasParallel && !parallelTerminalDPEEligible(tmpl) {
		return false
	}
	return true
}

// parallelTerminalDPEEligible rejects only the nested-fork topology that the
// released reducer cannot poison without inventing an activation/output pair:
// a fallible pass-only task between an outer fork and an unactivated nested
// fork. Directly nested forks and nested forks reached through explicit
// failure routing remain admitted.
func parallelTerminalDPEEligible(tmpl *model.Template) bool {
	for outerID, outer := range tmpl.Nodes {
		if outer.Type != model.NodeTypeParallel {
			continue
		}
		type visit struct {
			nodeID   string
			fallible bool
		}
		stack := make([]visit, 0, len(outer.Next))
		for _, nodeID := range outer.Next {
			stack = append(stack, visit{nodeID: nodeID})
		}
		seen := make(map[visit]struct{})
		for len(stack) > 0 {
			current := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if _, ok := seen[current]; ok {
				continue
			}
			seen[current] = struct{}{}
			node, ok := tmpl.Nodes[current.nodeID]
			if !ok {
				continue
			}
			if current.nodeID != outerID && node.Type == model.NodeTypeParallel && current.fallible {
				return false
			}
			fallible := current.fallible || (node.Type == model.NodeTypeTask && model.FailTarget(node.Next) == "")
			for _, nextID := range node.Next {
				stack = append(stack, visit{nodeID: nextID, fallible: fallible})
			}
		}
	}
	return true
}

func exclusiveV7WaitEligible(wait *model.WaitConfig) bool {
	if wait == nil {
		return false
	}
	duration := strings.TrimSpace(wait.Duration)
	until := strings.TrimSpace(wait.Until)
	signal := strings.TrimSpace(wait.Signal)
	configured := 0
	for _, value := range []string{duration, until, signal} {
		if value != "" {
			configured++
		}
	}
	if configured != 1 {
		return false
	}
	if signal != "" {
		return true
	}
	if until != "" {
		instant, err := model.ParseRFC3339(until)
		return err == nil && !instant.IsZero()
	}
	parsed, err := time.ParseDuration(duration)
	return err == nil && parsed > 0
}

func hasIssuedInternalCommand(st *state.State) bool {
	if st == nil {
		return false
	}
	for _, command := range st.OutstandingCommands {
		if command.Status == state.CommandStatusIssued && !isPerformerCommand(command.Kind) {
			return true
		}
	}
	return false
}

func (h *Host) resume(ctx context.Context, snapshot store.Snapshot) (bool, string, error) {
	st := snapshot.State
	if st.Status == state.RunStatusPaused {
		if st.Pause == nil {
			return false, "operator paused", nil
		}
		switch st.Pause.Kind {
		case state.PauseKindRateLimited:
			if h.now().Before(st.Pause.Until) {
				return false, st.Pause.Reason, nil
			}
			commandID := st.Pause.CommandID
			if command, ok := st.OutstandingCommands[commandID]; ok && command.Status == state.CommandStatusObserved {
				return true, "", h.unpause(ctx, snapshot)
			}
			if err := h.unpause(ctx, snapshot); err != nil {
				return false, "", err
			}
			_, err := h.Executor.RetryOutstanding(ctx, snapshot.Run.ID, commandID)
			if err != nil {
				var rateLimited *processexec.RateLimitError
				if errors.As(err, &rateLimited) {
					latest, loadErr := h.Store.LoadRun(ctx, snapshot.Run.ID)
					if loadErr != nil {
						return false, "", loadErr
					}
					reason := fmt.Sprintf("rate limited until %s", rateLimited.Until.UTC().Format(time.RFC3339))
					return false, reason, h.pause(ctx, latest, state.PauseState{Kind: state.PauseKindRateLimited, Reason: reason, CommandID: commandID, Until: rateLimited.Until.UTC()})
				}
				return false, "", err
			}
			return true, "", nil
		case state.PauseKindNeedsReconcile:
			command, ok := st.OutstandingCommands[st.Pause.CommandID]
			if ok && command.Status == state.CommandStatusObserved {
				return true, "", h.unpause(ctx, snapshot)
			}
			_, found, err := h.Executor.ReconcileOutstanding(ctx, snapshot.Run.ID, st.Pause.CommandID)
			if err != nil {
				return false, "", err
			}
			if !found {
				return false, st.Pause.Reason, nil
			}
			latest, err := h.Store.LoadRun(ctx, snapshot.Run.ID)
			if err != nil {
				return false, "", err
			}
			return true, "", h.unpause(ctx, latest)
		default:
			return false, st.Pause.Reason, nil
		}
	}

	for _, commandID := range sortedCommandIDs(st.OutstandingCommands) {
		command := st.OutstandingCommands[commandID]
		if command.Status != state.CommandStatusIssued {
			continue
		}
		if isPerformerCommand(command.Kind) {
			// Once a deferred adapter has recorded an external ref it is safe to
			// poll immediately; the reconcile grace remains for commands whose
			// dispatch crashed before the external ref append.
			if command.ExternalRef == "" && !command.ReconcileAfter.IsZero() && h.now().Before(command.ReconcileAfter) {
				return false, fmt.Sprintf("performer command %s is in flight", commandID), nil
			}
			_, reconcileStatus, err := h.Executor.ReconcileDeferredOutstanding(ctx, snapshot.Run.ID, commandID)
			if err != nil {
				return false, "", err
			}
			if reconcileStatus == processexec.DeferredObserved {
				return true, "", nil
			}
			if reconcileStatus == processexec.DeferredInFlight {
				latest, loadErr := h.Store.LoadRun(ctx, snapshot.Run.ID)
				if loadErr != nil {
					return false, "", loadErr
				}
				waiting, contactErr := h.serviceContact(ctx, latest, commandID)
				if contactErr != nil {
					return false, "", contactErr
				}
				latest, loadErr = h.Store.LoadRun(ctx, snapshot.Run.ID)
				if loadErr != nil {
					return false, "", loadErr
				}
				_, blockedErr := h.serviceBlockedContacts(ctx, latest)
				if blockedErr != nil {
					return false, "", blockedErr
				}
				return false, waiting, nil
			}
			reason := fmt.Sprintf("needs reconciliation: performer command %s has no discoverable observation", commandID)
			return false, reason, h.pause(ctx, snapshot, state.PauseState{Kind: state.PauseKindNeedsReconcile, Reason: reason, CommandID: commandID, Owner: "human:operator"})
		}
		if _, err := h.Executor.ResumeOutstanding(ctx, snapshot.Run.ID, commandID); err != nil {
			return false, "", err
		}
		return true, "", nil
	}
	_, err := h.serviceBlockedContacts(ctx, snapshot)
	return false, "", err
}

func (h *Host) serviceBlockedContacts(ctx context.Context, snapshot store.Snapshot) (store.Snapshot, error) {
	for _, commandID := range blockedContactCommandIDs(snapshot.State) {
		var err error
		_, err = h.serviceContact(ctx, snapshot, commandID)
		if err != nil {
			return snapshot, err
		}
		snapshot, err = h.Store.LoadRun(ctx, snapshot.Run.ID)
		if err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
}

func blockedContactCommandIDs(st *state.State) []string {
	if st == nil {
		return nil
	}
	var ids []string
	for commandID := range st.Contacts {
		command, ok := st.OutstandingCommands[commandID]
		if !ok || command.Kind != state.CommandKindBlockNode || command.Status != state.CommandStatusObserved {
			continue
		}
		node, ok := st.Nodes[command.NodeID]
		if !ok || node.Status != state.NodeStatusBlocked || node.BlockedAttempt != command.Attempt {
			continue
		}
		ids = append(ids, commandID)
	}
	slices.Sort(ids)
	return ids
}

func (h *Host) serviceContact(ctx context.Context, snapshot store.Snapshot, commandID string) (string, error) {
	contact, ok := snapshot.State.Contacts[commandID]
	if !ok {
		return fmt.Sprintf("performer command %s is in flight", commandID), nil
	}
	request, adapter, err := h.Executor.ContactRequest(ctx, snapshot.Run.ID, commandID)
	if err != nil {
		return "", err
	}
	contactAdapter, ok := adapter.(processexec.ContactAdapter)
	if !ok {
		return describeContact(contact), nil
	}
	now := h.now()
	contactSnapshot := legacyContactSnapshot(contact)
	activity, err := contactAdapter.Activity(ctx, request, contactSnapshot.ActivitySince())
	if err != nil {
		return "", err
	}
	decision := processexec.DecideContact(contactSnapshot, activity, now)
	changed := false
	if decision.Reset {
		contact.Used = 0
		contact.EscalatedAt = time.Time{}
		contact.LastRecoveredAt = decision.ResetAt
		clearHumanPreemption(&contact)
		if cadence, parseErr := time.ParseDuration(contact.Cadence); parseErr == nil {
			contact.NextContactAt = now.Add(cadence)
		}
		changed = true
	}
	if decision.ClearLatch {
		clearHumanPreemption(&contact)
		changed = true
	}
	if !decision.LatchAt.IsZero() {
		contact.HumanInteractedAt = decision.LatchAt
		changed = true
	}
	if decision.Pause {
		contact.Paused = true
		contact.PauseReason = processexec.ContactPauseReasonHumanPreemption
		changed = true
	}
	if changed {
		updated, updateErr := h.updateContact(ctx, snapshot, contact)
		if updateErr != nil {
			return "", updateErr
		}
		snapshot = updated
	}
	switch decision.Send {
	case processexec.ContactSendNudge:
		// The external nudge happens before its state append. A crash in that
		// narrow window can resend one duplicate; escalation is exactly-once
		// with respect to persisted ContactState, not the external transport.
		if err := contactAdapter.Contact(ctx, request, false); err != nil {
			return "", err
		}
		cadence, err := time.ParseDuration(contact.Cadence)
		if err != nil || cadence <= 0 {
			return "", fmt.Errorf("contact command %s has invalid cadence %q", commandID, contact.Cadence)
		}
		contact.Used++
		contact.LastContactedAt = now
		contact.NextContactAt = now.Add(cadence)
		if _, err := h.updateContact(ctx, snapshot, contact); err != nil {
			return "", err
		}
	case processexec.ContactSendEscalate:
		// Same accepted crash window as nudges above: one duplicate external
		// escalation is possible before EscalatedAt becomes durable.
		if err := contactAdapter.Contact(ctx, request, true); err != nil {
			return "", err
		}
		contact.EscalatedAt = now
		contact.NextContactAt = time.Time{}
		if _, err := h.updateContact(ctx, snapshot, contact); err != nil {
			return "", err
		}
	}
	return describeContact(contact), nil
}

// legacyContactSnapshot projects durable v6 contact state into the shared
// decision-core shape. An unparseable cadence maps to zero, which the core
// treats as "leave the schedule untouched on reset" — the send path still
// surfaces the typed cadence error exactly as before.
func legacyContactSnapshot(contact state.ContactState) processexec.ContactSnapshot {
	cadence, err := time.ParseDuration(contact.Cadence)
	if err != nil || cadence <= 0 {
		cadence = 0
	}
	return processexec.ContactSnapshot{
		Cadence: cadence, Budget: contact.Budget, Used: contact.Used,
		Paused: contact.Paused, PauseReason: contact.PauseReason,
		NextContactAt: contact.NextContactAt, LastContactedAt: contact.LastContactedAt,
		LastRecoveredAt: contact.LastRecoveredAt, EscalatedAt: contact.EscalatedAt,
		HumanInteractedAt: contact.HumanInteractedAt,
	}
}

func clearHumanPreemption(contact *state.ContactState) {
	contact.HumanInteractedAt = time.Time{}
	if contact.PauseReason == processexec.ContactPauseReasonHumanPreemption {
		contact.Paused = false
		contact.PauseReason = ""
	}
}

func (h *Host) updateContact(ctx context.Context, snapshot store.Snapshot, contact state.ContactState) (store.Snapshot, error) {
	at := h.now()
	entry := evidence.LogEntry{
		SchemaVersion: evidence.LogEntrySchemaVersion,
		At:            at,
		Scope:         evidence.Scope{Kind: evidence.ScopeNode, ID: snapshot.State.OutstandingCommands[contact.CommandID].NodeID},
		Kind:          evidence.EntryKindGate,
		Event:         &state.Event{Type: state.EventContactUpdated, At: at, NodeID: snapshot.State.OutstandingCommands[contact.CommandID].NodeID, Contact: &contact},
	}
	appended, err := h.Store.Append(ctx, snapshot.Run.ID, snapshot.State.LastLogSeq, []evidence.LogEntry{entry})
	if err != nil {
		return store.Snapshot{}, err
	}
	snapshot.State = appended.State
	return snapshot, nil
}

func describeContact(contact state.ContactState) string {
	if contact.Paused {
		return fmt.Sprintf("waiting on %s; automation paused: %s", contact.Assignee, contact.PauseReason)
	}
	if !contact.EscalatedAt.IsZero() {
		return fmt.Sprintf("waiting on %s; nudges exhausted %d/%d; escalated to %s", contact.Assignee, contact.Used, contact.Budget, contact.EscalationTarget)
	}
	return fmt.Sprintf("waiting on %s; next nudge at %s; %d/%d nudges sent", contact.Assignee, contact.NextContactAt.UTC().Format(time.RFC3339), contact.Used, contact.Budget)
}

func (h *Host) fireDueTimers(ctx context.Context, snapshot store.Snapshot) (bool, error) {
	var entries []evidence.LogEntry
	at := h.now().UTC()
	for _, timerID := range sortedTimerIDs(snapshot.State.Timers) {
		timer := snapshot.State.Timers[timerID]
		if timer.Status != state.WaitStatusPending || timer.DueAt.After(at) {
			continue
		}
		entries = append(entries, evidence.LogEntry{
			SchemaVersion: evidence.LogEntrySchemaVersion,
			At:            at,
			Scope:         evidence.Scope{Kind: evidence.ScopeNode, ID: timer.NodeID},
			Kind:          evidence.EntryKindSignal,
			Event:         &state.Event{Type: state.EventTimerSatisfied, At: at, NodeID: timer.NodeID, TimerID: timerID},
		})
	}
	if len(entries) == 0 {
		return false, nil
	}
	_, err := h.Store.Append(ctx, snapshot.Run.ID, snapshot.State.LastLogSeq, entries)
	return err == nil, err
}

func (h *Host) pause(ctx context.Context, snapshot store.Snapshot, pause state.PauseState) error {
	at := h.now().UTC()
	_, err := h.Store.Append(ctx, snapshot.Run.ID, snapshot.State.LastLogSeq, []evidence.LogEntry{{
		SchemaVersion: evidence.LogEntrySchemaVersion,
		At:            at,
		Scope:         evidence.Scope{Kind: evidence.ScopeRun},
		Kind:          evidence.KindForEvent(state.EventRunPaused),
		Event:         &state.Event{Type: state.EventRunPaused, At: at, Pause: &pause},
	}})
	return err
}

func (h *Host) unpause(ctx context.Context, snapshot store.Snapshot) error {
	at := h.now().UTC()
	_, err := h.Store.Append(ctx, snapshot.Run.ID, snapshot.State.LastLogSeq, []evidence.LogEntry{{
		SchemaVersion: evidence.LogEntrySchemaVersion,
		At:            at,
		Scope:         evidence.Scope{Kind: evidence.ScopeRun},
		Kind:          evidence.KindForEvent(state.EventRunResumed),
		Event:         &state.Event{Type: state.EventRunResumed, At: at},
	}})
	return err
}

func (h *Host) heartbeat(parent context.Context, runID string, ttl time.Duration) (context.Context, context.CancelFunc, <-chan error, <-chan struct{}) {
	ctx, cancel := context.WithCancel(parent)
	errs := make(chan error, 1)
	done := make(chan struct{})
	interval := ttl / 3
	if interval <= 0 {
		interval = time.Millisecond
	}
	go func() {
		defer close(done)
		startTimer := h.heartbeatTimer
		if startTimer == nil {
			startTimer = startHeartbeatTimer
		}
		ticks, stopTimer := startTimer(interval)
		defer stopTimer()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticks:
				if _, err := h.Store.AcquireRunLease(ctx, runID, h.Holder, ttl); err != nil {
					select {
					case errs <- fmt.Errorf("process run %q lease heartbeat lost: %w", runID, err):
					default:
					}
					cancel()
					return
				}
			}
		}
	}()
	return ctx, cancel, errs, done
}

func (h *Host) now() time.Time {
	if h.Now == nil {
		return time.Now().UTC()
	}
	return h.Now().UTC()
}

func firstDiagnostic(report processverify.Report) string {
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Severity == model.SeverityError {
			return fmt.Sprintf("%s at %s: %s", diagnostic.Code, diagnostic.Path, diagnostic.Message)
		}
	}
	return "verification failed"
}

func firstIssuedPerformer(st *state.State) string {
	for _, id := range sortedCommandIDs(st.OutstandingCommands) {
		command := st.OutstandingCommands[id]
		if command.Status == state.CommandStatusIssued && isPerformerCommand(command.Kind) {
			return id
		}
	}
	return ""
}

func sortedCommandIDs(commands map[string]state.OutstandingCommand) []string {
	ids := make([]string, 0, len(commands))
	for id := range commands {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func sortedTimerIDs(timers map[string]state.TimerRecord) []string {
	ids := make([]string, 0, len(timers))
	for id := range timers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func isPerformerCommand(kind state.CommandKind) bool {
	return kind == plan.CommandKindStartAttempt || kind == plan.CommandKindRecordDecision
}

func isTerminal(status state.RunStatus) bool {
	switch status {
	case state.RunStatusCompleted, state.RunStatusFailed, state.RunStatusCanceled:
		return true
	default:
		return false
	}
}
