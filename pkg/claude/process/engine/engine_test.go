package engine

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestInitializeExactV1CheckpointShape(t *testing.T) {
	tmpl := sequentialTemplate("task")
	checkpoint, err := Initialize("run-1", tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"runId":"run-1","status":"running","nodes":{"end":"pending","start":"ready","task":"pending"}}`
	if string(encoded) != want {
		t.Fatalf("checkpoint JSON\n got: %s\nwant: %s", encoded, want)
	}
}

func TestSequentialProgramsProgressToSuccessfulEnd(t *testing.T) {
	tmpl := sequentialTemplate("first", "second")
	tmpl.Nodes["first"] = programTask("second", "printf", "hello {{ params.name }}")
	tmpl.Params = map[string]model.Param{"name": {Type: "string"}}
	params := map[string]string{"name": "world"}

	initial, err := Initialize("run-1", tmpl, params)
	if err != nil {
		t.Fatal(err)
	}
	if command, err := Plan(initial, tmpl, params); err != nil || command != nil {
		t.Fatalf("plan before start advancement = %#v, %v", command, err)
	}
	firstRunning, err := AdvanceUntilQuiescent(initial, tmpl, params)
	if err != nil {
		t.Fatal(err)
	}
	if initial.Nodes["start"] != NodeReady || initial.OutstandingCommand != nil {
		t.Fatalf("advance mutated input: %#v", initial)
	}
	first := firstRunning.OutstandingCommand
	if first == nil || first.ID != "cmd_5_run-1_5_first_program" || first.NodeID != "first" {
		t.Fatalf("first command = %#v", first)
	}
	if first.Program.Run != "printf" || !reflect.DeepEqual(first.Program.Args, []string{"hello world"}) {
		t.Fatalf("bound program = %#v", first.Program)
	}
	replanned, err := Plan(firstRunning, tmpl, params)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replanned, first) {
		t.Fatalf("outstanding replanning changed command\n got: %#v\nwant: %#v", replanned, first)
	}

	secondReady, err := Apply(firstRunning, tmpl, params, observed(first, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if secondReady.Nodes["first"] != NodeDone || secondReady.Nodes["second"] != NodeReady || secondReady.OutstandingCommand != nil {
		t.Fatalf("state after first observation = %#v", secondReady)
	}
	plannedOnce, err := Plan(secondReady, tmpl, params)
	if err != nil {
		t.Fatal(err)
	}
	plannedTwice, err := Plan(secondReady, tmpl, params)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plannedOnce, plannedTwice) || plannedOnce.ID != "cmd_5_run-1_6_second_program" {
		t.Fatalf("ready-state replanning is unstable: %#v / %#v", plannedOnce, plannedTwice)
	}

	secondRunning, err := AdvanceUntilQuiescent(secondReady, tmpl, params)
	if err != nil {
		t.Fatal(err)
	}
	endReady, err := Apply(secondRunning, tmpl, params, observed(secondRunning.OutstandingCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	completed, err := AdvanceUntilQuiescent(endReady, tmpl, params)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != RunCompleted || completed.OutstandingCommand != nil {
		t.Fatalf("completed checkpoint = %#v", completed)
	}
	for nodeID, status := range completed.Nodes {
		if status != NodeDone {
			t.Fatalf("terminal node %q = %q", nodeID, status)
		}
	}
	if command, err := Plan(completed, tmpl, params); err != nil || command != nil {
		t.Fatalf("terminal plan = %#v, %v", command, err)
	}
}

func TestProgramFailureTerminatesRun(t *testing.T) {
	tmpl := sequentialTemplate("task", "never")
	checkpoint, err := Initialize("run-fail", tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = AdvanceUntilQuiescent(checkpoint, tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := Apply(checkpoint, tmpl, nil, observed(checkpoint.OutstandingCommand, ProgramFailed, 7))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != RunFailed || failed.Nodes["task"] != NodeFailed || failed.Nodes["never"] != NodePending || failed.OutstandingCommand != nil {
		t.Fatalf("failed checkpoint = %#v", failed)
	}
	quiescent, err := AdvanceUntilQuiescent(failed, tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(quiescent, failed) {
		t.Fatalf("failed state advanced: %#v", quiescent)
	}
}

func TestInitializeRejectsMissingOrBlankProgramBindingsAcrossWholeRun(t *testing.T) {
	for _, test := range []struct {
		name   string
		run    string
		params map[string]string
	}{
		{name: "missing whole executable", run: "{{ params.command }}"},
		{name: "blank whole executable", run: "{{ params.command }}", params: map[string]string{"command": "  "}},
		{name: "missing partial executable", run: "tools/{{ params.command }}"},
		{name: "blank partial executable", run: "tools/{{ params.command }}", params: map[string]string{"command": ""}},
	} {
		t.Run(test.name, func(t *testing.T) {
			tmpl := sequentialTemplate("first", "later")
			tmpl.Params = map[string]model.Param{"command": {Type: "string"}}
			tmpl.Nodes["later"] = programTask("end", test.run)
			if _, err := Initialize("run-bind", tmpl, test.params); !errors.Is(err, ErrInvalidProgramBinding) {
				t.Fatalf("Initialize error = %v", err)
			}
		})
	}
}

func TestDuplicateAndStaleObservationsAreRefused(t *testing.T) {
	tmpl := sequentialTemplate("task", "next")
	checkpoint, err := Initialize("run-stale", tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = AdvanceUntilQuiescent(checkpoint, tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	stale := observed(checkpoint.OutstandingCommand, ProgramSucceeded, 0)
	stale.Observation.CommandID += "-old"
	if _, err := Apply(checkpoint, tmpl, nil, stale); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("stale error = %v", err)
	}
	wrongNode := observed(checkpoint.OutstandingCommand, ProgramSucceeded, 0)
	wrongNode.Observation.NodeID = "next"
	if _, err := Apply(checkpoint, tmpl, nil, wrongNode); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("wrong-node error = %v", err)
	}

	accepted := observed(checkpoint.OutstandingCommand, ProgramSucceeded, 0)
	next, err := Apply(checkpoint, tmpl, nil, accepted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(next, tmpl, nil, accepted); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("duplicate error = %v", err)
	}
	nextRunning, err := AdvanceUntilQuiescent(next, tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(nextRunning, tmpl, nil, accepted); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("old-command error = %v", err)
	}

	initial, err := Initialize("run-unsolicited", tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	unsolicited := Transition{Kind: TransitionProgramObserved, Observation: &ProgramObservation{
		CommandID: "cmd_14_run-unsolicited_4_task_program",
		NodeID:    "task",
		Outcome:   ProgramSucceeded,
	}}
	if _, err := Apply(initial, tmpl, nil, unsolicited); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("unsolicited error = %v", err)
	}
}

func TestReducerRejectsForgedCommandWithoutMutatingInput(t *testing.T) {
	tmpl := sequentialTemplate("task")
	checkpoint, err := Initialize("run-forged", tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = Apply(checkpoint, tmpl, nil, Transition{Kind: TransitionAdvance})
	if err != nil {
		t.Fatal(err)
	}
	before := cloneCheckpoint(checkpoint)
	command, err := Plan(checkpoint, tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	command.Program.Run = "something-else"
	if _, err := Apply(checkpoint, tmpl, nil, Transition{Kind: TransitionCommandPlanned, Command: command}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("forged command error = %v", err)
	}
	if !reflect.DeepEqual(checkpoint, before) {
		t.Fatalf("reducer mutated rejected input\n got: %#v\nwant: %#v", checkpoint, before)
	}
}

func TestDecodeAndReducerRejectMalformedOrInvalidCheckpoint(t *testing.T) {
	tmpl := sequentialTemplate("task")
	checkpoint, err := Initialize("run-load", tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpoint(valid, tmpl, nil); err != nil {
		t.Fatalf("valid decode: %v", err)
	}
	unknown := []byte(strings.TrimSuffix(string(valid), "}") + `,"surprise":true}`)
	if _, err := DecodeCheckpoint(unknown, tmpl, nil); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("unknown-field error = %v", err)
	}
	if _, err := DecodeCheckpoint(append(valid, []byte(` {}`)...), tmpl, nil); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("trailing-value error = %v", err)
	}
	for _, duplicate := range []string{
		`{"version":1,"version":1,"runId":"run-load","status":"running","nodes":{"end":"pending","start":"ready","task":"pending"}}`,
		`{"version":1,"runId":"run-load","status":"running","nodes":{"end":"pending","start":"ready","task":"pending","t\u0061sk":"pending"}}`,
	} {
		if _, err := DecodeCheckpoint([]byte(duplicate), tmpl, nil); !errors.Is(err, ErrInvalidCheckpoint) {
			t.Fatalf("duplicate-member error = %v", err)
		}
	}

	checkpoint.Nodes["start"] = NodePending
	invalid, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpoint(invalid, tmpl, nil); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("semantic decode error = %v", err)
	}
	if _, err := Apply(checkpoint, tmpl, nil, Transition{Kind: TransitionAdvance}); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("invalid loaded reducer error = %v", err)
	}
}

func TestAdvanceUntilQuiescentRefusesPartialStateOnBudgetExhaustion(t *testing.T) {
	tmpl := sequentialTemplate("task")
	checkpoint, err := Initialize("run-budget", tmpl, nil)
	if err != nil {
		t.Fatal(err)
	}
	def, err := newDefinition(tmpl)
	if err != nil {
		t.Fatal(err)
	}
	got, err := advanceUntilQuiescent(checkpoint, def, nil, 1)
	if !errors.Is(err, ErrTransitionBudgetExhausted) {
		t.Fatalf("budget error = %v", err)
	}
	if !reflect.DeepEqual(got, checkpoint) {
		t.Fatalf("budget exhaustion exposed partial state\n got: %#v\nwant: %#v", got, checkpoint)
	}
}

func TestEndResultSelectsTerminalRunStatus(t *testing.T) {
	for _, test := range []struct {
		result string
		want   RunStatus
	}{
		{"", RunCompleted},
		{"failed", RunFailed},
		{"canceled", RunCanceled},
	} {
		t.Run(test.result, func(t *testing.T) {
			tmpl := sequentialTemplate("task")
			end := tmpl.Nodes["end"]
			end.Result = test.result
			tmpl.Nodes["end"] = end
			checkpoint, err := Initialize("run-end", tmpl, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = AdvanceUntilQuiescent(checkpoint, tmpl, nil)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = Apply(checkpoint, tmpl, nil, observed(checkpoint.OutstandingCommand, ProgramSucceeded, 0))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, err = AdvanceUntilQuiescent(checkpoint, tmpl, nil)
			if err != nil {
				t.Fatal(err)
			}
			if checkpoint.Status != test.want {
				t.Fatalf("status = %q, want %q", checkpoint.Status, test.want)
			}
		})
	}
}

func observed(command *Command, outcome ProgramOutcome, exitCode int) Transition {
	return Transition{
		Kind: TransitionProgramObserved,
		Observation: &ProgramObservation{
			CommandID: command.ID,
			NodeID:    command.NodeID,
			Outcome:   outcome,
			ExitCode:  exitCode,
		},
	}
}

func sequentialTemplate(taskIDs ...string) *model.Template {
	nodes := map[string]model.Node{
		"start": {Type: model.NodeTypeStart},
		"end":   {Type: model.NodeTypeEnd},
	}
	previous := "start"
	for _, taskID := range taskIDs {
		node := nodes[previous]
		node.Next = model.Next{model.DefaultOutcome: taskID}
		nodes[previous] = node
		nodes[taskID] = programTask("end", "true")
		previous = taskID
	}
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "sequential",
		Start:      "start",
		Nodes:      nodes,
	}
}

func programTask(next, run string, args ...string) model.Node {
	return model.Node{
		Type:      model.NodeTypeTask,
		Performer: &model.Performer{Kind: model.PerformerProgram, Run: run, Args: args},
		Next:      model.Next{model.DefaultOutcome: next},
	}
}
