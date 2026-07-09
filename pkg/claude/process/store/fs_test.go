package store_test

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

func TestTemplatePutGetIsContentAddressed(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(record.Ref, "demo@sha256:") {
		t.Fatalf("unexpected ref %q", record.Ref)
	}
	again, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	if again.Ref != record.Ref {
		t.Fatalf("ref changed: %q != %q", again.Ref, record.Ref)
	}
	tmpl, err := fs.GetTemplate(ctx, record.Ref)
	if err != nil {
		t.Fatal(err)
	}
	if tmpl.ID != "demo" || tmpl.Start != "implement" {
		t.Fatalf("template = %#v", tmpl)
	}
}

func TestAppendCASConflict(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	entry := storetest.LogEntry(runID, "implement", 0)

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := fs.Append(ctx, runID, 0, []evidence.LogEntry{entry})
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var wins, conflicts int
	for err := range results {
		switch {
		case err == nil:
			wins++
		case store.IsConflict(err):
			conflicts++
		default:
			t.Fatalf("unexpected append error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
}

func TestCreateRunConflictIsSerialized(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run_race"

	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
			_, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref}, st)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	var wins, failures int
	for err := range results {
		if err == nil {
			wins++
		} else {
			failures++
		}
	}
	if wins != 1 || failures != 1 {
		t.Fatalf("wins=%d failures=%d", wins, failures)
	}
}

func TestStoreRoundTripVerifiesEvidenceAndStateAnchors(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	entry := storetest.LogEntry(runID, "implement", 0)
	entry.At = time.Date(2026, 7, 9, 16, 30, 15, 120000000, time.FixedZone("TST", 90*60))
	entry.Event.At = entry.At

	result, err := fs.Append(ctx, runID, 0, []evidence.LogEntry{entry})
	if err != nil {
		t.Fatal(err)
	}
	if result.Entries[0].Seq != 1 || result.Entries[0].Event.Seq != 1 {
		t.Fatalf("entry seqs not assigned: %#v", result.Entries[0])
	}

	snapshot, err := fs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics := evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs); diagnostics.HasErrors() {
		t.Fatalf("evidence diagnostics: %#v", diagnostics)
	}
	if diagnostics := evidence.VerifyStateAnchors(snapshot.State, snapshot.Manifest); diagnostics.HasErrors() {
		t.Fatalf("state anchor diagnostics: %#v", diagnostics)
	}
	if snapshot.State.LastLogSeq != 1 || snapshot.State.LogChecksum != snapshot.Manifest[0].Checksum {
		t.Fatalf("state anchors = seq %d checksum %q", snapshot.State.LastLogSeq, snapshot.State.LogChecksum)
	}
}

func TestAppendRejectsStateBehindManifest(t *testing.T) {
	fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterManifestBeforeState)
	entry := storetest.AdminLogEntry(fixture.RunID, "implement", 0)
	_, err := fixture.Store.Append(t.Context(), fixture.RunID, 1, []evidence.LogEntry{entry})
	if !errors.Is(err, store.ErrRunInconsistent) {
		t.Fatalf("expected inconsistent run error, got %v", err)
	}

	snapshot, loadErr := fixture.Store.LoadRun(t.Context(), fixture.RunID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if len(snapshot.Manifest) != 1 || snapshot.State.LastLogSeq != 0 {
		t.Fatalf("append mutated stale run: manifest=%d stateSeq=%d", len(snapshot.Manifest), snapshot.State.LastLogSeq)
	}
}

func TestBatchedAppendValidationFailureDoesNotPartiallyCommit(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	valid := storetest.AdminLogEntry(runID, "implement", 0)
	invalid := storetest.AdminLogEntry(runID, "implement", 0)
	invalid.Event.Type = state.EventNodeAttemptSettled

	_, err := fs.Append(ctx, runID, 0, []evidence.LogEntry{valid, invalid})
	if err == nil {
		t.Fatal("expected invalid second event to fail")
	}
	manifest, err := fs.ReadManifest(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	nodeLog, err := fs.ReadNodeLog(ctx, runID, "implement")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest) != 0 || len(nodeLog) != 0 {
		t.Fatalf("batch partially committed: manifest=%d nodeLog=%d", len(manifest), len(nodeLog))
	}
}

func TestRunScopeLogRoundTrip(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	entry := storetest.AdminLogEntry(runID, "implement", 0)
	entry.Scope = evidence.Scope{Kind: evidence.ScopeRun}

	if _, err := fs.Append(ctx, runID, 0, []evidence.LogEntry{entry}); err != nil {
		t.Fatal(err)
	}
	runLog, err := fs.ReadRunLog(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(runLog) != 1 || runLog[0].Scope.Kind != evidence.ScopeRun {
		t.Fatalf("run log = %#v", runLog)
	}
	snapshot, err := fs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics := evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs); diagnostics.HasErrors() {
		t.Fatalf("evidence diagnostics: %#v", diagnostics)
	}
}

func TestCrashFixturesAreDetectable(t *testing.T) {
	t.Run("log ahead of manifest", func(t *testing.T) {
		fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterLogBeforeManifest)
		snapshot, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if !hasDiagnostic(evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs), "log_ahead_of_manifest") {
			t.Fatalf("expected log_ahead_of_manifest")
		}
	})
	t.Run("manifest ahead of state", func(t *testing.T) {
		fixture := storetest.BuildCrashFixture(t, storetest.CrashAfterManifestBeforeState)
		snapshot, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if !hasDiagnostic(evidence.VerifyStateAnchors(snapshot.State, snapshot.Manifest), "state_behind_manifest") {
			t.Fatalf("expected state_behind_manifest")
		}
	})
	t.Run("torn final log line", func(t *testing.T) {
		fixture := storetest.BuildCrashFixture(t, storetest.CrashTornFinalLogLine)
		_, err := fixture.Store.LoadRun(t.Context(), fixture.RunID)
		var readErr *evidence.ReadError
		if !errors.As(err, &readErr) || readErr.Kind != evidence.ReadErrorTornTail {
			t.Fatalf("expected torn-tail read error, got %#v", err)
		}
	})
}

func TestArtifactsAndLeases(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	artifact, err := fs.PutArtifact(ctx, runID, "note.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := fs.GetArtifact(ctx, runID, artifact.Ref)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("artifact data = %q", data)
	}

	if _, err := fs.AcquireRunLease(ctx, runID, "agent-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.AcquireRunLease(ctx, runID, "agent-b", time.Minute); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("expected held lease, got %v", err)
	}
	if err := fs.ReleaseRunLease(ctx, runID, "agent-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.AcquireRunLease(ctx, runID, "agent-b", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestPublicRunMethodsRejectUnsafeRunIDs(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	for name, call := range map[string]func() error{
		"get run":       func() error { _, err := fs.GetRun(ctx, "../x"); return err },
		"read manifest": func() error { _, err := fs.ReadManifest(ctx, "../x"); return err },
		"read node log": func() error { _, err := fs.ReadNodeLog(ctx, "../x", "node"); return err },
		"read run log":  func() error { _, err := fs.ReadRunLog(ctx, "../x"); return err },
		"put artifact":  func() error { _, err := fs.PutArtifact(ctx, "../x", "a", strings.NewReader("x")); return err },
		"get artifact": func() error {
			_, err := fs.GetArtifact(ctx, "../x", "artifact:sha256:0000000000000000000000000000000000000000000000000000000000000000")
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatalf("expected unsafe run id rejection")
			}
		})
	}
}

func TestConcurrentAppendHammer(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	const writers = 24

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for {
				snapshot, err := fs.LoadRun(ctx, runID)
				if err != nil {
					t.Errorf("load run: %v", err)
					return
				}
				entry := storetest.AdminLogEntry(runID, "implement", 0)
				entry.Event.Reason = "concurrent append probe " + string(rune('a'+i))
				_, err = fs.Append(ctx, runID, snapshot.State.LastLogSeq, []evidence.LogEntry{entry})
				if err == nil {
					return
				}
				if store.IsConflict(err) {
					continue
				}
				t.Errorf("append: %v", err)
				return
			}
		}(i)
	}
	wg.Wait()

	snapshot, err := fs.LoadRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Manifest) != writers {
		t.Fatalf("manifest len = %d, want %d", len(snapshot.Manifest), writers)
	}
	if diagnostics := evidence.VerifySequence(snapshot.Manifest, snapshot.NodeLogs); diagnostics.HasErrors() {
		t.Fatalf("evidence diagnostics: %#v", diagnostics)
	}
	if diagnostics := evidence.VerifyStateAnchors(snapshot.State, snapshot.Manifest); diagnostics.HasErrors() {
		t.Fatalf("state diagnostics: %#v", diagnostics)
	}
}

func initializedRun(t *testing.T) (*store.FS, string) {
	t.Helper()
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_test"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	return fs, runID
}

func newStore(t *testing.T) *store.FS {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func hasDiagnostic(diagnostics evidence.Diagnostics, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
