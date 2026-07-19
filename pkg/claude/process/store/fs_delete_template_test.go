package store_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	"github.com/tofutools/tclaude/pkg/claude/process/store/storetest"
)

// putRunWithStatus creates a run pinned to tmplRef and forces its checkpoint to
// status, so deletion guard cases can be expressed directly.
func putRunWithStatus(t *testing.T, fs *store.FS, runID, tmplRef string, status state.RunStatus) {
	t.Helper()
	initial := state.New(runID, tmplRef, tmplRef, []state.NodeInit{{ID: "implement", Type: model.NodeTypeTask}})
	initial.Status = status
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: tmplRef}, initial); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteTemplateRemovesEveryVersion(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)

	first, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	second := storetest.Template()
	second.Nodes["implement"] = model.Node{Type: model.NodeTypeTask, Next: model.Next{"done": "end"}, Name: "changed"}
	secondRecord, err := fs.PutTemplateVersion(ctx, second)
	if err != nil {
		t.Fatal(err)
	}
	if secondRecord.Ref == first.Ref {
		t.Fatal("expected a second distinct template version")
	}

	if err := fs.DeleteTemplate(ctx, "demo"); err != nil {
		t.Fatalf("delete template: %v", err)
	}

	for _, ref := range []string{first.Ref, secondRecord.Ref} {
		if _, err := fs.GetTemplate(ctx, ref); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("version %q still readable: %v", ref, err)
		}
	}
	records, err := fs.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no templates after delete, got %d", len(records))
	}
}

func TestDeleteTemplateMissingIsNotFound(t *testing.T) {
	fs := newStore(t)
	if err := fs.DeleteTemplate(t.Context(), "nope"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestDeleteTemplateRejectsUnsafeID(t *testing.T) {
	fs := newStore(t)
	if err := fs.DeleteTemplate(t.Context(), "../escape"); err == nil {
		t.Fatal("expected traversal-shaped id to be rejected")
	}
}

func TestDeleteTemplateBlockedByUnfinishedRun(t *testing.T) {
	// Every status a run can still leave must hold the template, including the
	// ones that cannot execute right now but remain resumable or repairable.
	for _, status := range []state.RunStatus{
		state.RunStatusPending,
		state.RunStatusRunning,
		state.RunStatusBlocked,
		state.RunStatusPaused,
		state.RunStatusDirty,
		state.RunStatusInconsistent,
	} {
		t.Run(string(status), func(t *testing.T) {
			ctx := t.Context()
			fs := newStore(t)
			record, err := fs.PutTemplate(ctx, storetest.Template())
			if err != nil {
				t.Fatal(err)
			}
			putRunWithStatus(t, fs, "run-1", record.Ref, status)

			err = fs.DeleteTemplate(ctx, "demo")
			var inUse *store.TemplateInUseError
			if !errors.As(err, &inUse) {
				t.Fatalf("expected TemplateInUseError, got %v", err)
			}
			if !errors.Is(err, store.ErrTemplateInUse) {
				t.Fatalf("error does not unwrap to ErrTemplateInUse: %v", err)
			}
			if len(inUse.RunIDs) != 1 || inUse.RunIDs[0] != "run-1" {
				t.Fatalf("unexpected blocking runs %v", inUse.RunIDs)
			}
			if _, err := fs.GetTemplate(ctx, record.Ref); err != nil {
				t.Fatalf("template should survive a refused delete: %v", err)
			}
		})
	}
}

func TestDeleteTemplateAllowedWhenRunsFinished(t *testing.T) {
	for _, status := range []state.RunStatus{
		state.RunStatusCompleted,
		state.RunStatusFailed,
		state.RunStatusCanceled,
	} {
		t.Run(string(status), func(t *testing.T) {
			ctx := t.Context()
			fs := newStore(t)
			record, err := fs.PutTemplate(ctx, storetest.Template())
			if err != nil {
				t.Fatal(err)
			}
			putRunWithStatus(t, fs, "run-1", record.Ref, status)

			if err := fs.DeleteTemplate(ctx, "demo"); err != nil {
				t.Fatalf("finished run should not block delete: %v", err)
			}
			// The run must stay auditable: CreateRun pinned the template into
			// run.json exactly so history survives the library entry going away.
			run, err := fs.GetRun(ctx, "run-1")
			if err != nil {
				t.Fatalf("run unreadable after template delete: %v", err)
			}
			if run.Template == nil {
				t.Fatal("run lost its pinned template snapshot")
			}
			if run.Template.ID != "demo" {
				t.Fatalf("unexpected pinned template id %q", run.Template.ID)
			}
		})
	}
}

// writeRawRunState replaces a run's checkpoint with an exact on-disk document.
// The deletion guard classifies runs by reading state.json directly (it cannot
// take run locks without inverting the store's run→template lock order), so
// these tests pin that raw contract rather than going through a decoder that
// only understands one schema.
func writeRawRunState(t *testing.T, root, runID, document string) {
	t.Helper()
	path := filepath.Join(root, "runs", runID, "state.json")
	if err := os.WriteFile(path, []byte(document), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Regression: pathv1 writes schema 7, which the legacy state decoder rejects
// outright (it is capped at 6). Classifying such a run by decoding it would
// make every schema-7 run look unfinished forever, so its template could never
// be deleted and no operator action could fix it.
func TestDeleteTemplateClassifiesSchema7Runs(t *testing.T) {
	for _, test := range []struct {
		name      string
		state     string
		deletable bool
	}{
		{"finished", `{"stateSchemaVersion":7,"execution":{"status":"completed"}}`, true},
		{"failed", `{"stateSchemaVersion":7,"execution":{"status":"failed"}}`, true},
		{"running", `{"stateSchemaVersion":7,"execution":{"status":"running"}}`, false},
		// An installed schema-7 checkpoint predating the mutable execution head
		// is running by definition, mirroring pathv1.CurrentRunStatus.
		{"no execution head", `{"stateSchemaVersion":7,"initialize":{"eventSeq":1}}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			fs := newStoreAt(t, root)
			record, err := fs.PutTemplate(ctx, storetest.Template())
			if err != nil {
				t.Fatal(err)
			}
			putRunWithStatus(t, fs, "run-1", record.Ref, state.RunStatusRunning)
			writeRawRunState(t, root, "run-1", test.state)

			err = fs.DeleteTemplate(ctx, "demo")
			if test.deletable && err != nil {
				t.Fatalf("a finished schema-7 run must not block deletion: %v", err)
			}
			if !test.deletable && !errors.Is(err, store.ErrTemplateInUse) {
				t.Fatalf("expected the run to block deletion, got %v", err)
			}
		})
	}
}

// The guard must fail closed: a run whose record cannot be decoded surfaces
// from ListRuns without a template ref, and we cannot prove it is unrelated to
// the template being deleted.
func TestDeleteTemplateBlockedByUnreadableRun(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	putRunWithStatus(t, fs, "run-broken", record.Ref, state.RunStatusCompleted)
	if err := os.WriteFile(filepath.Join(root, "runs", "run-broken", "run.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = fs.DeleteTemplate(ctx, "demo")
	var inUse *store.TemplateInUseError
	if !errors.As(err, &inUse) {
		t.Fatalf("expected TemplateInUseError, got %v", err)
	}
	if len(inUse.UnreadableRunIDs) != 1 || inUse.UnreadableRunIDs[0] != "run-broken" {
		t.Fatalf("unreadable run not reported: %+v", inUse)
	}
	if _, err := fs.GetTemplate(ctx, record.Ref); err != nil {
		t.Fatalf("template should survive a refused delete: %v", err)
	}
}

// A legacy run recorded before template pinning has no snapshot of its own, so
// the library entry is its ONLY copy of the definition — deleting it would
// destroy the run's provenance even though the run is finished.
func TestDeleteTemplateBlockedByFinishedRunWithoutPin(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	putRunWithStatus(t, fs, "run-legacy", record.Ref, state.RunStatusCompleted)
	// Strip the pin the way a pre-pinning record would have been written.
	runPath := filepath.Join(root, "runs", "run-legacy", "run.json")
	raw, err := os.ReadFile(runPath)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	delete(decoded, "template")
	stripped, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runPath, stripped, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := fs.DeleteTemplate(ctx, "demo"); !errors.Is(err, store.ErrTemplateInUse) {
		t.Fatalf("an unpinned finished run must block deletion, got %v", err)
	}
}

// Deletion detaches the tree with an atomic rename before removing it, so a
// reader can never observe a head pointing at an already-removed version. The
// detached name must not linger or become visible as a template.
func TestDeleteTemplateLeavesNoResidue(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	fs := newStoreAt(t, root)
	if _, err := fs.PutTemplate(ctx, storetest.Template()); err != nil {
		t.Fatal(err)
	}
	if err := fs.DeleteTemplate(ctx, "demo"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, "templates"))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Fatalf("templates dir should be empty after delete, found %q", entry.Name())
	}
	records, err := fs.ListTemplates(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no templates, got %d", len(records))
	}
}

// CreateRun pins under the template lock and holds it until run.json lands, so
// a delete cannot slip between the pin and the write. Whichever order the two
// resolve in, the outcome must be consistent: either the run exists and the
// template survived, or the template is gone and the run was never created.
func TestDeleteTemplateRacingCreateRunIsConsistent(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)
	record, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}

	createErr := make(chan error, 1)
	go func() {
		initial := state.New("run-race", record.Ref, record.Ref, []state.NodeInit{{ID: "implement", Type: model.NodeTypeTask}})
		_, err := fs.CreateRun(ctx, store.RunRecord{ID: "run-race", TemplateRef: record.Ref}, initial)
		createErr <- err
	}()
	deleteErr := fs.DeleteTemplate(ctx, "demo")
	created := <-createErr

	_, templateErr := fs.GetTemplate(ctx, record.Ref)
	switch {
	case deleteErr == nil:
		// The delete won: the run must have been refused, not left stranded on a
		// template that no longer exists.
		if created == nil {
			t.Fatal("a run was created against a template that was concurrently deleted")
		}
		if !errors.Is(templateErr, store.ErrNotFound) {
			t.Fatalf("template should be gone after a successful delete: %v", templateErr)
		}
	case errors.Is(deleteErr, store.ErrTemplateInUse):
		// The run won: it must exist and its template must survive.
		if created != nil {
			t.Fatalf("delete reported the run as blocking, but the run failed: %v", created)
		}
		if templateErr != nil {
			t.Fatalf("template must survive a refused delete: %v", templateErr)
		}
	default:
		t.Fatalf("unexpected delete error: %v", deleteErr)
	}
}

func TestDeleteTemplateIgnoresRunsOnOtherTemplates(t *testing.T) {
	ctx := t.Context()
	fs := newStore(t)

	keep, err := fs.PutTemplate(ctx, storetest.Template())
	if err != nil {
		t.Fatal(err)
	}
	other := storetest.Template()
	other.ID = "other"
	otherRecord, err := fs.PutTemplate(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	// An unfinished run on a different template must not block this delete.
	putRunWithStatus(t, fs, "run-other", otherRecord.Ref, state.RunStatusRunning)

	if err := fs.DeleteTemplate(ctx, "demo"); err != nil {
		t.Fatalf("delete should ignore runs on other templates: %v", err)
	}
	if _, err := fs.GetTemplate(ctx, otherRecord.Ref); err != nil {
		t.Fatalf("unrelated template was affected: %v", err)
	}
	if _, err := fs.GetTemplate(ctx, keep.Ref); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("target template still readable: %v", err)
	}
}
