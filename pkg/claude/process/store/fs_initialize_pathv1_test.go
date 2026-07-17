//go:build linux || darwin

package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

func TestInitializePathV1AtomicApplyExactReplayAndRuntimeIsolation(t *testing.T) {
	root := t.TempDir()
	fs, runID, proof := pristineInitializationRun(t, root)

	result, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	assert.Equal(t, pathv1.InitializationApplied, result.Disposition)
	require.NoError(t, pathv1.ValidateCheckpointV7(result.Checkpoint))

	data := readRunStateBytes(t, root, runID)
	installed, err := pathv1.DecodeCheckpointV7(data)
	require.NoError(t, err)
	assert.Equal(t, result.Checkpoint.Digest, installed.Digest)

	// Existing runtime/store reads retain the schema-6 ceiling. Nothing in the
	// generic Store/Host surface can silently accept or advance this container.
	_, err = fs.LoadRun(t.Context(), runID)
	assert.ErrorIs(t, err, state.ErrNewerSchemaVersion)
	_, err = fs.LoadRunState(t.Context(), runID)
	assert.ErrorIs(t, err, state.ErrNewerSchemaVersion)

	malformedReplay := proof
	malformedReplay.Checkpoint.Digest = ""
	_, err = fs.InitializePathV1(t.Context(), runID, malformedReplay)
	assert.ErrorIs(t, err, pathv1.ErrInitializationInconsistent)

	replay, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	assert.Equal(t, pathv1.InitializationAlreadyApplied, replay.Disposition)
	assert.Equal(t, installed.Digest, replay.Checkpoint.Digest)
}

func TestInitializePathV1RefusalsPreserveCompleteV6Bytes(t *testing.T) {
	t.Run("forged proof", func(t *testing.T) {
		root := t.TempDir()
		fs, runID, proof := pristineInitializationRun(t, root)
		before := readRunStateBytes(t, root, runID)
		proof.Checkpoint.Digest = strings.Repeat("c", 64)
		_, err := fs.InitializePathV1(t.Context(), runID, proof)
		assert.ErrorIs(t, err, pathv1.ErrInitializationInconsistent)
		assert.Equal(t, before, readRunStateBytes(t, root, runID))
	})

	t.Run("drain required", func(t *testing.T) {
		root := t.TempDir()
		fs, runID, proof := pristineInitializationRun(t, root)
		before := readRunStateBytes(t, root, runID)
		proof.Reason = pathv1.UpgradeLegacyDrainRequired
		proof.ActiveLegacyIDs = []pathv1.LegacyActiveID{{Kind: pathv1.LegacyActiveWait, ID: "wait"}}
		_, err := fs.InitializePathV1(t.Context(), runID, proof)
		assert.ErrorIs(t, err, pathv1.ErrInitializationInvalid)
		assert.Equal(t, before, readRunStateBytes(t, root, runID))
	})

	t.Run("stale checkpoint", func(t *testing.T) {
		root := t.TempDir()
		fs, runID, proof := pristineInitializationRun(t, root)
		_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "implement", 0)})
		require.NoError(t, err)
		current := readRunStateBytes(t, root, runID)
		_, err = fs.InitializePathV1(t.Context(), runID, proof)
		assert.ErrorIs(t, err, pathv1.ErrInitializationInconsistent)
		assert.Equal(t, current, readRunStateBytes(t, root, runID))
	})

	t.Run("ambiguous progressed state", func(t *testing.T) {
		root := t.TempDir()
		fs, runID, _ := pristineInitializationRun(t, root)
		snapshot, err := fs.LoadRun(t.Context(), runID)
		require.NoError(t, err)
		node := snapshot.State.Nodes["implement"]
		node.Status = state.NodeStatusPending
		snapshot.State.Nodes["implement"] = node
		data, err := state.Encode(snapshot.State)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(root, "runs", runID, "state.json"), data, 0o644))
		proof, err := fs.UpgradeNeeded(t.Context(), runID)
		require.NoError(t, err)
		before := readRunStateBytes(t, root, runID)
		_, err = fs.InitializePathV1(t.Context(), runID, proof)
		assert.ErrorIs(t, err, pathv1.ErrInitializationAmbiguous)
		assert.Equal(t, before, readRunStateBytes(t, root, runID))
	})
}

func TestInitializePathV1CrashBoundariesAndAmbiguousAcknowledgementReplay(t *testing.T) {
	root := t.TempDir()
	fs, runID, proof := pristineInitializationRun(t, root)
	v6 := readRunStateBytes(t, root, runID)
	crashBefore := errors.New("injected before rename")
	restore := fs.SetPathV1InitializeHooksForTest(func() error { return crashBefore }, nil)
	_, err := fs.InitializePathV1(t.Context(), runID, proof)
	restore()
	assert.ErrorIs(t, err, crashBefore)
	assert.Equal(t, v6, readRunStateBytes(t, root, runID))

	unknownAfter := errors.New("injected lost acknowledgement after rename")
	restore = fs.SetPathV1InitializeHooksForTest(nil, func() error { return unknownAfter })
	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	restore()
	assert.ErrorIs(t, err, unknownAfter)
	_, decodeErr := pathv1.DecodeCheckpointV7(readRunStateBytes(t, root, runID))
	require.NoError(t, decodeErr)

	replay, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	assert.Equal(t, pathv1.InitializationAlreadyApplied, replay.Disposition)
}

func TestInitializePathV1ReplayReconfirmsDirectoryDurability(t *testing.T) {
	root := t.TempDir()
	fs, runID, proof := pristineInitializationRun(t, root)
	syncFailure := errors.New("injected directory sync failure")
	restore := fs.SetPathV1InitializeDirSyncHookForTest(func() error { return syncFailure })
	_, err := fs.InitializePathV1(t.Context(), runID, proof)
	assert.ErrorIs(t, err, syncFailure)
	_, decodeErr := pathv1.DecodeCheckpointV7(readRunStateBytes(t, root, runID))
	require.NoError(t, decodeErr, "rename completed before the ambiguous durability failure")

	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	assert.ErrorIs(t, err, syncFailure, "replay must not acknowledge while directory durability still fails")
	restore()
	replay, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	assert.Equal(t, pathv1.InitializationAlreadyApplied, replay.Disposition)
}

func TestInitializePathV1ConcurrentCASAndAppendLockContention(t *testing.T) {
	t.Run("duplicate CAS", func(t *testing.T) {
		root := t.TempDir()
		fs, runID, proof := pristineInitializationRun(t, root)
		start := make(chan struct{})
		results := make(chan store.PathV1InitializationResult, 2)
		errs := make(chan error, 2)
		for range 2 {
			go func() {
				<-start
				result, err := fs.InitializePathV1(t.Context(), runID, proof)
				results <- result
				errs <- err
			}()
		}
		close(start)
		first, second := <-results, <-results
		require.NoError(t, <-errs)
		require.NoError(t, <-errs)
		dispositions := map[pathv1.InitializationDisposition]int{first.Disposition: 1}
		dispositions[second.Disposition]++
		assert.Equal(t, 1, dispositions[pathv1.InitializationApplied])
		assert.Equal(t, 1, dispositions[pathv1.InitializationAlreadyApplied])
	})

	t.Run("single run lock no reentry", func(t *testing.T) {
		root := t.TempDir()
		fs, runID, proof := pristineInitializationRun(t, root)
		entered := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		restore := fs.SetPathV1InitializeHooksForTest(func() error {
			once.Do(func() { close(entered) })
			<-release
			return nil
		}, nil)
		defer restore()

		initDone := make(chan error, 1)
		go func() {
			_, err := fs.InitializePathV1(t.Context(), runID, proof)
			initDone <- err
		}()
		<-entered
		appendDone := make(chan error, 1)
		go func() {
			_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "implement", 0)})
			appendDone <- err
		}()
		select {
		case err := <-appendDone:
			t.Fatalf("append escaped the initialization run lock: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		require.NoError(t, <-initDone)
		select {
		case err := <-appendDone:
			assert.ErrorIs(t, err, state.ErrNewerSchemaVersion)
		case <-time.After(5 * time.Second):
			t.Fatal("append deadlocked after initialization released the run lock")
		}
	})
}

func TestInitializePathV1CorruptInstalledCheckpointIsTypedInconsistency(t *testing.T) {
	root := t.TempDir()
	fs, runID, proof := pristineInitializationRun(t, root)
	_, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	path := filepath.Join(root, "runs", runID, "state.json")
	data := readRunStateBytes(t, root, runID)
	digest := bytes.LastIndex(data, []byte(`"digest":"`))
	require.NotEqual(t, -1, digest)
	digest += len(`"digest":"`)
	if data[digest] == '0' {
		data[digest] = '1'
	} else {
		data[digest] = '0'
	}
	require.NoError(t, os.WriteFile(path, data, 0o644))
	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	assert.ErrorIs(t, err, pathv1.ErrInitializationInconsistent)
}

func TestInitializePathV1ReplayValidatesEmbeddedTemplateIdentity(t *testing.T) {
	t.Run("exact match is already applied", func(t *testing.T) {
		root := t.TempDir()
		fs, runID, proof := pristineInitializationRun(t, root)
		_, err := fs.InitializePathV1(t.Context(), runID, proof)
		require.NoError(t, err)

		result, err := fs.InitializePathV1(t.Context(), runID, proof)
		require.NoError(t, err)
		assert.Equal(t, pathv1.InitializationAlreadyApplied, result.Disposition)
	})

	t.Run("legacy nil embedded template is already applied", func(t *testing.T) {
		root := t.TempDir()
		fs, runID, proof := pristineInitializationRun(t, root)

		runPath := filepath.Join(root, "runs", runID, "run.json")
		runData, err := os.ReadFile(runPath)
		require.NoError(t, err)
		var run store.RunRecord
		require.NoError(t, json.Unmarshal(runData, &run))
		require.NotNil(t, run.Template)
		run.Template = nil
		runData, err = json.MarshalIndent(run, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(runPath, append(runData, '\n'), 0o644))

		installed, err := fs.InitializePathV1(t.Context(), runID, proof)
		require.NoError(t, err)
		assert.Equal(t, pathv1.InitializationApplied, installed.Disposition)

		replay, err := fs.InitializePathV1(t.Context(), runID, proof)
		require.NoError(t, err)
		assert.Equal(t, pathv1.InitializationAlreadyApplied, replay.Disposition)
	})

	for _, tc := range []struct {
		name   string
		mutate func(*model.Template)
	}{
		{
			name: "same id semantic drift",
			mutate: func(tmpl *model.Template) {
				tmpl.Description = "drifted after initialization"
			},
		},
		{
			name: "template id mismatch",
			mutate: func(tmpl *model.Template) {
				tmpl.ID = "different-template"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fs, runID, proof := pristineInitializationRun(t, root)
			_, err := fs.InitializePathV1(t.Context(), runID, proof)
			require.NoError(t, err)

			runPath := filepath.Join(root, "runs", runID, "run.json")
			runData, err := os.ReadFile(runPath)
			require.NoError(t, err)
			var run store.RunRecord
			require.NoError(t, json.Unmarshal(runData, &run))
			require.NotNil(t, run.Template)
			tc.mutate(run.Template)
			runData, err = json.MarshalIndent(run, "", "  ")
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(runPath, append(runData, '\n'), 0o644))

			result, err := fs.InitializePathV1(t.Context(), runID, proof)
			assert.ErrorIs(t, err, pathv1.ErrInitializationInconsistent)
			assert.Zero(t, result.Disposition)
			assert.NotEqual(t, pathv1.InitializationAlreadyApplied, result.Disposition)
		})
	}
}

func TestInitializePathV1CancellationPreservesV6(t *testing.T) {
	root := t.TempDir()
	fs, runID, proof := pristineInitializationRun(t, root)
	before := readRunStateBytes(t, root, runID)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := fs.InitializePathV1(ctx, runID, proof)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, before, readRunStateBytes(t, root, runID))
}

func TestInitializePathV1RunDirectorySwapCannotRedirectCommit(t *testing.T) {
	root := t.TempDir()
	fs, runID, proof := pristineInitializationRun(t, root)
	v6 := readRunStateBytes(t, root, runID)
	runDir := filepath.Join(root, "runs", runID)
	movedRunDir := runDir + ".moved"
	redirect := t.TempDir()
	redirectState := []byte("outside-store-sentinel\n")
	require.NoError(t, os.WriteFile(filepath.Join(redirect, "state.json"), redirectState, 0o644))
	restore := fs.SetPathV1InitializeHooksForTest(func() error {
		require.NoError(t, os.Rename(runDir, movedRunDir))
		require.NoError(t, os.Symlink(redirect, runDir))
		return nil
	}, nil)
	_, err := fs.InitializePathV1(t.Context(), runID, proof)
	restore()
	assert.ErrorIs(t, err, store.ErrUnsafeRunPath)

	original, readErr := os.ReadFile(filepath.Join(movedRunDir, "state.json"))
	require.NoError(t, readErr)
	assert.Equal(t, v6, original, "validated run must remain complete v6")
	redirected, readErr := os.ReadFile(filepath.Join(redirect, "state.json"))
	require.NoError(t, readErr)
	assert.Equal(t, redirectState, redirected, "descriptor-relative commit must not follow replacement symlink")
	_, decodeErr := pathv1.DecodeCheckpointV7(original)
	assert.ErrorIs(t, decodeErr, pathv1.ErrCheckpointSchemaInvalid)
}

func TestInitializePathV1ReplayRejectsAlternateSelfConsistentContainer(t *testing.T) {
	root := t.TempDir()
	fs, runID, proof := pristineInitializationRun(t, root)
	result, err := fs.InitializePathV1(t.Context(), runID, proof)
	require.NoError(t, err)
	data, err := pathv1.EncodeCheckpointV7(result.Checkpoint)
	require.NoError(t, err)
	alternate, err := pathv1.DecodeCheckpointV7(data)
	require.NoError(t, err)

	event := &alternate.Initialize
	oldAdminID := event.AdminRecord.ID
	admin := event.AdminRecord
	admin.Actor = "system:alternate"
	admin.ID, err = pathv1.AdminRecordIdentity(admin)
	require.NoError(t, err)
	delete(event.Aggregate.AdminRecords, oldAdminID)
	event.Aggregate.AdminRecords[admin.ID] = admin
	event.AdminRecord = admin
	eventJSON, err := json.Marshal(event)
	require.NoError(t, err)
	canonical, err := independentStoreCanonicalJSON(eventJSON)
	require.NoError(t, err)
	digest := sha256.Sum256(canonical)
	alternate.Digest = hex.EncodeToString(digest[:])
	alternateBytes, err := pathv1.EncodeCheckpointV7(alternate)
	require.NoError(t, err, "alternate container must be internally self-consistent")
	require.NoError(t, os.WriteFile(filepath.Join(root, "runs", runID, "state.json"), alternateBytes, 0o644))

	_, err = fs.InitializePathV1(t.Context(), runID, proof)
	assert.ErrorIs(t, err, pathv1.ErrInitializationInconsistent)
}

func pristineInitializationRun(t *testing.T, root string) (*store.FS, string, pathv1.UpgradeNeeded) {
	t.Helper()
	fs, err := store.NewFS(root)
	require.NoError(t, err)
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "init-demo", Start: "implement",
		Nodes: map[string]model.Node{
			"implement": {Type: model.NodeTypeStart, Next: model.Next{"done": "end"}},
			"end":       {Type: model.NodeTypeEnd},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	require.NoError(t, err)
	runID := "run-init"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "implement", Type: model.NodeTypeStart, Status: state.NodeStatusReady},
		{ID: "end", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	st.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
	require.NoError(t, err)
	proof, err := fs.UpgradeNeeded(t.Context(), runID)
	require.NoError(t, err)
	require.NoError(t, pathv1.ValidateUpgradeNeeded(proof))
	return fs, runID, proof
}

func readRunStateBytes(t *testing.T, root, runID string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "runs", runID, "state.json"))
	require.NoError(t, err)
	return data
}

func independentStoreCanonicalJSON(data []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := writeIndependentStoreCanonical(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func writeIndependentStoreCanonical(out *bytes.Buffer, value any) error {
	switch value := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if value {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		encoded, _ := json.Marshal(value)
		out.Write(encoded)
	case json.Number:
		out.WriteString(string(value))
	case []any:
		out.WriteByte('[')
		for i, item := range value {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeIndependentStoreCanonical(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i > 0 {
				out.WriteByte(',')
			}
			encoded, _ := json.Marshal(key)
			out.Write(encoded)
			out.WriteByte(':')
			if err := writeIndependentStoreCanonical(out, value[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("unsupported independent JSON value %T", value)
	}
	return nil
}
