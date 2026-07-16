//go:build linux || darwin

package store_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

func TestWithExecutionViewHoldsRunAndTemplateLocksThroughCallback(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- fs.WithExecutionView(t.Context(), runID, func(view store.ExecutionView) error {
			require.Equal(t, runID, view.Snapshot.Run.ID)
			require.NotNil(t, view.Template)
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	appendDone := make(chan error, 1)
	go func() {
		_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "implement", 0)})
		appendDone <- err
	}()
	templateDone := make(chan error, 1)
	go func() {
		_, err := fs.PutTemplate(t.Context(), storetest.Template())
		templateDone <- err
	}()
	assertStillBlocked(t, appendDone, "append")
	assertStillBlocked(t, templateDone, "template writer")
	close(release)
	require.NoError(t, <-done)
	require.NoError(t, <-appendDone)
	require.NoError(t, <-templateDone)
}

func TestWithExecutionViewUsesRunThenTemplateLockOrder(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	templateLock := flock.New(filepath.Join(root, ".locks", "template-demo.lock"))
	require.NoError(t, templateLock.Lock())
	defer templateLock.Unlock()

	runLocked := make(chan struct{})
	templateLocked := make(chan struct{})
	var runOnce, templateOnce sync.Once
	restore := fs.SetExecutionViewHooksForTest(
		func() { runOnce.Do(func() { close(runLocked) }) },
		func() { templateOnce.Do(func() { close(templateLocked) }) },
		nil,
	)
	defer restore()
	viewDone := make(chan error, 1)
	go func() {
		viewDone <- fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error { return nil })
	}()
	<-runLocked

	appendDone := make(chan error, 1)
	go func() {
		_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "implement", 0)})
		appendDone <- err
	}()
	assertStillBlocked(t, appendDone, "append while execution view waits for template")
	select {
	case <-templateLocked:
		t.Fatal("execution view acquired template lock while it was externally held")
	default:
	}
	require.NoError(t, templateLock.Unlock())
	select {
	case <-templateLocked:
	case <-time.After(time.Second):
		t.Fatal("execution view did not acquire template after contention cleared")
	}
	require.NoError(t, <-viewDone)
	require.NoError(t, <-appendDone)
}

func TestWithExecutionViewStableAndChangingEvidenceDisagreement(t *testing.T) {
	t.Run("stable torn tail is inconsistent", func(t *testing.T) {
		fixture := storetest.BuildCrashFixture(t, storetest.CrashTornFinalLogLine)
		err := fixture.Store.WithExecutionView(t.Context(), fixture.RunID, func(store.ExecutionView) error {
			t.Fatal("callback ran for torn evidence")
			return nil
		})
		require.ErrorIs(t, err, store.ErrRunInconsistent)
		assert.NotErrorIs(t, err, store.ErrWriterInProgress)
		var readErr *evidence.ReadError
		require.ErrorAs(t, err, &readErr)
		assert.Equal(t, evidence.ReadErrorTornTail, readErr.Kind)
	})

	t.Run("changed tail is writer in progress", func(t *testing.T) {
		fixture := storetest.BuildCrashFixture(t, storetest.CrashTornFinalLogLine)
		path := filepath.Join(fixture.Root, "runs", fixture.RunID, "nodes", "implement", "log.jsonl")
		restore := fixture.Store.SetExecutionViewHooksForTest(nil, nil, func() {
			require.NoError(t, os.WriteFile(path, append([]byte(`{"schemaVersion":1,"seq":1}`), '\n'), 0o644))
		})
		defer restore()
		err := fixture.Store.WithExecutionView(t.Context(), fixture.RunID, func(store.ExecutionView) error {
			t.Fatal("callback ran while evidence changed")
			return nil
		})
		require.ErrorIs(t, err, store.ErrWriterInProgress)
		assert.NotErrorIs(t, err, store.ErrRunInconsistent)
	})

	t.Run("stable checkpoint anchor is inconsistent", func(t *testing.T) {
		fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterManifestBeforeState)
		err := fixture.Store.WithExecutionView(t.Context(), fixture.RunID, func(store.ExecutionView) error {
			t.Fatal("callback ran for a stale checkpoint anchor")
			return nil
		})
		require.ErrorIs(t, err, store.ErrRunInconsistent)
		assert.NotErrorIs(t, err, store.ErrWriterInProgress)
	})

	t.Run("changing checkpoint anchor is writer in progress", func(t *testing.T) {
		fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterManifestBeforeState)
		statePath := filepath.Join(fixture.Root, "runs", fixture.RunID, "state.json")
		restore := fixture.Store.SetExecutionViewHooksForTest(nil, nil, func() {
			data, err := os.ReadFile(statePath)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(statePath, append(data, ' '), 0o644))
		})
		defer restore()
		err := fixture.Store.WithExecutionView(t.Context(), fixture.RunID, func(store.ExecutionView) error {
			t.Fatal("callback ran while checkpoint anchor changed")
			return nil
		})
		require.ErrorIs(t, err, store.ErrWriterInProgress)
		assert.NotErrorIs(t, err, store.ErrRunInconsistent)
	})

	t.Run("otherwise valid changing view is writer in progress", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := initializedRunAt(t, root)
		statePath := filepath.Join(root, "runs", runID, "state.json")
		restore := fs.SetExecutionViewHooksForTest(nil, nil, func() {
			data, err := os.ReadFile(statePath)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(statePath, append(data, ' '), 0o644))
		})
		defer restore()
		err := fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error {
			t.Fatal("callback ran after anchors changed")
			return nil
		})
		require.ErrorIs(t, err, store.ErrWriterInProgress)
		assert.NotErrorIs(t, err, store.ErrRunInconsistent)
	})
}

func TestWithExecutionViewRequiresSingleRunJSONDocument(t *testing.T) {
	for _, tc := range executionJSONSuffixCases() {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fs, runID := initializedRunAt(t, root)
			path := filepath.Join(root, "runs", runID, "run.json")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, append(data, tc.suffix...), 0o644))

			called := false
			err = fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error {
				called = true
				return nil
			})
			if tc.accept {
				require.NoError(t, err)
				assert.True(t, called)
				return
			}
			require.ErrorIs(t, err, store.ErrRunInconsistent)
			assert.False(t, called)
			var decodeErr *store.DecodeError
			require.ErrorAs(t, err, &decodeErr)
			assert.Equal(t, "run record", decodeErr.Component)
		})
	}
}

func TestWithExecutionViewRequiresSingleTemplateJSONDocument(t *testing.T) {
	for _, tc := range executionJSONSuffixCases() {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fs, runID := initializedRunAt(t, root)
			path := exactTemplateBodyPathForRun(t, root, runID)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, append(data, tc.suffix...), 0o644))

			called := false
			err = fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error {
				called = true
				return nil
			})
			if tc.accept {
				require.NoError(t, err)
				assert.True(t, called)
				return
			}
			require.ErrorIs(t, err, store.ErrRunInconsistent)
			assert.False(t, called)
			assert.False(t, store.IsDecodeError(err), "exact-template decode classification changed")
			assert.NotErrorIs(t, err, store.ErrContentMismatch, "semantic hashing ran before trailing data was rejected")
			assert.Contains(t, err.Error(), "exact template cannot be decoded")
		})
	}
}

func TestExecutionViewTrailingJSONDecodePrecedence(t *testing.T) {
	t.Run("run suffix precedes identity validation", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := initializedRunAt(t, root)
		path := filepath.Join(root, "runs", runID, "run.json")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var run store.RunRecord
		require.NoError(t, json.Unmarshal(data, &run))
		run.ID = "wrong-id"
		data, err = json.Marshal(run)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(data, []byte(`{}`)...), 0o644))

		err = fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error {
			t.Fatal("callback ran for invalid run JSON")
			return nil
		})
		var decodeErr *store.DecodeError
		require.ErrorAs(t, err, &decodeErr)
		assert.Equal(t, "run record", decodeErr.Component)
	})

	t.Run("template field validation precedes suffix validation", func(t *testing.T) {
		root := t.TempDir()
		fs, runID := initializedRunAt(t, root)
		path := exactTemplateBodyPathForRun(t, root, runID)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		var fields map[string]any
		require.NoError(t, json.Unmarshal(data, &fields))
		fields["unexpected"] = true
		data, err = json.Marshal(fields)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, append(data, []byte(`{}`)...), 0o644))

		err = fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error {
			t.Fatal("callback ran for invalid template JSON")
			return nil
		})
		require.ErrorIs(t, err, store.ErrRunInconsistent)
		assert.Contains(t, err.Error(), `unknown field "unexpected"`)
		assert.NotContains(t, err.Error(), "unexpected trailing JSON value")
	})
}

func TestWithExecutionViewTypedBudgetBoundaries(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	total, largest := executionViewFileBounds(t, root, runID)
	restore := fs.SetViewerResourceLimitsForTest(largest, total, 100, 100)
	require.NoError(t, fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error { return nil }))
	restore()

	for _, tc := range []struct {
		name              string
		maxFile, maxTotal int64
		limit             string
	}{
		{"file plus one", largest - 1, total * 2, "file_bytes"},
		{"total plus one", largest, total - 1, "total_bytes"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restore := fs.SetViewerResourceLimitsForTest(tc.maxFile, tc.maxTotal, 100, 100)
			defer restore()
			err := fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error {
				t.Fatal("over-budget callback ran")
				return nil
			})
			var budgetErr *store.ExecutionViewOverBudgetError
			require.ErrorAs(t, err, &budgetErr)
			assert.Equal(t, tc.limit, budgetErr.Limit)
			assert.ErrorIs(t, err, store.ErrExecutionViewOverBudget)
			var readErr *evidence.ReadError
			assert.False(t, errors.As(err, &readErr), "budget failure was classified as torn evidence")
		})
	}
}

func TestOrdinaryViewerRetainsLegacyBudgetError(t *testing.T) {
	fs, runID := initializedRun(t)
	restore := fs.SetViewerResourceLimitsForTest(1, 1, 1, 1)
	defer restore()
	_, err := fs.LoadRunView(t.Context(), runID)
	require.ErrorIs(t, err, store.ErrViewerResourceLimit)
	assert.NotErrorIs(t, err, store.ErrExecutionViewOverBudget)
	var typed *store.ExecutionViewOverBudgetError
	assert.False(t, errors.As(err, &typed), "ordinary viewer adopted execution-only typed budget errors")
}

func TestWithExecutionViewTypedCountBoundaries(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "implement", 0)})
	require.NoError(t, err)

	// One manifest record plus one owning-log record is exactly two.
	restore := fs.SetViewerResourceLimitsForTest(1<<20, 1<<20, 2, 1)
	require.NoError(t, fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error { return nil }))
	restore()

	for _, tc := range []struct {
		name                string
		maxRecords, maxDirs int
		limit               string
	}{
		{"record plus one", 1, 10, "records"},
		{"directory plus one", 10, 1, "directory_entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.limit == "directory_entries" {
				require.NoError(t, os.MkdirAll(filepath.Join(root, "runs", runID, "nodes", "second"), 0o755))
			}
			restore := fs.SetViewerResourceLimitsForTest(1<<20, 1<<20, tc.maxRecords, tc.maxDirs)
			defer restore()
			err := fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error {
				t.Fatal("over-budget callback ran")
				return nil
			})
			var budgetErr *store.ExecutionViewOverBudgetError
			require.ErrorAs(t, err, &budgetErr)
			assert.Equal(t, tc.limit, budgetErr.Limit)
		})
	}
}

func TestWithExecutionViewRejectsIntermediateAndFinalSymlinks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, string, string)
	}{
		{"run directory", func(t *testing.T, root, runID string) {
			path := filepath.Join(root, "runs", runID)
			target := path + "-target"
			require.NoError(t, os.Rename(path, target))
			require.NoError(t, os.Symlink(target, path))
		}},
		{"nodes directory", func(t *testing.T, root, runID string) {
			path := filepath.Join(root, "runs", runID, "nodes")
			require.NoError(t, os.MkdirAll(path, 0o755))
			require.NoError(t, os.Rename(path, path+"-target"))
			require.NoError(t, os.Symlink(path+"-target", path))
		}},
		{"state file", func(t *testing.T, root, runID string) {
			path := filepath.Join(root, "runs", runID, "state.json")
			require.NoError(t, os.Rename(path, path+"-target"))
			require.NoError(t, os.Symlink(path+"-target", path))
		}},
		{"template id directory", func(t *testing.T, root, _ string) {
			path := filepath.Join(root, "templates", "demo")
			require.NoError(t, os.Rename(path, path+"-target"))
			require.NoError(t, os.Symlink(path+"-target", path))
		}},
		{"template file", func(t *testing.T, root, runID string) {
			run, err := os.ReadFile(filepath.Join(root, "runs", runID, "run.json"))
			require.NoError(t, err)
			var record store.RunRecord
			require.NoError(t, json.Unmarshal(run, &record))
			_, hash, err := splitTemplateRef(record.TemplateRef)
			require.NoError(t, err)
			path := filepath.Join(root, "templates", "demo", "sha256-"+hash, "template.json")
			require.NoError(t, os.Rename(path, path+"-target"))
			require.NoError(t, os.Symlink(path+"-target", path))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			fs, runID := initializedRunAt(t, root)
			tc.mutate(t, root, runID)
			err := fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error { return nil })
			require.ErrorIs(t, err, store.ErrUnsafeRunPath)
		})
	}
}

func TestWithExecutionViewReleasesLocksOnErrorCallbackAndPanic(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	want := errors.New("callback failed")
	require.ErrorIs(t, fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error { return want }), want)
	_, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{storetest.AdminLogEntry(runID, "implement", 0)})
	require.NoError(t, err)

	func() {
		defer func() { require.Equal(t, "boom", recover()) }()
		_ = fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error { panic("boom") })
	}()
	_, err = fs.PutTemplate(t.Context(), storetest.Template())
	require.NoError(t, err)

	statePath := filepath.Join(root, "runs", runID, "state.json")
	valid, err := os.ReadFile(statePath)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(statePath, []byte(`{"stateSchemaVersion":6,"pause":{"until":"bad"}}`), 0o644))
	err = fs.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error { return nil })
	require.ErrorIs(t, err, store.ErrRunInconsistent)
	require.NoError(t, os.WriteFile(statePath, valid, 0o644))
	// A fresh store instance models restart and must observe released flock state.
	restarted, err := store.NewFS(root)
	require.NoError(t, err)
	require.NoError(t, restarted.WithExecutionView(t.Context(), runID, func(store.ExecutionView) error { return nil }))
}

func TestWithExecutionViewUsesCanonicalLegacyPredecodeAndProvenance(t *testing.T) {
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	snapshot, err := fs.LoadRun(t.Context(), runID)
	require.NoError(t, err)
	snapshot.State.AdminRecords = append(snapshot.State.AdminRecords, state.AdminRecord{
		Type: state.EventAdminRepairRecorded, Actor: "human:johan", Reason: "repair",
		EvidenceRef: "ticket-1", Timestamp: time.Date(2026, 7, 15, 14, 0, 0, 123400000, time.FixedZone("CEST", 2*60*60)),
	})
	data, err := state.Encode(snapshot.State)
	require.NoError(t, err)
	require.Contains(t, string(data), "+02:00", "fixture must exercise raw offset canonicalization")
	require.NoError(t, os.WriteFile(filepath.Join(root, "runs", runID, "state.json"), data, 0o644))

	err = fs.WithExecutionView(t.Context(), runID, func(view store.ExecutionView) error {
		require.Len(t, view.LegacyAdminRecords, 1)
		require.Empty(t, view.LegacyAdminResolutions)
		for _, record := range view.LegacyAdminRecords {
			assert.Equal(t, "2026-07-15T12:00:00.1234Z", record.Timestamp)
			want, identityErr := pathv1.LegacyAdminRecordIdentity(record)
			require.NoError(t, identityErr)
			assert.Equal(t, want, record.ID)
		}
		assert.Equal(t, time.UTC, view.Snapshot.State.AdminRecords[0].Timestamp.Location())
		return nil
	})
	require.NoError(t, err)
}

func assertStillBlocked(t *testing.T, done <-chan error, operation string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s completed while execution view held locks: %v", operation, err)
	case <-time.After(100 * time.Millisecond):
	}
}

func executionViewFileBounds(t *testing.T, root, runID string) (total, largest int64) {
	t.Helper()
	runDir := filepath.Join(root, "runs", runID)
	paths := []string{
		filepath.Join(runDir, "run.json"),
		filepath.Join(runDir, "state.json"),
		filepath.Join(runDir, "manifest.jsonl"),
	}
	var record store.RunRecord
	runData, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(runData, &record))
	_, hash, err := splitTemplateRef(record.TemplateRef)
	require.NoError(t, err)
	versionDir := filepath.Join(root, "templates", "demo", "sha256-"+hash)
	paths = append(paths,
		filepath.Join(versionDir, "template.json"),
		filepath.Join(versionDir, "template.yaml"),
	)
	for _, path := range paths {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		require.NoError(t, err)
		total += info.Size()
		largest = max(largest, info.Size())
	}
	return total, largest
}

func executionJSONSuffixCases() []struct {
	name   string
	suffix string
	accept bool
} {
	return []struct {
		name   string
		suffix string
		accept bool
	}{
		{name: "JSON whitespace", suffix: " \t\r\n", accept: true},
		{name: "large bounded JSON whitespace", suffix: strings.Repeat(" \t\r\n", 16<<10), accept: true},
		{name: "second object", suffix: "\n{}"},
		{name: "second array", suffix: "\n[]"},
		{name: "second scalar", suffix: "\n42"},
		{name: "second null", suffix: "\nnull"},
		{name: "same-line concatenation", suffix: `{}`},
		{name: "trailing garbage", suffix: "\ngarbage"},
		{name: "malformed suffix", suffix: "\n{\"truncated\":"},
	}
}

func exactTemplateBodyPathForRun(t *testing.T, root, runID string) string {
	t.Helper()
	runData, err := os.ReadFile(filepath.Join(root, "runs", runID, "run.json"))
	require.NoError(t, err)
	var run store.RunRecord
	require.NoError(t, json.Unmarshal(runData, &run))
	id, hash, err := splitTemplateRef(run.TemplateRef)
	require.NoError(t, err)
	return filepath.Join(root, "templates", id, "sha256-"+hash, "template.json")
}
