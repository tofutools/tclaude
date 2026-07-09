package processcmd

import (
	"bytes"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

func TestObserveRecordsOutstandingResultAndResumesReconcilePause(t *testing.T) {
	root := t.TempDir()
	fs, err := store.NewFS(root)
	if err != nil {
		t.Fatal(err)
	}
	performer := model.Performer{Kind: model.PerformerProgram, Run: "/lost-result"}
	tmpl := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "observe-demo",
		Start:      "work",
		Nodes: map[string]model.Node{
			"work": {Type: model.NodeTypeTask, Performer: &performer, Next: model.Next{"pass": "end"}},
			"end":  {Type: model.NodeTypeEnd},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	initial := state.New("observe-run", record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "end", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	initial.Status = state.RunStatusRunning
	if _, err := fs.CreateRun(t.Context(), store.RunRecord{ID: "observe-run", TemplateRef: record.Ref}, initial); err != nil {
		t.Fatal(err)
	}
	commands, err := plan.Plan(&initial, tmpl)
	if err != nil || len(commands) != 1 {
		t.Fatalf("plan = %#v, err = %v", commands, err)
	}
	at := time.Date(2026, 7, 9, 21, 30, 0, 0, time.UTC)
	outstanding, err := commands[0].OutstandingCommand(at)
	if err != nil {
		t.Fatal(err)
	}
	outstanding.ReconcileAfter = at
	result, err := fs.Append(t.Context(), "observe-run", 0, []evidence.LogEntry{
		nodeLogEntry("work", evidence.EntryKindGate, state.Event{Type: state.EventCommandIssued, Command: &outstanding}, "", at),
		nodeLogEntry("work", evidence.EntryKindAttempt, state.Event{Type: state.EventNodeAttemptStarted, Attempt: 1, CommandID: commands[0].ID}, "", at),
	})
	if err != nil {
		t.Fatal(err)
	}
	pause := state.PauseState{
		Kind: state.PauseKindNeedsReconcile, Reason: "needs human verdict", CommandID: commands[0].ID, Owner: "human:operator",
	}
	if _, err := fs.Append(t.Context(), "observe-run", result.State.LastLogSeq, []evidence.LogEntry{
		runLogEntry(evidence.EntryKindGate, state.Event{Type: state.EventRunPaused, Pause: &pause}, "", at),
	}); err != nil {
		t.Fatal(err)
	}

	oldNow := processNow
	processNow = func() time.Time { return at.Add(time.Minute) }
	t.Cleanup(func() { processNow = oldNow })
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(t.Context())
	if err := runObserve(cmd, &observeParams{
		RunID: "observe-run", CommandID: commands[0].ID, StoreRoot: root,
		Verdict: "pass", EvidenceRef: "artifact:human-confirmation", Actor: "human:test",
	}, &out); err != nil {
		t.Fatal(err)
	}
	snapshot, err := fs.LoadRun(t.Context(), "observe-run")
	if err != nil {
		t.Fatal(err)
	}
	observed := snapshot.State.OutstandingCommands[commands[0].ID]
	if observed.Status != state.CommandStatusObserved || observed.Actor != "human:test" || observed.Verdict != "pass" || observed.EvidenceRef != "artifact:human-confirmation" {
		t.Fatalf("observation = %#v", observed)
	}
	if snapshot.State.Status != state.RunStatusRunning || snapshot.State.Pause != nil {
		t.Fatalf("state = %#v", snapshot.State)
	}
}
