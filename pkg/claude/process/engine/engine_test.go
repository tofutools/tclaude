package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

func TestInitializeExactV2CheckpointShape(t *testing.T) {
	tmpl := sequentialTemplate("task")
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-1", definition)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	// The durable outbox arrays are parallel-ready plural fields without
	// omitempty, so an empty pre-execution checkpoint serializes them as null.
	want := `{"version":2,"runId":"run-1","status":"running",` +
		`"nodes":{"end":"pending","start":"ready","task":"pending"},` +
		`"edges":{"start":{"next":"unresolved"},"task":{"next":"unresolved"}},` +
		`"awaitingDecisions":null,"commands":null}`
	if string(encoded) != want {
		t.Fatalf("checkpoint JSON\n got: %s\nwant: %s", encoded, want)
	}
}

func TestSequentialProgramsProgressToSuccessfulEnd(t *testing.T) {
	tmpl := sequentialTemplate("first", "second")
	tmpl.Nodes["first"] = programTask("second", "printf", "hello {{ params.name }}")
	tmpl.Params = map[string]model.Param{"name": {Type: "string"}}
	params := map[string]string{"name": "world"}
	definition := mustPrepare(t, tmpl, params)

	initial, err := Initialize("run-1", definition)
	if err != nil {
		t.Fatal(err)
	}
	if _, plannable := nextPlannableTask(initial, definition); plannable {
		t.Fatal("a task was plannable before the start node advanced")
	}
	firstRunning, first := advanceAndPlan(t, initial, definition)
	if initial.Nodes["start"] != NodeReady || len(initial.Commands) != 0 {
		t.Fatalf("advance mutated input: %#v", initial)
	}
	if first == nil || first.ID != "cmd_5_run-1_5_first_program" || first.NodeID != "first" {
		t.Fatalf("first command = %#v", first)
	}
	if first.Program.Run != "printf" || !reflect.DeepEqual(first.Program.Args, []string{"hello world"}) {
		t.Fatalf("bound program = %#v", first.Program)
	}
	// Planning again while that command is outstanding must neither duplicate it
	// nor mint a second one: the task is running, so nothing is plannable.
	replanned, again := advanceAndPlan(t, firstRunning, definition)
	if again != nil {
		t.Fatalf("outstanding command was replanned: %#v", again)
	}
	if !reflect.DeepEqual(replanned.Commands, firstRunning.Commands) {
		t.Fatalf("replanning changed the outbox\n got: %#v\nwant: %#v", replanned.Commands, firstRunning.Commands)
	}

	secondReady, err := Apply(firstRunning, definition, observed(first, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if secondReady.Nodes["first"] != NodeDone || secondReady.Nodes["second"] != NodeReady || len(secondReady.Commands) != 0 {
		t.Fatalf("state after first observation = %#v", secondReady)
	}
	_, plannedOnce := advanceAndPlan(t, secondReady, definition)
	_, plannedTwice := advanceAndPlan(t, secondReady, definition)
	if !reflect.DeepEqual(plannedOnce, plannedTwice) || plannedOnce.ID != "cmd_5_run-1_6_second_program" {
		t.Fatalf("ready-state planning is unstable: %#v / %#v", plannedOnce, plannedTwice)
	}

	secondRunning, second := advanceAndPlan(t, secondReady, definition)
	endReady, err := Apply(secondRunning, definition, observed(second, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	completed, terminalCommand := advanceAndPlan(t, endReady, definition)
	if completed.Status != RunCompleted || len(completed.Commands) != 0 || terminalCommand != nil {
		t.Fatalf("completed checkpoint = %#v", completed)
	}
	for nodeID, status := range completed.Nodes {
		if status != NodeDone {
			t.Fatalf("terminal node %q = %q", nodeID, status)
		}
	}
}

func TestPreparedDefinitionIsReusedWithoutTemplateValidationOrRebinding(t *testing.T) {
	tmpl := sequentialTemplate("first", "second")
	tmpl.Params = map[string]model.Param{
		"command": {Type: "string"},
		"value":   {Type: "string"},
	}
	tmpl.Nodes["first"] = programTask("second", "{{ params.command }}", "{{ params.value }}")
	performer := tmpl.Nodes["first"].Performer
	params := map[string]string{"command": "printf", "value": "prepared"}
	definition := mustPrepare(t, tmpl, params)

	// Neither mutable input is retained. Making both invalid after preparation
	// must not trigger revalidation or change a later planned command.
	params["command"] = ""
	delete(params, "value")
	performer.Run = "mutated"
	performer.Args[0] = "mutated"
	tmpl.Start = "missing"
	tmpl.Nodes = nil

	checkpoint, err := Initialize("run-prepared", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, planned := advanceAndPlan(t, checkpoint, definition)
	if planned.Program.Run != "printf" || !reflect.DeepEqual(planned.Program.Args, []string{"prepared"}) {
		t.Fatalf("prepared command was rebound: %#v", planned.Program)
	}

	checkpoint, err = Apply(checkpoint, definition, observed(planned, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, second := advanceAndPlan(t, checkpoint, definition)
	checkpoint, err = Apply(checkpoint, definition, observed(second, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("prepared run status = %q", checkpoint.Status)
	}
}

func TestProgramFailureTerminatesRun(t *testing.T) {
	tmpl := sequentialTemplate("task", "never")
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-fail", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	failed, err := Apply(checkpoint, definition, observed(command, ProgramFailed, 7))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != RunFailed || failed.Nodes["task"] != NodeFailed || failed.Nodes["never"] != NodePending || len(failed.Commands) != 0 {
		t.Fatalf("failed checkpoint = %#v", failed)
	}
	quiescent, err := AdvanceUntilQuiescent(failed, definition)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(quiescent, failed) {
		t.Fatalf("failed state advanced: %#v", quiescent)
	}
}

func TestPrepareRejectsMissingOrBlankProgramBindingsAcrossWholeRun(t *testing.T) {
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
			if _, err := Prepare(tmpl, test.params); !errors.Is(err, ErrInvalidProgramBinding) {
				t.Fatalf("Prepare error = %v", err)
			}
		})
	}
}

func TestPrepareRejectsMissingArgumentBinding(t *testing.T) {
	tmpl := sequentialTemplate("task")
	tmpl.Params = map[string]model.Param{"value": {Type: "string"}}
	tmpl.Nodes["task"] = programTask("end", "printf", "{{ params.value }}")

	if _, err := Prepare(tmpl, nil); !errors.Is(err, ErrInvalidProgramBinding) {
		t.Fatalf("Prepare error = %v", err)
	}
}

func TestDuplicateAndStaleObservationsAreRefused(t *testing.T) {
	tmpl := sequentialTemplate("task", "next")
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-stale", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	stale := observed(command, ProgramSucceeded, 0)
	stale.Observation.CommandID += "-old"
	if _, err := Apply(checkpoint, definition, stale); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("stale error = %v", err)
	}
	wrongNode := observed(command, ProgramSucceeded, 0)
	wrongNode.Observation.NodeID = "next"
	if _, err := Apply(checkpoint, definition, wrongNode); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("wrong-node error = %v", err)
	}

	accepted := observed(command, ProgramSucceeded, 0)
	next, err := Apply(checkpoint, definition, accepted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(next, definition, accepted); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("duplicate error = %v", err)
	}
	nextRunning, _ := advanceAndPlan(t, next, definition)
	if _, err := Apply(nextRunning, definition, accepted); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("old-command error = %v", err)
	}

	initial, err := Initialize("run-unsolicited", definition)
	if err != nil {
		t.Fatal(err)
	}
	unsolicited := Transition{Kind: TransitionProgramObserved, Observation: &ProgramObservation{
		CommandID: "cmd_14_run-unsolicited_4_task_program",
		NodeID:    "task",
		Outcome:   ProgramSucceeded,
	}}
	if _, err := Apply(initial, definition, unsolicited); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("unsolicited error = %v", err)
	}
}

func TestReducerRejectsForgedCommandWithoutMutatingInput(t *testing.T) {
	tmpl := sequentialTemplate("task")
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-forged", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err = Apply(checkpoint, definition, advanced("start"))
	if err != nil {
		t.Fatal(err)
	}
	before := cloneCheckpoint(checkpoint)
	_, command := advanceAndPlan(t, checkpoint, definition)
	command.Program.Run = "something-else"
	if _, err := Apply(checkpoint, definition, Transition{Kind: TransitionCommandPlanned, Command: command}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("forged command error = %v", err)
	}
	if !reflect.DeepEqual(checkpoint, before) {
		t.Fatalf("reducer mutated rejected input\n got: %#v\nwant: %#v", checkpoint, before)
	}
}

func TestDecodeAndReducerRejectMalformedOrInvalidCheckpoint(t *testing.T) {
	tmpl := sequentialTemplate("task")
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-load", definition)
	if err != nil {
		t.Fatal(err)
	}
	valid, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpoint(valid, definition); err != nil {
		t.Fatalf("valid decode: %v", err)
	}
	unknown := []byte(strings.TrimSuffix(string(valid), "}") + `,"surprise":true}`)
	if _, err := DecodeCheckpoint(unknown, definition); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("unknown-field error = %v", err)
	}
	if _, err := DecodeCheckpoint(append(valid, []byte(` {}`)...), definition); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("trailing-value error = %v", err)
	}
	for _, duplicate := range []string{
		`{"version":1,"version":1,"runId":"run-load","status":"running","nodes":{"end":"pending","start":"ready","task":"pending"}}`,
		`{"version":1,"runId":"run-load","status":"running","nodes":{"end":"pending","start":"ready","task":"pending","t\u0061sk":"pending"}}`,
	} {
		if _, err := DecodeCheckpoint([]byte(duplicate), definition); !errors.Is(err, ErrInvalidCheckpoint) {
			t.Fatalf("duplicate-member error = %v", err)
		}
	}

	// An unknown node status is a concrete structural violation the trusted
	// load/persist boundary (DecodeCheckpoint) still rejects, unlike the
	// whole-graph slice invariants demoted to ClassifyCheckpoint.
	checkpoint.Nodes["start"] = NodeStatus("bogus")
	invalid, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpoint(invalid, definition); !errors.Is(err, ErrInvalidCheckpoint) {
		t.Fatalf("semantic decode error = %v", err)
	}
	// The reducer no longer re-validates the whole checkpoint on entry — that
	// O(nodes+edges) scan is confined to the load/creation boundary. Handed such
	// a structurally broken state directly it must still fail closed via a local
	// precondition (no panic, no silent progress) rather than act on it.
	if _, err := Apply(checkpoint, definition, advanced("start")); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("invalid loaded reducer error = %v", err)
	}
}

func TestAdvanceUntilQuiescentRefusesPartialStateOnBudgetExhaustion(t *testing.T) {
	// start -> end needs two engine-owned advances, so a budget of one leaves
	// work pending and must surface as exhaustion rather than partial state.
	definition := mustPrepare(t, startToEndTemplate(), nil)
	checkpoint, err := Initialize("run-budget", definition)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := advanceUntilQuiescent(checkpoint, definition, 1)
	if !errors.Is(err, ErrTransitionBudgetExhausted) {
		t.Fatalf("budget error = %v", err)
	}
	if !reflect.DeepEqual(got, checkpoint) {
		t.Fatalf("budget exhaustion exposed partial state\n got: %#v\nwant: %#v", got, checkpoint)
	}
}

// TestTransitionBudgetDerivesFromPreparedGraphSize proves the budget is the
// acyclic node-completion bound of the prepared graph, not a sequential
// constant: a fan-out wide enough to need many engine-owned advances still
// reaches quiescence in one pass.
func TestTransitionBudgetDerivesFromPreparedGraphSize(t *testing.T) {
	definition := mustPrepare(t, wideFanOutTemplate(24), nil)
	if got, want := transitionBudget(definition), len(definition.nodes); got != want {
		t.Fatalf("transition budget = %d, want the prepared node count %d", got, want)
	}
	if transitionBudget(definition) <= 8 {
		t.Fatal("the wide fixture must exceed the old sequential constant to be meaningful")
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
			definition := mustPrepare(t, tmpl, nil)
			checkpoint, err := Initialize("run-end", definition)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, command := advanceAndPlan(t, checkpoint, definition)
			checkpoint, err = Apply(checkpoint, definition, observed(command, ProgramSucceeded, 0))
			if err != nil {
				t.Fatal(err)
			}
			checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
			if checkpoint.Status != test.want {
				t.Fatalf("status = %q, want %q", checkpoint.Status, test.want)
			}
		})
	}
}

// advanceAndPlan mirrors exactly what the sequential driver does between two
// external events: commit every engine-owned transition, then plan the single
// next command it can carry.
func advanceAndPlan(t *testing.T, checkpoint Checkpoint, definition *Definition) (Checkpoint, *Command) {
	t.Helper()
	next, command, _, err := AdvanceAndPlan(checkpoint, definition)
	if err != nil {
		t.Fatalf("advance and plan: %v", err)
	}
	return next, command
}

// advanced applies one node-addressed engine-owned advance.
func advanced(nodeID string) Transition {
	return Transition{Kind: TransitionAdvance, NodeID: nodeID}
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

func mustPrepare(t *testing.T, tmpl *model.Template, params map[string]string) *Definition {
	t.Helper()
	definition, err := Prepare(tmpl, params)
	if err != nil {
		t.Fatal(err)
	}
	return definition
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

func startToEndTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "start-to-end", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "end"}},
			"end":   {Type: model.NodeTypeEnd},
		},
	}
}

// fanOutTemplate builds start -> fork -> {branch...} -> join(all) -> end, with
// one program task per branch. It is the canonical structured fan-out shape:
// every branch reduces at exactly one join before leaving the fork's scope.
func fanOutTemplate(branches ...string) *model.Template {
	nodes := map[string]model.Node{
		"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
		"join":  joinAllTask("end", "true"),
		"end":   {Type: model.NodeTypeEnd},
	}
	next := model.Next{}
	for _, branch := range branches {
		next[branch] = branch
		nodes[branch] = programTask("join", "true")
	}
	nodes["fork"] = model.Node{Type: model.NodeTypeParallel, Next: next}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "fan-out", Start: "start", Nodes: nodes,
	}
}

func wideFanOutTemplate(width int) *model.Template {
	branches := make([]string, 0, width)
	for i := range width {
		branches = append(branches, fmt.Sprintf("branch%02d", i))
	}
	return fanOutTemplate(branches...)
}

func joinAllTask(next, run string) model.Node {
	node := programTask(next, run)
	node.Join = model.JoinAll
	return node
}

func programTask(next, run string, args ...string) model.Node {
	return model.Node{
		Type:      model.NodeTypeTask,
		Performer: &model.Performer{Kind: model.PerformerProgram, Run: run, Args: args},
		Next:      model.Next{model.DefaultOutcome: next},
	}
}
