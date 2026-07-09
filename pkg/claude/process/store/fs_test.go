package store_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
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
	records, err := fs.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].Ref != record.Ref {
		t.Fatalf("template records = %#v", records)
	}
}

func TestTemplateGetRejectsTamperedContent(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := splitTemplateRef(record.Ref)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "templates", "demo", "sha256-"+hash, "template.json")
	tampered := storetest.Template()
	tampered.Nodes["extra"] = model.Node{Type: model.NodeTypeTask}
	data, err := model.CanonicalSemanticJSON(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.GetTemplate(ctx, record.Ref); !errors.Is(err, store.ErrContentMismatch) {
		t.Fatalf("expected content mismatch, got %v", err)
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

func TestListRuns(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	for _, runID := range []string{"run_b", "run_a"} {
		st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
		if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref, Params: map[string]string{"name": runID}}, st); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := fs.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "run_a" || runs[1].ID != "run_b" || runs[0].Params["name"] != "run_a" {
		t.Fatalf("runs = %#v", runs)
	}
}

func TestSetProgramsAllowedRequiresDurableAdminAudit(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run_programs"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref, AllowPrograms: true}, st); err == nil || !strings.Contains(err.Error(), "admin audit") {
		t.Fatalf("expected create-time opt-in refusal, got %v", err)
	}
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.SetProgramsAllowed(ctx, runID); err == nil || !strings.Contains(err.Error(), "no admin") {
		t.Fatalf("expected unaudited opt-in refusal, got %v", err)
	}
	at := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	_, err = fs.Append(ctx, runID, 0, []evidence.LogEntry{{
		At:    at,
		Scope: evidence.Scope{Kind: evidence.ScopeRun},
		Kind:  evidence.EntryKindAdmin,
		Event: &state.Event{
			Type:   state.EventAdminProgramsAllowed,
			At:     at,
			Actor:  "human:test",
			Reason: "explicit test opt-in",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	run, err := fs.SetProgramsAllowed(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if !run.AllowPrograms {
		t.Fatal("audited program opt-in was not persisted")
	}
}

func TestSetProgramsAllowedRejectsUncommittedAuditLogTail(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	const runID = "run_programs_tail"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	entry := evidence.LogEntry{
		SchemaVersion: evidence.LogEntrySchemaVersion,
		Seq:           1,
		At:            at,
		Scope:         evidence.Scope{Kind: evidence.ScopeRun},
		Kind:          evidence.EntryKindAdmin,
		Event: &state.Event{
			Type:   state.EventAdminProgramsAllowed,
			Seq:    1,
			At:     at,
			Actor:  "human:test",
			Reason: "uncommitted test opt-in",
		},
	}
	path := filepath.Join(root, "runs", runID, "run", "log.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := evidence.AppendLogEntry(file, entry); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.SetProgramsAllowed(ctx, runID); !errors.Is(err, store.ErrRunInconsistent) {
		t.Fatalf("expected inconsistent audit refusal, got %v", err)
	}
	run, err := fs.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.AllowPrograms {
		t.Fatal("uncommitted audit enabled program execution")
	}
}

func TestListRunsToleratesBadRunJSON(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	st := state.New("run_good", record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: "run_good", TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	badDir := filepath.Join(root, "runs", "run_bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "run.json"), []byte(`{"id":`), 0o644); err != nil {
		t.Fatal(err)
	}

	runs, err := fs.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != "run_bad" || runs[1].ID != "run_good" {
		t.Fatalf("runs = %#v", runs)
	}
}

func TestCreateRunStateOnlyLeftoverIsRetriable(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_half_created"
	st := state.New(runID, record.Ref, record.Ref, []state.NodeInit{{ID: "implement"}})
	data, err := state.Encode(&st)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(root, "runs", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "state.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.GetRun(ctx, runID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("state-only run should be invisible, got %v", err)
	}
	if _, err := fs.CreateRun(ctx, store.RunRecord{ID: runID, TemplateRef: record.Ref}, st); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.LoadRun(ctx, runID); err != nil {
		t.Fatal(err)
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

func TestEmptyAppendReturnsCurrentStateAndValidatesRun(t *testing.T) {
	ctx := t.Context()
	fs, runID := initializedRun(t)
	result, err := fs.Append(ctx, runID, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.State == nil || result.State.RunID != runID {
		t.Fatalf("empty append state = %#v", result.State)
	}
	if _, err := fs.Append(ctx, "../x", 0, nil); err == nil {
		t.Fatal("expected invalid run id error")
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
		if readErr.File != "nodes/implement/log.jsonl" {
			t.Fatalf("read error file = %q", readErr.File)
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

func TestArtifactGetRejectsTamperedContent(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	artifact, err := fs.PutArtifact(ctx, runID, "note.txt", strings.NewReader("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "runs", runID, "artifacts", artifact.SHA256), []byte("EVIL"), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := fs.GetArtifact(ctx, runID, artifact.Ref)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(r)
	closeErr := r.Close()
	if !errors.Is(readErr, store.ErrContentMismatch) && !errors.Is(closeErr, store.ErrContentMismatch) {
		t.Fatalf("expected content mismatch, data=%q readErr=%v closeErr=%v", data, readErr, closeErr)
	}
}

func TestAcquireRunLeaseRequiresExistingRun(t *testing.T) {
	fs := newStore(t)
	_, err := fs.AcquireRunLease(t.Context(), "missing_run", "agent-a", time.Minute)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected missing run, got %v", err)
	}
}

func TestAcquireRunLeaseExpiredHolderCanBeReplaced(t *testing.T) {
	fs, runID := initializedRun(t)
	now := time.Date(2026, 7, 9, 20, 0, 0, 0, time.UTC)
	t.Cleanup(fs.SetNowForTest(func() time.Time { return now }))
	if _, err := fs.AcquireRunLease(t.Context(), runID, "agent-a", time.Minute); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	lease, err := fs.AcquireRunLease(t.Context(), runID, "agent-b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Holder != "agent-b" {
		t.Fatalf("lease holder = %q", lease.Holder)
	}
	if err := fs.ReleaseRunLease(t.Context(), runID, "agent-a"); !errors.Is(err, store.ErrLeaseHeld) {
		t.Fatalf("stale holder release = %v", err)
	}
}

func TestRunLockHonorsContextWhileFlockHeld(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs, runID := initializedRunAt(t, root)
	lockPath := filepath.Join(root, ".locks", runID+".lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	fl := flock.New(lockPath)
	if err := fl.Lock(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fl.Unlock() }()

	lockCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	_, err := fs.AcquireRunLease(lockCtx, runID, "agent-a", time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
	}
}

func TestNewFSAnchorsRelativeRoot(t *testing.T) {
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	if err := os.Chdir(base); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	fs, runID := initializedRunAt(t, "relative-store")
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.GetRun(t.Context(), runID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "relative-store", "runs", runID, "run.json")); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultRootFailsClosedWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if root := store.DefaultRoot(); root != "" {
		t.Fatalf("default root without home = %q", root)
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
	return initializedRunAt(t, t.TempDir())
}

func initializedRunAt(t *testing.T, root string) (*store.FS, string) {
	t.Helper()
	ctx := t.Context()
	fs := newStoreAt(t, root)
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
	return newStoreAt(t, t.TempDir())
}

func newStoreAt(t *testing.T, root string) *store.FS {
	t.Helper()
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func splitTemplateRef(ref string) (string, string, error) {
	id, hash, ok := strings.Cut(ref, "@sha256:")
	if !ok {
		return "", "", errors.New("invalid template ref")
	}
	return id, hash, nil
}

func hasDiagnostic(diagnostics evidence.Diagnostics, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
