package processexec

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

type exclusiveV7Adapter struct {
	mu        sync.Mutex
	performs  int
	perform   func(Request)
	reconcile func(Request) (Observation, bool, error)
	results   []Observation
}

func (a *exclusiveV7Adapter) Validate(Request) error { return nil }

func (a *exclusiveV7Adapter) Perform(_ context.Context, request Request) (Observation, error) {
	a.mu.Lock()
	a.performs++
	index := a.performs - 1
	perform := a.perform
	var observation Observation
	if index < len(a.results) {
		observation = a.results[index]
	}
	a.mu.Unlock()
	if perform != nil {
		perform(request)
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

func exclusiveV7Run(t *testing.T) (*store.FS, string) {
	return exclusiveV7RunFixture(t, nil)
}

func exclusiveV7RunWithRetry(t *testing.T) (*store.FS, string) {
	return exclusiveV7RunFixture(t, &model.RetryPolicy{MaxAttempts: 2})
}

func exclusiveV7RunFixture(t *testing.T, retry *model.RetryPolicy) (*store.FS, string) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "exclusive-v7", Start: "work",
		Nodes: map[string]model.Node{
			"work":   {Type: model.NodeTypeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "work {{.target}}"}, Retry: retry, Next: model.Next{"pass": "done", "fail": "failed"}},
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
