package processexec

import (
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
	processverify "github.com/tofutools/tclaude/pkg/claude/process/verify"
)

// compoundExecutorFixture stages a program-performer compound node: a do
// stage, one check gate, and a review gate, all real programs, so Drive
// exercises expansion, stage sequencing, and gate settlement end to end.
func compoundExecutorFixture(t *testing.T, checkScript string) (*store.FS, store.Snapshot) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	program := func(script string) *model.Performer {
		return &model.Performer{Kind: model.PerformerProgram, Run: "/bin/sh", Args: []string{"-c", script}}
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "executor-compound",
		Start:      "work",
		Nodes: map[string]model.Node{
			"work": {
				Type:      model.NodeTypeTask,
				Performer: program("exit 0"),
				Checks:    []model.Step{{ID: "tests", Performer: *program(checkScript)}},
				Review:    &model.Step{ID: "review", Performer: *program("exit 0")},
				Next:      model.Next{"pass": "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_compound"
	initial := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "end", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	initial.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: runID, TemplateRef: record.Ref}, initial); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.Append(t.Context(), runID, 0, []evidence.LogEntry{{
		At:    executorTestTime,
		Scope: evidence.Scope{Kind: evidence.ScopeRun},
		Kind:  evidence.EntryKindAdmin,
		Event: &state.Event{
			Type:   state.EventAdminProgramsAllowed,
			At:     executorTestTime,
			Actor:  "human:test",
			Reason: "test program opt-in",
		},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.SetProgramsAllowed(t.Context(), runID); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return fs, snapshot
}

func TestDriveCompoundNodeThroughGates(t *testing.T) {
	fs, snapshot := compoundExecutorFixture(t, "exit 0")
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusCompleted {
		t.Fatalf("finished status = %s", finished.State.Status)
	}
	parent := finished.State.Nodes["work"]
	wantChildren := []string{"work.do", "work.test.tests", "work.review", "work.done"}
	if parent.Status != state.NodeStatusCompleted || strings.Join(parent.Children, " ") != strings.Join(wantChildren, " ") {
		t.Fatalf("parent = %#v", parent)
	}
	for _, childID := range wantChildren {
		child := finished.State.Nodes[childID]
		if child.Status != state.NodeStatusCompleted {
			t.Fatalf("child %s = %#v", childID, child)
		}
		if child.Stage == model.StageDone {
			continue
		}
		if child.ActiveAttempt == nil || child.ActiveAttempt.EvidenceRef == "" || child.ActiveAttempt.Actor != "program:/bin/sh@exit0" {
			t.Fatalf("child %s attempt = %#v", childID, child.ActiveAttempt)
		}
	}
	if report := processverify.StoreRun(t.Context(), fs, snapshot.Run.ID); report.HasErrors() {
		t.Fatalf("verify after drive: %#v", report.Diagnostics)
	}
}

func TestDriveCompoundGateFailurePoisonsToBlocked(t *testing.T) {
	fs, snapshot := compoundExecutorFixture(t, "exit 1")
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Poison blocks the gate child and the parent mirror atomically; the run
	// itself keeps running for a human or decision node to resolve.
	if finished.State.Status != state.RunStatusRunning {
		t.Fatalf("finished status = %s", finished.State.Status)
	}
	gate := finished.State.Nodes["work.test.tests"]
	parent := finished.State.Nodes["work"]
	if gate.Status != state.NodeStatusBlocked || parent.Status != state.NodeStatusBlocked {
		t.Fatalf("gate = %#v, parent = %#v", gate, parent)
	}
	for _, node := range []state.NodeState{gate, parent} {
		if !strings.Contains(node.BlockedReason, `gate "work.test.tests" failed`) || node.BlockedOwner == "" {
			t.Fatalf("blocked node = %#v", node)
		}
	}
	if finished.State.Nodes["work.review"].Status != state.NodeStatusPending {
		t.Fatalf("review must not run after a poisoned check: %#v", finished.State.Nodes["work.review"])
	}
	if report := processverify.StoreRun(t.Context(), fs, snapshot.Run.ID); report.HasErrors() {
		t.Fatalf("verify after poison: %#v", report.Diagnostics)
	}
}
