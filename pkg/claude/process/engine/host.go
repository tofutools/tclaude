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
	Holder            string
	LeaseTTL          time.Duration
	MaxConcurrentRuns int
	Now               func() time.Time
	tickMu            sync.Mutex
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
		limited[kind] = &limitedAdapter{adapter: adapter, slots: make(chan struct{}, 1)}
	}
	return &Host{
		Store:             st,
		Executor:          processexec.New(st, limited),
		Holder:            holder,
		LeaseTTL:          DefaultLeaseTTL,
		MaxConcurrentRuns: DefaultMaxConcurrentRuns,
		Now:               time.Now,
	}
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
	checkpoint, err := h.Store.LoadRunState(ctx, runID)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.Status = checkpoint.Status
	if isTerminal(checkpoint.Status) {
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
			if !command.ReconcileAfter.IsZero() && h.now().Before(command.ReconcileAfter) {
				return false, fmt.Sprintf("performer command %s is in flight", commandID), nil
			}
			_, found, err := h.Executor.ReconcileOutstanding(ctx, snapshot.Run.ID, commandID)
			if err != nil {
				return false, "", err
			}
			if found {
				return true, "", nil
			}
			reason := fmt.Sprintf("needs reconciliation: performer command %s has no discoverable observation", commandID)
			return false, reason, h.pause(ctx, snapshot, state.PauseState{Kind: state.PauseKindNeedsReconcile, Reason: reason, CommandID: commandID, Owner: "human:operator"})
		}
		if _, err := h.Executor.ResumeOutstanding(ctx, snapshot.Run.ID, commandID); err != nil {
			return false, "", err
		}
		return true, "", nil
	}
	return false, "", nil
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
		Kind:          evidence.EntryKindGate,
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
		Kind:          evidence.EntryKindGate,
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
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
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
