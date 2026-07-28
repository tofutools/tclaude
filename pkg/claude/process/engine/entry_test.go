package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// An explicit start-typed node is optional in the authoring contract, so the
// template's entry may be any node kind the engine already executes. These
// tests pin what "ready" then means for each entry kind, and that the shapes
// the engine still cannot execute stay refused before a run can be created.

// directTaskEntryTemplate: task -> end, with no start node at all.
func directTaskEntryTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "direct-task-entry", Start: "task",
		Nodes: map[string]model.Node{
			"task": programTask("end", "true"),
			"end":  {Type: model.NodeTypeEnd},
		},
	}
}

// directDecisionEntryTemplate parks the run on a human the moment it exists.
func directDecisionEntryTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "direct-decision-entry", Start: "choose",
		Nodes: map[string]model.Node{
			"choose": humanDecision("Proceed?", model.Next{"proceed": "work", "abort": "canceled"}),
			"work":   programTask("done", "work"),
			"done":   {Type: model.NodeTypeEnd},
			"canceled": {
				Type: model.NodeTypeEnd, Result: "canceled",
			},
		},
	}
}

// directParallelEntryTemplate is parallelTemplate() with the redundant start
// node removed: start names the fork itself.
func directParallelEntryTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "direct-parallel-entry", Start: "fork",
		Nodes: map[string]model.Node{
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"left": "left", "right": "right"}},
			"left":  programTask("join", "left"),
			"right": programTask("join", "right"),
			"join":  {Type: model.NodeTypeEnd, Join: model.JoinAll},
		},
	}
}

// directEndEntryTemplate is the degenerate graph: the entry is the terminal.
func directEndEntryTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "direct-end-entry", Start: "end",
		Nodes: map[string]model.Node{"end": {Type: model.NodeTypeEnd}},
	}
}

// m2ParallelJoinAllValidationTemplate reproduces the exact deployed shape from
// the TCL-649 human+agent validation, minus the explicit start node that had to
// be inserted to make it runnable: a fork straight from `start` into four
// program branches and one human decision branch, all reducing at one join: all
// task before the end.
func m2ParallelJoinAllValidationTemplate() *model.Template {
	nodes := map[string]model.Node{
		"fork": {Type: model.NodeTypeParallel, Next: model.Next{
			"a": "sleep-a", "b": "sleep-b", "c": "sleep-c", "d": "sleep-d", "choice": "operator-choice",
		}},
		"operator-choice": humanDecision("Continue?", model.Next{"continue": "join", "stop": "join"}),
		"join":            joinAllTask("end", "touch"),
		"end":             {Type: model.NodeTypeEnd, Result: "success"},
	}
	for _, branch := range []string{"sleep-a", "sleep-b", "sleep-c", "sleep-d"} {
		nodes[branch] = programTask("join", "sleep", "0")
	}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind,
		ID: "m2-parallel-joinall-validation", Start: "fork", Nodes: nodes,
	}
}

func TestDirectEntryTemplatesAreAuthoringValidAndEligible(t *testing.T) {
	for _, tmpl := range []*model.Template{
		directTaskEntryTemplate(),
		directDecisionEntryTemplate(),
		directParallelEntryTemplate(),
		directEndEntryTemplate(),
		m2ParallelJoinAllValidationTemplate(),
	} {
		assertAuthoringValid(t, tmpl)
		if diagnostics := CheckEligibility(tmpl); len(diagnostics) != 0 {
			t.Fatalf("eligibility diagnostics for %q = %#v", tmpl.ID, diagnostics)
		}
		if _, err := Prepare(tmpl, nil); err != nil {
			t.Fatalf("prepare %q: %v", tmpl.ID, err)
		}
	}
}

// TestInitializeReadiesTheEntryNodeByItsOwnKind is the initialization contract:
// whatever kind the entry node is, it is the sole ready node, and a decision
// entry additionally takes its durable obligation so a human can answer it.
func TestInitializeReadiesTheEntryNodeByItsOwnKind(t *testing.T) {
	tests := []struct {
		name       string
		tmpl       *model.Template
		entry      string
		obligation bool
	}{
		{name: "task", tmpl: directTaskEntryTemplate(), entry: "task"},
		{name: "decision", tmpl: directDecisionEntryTemplate(), entry: "choose", obligation: true},
		{name: "parallel", tmpl: directParallelEntryTemplate(), entry: "fork"},
		{name: "end", tmpl: directEndEntryTemplate(), entry: "end"},
		{name: "explicit start", tmpl: sequentialTemplate("task"), entry: "start"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			definition := mustPrepare(t, test.tmpl, nil)
			checkpoint, err := Initialize("run-entry", definition)
			if err != nil {
				t.Fatal(err)
			}
			for nodeID, status := range checkpoint.Nodes {
				want := NodePending
				if nodeID == test.entry {
					want = NodeReady
				}
				if status != want {
					t.Fatalf("node %q = %q; want %q", nodeID, status, want)
				}
			}
			if len(checkpoint.Commands) != 0 {
				t.Fatalf("initialization planned a command: %#v", checkpoint.Commands)
			}
			var wantObligations []DecisionObligation
			if test.obligation {
				wantObligations = []DecisionObligation{{NodeID: test.entry}}
			}
			if !reflect.DeepEqual(checkpoint.AwaitingDecisions, wantObligations) {
				t.Fatalf("awaiting decisions = %#v; want %#v", checkpoint.AwaitingDecisions, wantObligations)
			}
		})
	}
}

// TestInitializeDecisionEntryExactCheckpointShape pins the one durable shape
// this change adds: a run that is parked on a human before anything ran.
func TestInitializeDecisionEntryExactCheckpointShape(t *testing.T) {
	definition := mustPrepare(t, directDecisionEntryTemplate(), nil)
	checkpoint, err := Initialize("run-1", definition)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":3,"runId":"run-1","status":"running",` +
		`"nodes":{"canceled":"pending","choose":"ready","done":"pending","work":"pending"},` +
		`"edges":{"choose":{"abort":"unresolved","proceed":"unresolved"},"work":{"next":"unresolved"}},` +
		`"awaitingDecisions":[{"nodeId":"choose"}],"commands":null}`
	if string(encoded) != want {
		t.Fatalf("checkpoint JSON\n got: %s\nwant: %s", encoded, want)
	}
}

// TestDirectTaskEntryPlansAndCompletes proves a task entry is immediately
// plannable — no engine-owned advance stands between creation and the first
// command — and that the run stays strictly classifiable throughout.
func TestDirectTaskEntryPlansAndCompletes(t *testing.T) {
	definition := mustPrepare(t, directTaskEntryTemplate(), nil)
	checkpoint, err := Initialize("run-task-entry", definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
		t.Fatalf("initial checkpoint: %v", err)
	}
	running, command := advanceAndPlan(t, checkpoint, definition)
	if command == nil || command.NodeID != "task" {
		t.Fatalf("first command = %#v", command)
	}
	if err := ClassifyCheckpoint(running, definition); err != nil {
		t.Fatalf("running checkpoint: %v", err)
	}
	observedTask, err := Apply(running, definition, observed(command, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	completed, trailing := advanceAndPlan(t, observedTask, definition)
	if trailing != nil || completed.Status != RunCompleted {
		t.Fatalf("terminal checkpoint = %#v (command %#v)", completed, trailing)
	}
	if err := ClassifyCheckpoint(completed, definition); err != nil {
		t.Fatalf("terminal checkpoint: %v", err)
	}
}

// TestDirectTaskEntryFailureIsClassifiable covers the entry node in its failed
// state: no arrival ever activated it, so the classifier must not demand a
// settled candidate set for it.
func TestDirectTaskEntryFailureIsClassifiable(t *testing.T) {
	definition := mustPrepare(t, directTaskEntryTemplate(), nil)
	checkpoint, err := Initialize("run-task-entry-fail", definition)
	if err != nil {
		t.Fatal(err)
	}
	running, command := advanceAndPlan(t, checkpoint, definition)
	failed, err := Apply(running, definition, observed(command, ProgramFailed, 3))
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != RunFailed || failed.Nodes["task"] != NodeFailed {
		t.Fatalf("failed checkpoint = %#v", failed)
	}
	if err := ClassifyCheckpoint(failed, definition); err != nil {
		t.Fatalf("failed checkpoint: %v", err)
	}
}

// TestDirectDecisionEntryAcceptsAVerdictAcrossRestart drives the decision entry
// through a cold load: the durable obligation the creation boundary wrote is
// the same one a restarted daemon reads back and can resolve.
func TestDirectDecisionEntryAcceptsAVerdictAcrossRestart(t *testing.T) {
	definition := mustPrepare(t, directDecisionEntryTemplate(), nil)
	checkpoint, err := Initialize("run-decision-entry", definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
		t.Fatalf("initial checkpoint: %v", err)
	}
	// Nothing is engine-owned or plannable: the run is parked on a human.
	if quiescent, command, advanced, err := AdvanceAndPlan(checkpoint, definition); err != nil {
		t.Fatal(err)
	} else if advanced || command != nil || !reflect.DeepEqual(quiescent, checkpoint) {
		t.Fatalf("decision entry advanced without a verdict: %#v (command %#v)", quiescent, command)
	}

	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := DecodeCheckpoint(encoded, definition)
	if err != nil {
		t.Fatalf("cold load: %v", err)
	}
	if !reflect.DeepEqual(reloaded, checkpoint) {
		t.Fatalf("restart round trip drifted\n got: %#v\nwant: %#v", reloaded, checkpoint)
	}
	if _, err := Apply(reloaded, definition, decided("choose", "maybe")); err == nil {
		t.Fatal("an unauthored verdict was accepted")
	}

	chosen, err := Apply(reloaded, definition, decided("choose", "proceed"))
	if err != nil {
		t.Fatal(err)
	}
	if chosen.Nodes["work"] != NodeReady || len(chosen.AwaitingDecisions) != 0 {
		t.Fatalf("post-verdict checkpoint = %#v", chosen)
	}
	if err := ClassifyCheckpoint(chosen, definition); err != nil {
		t.Fatalf("post-verdict checkpoint: %v", err)
	}
	running, command := advanceAndPlan(t, chosen, definition)
	if command == nil || command.NodeID != "work" {
		t.Fatalf("chosen branch command = %#v", command)
	}
	observedWork, err := Apply(running, definition, observed(command, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	completed, _ := advanceAndPlan(t, observedWork, definition)
	if completed.Status != RunCompleted || completed.Nodes["canceled"] != NodeSkipped {
		t.Fatalf("terminal checkpoint = %#v", completed)
	}
	if err := ClassifyCheckpoint(completed, definition); err != nil {
		t.Fatalf("terminal checkpoint: %v", err)
	}

	// The obligation is one-shot even though it was minted at creation.
	if _, err := Apply(chosen, definition, decided("choose", "abort")); err == nil {
		t.Fatal("a duplicate verdict was accepted")
	}
}

// TestDirectEndEntryTerminatesImmediately covers the degenerate model the
// authoring layer permits: an entry that is already the terminal.
func TestDirectEndEntryTerminatesImmediately(t *testing.T) {
	definition := mustPrepare(t, directEndEntryTemplate(), nil)
	checkpoint, err := Initialize("run-end-entry", definition)
	if err != nil {
		t.Fatal(err)
	}
	completed, command := advanceAndPlan(t, checkpoint, definition)
	if command != nil {
		t.Fatalf("end entry planned a command: %#v", command)
	}
	if completed.Status != RunCompleted || completed.Nodes["end"] != NodeDone {
		t.Fatalf("terminal checkpoint = %#v", completed)
	}
	if err := ClassifyCheckpoint(completed, definition); err != nil {
		t.Fatalf("terminal checkpoint: %v", err)
	}
}

// TestDirectParallelEntryFansOutAndReducesAtJoinAll reproduces the deployed
// TCL-649 validation template without its inserted start node: the fork is the
// entry, every branch runs independently, and the join: all node only becomes
// ready once the four programs and the human decision have all settled.
func TestDirectParallelEntryFansOutAndReducesAtJoinAll(t *testing.T) {
	definition := mustPrepare(t, m2ParallelJoinAllValidationTemplate(), nil)
	checkpoint, err := Initialize("run-m2-parallel", definition)
	if err != nil {
		t.Fatal(err)
	}
	branches := []string{"sleep-a", "sleep-b", "sleep-c", "sleep-d"}

	// One pass over the fork settles every branch: four tasks become ready and
	// the decision branch is parked on its human in the same advance.
	commands := map[string]Command{}
	for range branches {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		if command == nil {
			t.Fatalf("only %d branch commands were planned", len(commands))
			continue
		}
		commands[command.NodeID] = *command
	}
	if checkpoint.Nodes["fork"] != NodeDone {
		t.Fatalf("fork entry = %q", checkpoint.Nodes["fork"])
	}
	for _, branch := range branches {
		if checkpoint.Nodes[branch] != NodeRunning {
			t.Fatalf("branch %q = %q", branch, checkpoint.Nodes[branch])
		}
	}
	if len(checkpoint.AwaitingDecisions) != 1 || checkpoint.AwaitingDecisions[0].NodeID != "operator-choice" {
		t.Fatalf("awaiting decisions = %#v", checkpoint.AwaitingDecisions)
	}
	if checkpoint.Nodes["join"] != NodePending {
		t.Fatalf("join reduced before its candidate set settled: %q", checkpoint.Nodes["join"])
	}

	// The sibling verdict lands while the programs are still outstanding.
	checkpoint, err = Apply(checkpoint, definition, decided("operator-choice", "continue"))
	if err != nil {
		t.Fatal(err)
	}
	for _, branch := range branches {
		command := commands[branch]
		if checkpoint.Nodes["join"] != NodePending {
			t.Fatalf("join activated with branch %q still outstanding", branch)
		}
		checkpoint, err = Apply(checkpoint, definition, observed(&command, ProgramSucceeded, 0))
		if err != nil {
			t.Fatal(err)
		}
	}
	if checkpoint.Nodes["join"] != NodeReady {
		t.Fatalf("join did not reduce once every branch settled: %#v", checkpoint.Nodes)
	}

	joined, joinCommand := advanceAndPlan(t, checkpoint, definition)
	if joinCommand == nil || joinCommand.NodeID != "join" {
		t.Fatalf("join command = %#v", joinCommand)
	}
	settled, err := Apply(joined, definition, observed(joinCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	completed, trailing := advanceAndPlan(t, settled, definition)
	if trailing != nil || completed.Status != RunCompleted {
		t.Fatalf("terminal checkpoint = %#v (command %#v)", completed, trailing)
	}
	for nodeID, status := range completed.Nodes {
		if status != NodeDone {
			t.Fatalf("terminal node %q = %q", nodeID, status)
		}
	}
}

// TestDirectEntryDoesNotRelaxUnsupportedShapes proves the entry relaxation is
// exactly that: every other capability refusal still fires on the entry node
// itself, with its own path.
func TestDirectEntryDoesNotRelaxUnsupportedShapes(t *testing.T) {
	tests := []struct {
		name string
		code string
		path string
		tmpl func() *model.Template
	}{
		{
			name: "agent decider as the entry",
			code: "unsupported_performer",
			path: "nodes.choose.performer.kind",
			tmpl: func() *model.Template {
				tmpl := directDecisionEntryTemplate()
				node := tmpl.Nodes["choose"]
				node.Performer = &model.Performer{Kind: model.PerformerAgent, Prompt: "Decide"}
				tmpl.Nodes["choose"] = node
				return tmpl
			},
		},
		{
			name: "human performer on a direct task entry",
			code: "unsupported_performer",
			path: "nodes.task.performer.kind",
			tmpl: func() *model.Template {
				tmpl := directTaskEntryTemplate()
				node := tmpl.Nodes["task"]
				node.Performer = &model.Performer{Kind: model.PerformerHuman, Ask: "Do it?"}
				tmpl.Nodes["task"] = node
				return tmpl
			},
		},
		{
			// A bounded fresh-attempt retry IS executable on a program task, entry
			// or not; the backoff wait between attempts is what stays unsupported,
			// and being the entry node must not relax that.
			name: "retry backoff on a direct task entry",
			code: "unsupported_retry",
			path: "nodes.task.retry.backoff",
			tmpl: func() *model.Template {
				tmpl := directTaskEntryTemplate()
				node := tmpl.Nodes["task"]
				node.Retry = &model.RetryPolicy{MaxAttempts: 2, Backoff: "30s"}
				tmpl.Nodes["task"] = node
				return tmpl
			},
		},
		{
			name: "wait as the entry",
			code: "unsupported_wait",
			path: "nodes.hold.type",
			tmpl: func() *model.Template {
				return &model.Template{
					APIVersion: model.APIVersion, Kind: model.Kind, ID: "direct-wait-entry", Start: "hold",
					Nodes: map[string]model.Node{
						"hold": {
							Type: model.NodeTypeWait, Wait: &model.WaitConfig{Duration: "1s"},
							Next: model.Next{model.DefaultOutcome: "end"},
						},
						"end": {Type: model.NodeTypeEnd},
					},
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tmpl := test.tmpl()
			assertAuthoringValid(t, tmpl)
			diagnostics := CheckEligibility(tmpl)
			if !hasCodeAtPath(diagnostics, test.code, test.path) {
				t.Fatalf("missing %q at %q in %#v", test.code, test.path, diagnostics)
			}
			if _, err := Prepare(tmpl, nil); err == nil {
				t.Fatalf("Prepare admitted %q", tmpl.ID)
			}
		})
	}
}

// TestEligibilityDiagnosticsDropTheStaleEngineName guards the rename: no
// operator-facing refusal may name the retired exclusive-decision engine.
func TestEligibilityDiagnosticsDropTheStaleEngineName(t *testing.T) {
	tmpl := directTaskEntryTemplate()
	node := tmpl.Nodes["task"]
	node.Retry = &model.RetryPolicy{MaxAttempts: 2}
	node.Performer = &model.Performer{Kind: model.PerformerAgent, Prompt: "Do it"}
	node.Wait = &model.WaitConfig{Duration: "1s"}
	tmpl.Nodes["task"] = node
	diagnostics := CheckEligibility(tmpl)
	if len(diagnostics) == 0 {
		t.Fatal("expected refusals")
	}
	for _, diagnostic := range diagnostics {
		if strings.Contains(diagnostic.Message, "exclusive-decision") {
			t.Fatalf("stale engine name in %q", diagnostic.Message)
		}
	}
	if strings.Contains(ErrTemplateIneligible.Error(), "exclusive-decision") {
		t.Fatalf("stale engine name in %q", ErrTemplateIneligible.Error())
	}
}

func hasCodeAtPath(diagnostics model.Diagnostics, code, path string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code && diagnostic.Path == path {
			return true
		}
	}
	return false
}
