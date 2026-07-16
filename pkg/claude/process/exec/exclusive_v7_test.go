package processexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

func TestExclusiveV7DriveClaimsPerformsObservesRoutesAndCompletes(t *testing.T) {
	fs, runID := exclusiveV7Run(t)
	adapter := &exclusiveV7Adapter{}
	executor := NewExclusiveV7(fs, map[model.PerformerKind]Adapter{model.PerformerAgent: adapter})

	checkpoint, err := executor.Drive(t.Context(), runID)
	require.NoError(t, err)
	assert.Equal(t, "completed", pathv1.CurrentRunStatus(checkpoint))
	assert.Equal(t, 1, adapter.count())
	assert.GreaterOrEqual(t, pathv1.CheckpointRevision(checkpoint), uint64(5))

	aggregate, err := pathv1.CurrentAggregateCheckpoint(checkpoint)
	require.NoError(t, err)
	completion, err := pathv1.AssessAggregateCompletion(aggregate.View())
	require.NoError(t, err)
	assert.Equal(t, "completed", completion.Result)
	for _, command := range aggregate.Commands {
		assert.False(t, command.State.Active(), "terminal checkpoint retained active command %q", command.ID)
	}
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
	for _, reservation := range aggregate.Routing.Reservations {
		if reservation.NodeID == "left-next" {
			assert.Equal(t, pathv1.ReservationClosedNoActivation, reservation.State)
		}
	}
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
