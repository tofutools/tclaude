package store_test

import (
	"errors"
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
