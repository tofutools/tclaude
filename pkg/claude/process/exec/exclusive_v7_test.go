package processexec

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processview "github.com/tofutools/tclaude/pkg/claude/process/view"
)

type exclusiveV7Adapter struct {
	mu        sync.Mutex
	performs  int
	perform   func(Request)
	observe   func(Request) Observation
	reconcile func(Request) (Observation, bool, error)
	results   []Observation
}

type exclusiveV7DeferredAdapter struct {
	mu             sync.Mutex
	dispatches     int
	reconcileCalls int
}

type exclusiveV7ControlledDeferredAdapter struct {
	mu             sync.Mutex
	dispatches     int
	reconcileCalls int
	observed       bool
}

func (a *exclusiveV7Adapter) Validate(Request) error { return nil }

func (a *exclusiveV7Adapter) Perform(_ context.Context, request Request) (Observation, error) {
	a.mu.Lock()
	a.performs++
	index := a.performs - 1
	perform := a.perform
	observe := a.observe
	var observation Observation
	if index < len(a.results) {
		observation = a.results[index]
	}
	a.mu.Unlock()
	if perform != nil {
		perform(request)
	}
	if observe != nil {
		return observe(request), nil
	}
	if observation.Actor != "" {
		return observation, nil
	}
	return Observation{Actor: "agent:agt_test1", Verdict: "pass", EvidenceRef: "artifact:exact"}, nil
}

func (a *exclusiveV7Adapter) Reconcile(_ context.Context, request Request) (Observation, bool, error) {
	if a.reconcile != nil {
		return a.reconcile(request)
	}
	return Observation{}, false, nil
}

func (a *exclusiveV7Adapter) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.performs
}

func (a *exclusiveV7DeferredAdapter) Validate(Request) error { return nil }

func (a *exclusiveV7DeferredAdapter) Perform(context.Context, Request) (Observation, error) {
	panic("Perform should not be called on a deferred adapter")
}

func (a *exclusiveV7DeferredAdapter) Dispatch(_ context.Context, request Request) (DispatchResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dispatches++
	return DispatchResult{ExternalRef: fmt.Sprintf("dispatch-%s-%d", request.Command.ID, a.dispatches)}, nil
}

func (a *exclusiveV7DeferredAdapter) ReconcileDeferred(_ context.Context, request Request) (Observation, DeferredStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reconcileCalls++
	switch a.reconcileCalls {
	case 1:
		return Observation{}, DeferredMissing, nil
	default:
		return Observation{Actor: "agent:agt_test1", Verdict: "pass", EvidenceRef: "artifact:found"}, DeferredObserved, nil
	}
}

func (a *exclusiveV7DeferredAdapter) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dispatches, a.reconcileCalls
}

func (a *exclusiveV7ControlledDeferredAdapter) Validate(Request) error { return nil }

func (a *exclusiveV7ControlledDeferredAdapter) Perform(context.Context, Request) (Observation, error) {
	panic("Perform should not be called on a deferred adapter")
}

func (a *exclusiveV7ControlledDeferredAdapter) Dispatch(_ context.Context, request Request) (DispatchResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dispatches++
	return DispatchResult{ExternalRef: fmt.Sprintf("dispatch-%s-%d", request.Command.ID, a.dispatches)}, nil
}

func (a *exclusiveV7ControlledDeferredAdapter) ReconcileDeferred(_ context.Context, _ Request) (Observation, DeferredStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reconcileCalls++
	if !a.observed {
		return Observation{}, DeferredInFlight, nil
	}
	return Observation{Actor: "agent:agt_test1", Verdict: "pass", EvidenceRef: "artifact:controlled"}, DeferredObserved, nil
}

func (a *exclusiveV7ControlledDeferredAdapter) setObserved() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.observed = true
}

func (a *exclusiveV7ControlledDeferredAdapter) counts() (int, int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dispatches, a.reconcileCalls
}

func TestExclusiveV7DriveClaimsPerformsObservesRoutesAndCompletes(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	adapter := &exclusiveV7Adapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 1, adapter.count())
	assert.GreaterOrEqual(t, pathv1.CheckpointRevision(checkpoint), uint64(5))

	snapshot, err := fs.LoadPathV1RunView(t.Context(), runID)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(snapshot.Checkpoint)
	require.NoError(t, err)
	completion, err := pathv1.AssessAggregateCompletion(aggregate.View())
	require.NoError(t, err)
	assert.Equal(t, "completed", completion.Result)
	for _, command := range aggregate.Commands {
		assert.False(t, command.State.Active(), "terminal checkpoint retained active command %q", command.ID)
	}
}

func TestExclusiveV7NoFailTargetRetriesNonPassThenFails(t *testing.T) {
	fs, runID := exclusiveV7NoFailRun(t, &model.RetryPolicy{MaxAttempts: 2})
	adapter := &exclusiveV7Adapter{results: []Observation{
		{Actor: "agent:agt_test1", Verdict: "needs-work", EvidenceRef: "artifact:first"},
		{Actor: "agent:agt_test1", Verdict: "needs-work", EvidenceRef: "artifact:second"},
	}}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "failed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 2, adapter.count())
	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	require.NoError(t, err)
	completion, err := pathv1.AssessAggregateCompletion(aggregate.View())
	require.NoError(t, err)
	assert.Equal(t, "failed", completion.Result)
	failedPaths := 0
	for _, path := range aggregate.Routing.Paths {
		if path.Kind != pathv1.PathActivationOutput || path.State != pathv1.PathFailed {
			continue
		}
		failedPaths++
		require.NotNil(t, path.Disposition)
		assert.Equal(t, "performer_failed", path.Disposition.ReasonCode)
		cause := aggregate.Routing.CauseRecords[path.TerminalCauseID]
		assert.Equal(t, pathv1.TerminalFailed, cause.TerminalKind)
		assert.Equal(t, "performer_failed", cause.DispositionReason)
	}
	assert.Equal(t, 1, failedPaths)
}

func TestExclusiveV7DriveParallelAllClaimsEveryBranchAndCompletes(t *testing.T) {
	fs, runID := parallelAllV7Run(t)
	adapter := &exclusiveV7Adapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 2, adapter.count())
	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	require.NoError(t, err)
	reducing := 0
	for _, reservation := range aggregate.Routing.Reservations {
		if reservation.JoinPolicy == pathv1.JoinAll && reservation.IsReducing {
			reducing++
			assert.Equal(t, pathv1.ReservationActivated, reservation.State)
		}
	}
	assert.Equal(t, 1, reducing)
	snapshot, err := fs.LoadPathV1RunView(t.Context(), runID)
	require.NoError(t, err)
	viewer, err := processview.BuildCurrentPathV1Envelope(t.Context(), snapshot)
	require.NoError(t, err)
	require.True(t, viewer.ViewerV2.RoutingAvailable, "viewer reason=%s", viewer.ViewerV2.RoutingUnavailableReason)
	require.NotNil(t, viewer.ViewerV2.Routing)
	assert.Len(t, viewer.ViewerV2.Routing.Joins, 1)
	assert.GreaterOrEqual(t, len(viewer.ViewerV2.Routing.Scopes), 2)
	assert.Equal(t, len(aggregate.Routing.Paths), viewer.ViewerV2.Routing.Aggregate.Paths)
}

func TestExclusiveV7DriveParallelAllTerminalMixturePoisonsJoin(t *testing.T) {
	fs, runID := parallelAllV7Run(t)
	adapter := &exclusiveV7Adapter{results: []Observation{
		{Actor: "agent:agt_test1", Verdict: "fail", EvidenceRef: "artifact:failed-branch"},
		{Actor: "agent:agt_test1", Verdict: "pass", EvidenceRef: "artifact:passed-branch"},
	}}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "failed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 2, adapter.count())
	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	require.NoError(t, err)
	for _, reservation := range aggregate.Routing.Reservations {
		if reservation.JoinPolicy == pathv1.JoinAll && reservation.IsReducing {
			assert.Equal(t, pathv1.ReservationClosedNoActivation, reservation.State)
			assert.Equal(t, string(pathv1.ScopeCloseCandidateNonSuccess), reservation.ClosedReason)
		}
	}
}

func TestExclusiveV7DriveParallelAnyRestartsWithDetachedLoserInFlight(t *testing.T) {
	root := t.TempDir()
	fs, runID := parallelAnyV7RunAt(t, root)
	adapter := &exclusiveV7ControlledDeferredAdapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "running", pathv1.CurrentRunStatus(checkpoint))
	dispatches, reconciles := adapter.counts()
	assert.Equal(t, 1, dispatches)
	assert.Equal(t, 0, reconciles)
	snapshot, err := fs.LoadPathV1RunView(t.Context(), runID)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(snapshot.Checkpoint)
	require.NoError(t, err)
	assertParallelAnyActivated(t, aggregate)
	ended, live := 0, 0
	for _, path := range aggregate.Routing.Paths {
		if path.Kind != pathv1.PathActivationOutput {
			continue
		}
		switch path.State {
		case pathv1.PathEnded:
			ended++
		case pathv1.PathLive:
			live++
		}
	}
	assert.Equal(t, 1, ended, "unrelated live work must not block the winner end")
	assert.Equal(t, 1, live, "detached loser must remain owned and runnable")
	_, completionErr := pathv1.AssessAggregateCompletion(aggregate.View())
	assert.ErrorIs(t, completionErr, pathv1.ErrAggregateUnsettled, "owned detached work must block aggregate completion")

	restarted, err := store.NewFS(root)
	require.NoError(t, err)
	adapter.setObserved()
	restartedExecutor := NewExclusiveV7(restarted, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	checkpoint, err = restartedExecutor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	dispatches, reconciles = adapter.counts()
	assert.Equal(t, 1, dispatches, "restart must not redispatch the detached loser")
	assert.Equal(t, 1, reconciles)
	aggregate, err = pathv1.CurrentAggregateCheckpoint(checkpoint)
	require.NoError(t, err)
	sinks := 0
	for _, path := range aggregate.Routing.Paths {
		if path.State == pathv1.PathDetachedSink && path.DetachedSink != nil {
			sinks++
		}
	}
	assert.Equal(t, 1, sinks)
}

func TestExclusiveV7DriveParallelAnyAllCandidatesFailClosesWithoutActivation(t *testing.T) {
	fs, runID := parallelAnyFailureV7Run(t)
	adapter := &exclusiveV7Adapter{results: []Observation{
		{Actor: "agent:agt_test1", Verdict: "fail", EvidenceRef: "artifact:first-failure"},
		{Actor: "agent:agt_test1", Verdict: "fail", EvidenceRef: "artifact:second-failure"},
	}}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "failed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 2, adapter.count())
	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	require.NoError(t, err)
	found := false
	for _, reservation := range aggregate.Routing.Reservations {
		if reservation.JoinPolicy != pathv1.JoinAny || !reservation.IsReducing {
			continue
		}
		found = true
		assert.Equal(t, pathv1.ReservationClosedNoActivation, reservation.State)
		assert.Nil(t, reservation.Activation)
		assert.Equal(t, string(pathv1.ScopeCloseCandidateNonSuccess), reservation.ClosedReason)
	}
	assert.True(t, found)
}

func TestParallelAnyExactCASHasOneWinnerEvent(t *testing.T) {
	fs, runID := parallelDirectAnyV7Run(t)
	appendTransition := func(plan func(store.PathV1ExecutionView) (*pathv1.ExecutionTransition, error)) {
		t.Helper()
		var transition *pathv1.ExecutionTransition
		require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
			var err error
			transition, err = plan(view)
			return err
		}))
		_, err := fs.AppendPathV1(t.Context(), runID, transition)
		require.NoError(t, err)
	}
	appendTransition(func(view store.PathV1ExecutionView) (*pathv1.ExecutionTransition, error) {
		aggregate, err := pathv1.CurrentAggregateCheckpoint(view.Checkpoint)
		if err != nil {
			return nil, err
		}
		for _, path := range aggregate.Routing.Paths {
			if path.Kind == pathv1.PathActivationOutput && path.State == pathv1.PathLive {
				return pathv1.AdvanceParallelSplit(t.Context(), view.Input, path.ID)
			}
		}
		return nil, errors.New("parallel root output not found")
	})
	var transition *pathv1.ExecutionTransition
	require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
		var err error
		transition, err = pathv1.AdvanceParallelAny(t.Context(), view.Input)
		return err
	}))

	start := make(chan struct{})
	results := make(chan store.PathV1AppendDisposition, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, appendErr := fs.AppendPathV1(t.Context(), runID, transition)
			errs <- appendErr
			results <- result.Disposition
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	for appendErr := range errs {
		require.NoError(t, appendErr)
	}
	counts := map[store.PathV1AppendDisposition]int{}
	for disposition := range results {
		counts[disposition]++
	}
	assert.Equal(t, 1, counts[store.PathV1AppendApplied])
	assert.Equal(t, 1, counts[store.PathV1AppendAlreadyApplied])
	snapshot, err := fs.LoadPathV1RunView(t.Context(), runID)
	require.NoError(t, err)
	aggregate, err := pathv1.CurrentAggregateCheckpoint(snapshot.Checkpoint)
	require.NoError(t, err)
	assertParallelAnyActivated(t, aggregate)
}

func TestExclusiveV7ParallelFailurePropagatesThroughUnmaterializedBranchNode(t *testing.T) {
	fs, runID := parallelAllV7RunWithIntermediate(t)
	adapter := &exclusiveV7Adapter{observe: func(request Request) Observation {
		if request.Command.NodeID == "left" {
			return Observation{Actor: "agent:agt_test1", Verdict: "fail", EvidenceRef: "artifact:failed-branch"}
		}
		return Observation{Actor: "agent:agt_test1", Verdict: "pass", EvidenceRef: "artifact:passed-branch"}
	}}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "failed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 2, adapter.count(), "poisoned intermediate task must never be performed")
	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	require.NoError(t, err)
	foundIntermediate := false
	for _, reservation := range aggregate.Routing.Reservations {
		if reservation.NodeID == "left-next" {
			foundIntermediate = true
			assert.Equal(t, pathv1.ReservationClosedNoActivation, reservation.State)
		}
	}
	assert.True(t, foundIntermediate, "poison propagation must materialize the intermediate reservation")
}

func TestExclusiveV7RecoveredParallelWaitDoesNotStarveRunnableSibling(t *testing.T) {
	fs, runID := parallelAllV7WaitRun(t)
	claimParallelWaitForTest(t, fs, runID)

	adapter := &exclusiveV7Adapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.count(), "runnable sibling must execute before the recovered wait fallback")
	assert.Equal(t, "running", pathv1.CurrentRunStatus(checkpoint))
}

func TestExclusiveV7ParallelPassOnlyTaskRetriesThenCompletes(t *testing.T) {
	fs, runID := parallelAllV7RetryRun(t)
	var performed []string
	adapter := &exclusiveV7Adapter{}
	adapter.perform = func(request Request) {
		performed = append(performed, fmt.Sprintf("%s/%d", request.Command.NodeID, request.Command.Attempt))
	}
	adapter.observe = func(request Request) Observation {
		verdict := "pass"
		if request.Command.NodeID == "work" && request.Command.Attempt == 1 {
			verdict = "fail"
		}
		return Observation{Actor: "agent:agt_test1", Verdict: verdict, EvidenceRef: "artifact:" + request.Command.NodeID}
	}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	slices.Sort(performed)
	assert.Equal(t, []string{"peer/1", "work/1", "work/2"}, performed)
}

func TestExclusiveV7ParallelPendingRoutesRestartOneAtATime(t *testing.T) {
	root := t.TempDir()
	fs, runID := parallelAllV7WaitRunAt(t, root)
	claimParallelWaitForTest(t, fs, runID)
	adapter := &exclusiveV7ControlledDeferredAdapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "running", pathv1.CurrentRunStatus(checkpoint))
	dispatches, reconciles := adapter.counts()
	assert.Equal(t, 1, dispatches)
	assert.Equal(t, 0, reconciles)

	_, err = executor.SatisfySignal(t.Context(), runID, "wait", "release", "agent:agt_test1")
	require.NoError(t, err)
	var live []pathv1.PathID
	require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
		aggregate, aggregateErr := pathv1.CurrentAggregateCheckpoint(view.Checkpoint)
		if aggregateErr != nil {
			return aggregateErr
		}
		for _, path := range aggregate.Routing.Paths {
			if path.Kind == pathv1.PathActivationOutput && path.State == pathv1.PathLive {
				live = append(live, path.ID)
			}
		}
		return nil
	}))
	slices.Sort(live)
	require.Len(t, live, 2)

	adapter.setObserved()
	injected := errors.New("restart after first parallel route")
	appends := 0
	restore := fs.SetPathV1AppendHooksForTest(nil, func() error {
		appends++
		if appends == 2 {
			return injected
		}
		return nil
	})
	_, err = executor.Drive(t.Context(), runID)
	restore()
	assert.ErrorIs(t, err, injected)
	assert.Equal(t, 2, appends, "task observation and exactly one route must commit before restart")

	restarted, err := store.NewFS(root)
	require.NoError(t, err)
	require.NoError(t, restarted.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
		aggregate, aggregateErr := pathv1.CurrentAggregateCheckpoint(view.Checkpoint)
		if aggregateErr != nil {
			return aggregateErr
		}
		assert.Equal(t, pathv1.PathRouted, aggregate.Routing.Paths[live[0]].State)
		pending, found, pendingErr := pathv1.PendingExclusiveObservation(t.Context(), view.Input)
		if pendingErr != nil {
			return pendingErr
		}
		assert.True(t, found)
		assert.Equal(t, live[1], pending.SourcePathID)
		return nil
	}))

	restartedExecutor := NewExclusiveV7(restarted, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	checkpoint, err = restartedExecutor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	dispatches, reconciles = adapter.counts()
	assert.Equal(t, 1, dispatches, "restart must not redispatch the observed task")
	assert.Equal(t, 1, reconciles, "task must be reconciled exactly once")
}

func TestExclusiveV7ParallelSiblingSignalsClaimRestartAndCompleteInEitherOrder(t *testing.T) {
	orders := []struct {
		name  string
		nodes []string
	}{
		{name: "forward", nodes: []string{"wait-a", "wait-b"}},
		{name: "reverse", nodes: []string{"wait-b", "wait-a"}},
	}
	for _, test := range orders {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			fs, runID := parallelAllV7SiblingWaitRunAt(t, root,
				&model.WaitConfig{Signal: "release-a"}, &model.WaitConfig{Signal: "release-b"})
			executor := NewExclusiveV7(fs, nil)
			checkpoint, err := executor.Drive(t.Context(), runID)
			require.NoError(t, err)
			assert.Equal(t, "running", pathv1.CurrentRunStatus(checkpoint))

			var claimedIDs []string
			require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
				waits, waitErr := pathv1.RecoverExclusiveWaits(t.Context(), view.Input)
				if waitErr != nil {
					return waitErr
				}
				for _, wait := range waits {
					claimedIDs = append(claimedIDs, wait.Command().ID)
				}
				return nil
			}))
			require.Len(t, claimedIDs, 2)
			slices.Sort(claimedIDs)

			restarted, err := store.NewFS(root)
			require.NoError(t, err)
			require.NoError(t, restarted.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
				waits, waitErr := pathv1.RecoverExclusiveWaits(t.Context(), view.Input)
				if waitErr != nil {
					return waitErr
				}
				restartedIDs := make([]string, 0, len(waits))
				for _, wait := range waits {
					restartedIDs = append(restartedIDs, wait.Command().ID)
				}
				slices.Sort(restartedIDs)
				assert.Equal(t, claimedIDs, restartedIDs, "restart must preserve both exact claims without duplicates")
				return nil
			}))

			restartedExecutor := NewExclusiveV7(restarted, nil)
			for _, nodeID := range test.nodes {
				signal := "release-a"
				if nodeID == "wait-b" {
					signal = "release-b"
				}
				_, err = restartedExecutor.SatisfySignal(t.Context(), runID, nodeID, signal, "agent:agt_test1")
				require.NoError(t, err)
			}
			checkpoint, err = restartedExecutor.Drive(t.Context(), runID)
			require.NoError(t, err)
			assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
		})
	}
}

func TestExclusiveV7ParallelSiblingDurationsScheduleTogetherAndComplete(t *testing.T) {
	root := t.TempDir()
	fs, runID := parallelAllV7SiblingWaitRunAt(t, root,
		&model.WaitConfig{Duration: "5m"}, &model.WaitConfig{Duration: "7m"})
	scheduledAt := time.Date(2026, time.July, 16, 8, 0, 0, 0, time.UTC)
	executor := NewExclusiveV7(fs, nil)
	executor.Now = func() time.Time { return scheduledAt }
	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "running", pathv1.CurrentRunStatus(checkpoint))

	require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
		waits, waitErr := pathv1.RecoverExclusiveWaits(t.Context(), view.Input)
		if waitErr != nil {
			return waitErr
		}
		require.Len(t, waits, 2)
		due := []time.Time{waits[0].DueAt(), waits[1].DueAt()}
		slices.SortFunc(due, func(a, b time.Time) int { return a.Compare(b) })
		assert.Equal(t, []time.Time{scheduledAt.Add(5 * time.Minute), scheduledAt.Add(7 * time.Minute)}, due)
		return nil
	}))

	restarted, err := store.NewFS(root)
	require.NoError(t, err)
	restartedExecutor := NewExclusiveV7(restarted, nil)
	restartedExecutor.Now = func() time.Time { return scheduledAt.Add(10 * time.Minute) }
	checkpoint, err = restartedExecutor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
}

func TestExclusiveV7AdapterRunsAfterCoherentLocksRelease(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	adapter := &exclusiveV7Adapter{}
	adapter.perform = func(Request) {
		require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(store.PathV1ExecutionView) error { return nil }))
	}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	_, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, 1, adapter.count())
}

func TestExclusiveV7RetryUsesNextAttemptWithoutReperformingClaim(t *testing.T) {
	fs, runID := exclusiveV7RunWithRetry(t)
	adapter := &exclusiveV7Adapter{results: []Observation{
		{Actor: "agent:agt_test1", Verdict: "fail", EvidenceRef: "artifact:first"},
		{Actor: "agent:agt_test1", Verdict: "pass", EvidenceRef: "artifact:second"},
	}}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 2, adapter.count())
}

func TestExclusiveV7ExhaustedTaskFailureCompletesFailed(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	adapter := &exclusiveV7Adapter{results: []Observation{{
		Actor: "agent:agt_test1", Verdict: "fail", EvidenceRef: "artifact:failed",
	}}}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "failed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 1, adapter.count())
	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	require.NoError(t, err)
	completion, err := pathv1.AssessAggregateCompletion(aggregate.View())
	require.NoError(t, err)
	assert.Equal(t, "failed", completion.Result)
}

func TestExclusiveV7TaskAliasNormalizesBeforeSettlement(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	executor := NewExclusiveV7(fs, nil)
	commandID := claimExclusiveAttemptForTest(t, fs, runID, 1)
	observation := Observation{Actor: "human:operator", Verdict: "ask-changes", EvidenceRef: "artifact:changes"}
	_, err := executor.RecordObservation(t.Context(), runID, "work", commandID, observation)
	require.NoError(t, err)
	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "failed", pathv1.CurrentRunStatus(checkpoint), "ask-changes must normalize to fail, never the pass fallback")
}

func TestExclusiveV7CompositeEventSequencesAdvanceAcrossDecisionTaskChain(t *testing.T) {
	fs, runID := exclusiveV7DecisionTaskRun(t)
	adapter := &exclusiveV7Adapter{results: []Observation{
		{Actor: "human:operator", Verdict: "ship", EvidenceRef: "decision:ship"},
		{Actor: "agent:agt_test1", Verdict: "pass", EvidenceRef: "artifact:work"},
	}}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{
		model.PerformerHuman: adapter,
		model.PerformerAgent: adapter,
	})
	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 2, adapter.count())

	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	require.NoError(t, err)
	var workOutput pathv1.PathRecord
	for _, activation := range aggregate.Routing.Activations {
		reservation := aggregate.Routing.Reservations[activation.ReservationID]
		if reservation.NodeID == "work" {
			workOutput = aggregate.Routing.Paths[activation.OutputPathID]
			break
		}
	}
	require.NotEmpty(t, workOutput.ID)
	assert.Greater(t, workOutput.UpdatedSeq, workOutput.CreatedSeq, "later task routing reused its composite creation sequence")
	assert.Greater(t, pathv1.CurrentLastLogSeq(checkpoint), uint64(workOutput.UpdatedSeq), "completion basis did not advance beyond routing")
	assert.Greater(t, pathv1.CurrentLastLogSeq(checkpoint), pathv1.CurrentCheckpointBinding(checkpoint).Generation, "logical log sequence remained coupled to one-per-CAS revision")
}

func TestExclusiveV7AmbiguousClaimNeverReperforms(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	adapter := &exclusiveV7Adapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	injected := errors.New("ambiguous claim")
	restore := fs.SetPathV1AppendHooksForTest(nil, func() error { return injected })
	_, err := executor.Drive(t.Context(), runID)
	restore()
	assert.ErrorIs(t, err, injected)
	assert.Equal(t, 0, adapter.count())

	_, err = executor.Drive(t.Context(), runID)
	assert.ErrorContains(t, err, "refusing to perform it again")
	assert.Equal(t, 0, adapter.count())
}

func TestExclusiveV7ProgramPerformerRejectsTamperedAllowProgramsMirror(t *testing.T) {
	root := t.TempDir()
	fs, runID := exclusiveV7RunAt(t, root, &model.Performer{Kind: model.PerformerProgram, Run: "/bin/true"}, nil)
	runPath := filepath.Join(root, "runs", runID, "run.json")
	data, err := os.ReadFile(runPath)
	require.NoError(t, err)
	var run store.RunRecord
	require.NoError(t, json.Unmarshal(data, &run))
	run.AllowPrograms = true
	data, err = json.MarshalIndent(run, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(runPath, append(data, '\n'), 0o644))

	adapter := &exclusiveV7Adapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerProgram: adapter})
	_, err = executor.Drive(t.Context(), runID)
	assert.ErrorContains(t, err, "immutable audited authority")
	assert.Equal(t, 0, adapter.count())
}

func TestExclusiveV7RecoveredProgramClaimFailsBeforeAdapter(t *testing.T) {
	fs, runID := exclusiveV7RunAt(t, t.TempDir(), &model.Performer{Kind: model.PerformerProgram, Run: "/bin/true"}, nil)
	var claim *pathv1.ExecutionTransition
	require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
		aggregate, err := pathv1.CurrentAggregateCheckpoint(view.Checkpoint)
		if err != nil {
			return err
		}
		planned, err := pathv1.PlanExclusiveAttempt(t.Context(), view.Input, aggregate.Authority.Genesis.OutputPathID, 1, view.Run.Params)
		if err != nil {
			return err
		}
		claim, err = pathv1.ClaimExclusiveAttempt(t.Context(), view.Input, planned)
		return err
	}))
	_, err := fs.AppendPathV1(t.Context(), runID, claim)
	require.NoError(t, err)

	adapter := &exclusiveV7Adapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerProgram: adapter})
	_, err = executor.Drive(t.Context(), runID)
	assert.ErrorContains(t, err, "immutable audited authority")
	assert.Equal(t, 0, adapter.count())
}

func TestExclusiveV7RecoveredMissingDeferredClaimRedispatches(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	adapter := &exclusiveV7DeferredAdapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	_, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	dispatches, reconciles := adapter.counts()
	assert.Equal(t, 1, dispatches)
	assert.Equal(t, 0, reconciles)

	_, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	dispatches, reconciles = adapter.counts()
	assert.Equal(t, 2, dispatches, "missing reconcile must redispatch the durable claim")
	assert.Equal(t, 1, reconciles)

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	dispatches, reconciles = adapter.counts()
	assert.Equal(t, 2, dispatches)
	assert.Equal(t, 2, reconciles)
}

func TestExclusiveV7AmbiguousObservationRecoversWithoutReperform(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	injected := errors.New("ambiguous observation")
	var restore func()
	adapter := &exclusiveV7Adapter{}
	adapter.perform = func(Request) {
		restore = fs.SetPathV1AppendHooksForTest(nil, func() error { return injected })
	}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	_, err := executor.Drive(t.Context(), runID)
	require.NotNil(t, restore)
	restore()
	assert.ErrorIs(t, err, injected)
	assert.Equal(t, 1, adapter.count())

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 1, adapter.count())
}

func TestExclusiveV7InlineEvidenceExactReplayAfterAmbiguousCommit(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	executor := NewExclusiveV7(fs, nil)
	commandID := claimExclusiveAttemptForTest(t, fs, runID, 1)
	observation := Observation{
		Actor:    "agent:agt_test1",
		Verdict:  "pass",
		Feedback: "inline evidence",
		Evidence: &Artifact{Name: "result.txt", Data: []byte("durable result")},
	}

	injected := errors.New("ambiguous inline observation")
	restore := fs.SetPathV1AppendHooksForTest(nil, func() error { return injected })
	_, err := executor.RecordObservation(t.Context(), runID, "work", commandID, observation)
	restore()
	assert.ErrorIs(t, err, injected)

	checkpoint, err := executor.RecordObservation(t.Context(), runID, "work", commandID, observation)
	require.NoError(t, err, "inline evidence must canonicalize to the stored ref/hash on exact replay")
	assert.NotNil(t, checkpoint)
}

func TestExclusiveV7ReportReplayBindsExactLatestAttempt(t *testing.T) {
	fs, runID := exclusiveV7RunWithRetry(t)
	executor := NewExclusiveV7(fs, nil)
	firstCommand := claimExclusiveAttemptForTest(t, fs, runID, 1)
	first := Observation{Actor: "agent:agt_test1", Verdict: "fail", Feedback: "retry", EvidenceRef: "artifact:first"}
	_, err := executor.RecordObservation(t.Context(), runID, "work", firstCommand, first)
	require.NoError(t, err)
	_, err = executor.RecordObservation(t.Context(), runID, "work", firstCommand, first)
	require.NoError(t, err, "exact retry after committed observation must be idempotent")
	changed := first
	changed.Actor = "agent:agt_other1"
	_, err = executor.RecordObservation(t.Context(), runID, "work", firstCommand, changed)
	assert.ErrorContains(t, err, "replay authority differs")

	secondCommand := claimExclusiveAttemptForTest(t, fs, runID, 2)
	second := Observation{Actor: "agent:agt_test1", Verdict: "pass", Feedback: "done", EvidenceRef: "artifact:second"}
	_, err = executor.RecordObservation(t.Context(), runID, "work", secondCommand, second)
	require.NoError(t, err)
	_, err = executor.RecordObservation(t.Context(), runID, "work", secondCommand, second)
	require.NoError(t, err, "latest exact retry must skip an unrelated historical attempt")
	_, _, err = executor.RecordNodeObservation(t.Context(), runID, "work", second)
	require.NoError(t, err, "node-only exact replay must select the unique matching observation")
}

func TestExclusiveV7RecordObservationRequiresCommandAlias(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	executor := NewExclusiveV7(fs, nil)
	claimExclusiveAttemptForTest(t, fs, runID, 1)
	_, err := executor.RecordObservation(t.Context(), runID, "work", "", Observation{
		Actor: "agent:agt_test1", Verdict: "pass", EvidenceRef: "artifact:exact",
	})
	assert.ErrorContains(t, err, "command id is required")
}

func TestExclusiveV7SignalWaitBlocksSatisfiesAndReplaysExactly(t *testing.T) {
	fs, runID := exclusiveV7WaitRun(t, &model.WaitConfig{Signal: "deploy/prod"})
	executor := NewExclusiveV7(fs, nil)
	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "running", pathv1.CurrentRunStatus(checkpoint))

	injected := errors.New("ambiguous signal observation")
	restore := fs.SetPathV1AppendHooksForTest(nil, func() error { return injected })
	_, err = executor.SatisfySignal(t.Context(), runID, "wait", "deploy/prod", "agent:agt_test1")
	restore()
	assert.ErrorIs(t, err, injected)
	_, err = executor.SatisfySignal(t.Context(), runID, "wait", "deploy/prod", "agent:agt_test1")
	require.NoError(t, err, "exact retry after committed signal must be idempotent")
	_, err = executor.SatisfySignal(t.Context(), runID, "wait", "deploy/prod", "agent:agt_other1")
	assert.ErrorContains(t, err, "authority differs")
	_, err = executor.SatisfySignal(t.Context(), runID, "wait", "deploy/stage", "agent:agt_test1")
	assert.Error(t, err)

	checkpoint, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
}

func TestExclusiveV7TimerWaitPersistsScheduleAndBlocksUntilDue(t *testing.T) {
	t0 := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	fs, runID := exclusiveV7WaitRun(t, &model.WaitConfig{Duration: "5m"})
	now := t0
	executor := NewExclusiveV7(fs, nil)
	executor.Now = func() time.Time { return now }
	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "running", pathv1.CurrentRunStatus(checkpoint))

	now = t0.Add(4*time.Minute + 59*time.Second)
	checkpoint, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "running", pathv1.CurrentRunStatus(checkpoint))
	now = t0.Add(5 * time.Minute)
	checkpoint, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
}

func TestExclusiveV7UntilWaitUsesImmutableDueInstant(t *testing.T) {
	due := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	fs, runID := exclusiveV7WaitRun(t, &model.WaitConfig{Until: due.Format(time.RFC3339)})
	now := due.Add(-time.Second)
	executor := NewExclusiveV7(fs, nil)
	executor.Now = func() time.Time { return now }
	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "running", pathv1.CurrentRunStatus(checkpoint))
	now = due
	checkpoint, err = executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
}

func TestExclusiveV7TerminalRetryReconfirmsDirectoryDurability(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	adapter := &exclusiveV7Adapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})
	terminalSync := errors.New("terminal directory sync")
	syncs := 0
	restore := fs.SetPathV1AppendDirSyncHookForTest(func() error {
		syncs++
		if syncs == 5 {
			return terminalSync
		}
		return nil
	})
	_, err := executor.Drive(t.Context(), runID)
	restore()
	assert.ErrorIs(t, err, terminalSync)
	assert.Equal(t, 1, adapter.count())

	reconfirmSync := errors.New("reconfirm directory sync")
	restore = fs.SetPathV1AppendDirSyncHookForTest(func() error { return reconfirmSync })
	_, err = executor.Drive(t.Context(), runID)
	restore()
	assert.ErrorIs(t, err, reconfirmSync, "visible terminal state must not bypass durability reconfirmation")
	assert.Equal(t, 1, adapter.count())

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
}

func exclusiveV7Run(t *testing.T) (*store.FS, string) {
	return exclusiveV7RunFixture(t, nil)
}

func exclusiveV7NoFailRun(t *testing.T, retry *model.RetryPolicy) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "exclusive-v7-no-fail", Start: "work",
		Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Retry: retry, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-exclusive-v7-no-fail"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	result, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	require.Equal(t, pathv1.InitializationApplied, result.Disposition)
	return fs, runID
}

func parallelAllV7Run(t *testing.T) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "parallel-all-v7", Start: "fork",
		Nodes: map[string]model.Node{
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "left", "right": "right"}},
			"left":  {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "left"}, Next: model.Next{"pass": "merge"}},
			"right": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "right"}, Next: model.Next{"pass": "merge"}},
			"merge": {Type: model.NodeTypeEnd, Join: model.JoinAll, Result: "completed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-parallel-all-v7"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "fork", Type: model.NodeTypeParallel, Status: state.NodeStatusReady},
		{ID: "left", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "right", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "merge", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	result, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	require.Equal(t, pathv1.InitializationApplied, result.Disposition)
	return fs, runID
}

func parallelAnyV7RunAt(t *testing.T, root string) (*store.FS, string) {
	t.Helper()
	return createParallelAnyV7Run(t, root, "parallel-any-v7", map[string]model.Node{
		"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"quick": "merge", "slow": "slow"}},
		"slow":  {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "slow"}, Next: model.Next{"pass": "merge"}},
		"merge": {Type: model.NodeTypeEnd, Join: model.JoinAny, Result: "completed"},
	})
}

func parallelAnyFailureV7Run(t *testing.T) (*store.FS, string) {
	t.Helper()
	return createParallelAnyV7Run(t, t.TempDir(), "parallel-any-failure-v7", map[string]model.Node{
		"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "left", "right": "right"}},
		"left":  {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "left"}, Next: model.Next{"pass": "merge"}},
		"right": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "right"}, Next: model.Next{"pass": "merge"}},
		"merge": {Type: model.NodeTypeEnd, Join: model.JoinAny, Result: "completed"},
	})
}

func parallelDirectAnyV7Run(t *testing.T) (*store.FS, string) {
	t.Helper()
	return createParallelAnyV7Run(t, t.TempDir(), "parallel-direct-any-v7", map[string]model.Node{
		"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "merge", "right": "merge"}},
		"merge": {Type: model.NodeTypeEnd, Join: model.JoinAny, Result: "completed"},
	})
}

func createParallelAnyV7Run(t *testing.T, root, id string, nodes map[string]model.Node) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	tmpl := &model.Template{APIVersion: model.APIVersion, Kind: model.Kind, ID: id, Start: "fork", Nodes: nodes}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-" + id
	inits := make([]state.NodeInit, 0, len(nodes))
	for nodeID, node := range nodes {
		status := state.NodeStatusPending
		if nodeID == "fork" {
			status = state.NodeStatusReady
		}
		inits = append(inits, state.NodeInit{ID: nodeID, Type: node.Type, Status: status})
	}
	slices.SortFunc(inits, func(a, b state.NodeInit) int { return cmp.Compare(a.ID, b.ID) })
	st := state.New(runID, record.Ref, record.Ref, inits)
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	result, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	require.Equal(t, pathv1.InitializationApplied, result.Disposition)
	return fs, runID
}

func assertParallelAnyActivated(t *testing.T, aggregate pathv1.AggregateCheckpoint) {
	t.Helper()
	found := false
	for _, reservation := range aggregate.Routing.Reservations {
		if reservation.JoinPolicy != pathv1.JoinAny || !reservation.IsReducing {
			continue
		}
		found = true
		assert.Equal(t, pathv1.ReservationActivated, reservation.State)
		assert.NotNil(t, reservation.Activation)
		assert.Equal(t, reservation.EventSeq, aggregate.Routing.Scopes[reservation.ReducesScopeID].EventSeq)
	}
	assert.True(t, found)
}

func parallelAllV7RunWithIntermediate(t *testing.T) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "parallel-all-v7-intermediate", Start: "fork",
		Nodes: map[string]model.Node{
			"fork":      {Type: model.NodeTypeParallel, Next: model.Next{"left": "left", "right": "right"}},
			"left":      {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "left"}, Next: model.Next{"pass": "left-next"}},
			"left-next": {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "must not run"}, Next: model.Next{"pass": "merge"}},
			"right":     {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "right"}, Next: model.Next{"pass": "merge"}},
			"merge":     {Type: model.NodeTypeEnd, Join: model.JoinAll, Result: "completed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-parallel-all-v7-intermediate"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "fork", Type: model.NodeTypeParallel, Status: state.NodeStatusReady},
		{ID: "left", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "left-next", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "right", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "merge", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	result, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	require.Equal(t, pathv1.InitializationApplied, result.Disposition)
	return fs, runID
}

func parallelAllV7WaitRun(t *testing.T) (*store.FS, string) {
	t.Helper()
	return parallelAllV7WaitRunAt(t, t.TempDir())
}

func parallelAllV7WaitRunAt(t *testing.T, root string) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "parallel-all-v7-wait", Start: "fork",
		Nodes: map[string]model.Node{
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"wait": "wait", "work": "work"}},
			"wait":  {Type: model.NodeTypeWait, Wait: &model.WaitConfig{Signal: "release"}, Next: model.Next{"pass": "merge"}},
			"work":  {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"pass": "merge"}},
			"merge": {Type: model.NodeTypeEnd, Join: model.JoinAll, Result: "completed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-parallel-all-v7-wait"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "fork", Type: model.NodeTypeParallel, Status: state.NodeStatusReady},
		{ID: "wait", Type: model.NodeTypeWait, Status: state.NodeStatusPending},
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "merge", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	return fs, runID
}

func parallelAllV7RetryRun(t *testing.T) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "parallel-all-v7-retry", Start: "fork",
		Nodes: map[string]model.Node{
			"fork": {Type: model.NodeTypeParallel, Next: model.Next{"work": "work", "peer": "peer"}},
			"work": {
				Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"},
				Retry: &model.RetryPolicy{MaxAttempts: 2}, Next: model.Next{"pass": "merge"},
			},
			"peer":  {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "peer"}, Next: model.Next{"pass": "merge"}},
			"merge": {Type: model.NodeTypeEnd, Join: model.JoinAll, Result: "completed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-parallel-all-v7-retry"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "fork", Type: model.NodeTypeParallel, Status: state.NodeStatusReady},
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "peer", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "merge", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	return fs, runID
}

func parallelAllV7SiblingWaitRunAt(t *testing.T, root string, waitA, waitB *model.WaitConfig) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "parallel-all-v7-sibling-waits", Start: "fork",
		Nodes: map[string]model.Node{
			"fork":   {Type: model.NodeTypeParallel, Next: model.Next{"wait-a": "wait-a", "wait-b": "wait-b"}},
			"wait-a": {Type: model.NodeTypeWait, Wait: waitA, Next: model.Next{"pass": "merge"}},
			"wait-b": {Type: model.NodeTypeWait, Wait: waitB, Next: model.Next{"pass": "merge"}},
			"merge":  {Type: model.NodeTypeEnd, Join: model.JoinAll, Result: "completed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-parallel-all-v7-sibling-waits"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "fork", Type: model.NodeTypeParallel, Status: state.NodeStatusReady},
		{ID: "wait-a", Type: model.NodeTypeWait, Status: state.NodeStatusPending},
		{ID: "wait-b", Type: model.NodeTypeWait, Status: state.NodeStatusPending},
		{ID: "merge", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	return fs, runID
}

func claimParallelWaitForTest(t *testing.T, fs *store.FS, runID string) {
	t.Helper()
	appendTransition := func(plan func(store.PathV1ExecutionView) (*pathv1.ExecutionTransition, error)) {
		t.Helper()
		var transition *pathv1.ExecutionTransition
		require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
			var err error
			transition, err = plan(view)
			return err
		}))
		_, err := fs.AppendPathV1(t.Context(), runID, transition)
		require.NoError(t, err)
	}
	appendTransition(func(view store.PathV1ExecutionView) (*pathv1.ExecutionTransition, error) {
		aggregate, err := pathv1.CurrentAggregateCheckpoint(view.Checkpoint)
		if err != nil {
			return nil, err
		}
		for _, path := range aggregate.Routing.Paths {
			if path.Kind == pathv1.PathActivationOutput && path.State == pathv1.PathLive {
				return pathv1.AdvanceParallelSplit(t.Context(), view.Input, path.ID)
			}
		}
		return nil, errors.New("parallel root output not found")
	})
	for range 2 {
		appendTransition(func(view store.PathV1ExecutionView) (*pathv1.ExecutionTransition, error) {
			return pathv1.AdvanceParallelExclusiveArrival(t.Context(), view.Input)
		})
	}
	appendTransition(func(view store.PathV1ExecutionView) (*pathv1.ExecutionTransition, error) {
		aggregate, err := pathv1.CurrentAggregateCheckpoint(view.Checkpoint)
		if err != nil {
			return nil, err
		}
		for _, path := range aggregate.Routing.Paths {
			if path.Kind != pathv1.PathActivationOutput || path.State != pathv1.PathLive {
				continue
			}
			activation := aggregate.Routing.Activations[path.SourceActivation.ID]
			if aggregate.Routing.Reservations[activation.ReservationID].NodeID != "wait" {
				continue
			}
			wait, planErr := pathv1.PlanExclusiveWait(t.Context(), view.Input, path.ID, time.Now())
			if planErr != nil {
				return nil, planErr
			}
			return pathv1.ClaimExclusiveWait(t.Context(), view.Input, wait)
		}
		return nil, errors.New("wait output not found")
	})
}

func exclusiveV7RunWithRetry(t *testing.T) (*store.FS, string) {
	return exclusiveV7RunFixture(t, &model.RetryPolicy{MaxAttempts: 2})
}

func exclusiveV7RunFixture(t *testing.T, retry *model.RetryPolicy) (*store.FS, string) {
	return exclusiveV7RunAt(t, t.TempDir(), &model.Performer{Kind: model.PerformerAgent, Prompt: "work {{.target}}"}, retry)
}

func exclusiveV7RunAt(t *testing.T, root string, performer *model.Performer, retry *model.RetryPolicy) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "exclusive-v7", Start: "work",
		Nodes: map[string]model.Node{
			"work":   {Type: model.NodeTypeTask, Performer: performer, Retry: retry, Next: model.Next{"pass": "done", "fail": "failed"}},
			"done":   {Type: model.NodeTypeEnd, Result: "completed"},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-exclusive-v7"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "failed", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref, Params: map[string]string{"target": "exact"}}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	result, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	require.Equal(t, pathv1.InitializationApplied, result.Disposition)
	return fs, runID
}

func claimExclusiveAttemptForTest(t *testing.T, fs *store.FS, runID string, attempt uint64) string {
	t.Helper()
	var transition *pathv1.ExecutionTransition
	var commandID string
	require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
		aggregate, err := pathv1.CurrentAggregateCheckpoint(view.Checkpoint)
		if err != nil {
			return err
		}
		var live pathv1.PathID
		for _, candidate := range aggregate.Routing.Paths {
			if candidate.Kind == pathv1.PathActivationOutput && candidate.State == pathv1.PathLive {
				live = candidate.ID
			}
		}
		planned, err := pathv1.PlanExclusiveAttempt(t.Context(), view.Input, live, attempt, view.Run.Params)
		if err != nil {
			return err
		}
		commandID = exclusiveExternalCommandID(planned.Command().ID)
		transition, err = pathv1.ClaimExclusiveAttempt(t.Context(), view.Input, planned)
		return err
	}))
	_, err := fs.AppendPathV1(t.Context(), runID, transition)
	require.NoError(t, err)
	return commandID
}

func exclusiveV7DecisionTaskRun(t *testing.T) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "exclusive-v7-chain", Start: "choose",
		Nodes: map[string]model.Node{
			"choose": {Type: model.NodeTypeDecision, Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Ship?"}, Next: model.Next{"ship": "work", "hold": "held"}},
			"work":   {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work"}, Next: model.Next{"pass": "done"}},
			"held":   {Type: model.NodeTypeEnd, Result: "completed"},
			"done":   {Type: model.NodeTypeEnd, Result: "completed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-exclusive-v7-chain"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "choose", Type: model.NodeTypeDecision, Status: state.NodeStatusReady},
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusPending},
		{ID: "held", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	return fs, runID
}

func exclusiveV7WaitRun(t *testing.T, wait *model.WaitConfig) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "exclusive-v7-wait", Start: "wait",
		Nodes: map[string]model.Node{
			"wait": {Type: model.NodeTypeWait, Wait: wait, Next: model.Next{"pass": "done"}},
			"done": {Type: model.NodeTypeEnd, Result: "completed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-exclusive-v7-wait"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "wait", Type: model.NodeTypeWait, Status: state.NodeStatusReady},
		{ID: "done", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	return fs, runID
}
