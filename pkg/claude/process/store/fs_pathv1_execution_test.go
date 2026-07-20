//go:build linux || darwin

package store_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestPathV1ExecutionViewAndAppendExactReplayCAS(t *testing.T) {
	fs, runID, initial := initializedPathV1ExecutionRun(t)
	initialCanonical, initialDigest, err := pathv1.CanonicalInitializationAnchor(initial)
	require.NoError(t, err)
	base, claim := planPathV1Claim(t, fs, runID)

	applied, err := fs.AppendPathV1(t.Context(), runID, claim)
	require.NoError(t, err)
	assert.Equal(t, store.PathV1AppendApplied, applied.Disposition)
	assert.Equal(t, uint64(1), pathv1.CheckpointRevision(applied.Checkpoint))
	assert.Equal(t, base.Generation+1, applied.Binding.Generation)
	assert.NotEqual(t, base.Digest, applied.Binding.Digest)

	replayed, err := fs.AppendPathV1(t.Context(), runID, claim)
	require.NoError(t, err)
	assert.Equal(t, store.PathV1AppendAlreadyApplied, replayed.Disposition)
	assert.Equal(t, applied.Binding, replayed.Binding)

	require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
		assert.Equal(t, applied.Binding, view.Binding)
		assert.Equal(t, uint64(1), pathv1.CheckpointRevision(view.Checkpoint))
		currentCanonical, currentDigest, err := pathv1.CanonicalInitializationAnchor(view.Checkpoint)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(initialCanonical, currentCanonical), "persisted canonical initialization bytes changed")
		assert.Equal(t, initialDigest, currentDigest, "persisted initialization digest basis changed")
		assert.NotEmpty(t, view.TemplateSource)
		assert.NotNil(t, view.Input)
		return nil
	}))
}

func TestPathV1ExecutionViewRejectsTrailingAuthoritativeJSON(t *testing.T) {
	for _, tc := range []struct {
		name string
		path func(*testing.T, string, string) string
	}{
		{
			name: "run",
			path: func(_ *testing.T, root, runID string) string {
				return filepath.Join(root, "runs", runID, "run.json")
			},
		},
		{name: "exact template", path: exactTemplateBodyPathForRun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fs, runID, _ := initializedPathV1ExecutionRunAt(t, root)
			path := tc.path(t, root, runID)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, append(data, []byte(`null`)...), 0o644))

			err = fs.WithPathV1ExecutionView(t.Context(), runID, func(store.PathV1ExecutionView) error {
				t.Fatal("callback ran for trailing authoritative JSON")
				return nil
			})
			require.ErrorIs(t, err, store.ErrRunInconsistent)
			if tc.name == "run" {
				assert.True(t, store.IsDecodeError(err))
			} else {
				assert.False(t, store.IsDecodeError(err))
				assert.NotErrorIs(t, err, store.ErrContentMismatch)
			}
		})
	}
}

func TestInitializePathV1ReplayAcceptsAuthenticatedMutableExecutionHead(t *testing.T) {
	fs, runID, initial := initializedPathV1ExecutionRun(t)
	_, claim := planPathV1Claim(t, fs, runID)
	applied, err := fs.AppendPathV1(t.Context(), runID, claim)
	require.NoError(t, err)
	require.Equal(t, uint64(1), pathv1.CheckpointRevision(applied.Checkpoint))

	replayed, err := fs.InitializePathV1(t.Context(), runID, initial.Initialize.UpgradeNeeded)
	require.NoError(t, err)
	assert.Equal(t, pathv1.InitializationAlreadyApplied, replayed.Disposition)
	assert.Equal(t, applied.Binding, pathv1.CurrentCheckpointBinding(replayed.Checkpoint))
	assert.Equal(t, uint64(1), pathv1.CheckpointRevision(replayed.Checkpoint))
}

func TestLoadPathV1RunHistoryViewDoesNotReadLegacyEvidenceWithoutProjectionMetadata(t *testing.T) {
	root := t.TempDir()
	fs, runID, _ := initializedPathV1ExecutionRunAt(t, root)
	manifestPath := filepath.Join(root, "runs", runID, "manifest.jsonl")
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	require.NoError(t, os.Symlink("state.json", manifestPath))

	snapshot, err := fs.LoadPathV1RunHistoryView(t.Context(), runID)
	require.NoError(t, err)
	assert.Nil(t, snapshot.LegacyEvidence)
}

func TestPathV1AppendCrashBoundariesAndAmbiguousAcknowledgement(t *testing.T) {
	t.Run("before rename", func(t *testing.T) {
		fs, runID, _ := initializedPathV1ExecutionRun(t)
		base, desired := planPathV1Claim(t, fs, runID)
		injected := errors.New("before rename")
		restore := fs.SetPathV1AppendHooksForTest(func() error { return injected }, nil)
		_, err := fs.AppendPathV1(t.Context(), runID, desired)
		restore()
		assert.ErrorIs(t, err, injected)
		require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
			assert.Equal(t, base, view.Binding)
			assert.Zero(t, pathv1.CheckpointRevision(view.Checkpoint))
			return nil
		}))
	})

	t.Run("after durable rename", func(t *testing.T) {
		fs, runID, _ := initializedPathV1ExecutionRun(t)
		_, desired := planPathV1Claim(t, fs, runID)
		injected := errors.New("ambiguous acknowledgement")
		restore := fs.SetPathV1AppendHooksForTest(nil, func() error { return injected })
		_, err := fs.AppendPathV1(t.Context(), runID, desired)
		restore()
		assert.ErrorIs(t, err, injected)

		var syncs atomic.Int32
		restoreSync := fs.SetPathV1AppendDirSyncHookForTest(func() error {
			syncs.Add(1)
			return nil
		})
		recovered, err := fs.AppendPathV1(t.Context(), runID, desired)
		restoreSync()
		require.NoError(t, err)
		assert.Equal(t, store.PathV1AppendAlreadyApplied, recovered.Disposition)
		assert.Equal(t, int32(1), syncs.Load(), "exact replay must reconfirm directory durability")
	})
}

func TestPathV1AppendContentionSerializesWithoutFalseTornRead(t *testing.T) {
	fs, runID, _ := initializedPathV1ExecutionRun(t)
	_, desired := planPathV1Claim(t, fs, runID)
	entered := make(chan struct{})
	release := make(chan struct{})
	restore := fs.SetPathV1AppendHooksForTest(func() error {
		close(entered)
		<-release
		return nil
	}, nil)
	defer restore()

	appendErr := make(chan error, 1)
	go func() {
		_, err := fs.AppendPathV1(t.Context(), runID, desired)
		appendErr <- err
	}()
	<-entered

	viewErr := make(chan error, 1)
	go func() {
		viewErr <- fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
			if pathv1.CheckpointRevision(view.Checkpoint) != 1 {
				t.Errorf("coherent view revision = %d", pathv1.CheckpointRevision(view.Checkpoint))
			}
			return nil
		})
	}()
	select {
	case err := <-viewErr:
		t.Fatalf("coherent read escaped held append lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-appendErr)
	require.NoError(t, <-viewErr)
}

func TestPathV1ConcurrentExactCASHasOneApplyAndOneReplay(t *testing.T) {
	fs, runID, _ := initializedPathV1ExecutionRun(t)
	_, claim := planPathV1Claim(t, fs, runID)

	start := make(chan struct{})
	results := make(chan store.PathV1AppendDisposition, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := fs.AppendPathV1(t.Context(), runID, claim)
			errs <- err
			results <- result.Disposition
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	applied, replayed := 0, 0
	for err := range errs {
		require.NoError(t, err)
	}
	for disposition := range results {
		switch disposition {
		case store.PathV1AppendApplied:
			applied++
		case store.PathV1AppendAlreadyApplied:
			replayed++
		}
	}
	assert.Equal(t, 1, applied)
	assert.Equal(t, 1, replayed)
}

func TestPathV1AppendAuthorityIsSealed(t *testing.T) {
	typeOf := reflect.TypeOf(pathv1.ExecutionTransition{})
	for index := 0; index < typeOf.NumField(); index++ {
		assert.NotEmpty(t, typeOf.Field(index).PkgPath, "transition field %q is publicly mutable", typeOf.Field(index).Name)
	}
	method, ok := reflect.TypeOf((*store.FS)(nil)).MethodByName("AppendPathV1")
	require.True(t, ok)
	require.Equal(t, 4, method.Type.NumIn()) // receiver, context, run id, transition
	assert.Equal(t, reflect.TypeOf((*pathv1.ExecutionTransition)(nil)), method.Type.In(3))

	fs, runID, _ := initializedPathV1ExecutionRun(t)
	_, err := fs.AppendPathV1(t.Context(), runID, nil)
	assert.Error(t, err, "public append must reject absent sealed authority")
}

func initializedPathV1ExecutionRun(t *testing.T) (*store.FS, string, *pathv1.CheckpointV7) {
	t.Helper()
	return initializedPathV1ExecutionRunAt(t, t.TempDir())
}

func initializedPathV1ExecutionRunAt(t *testing.T, root string) (*store.FS, string, *pathv1.CheckpointV7) {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "execution-demo", Start: "work",
		Nodes: map[string]model.Node{
			"work": {
				Type: model.NodeTypeTask,
				Performer: &model.Performer{
					Kind: model.PerformerAgent, Prompt: "perform exact work",
				},
				Next: model.Next{"pass": "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-execution"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "end", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	result, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	require.Equal(t, pathv1.InitializationApplied, result.Disposition)
	return fs, runID, result.Checkpoint
}

func planPathV1Claim(t *testing.T, fs *store.FS, runID string) (pathv1.CheckpointBinding, *pathv1.ExecutionTransition) {
	t.Helper()
	var binding pathv1.CheckpointBinding
	var transition *pathv1.ExecutionTransition
	require.NoError(t, fs.WithPathV1ExecutionView(t.Context(), runID, func(view store.PathV1ExecutionView) error {
		binding = view.Binding
		aggregate, err := pathv1.CurrentAggregateCheckpoint(view.Checkpoint)
		if err != nil {
			return err
		}
		pathID := aggregate.Authority.Genesis.OutputPathID
		plan, err := pathv1.PlanExclusiveAttempt(t.Context(), view.Input, pathID, 1, view.Run.Params)
		if err != nil {
			return err
		}
		transition, err = pathv1.ClaimExclusiveAttempt(t.Context(), view.Input, plan)
		return err
	}))
	return binding, transition
}
