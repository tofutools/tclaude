package processexec

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/evidence"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/plan"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

var executorTestTime = time.Date(2026, 7, 9, 18, 0, 0, 0, time.UTC)

type fakeAdapter struct {
	observation Observation
	requests    []Request
}

func (a *fakeAdapter) Validate(Request) error { return nil }

func (a *fakeAdapter) Perform(_ context.Context, request Request) (Observation, error) {
	a.requests = append(a.requests, request)
	return a.observation, nil
}

func TestExecutePerformerCommandRecordsObservationAndSettlement(t *testing.T) {
	fs, snapshot := executorFixture(t, true, model.Performer{Kind: model.PerformerProgram, Run: "/fake"})
	adapter := &fakeAdapter{observation: Observation{
		Actor:   "program:fake@exit0",
		Verdict: "pass",
		Evidence: &Artifact{
			Name: "fake.json",
			Data: []byte("fake evidence"),
		},
	}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerProgram: adapter})
	executor.Now = func() time.Time { return executorTestTime }

	commands, err := plan.Plan(snapshot.State, mustTemplate(t, fs, snapshot.Run.TemplateRef))
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 {
		t.Fatalf("commands = %#v", commands)
	}
	result, err := executor.Execute(t.Context(), commands[0])
	if err != nil {
		t.Fatal(err)
	}
	if !result.Claimed || result.Observation == nil || len(adapter.requests) != 1 {
		t.Fatalf("result = %#v, requests = %d", result, len(adapter.requests))
	}
	observed := result.State.OutstandingCommands[commands[0].ID]
	if observed.Status != state.CommandStatusObserved || observed.Actor != "program:fake@exit0" || observed.Verdict != "pass" || observed.EvidenceRef == "" {
		t.Fatalf("observed command = %#v", observed)
	}
	if adapter.requests[0].Input.RunID != snapshot.Run.ID || adapter.requests[0].Input.NodeID != "work" || adapter.requests[0].Input.Params["ticket"] != "TCL-274" {
		t.Fatalf("adapter request = %#v", adapter.requests[0])
	}

	nextSnapshot, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	commands, err = plan.Plan(nextSnapshot.State, mustTemplate(t, fs, snapshot.Run.TemplateRef))
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].Kind != plan.CommandKindSettleAttempt {
		t.Fatalf("settle commands = %#v", commands)
	}
	if _, err := executor.Execute(t.Context(), commands[0]); err != nil {
		t.Fatal(err)
	}
	settled, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	attempt := settled.State.Nodes["work"].ActiveAttempt
	if attempt == nil || settled.State.Nodes["work"].Status != state.NodeStatusCompleted || attempt.Actor != "program:fake@exit0" || attempt.Outcome != "pass" || attempt.EvidenceRef == "" {
		t.Fatalf("settled attempt = %#v, node = %#v", attempt, settled.State.Nodes["work"])
	}
}

func TestClaimedCommandIsNeverReperformedAfterCrash(t *testing.T) {
	fs, snapshot := executorFixture(t, true, model.Performer{Kind: model.PerformerProgram, Run: "/fake"})
	adapter := &fakeAdapter{observation: Observation{Actor: "program:fake@exit0", Verdict: "pass"}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerProgram: adapter})
	executor.Now = func() time.Time { return executorTestTime }
	commands, err := plan.Plan(snapshot.State, mustTemplate(t, fs, snapshot.Run.TemplateRef))
	if err != nil {
		t.Fatal(err)
	}
	claimed, _, err := executor.claim(t.Context(), snapshot, commands[0])
	if err != nil || !claimed {
		t.Fatalf("claim = %v, err = %v", claimed, err)
	}

	result, err := executor.Execute(t.Context(), commands[0])
	if err != nil {
		t.Fatal(err)
	}
	if result.Claimed || len(adapter.requests) != 0 {
		t.Fatalf("reclaimed command result = %#v, adapter calls = %d", result, len(adapter.requests))
	}
	loaded, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if command := loaded.State.OutstandingCommands[commands[0].ID]; command.Status != state.CommandStatusIssued || command.IdempotencyKey != commands[0].IdempotencyKey || command.PayloadHash == "" || len(command.Payload) == 0 {
		t.Fatalf("outstanding command = %#v", command)
	}
	reconciled, err := executor.RecordOutstandingObservation(t.Context(), snapshot.Run.ID, commands[0].ID, Observation{
		Actor:   "program:fake@exit0",
		Verdict: "pass",
		Evidence: &Artifact{
			Name: "recovered.json",
			Data: []byte("recovered after crash"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.OutstandingCommands[commands[0].ID].Status != state.CommandStatusObserved {
		t.Fatalf("reconciled command = %#v", reconciled.OutstandingCommands[commands[0].ID])
	}
	retriedObservation, err := executor.RecordOutstandingObservation(t.Context(), snapshot.Run.ID, commands[0].ID, Observation{
		Actor:   "program:fake@exit0",
		Verdict: "pass",
	})
	if err != nil {
		t.Fatal(err)
	}
	if retriedObservation.LastLogSeq != reconciled.LastLogSeq {
		t.Fatalf("observation retry appended state: first seq %d, retry seq %d", reconciled.LastLogSeq, retriedObservation.LastLogSeq)
	}
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusCompleted || len(adapter.requests) != 0 {
		t.Fatalf("finished status = %s, adapter calls = %d", finished.State.Status, len(adapter.requests))
	}
}

func TestIssuedInternalCommandResumesFromDurablePayload(t *testing.T) {
	fs, snapshot := executorFixture(t, true, model.Performer{Kind: model.PerformerProgram, Run: "/fake"})
	adapter := &fakeAdapter{observation: Observation{Actor: "program:fake@exit0", Verdict: "pass"}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerProgram: adapter})
	commands, err := plan.Plan(snapshot.State, mustTemplate(t, fs, snapshot.Run.TemplateRef))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(t.Context(), commands[0]); err != nil {
		t.Fatal(err)
	}
	running, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	commands, err = plan.Plan(running.State, mustTemplate(t, fs, snapshot.Run.TemplateRef))
	if err != nil {
		t.Fatal(err)
	}
	if len(commands) != 1 || commands[0].Kind != plan.CommandKindSettleAttempt {
		t.Fatalf("settle commands = %#v", commands)
	}
	settle := commands[0]
	claimed, _, err := executor.claim(t.Context(), running, settle)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, err = %v", claimed, err)
	}
	altered := settle
	altered.MaxAttempts++
	altered.Performer = &model.Performer{Kind: model.PerformerProgram, Run: "/forged"}
	if _, err := executor.ResumeIssued(t.Context(), altered); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected altered internal command refusal, got %v", err)
	}
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusCompleted || finished.State.Nodes["work"].Status != state.NodeStatusCompleted {
		t.Fatalf("resumed state = %#v", finished.State)
	}
	retried, err := executor.ResumeOutstanding(t.Context(), snapshot.Run.ID, settle.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retried.LastLogSeq != finished.State.LastLogSeq {
		t.Fatalf("retry appended state: first seq %d, retry seq %d", finished.State.LastLogSeq, retried.LastLogSeq)
	}
}

func TestExecuteRejectsForgedProgramCommand(t *testing.T) {
	fs, snapshot := executorFixture(t, true, model.Performer{Kind: model.PerformerProgram, Run: "/reviewed"})
	adapter := &fakeAdapter{observation: Observation{Actor: "program:forged@exit0", Verdict: "pass"}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerProgram: adapter})
	commands, err := plan.Plan(snapshot.State, mustTemplate(t, fs, snapshot.Run.TemplateRef))
	if err != nil {
		t.Fatal(err)
	}
	forged := commands[0]
	forged.Performer = &model.Performer{Kind: model.PerformerProgram, Run: "/forged"}
	_, err = executor.Execute(t.Context(), forged)
	if err == nil || !strings.Contains(err.Error(), "not a current planner output") {
		t.Fatalf("expected forged-command refusal, got %v", err)
	}
	loaded, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(adapter.requests) != 0 || len(loaded.State.OutstandingCommands) != 0 {
		t.Fatalf("adapter calls = %d, outstanding = %#v", len(adapter.requests), loaded.State.OutstandingCommands)
	}
}

func TestDriveDoesNotResumeInternalCommandAfterCancellation(t *testing.T) {
	fs, snapshot := executorFixture(t, true, model.Performer{Kind: model.PerformerProgram, Run: "/fake"})
	adapter := &fakeAdapter{observation: Observation{Actor: "program:fake@exit0", Verdict: "pass"}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerProgram: adapter})
	tmpl := mustTemplate(t, fs, snapshot.Run.TemplateRef)

	commands, err := plan.Plan(snapshot.State, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(t.Context(), commands[0]); err != nil {
		t.Fatal(err)
	}
	running, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	commands, err = plan.Plan(running.State, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.Execute(t.Context(), commands[0]); err != nil {
		t.Fatal(err)
	}
	completed, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	commands, err = plan.Plan(completed.State, tmpl)
	if err != nil {
		t.Fatal(err)
	}
	var activate, complete plan.Command
	for _, command := range commands {
		switch command.Kind {
		case plan.CommandKindActivateNode:
			activate = command
		case plan.CommandKindCompleteRun:
			complete = command
		}
	}
	if activate.ID == "" || complete.ID == "" {
		t.Fatalf("terminal commands = %#v", commands)
	}
	if _, err := executor.Execute(t.Context(), activate); err != nil {
		t.Fatal(err)
	}
	beforeClaim, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, claimState, err := executor.claim(t.Context(), beforeClaim, complete)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, err = %v", claimed, err)
	}
	_, err = fs.Append(t.Context(), snapshot.Run.ID, claimState.LastLogSeq, []evidence.LogEntry{{
		At:    executorTestTime.Add(time.Minute),
		Scope: evidence.Scope{Kind: evidence.ScopeRun},
		Kind:  evidence.EntryKindAdmin,
		Event: &state.Event{
			Type:      state.EventRunStatusSet,
			At:        executorTestTime.Add(time.Minute),
			RunStatus: state.RunStatusCanceled,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusCanceled || finished.State.OutstandingCommands[complete.ID].Status != state.CommandStatusIssued {
		t.Fatalf("canceled state = %#v", finished.State)
	}
	if _, err := executor.ResumeOutstanding(t.Context(), snapshot.Run.ID, complete.ID); err == nil || !strings.Contains(err.Error(), "cannot be resumed") {
		t.Fatalf("expected canceled-run resume refusal, got %v", err)
	}
}

func TestExecuteRefusesProgramWithoutRunOptIn(t *testing.T) {
	fs, snapshot := executorFixture(t, false, model.Performer{Kind: model.PerformerProgram, Run: "/fake"})
	adapter := &fakeAdapter{observation: Observation{Actor: "program:fake@exit0", Verdict: "pass"}}
	executor := New(fs, map[model.PerformerKind]Adapter{model.PerformerProgram: adapter})
	commands, err := plan.Plan(snapshot.State, mustTemplate(t, fs, snapshot.Run.TemplateRef))
	if err != nil {
		t.Fatal(err)
	}
	_, err = executor.Execute(t.Context(), commands[0])
	if err == nil || !strings.Contains(err.Error(), "--allow-programs") {
		t.Fatalf("expected opt-in refusal, got %v", err)
	}
	loaded, err := fs.LoadRun(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(adapter.requests) != 0 || len(loaded.State.OutstandingCommands) != 0 {
		t.Fatalf("adapter calls = %d, outstanding = %#v", len(adapter.requests), loaded.State.OutstandingCommands)
	}
}

func TestDriveProgramTaskToCompletionWithEvidence(t *testing.T) {
	performer := model.Performer{
		Kind: model.PerformerProgram,
		Run:  "/bin/sh",
		Args: []string{"-c", "printf %s \"$TCLAUDE_PROCESS_COMMAND_ID\"; printf program-err >&2"},
	}
	fs, snapshot := executorFixture(t, true, performer)
	executor := New(fs, map[model.PerformerKind]Adapter{
		model.PerformerProgram: ProgramAdapter{DefaultTimeout: 5 * time.Second},
	})
	finished, err := executor.Drive(t.Context(), snapshot.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if finished.State.Status != state.RunStatusCompleted || finished.State.Nodes["work"].Status != state.NodeStatusCompleted {
		t.Fatalf("finished state = %#v", finished.State)
	}
	attempt := finished.State.Nodes["work"].ActiveAttempt
	if attempt == nil || attempt.EvidenceRef == "" || attempt.Actor != "program:/bin/sh@exit0" {
		t.Fatalf("attempt = %#v", attempt)
	}
	reader, err := fs.GetArtifact(t.Context(), snapshot.Run.ID, attempt.EvidenceRef)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var programEvidence ProgramEvidence
	if err := json.Unmarshal(body, &programEvidence); err != nil {
		t.Fatal(err)
	}
	if programEvidence.ExitCode != 0 || programEvidence.StdoutTail != programEvidence.CommandID || programEvidence.StderrTail != "program-err" || programEvidence.CommandID == "" || programEvidence.IdempotencyKey == "" {
		t.Fatalf("program evidence = %#v", programEvidence)
	}
}

func executorFixture(t *testing.T, allowPrograms bool, performer model.Performer) (*store.FS, store.Snapshot) {
	t.Helper()
	fs, err := store.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "executor-demo",
		Start:      "work",
		Nodes: map[string]model.Node{
			"work": {
				Type:      model.NodeTypeTask,
				Performer: &performer,
				Next:      model.Next{"pass": "end", "fail": "failed"},
			},
			"end":    {Type: model.NodeTypeEnd},
			"failed": {Type: model.NodeTypeEnd, Result: "failed"},
		},
	}
	record, err := fs.PutTemplate(t.Context(), tmpl)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_executor"
	initial := state.New(runID, record.Ref, record.Ref, []state.NodeInit{
		{ID: "work", Type: model.NodeTypeTask, Status: state.NodeStatusReady},
		{ID: "end", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
		{ID: "failed", Type: model.NodeTypeEnd, Status: state.NodeStatusPending},
	})
	initial.Status = state.RunStatusRunning
	_, err = fs.CreateRun(t.Context(), store.RunRecord{
		ID:          runID,
		TemplateRef: record.Ref,
		Params:      map[string]string{"ticket": "TCL-274"},
	}, initial)
	if err != nil {
		t.Fatal(err)
	}
	if allowPrograms {
		_, err = fs.Append(t.Context(), runID, 0, []evidence.LogEntry{{
			At:    executorTestTime,
			Scope: evidence.Scope{Kind: evidence.ScopeRun},
			Kind:  evidence.EntryKindAdmin,
			Event: &state.Event{
				Type:   state.EventAdminProgramsAllowed,
				At:     executorTestTime,
				Actor:  "human:test",
				Reason: "test program opt-in",
			},
		}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := fs.SetProgramsAllowed(t.Context(), runID); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := fs.LoadRun(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return fs, snapshot
}

func mustTemplate(t *testing.T, fs store.Store, ref string) *model.Template {
	t.Helper()
	tmpl, err := fs.GetTemplate(t.Context(), ref)
	if err != nil {
		t.Fatal(err)
	}
	return tmpl
}
