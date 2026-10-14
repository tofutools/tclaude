package engine

import (
	"errors"
	"slices"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// reviewSubprocessTemplate is the canonical subprocess fixture:
//
//	start -> review ▣ { lint -> gate? {pass: ok, fail: bad} } -> {approved: ship, rejected: abandon}
//
// The body is its own param scope: its target param is bound from the
// parent's repo param through an in-port, and its two end nodes leave through
// out-ports onto the parent's two outcomes.
func reviewSubprocessTemplate() *model.Template {
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "review-flow", Start: "start",
		Params: map[string]model.Param{"repo": {Type: "string"}},
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "review"}},
			"review": {
				Type: model.NodeTypeSubprocess,
				Subprocess: &model.Subprocess{
					Params: map[string]model.Param{"target": {Type: "string"}},
					Start:  "lint",
					Nodes: map[string]model.Node{
						"lint": programTask("gate", "lint", "{{ params.target }}"),
						"gate": {
							Type:      model.NodeTypeDecision,
							Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Ship it?"},
							Next:      model.Next{"pass": "ok", "fail": "bad"},
						},
						"ok":  {Type: model.NodeTypeEnd},
						"bad": {Type: model.NodeTypeEnd, Result: "failed"},
					},
					Inputs:  map[string]string{"target": "{{ params.repo }}"},
					Outputs: map[string]string{"ok": "approved", "bad": "rejected"},
				},
				Next: model.Next{"approved": "ship", "rejected": "abandon"},
			},
			"ship":    programTask("end", "ship"),
			"abandon": {Type: model.NodeTypeEnd, Result: "canceled"},
			"end":     {Type: model.NodeTypeEnd},
		},
	}
}

func TestPrepareNamespacesSubprocessBodyContiguouslyWithBoundParams(t *testing.T) {
	definition := mustPrepare(t, reviewSubprocessTemplate(), map[string]string{"repo": "webshop"})

	want := []string{"start", "review", "review/lint", "review/gate", "review/bad", "review/ok", "abandon", "ship", "end"}
	if got := preparedNodeIDs(definition); !slices.Equal(got, want) {
		t.Fatalf("prepared order\n got: %v\nwant: %v", got, want)
	}
	review := definition.nodes[definition.index["review"]]
	if review.kind != definitionSubprocess || definition.nodes[review.entry].id != "review/lint" {
		t.Fatalf("review = kind %d entry %d", review.kind, review.entry)
	}
	if review.scopeEnd != definition.index["review/ok"]+1 {
		t.Fatalf("review body range ends at %d", review.scopeEnd)
	}
	lint := definition.nodes[definition.index["review/lint"]]
	if lint.scope != definition.index["review"] || !slices.Equal(lint.program.Args, []string{"webshop"}) {
		t.Fatalf("lint = scope %d args %v; the in-port must bind the body param from the parent", lint.scope, lint.program.Args)
	}
	for id, outcome := range map[string]string{"review/ok": "approved", "review/bad": "rejected"} {
		exit := definition.nodes[definition.index[id]]
		if exit.kind != definitionSubprocessExit || exit.exitOutcome != outcome || len(exit.outgoing) != 0 {
			t.Fatalf("exit %q = kind %d outcome %q out %v", id, exit.kind, exit.exitOutcome, exit.outgoing)
		}
	}
	// The subprocess node keeps exactly its authored route; the body's edges are
	// its own, namespaced, and never leave it.
	if len(review.outgoing) != 2 || len(review.incoming) != 1 {
		t.Fatalf("review edges: in %v out %v", review.incoming, review.outgoing)
	}
	if verdicts, ok := definition.DecisionVerdicts("review/gate"); !ok || !slices.Equal(verdicts, []string{"fail", "pass"}) {
		t.Fatalf("namespaced decision verdicts = %v, %v", verdicts, ok)
	}
}

func TestSubprocessRunsItsBodyAndLeavesThroughTheChosenOutPort(t *testing.T) {
	definition := mustPrepare(t, reviewSubprocessTemplate(), map[string]string{"repo": "webshop"})
	checkpoint, err := Initialize("run-subprocess", definition)
	if err != nil {
		t.Fatal(err)
	}

	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Nodes["review"] != NodeRunning || command == nil || command.NodeID != "review/lint" {
		t.Fatalf("review = %q, first command %#v", checkpoint.Nodes["review"], command)
	}
	if checkpoint.Edges["review"]["approved"] != EdgeUnresolved {
		t.Fatalf("the subprocess route settled before its body finished")
	}
	checkpoint = mustApply(t, checkpoint, definition, observed(command, ProgramSucceeded, 0))
	if checkpoint.Nodes["review/gate"] != NodeReady || !hasObligation(checkpoint, "review/gate") {
		t.Fatalf("gate = %q, obligations %v", checkpoint.Nodes["review/gate"], checkpoint.AwaitingDecisions)
	}

	checkpoint = mustApply(t, checkpoint, definition, decided("review/gate", "pass"))
	checkpoint, command = advanceAndPlan(t, checkpoint, definition)
	for id, want := range map[string]NodeStatus{
		"review": NodeDone, "review/ok": NodeDone, "review/bad": NodeSkipped, "abandon": NodeSkipped, "ship": NodeRunning,
	} {
		if got := checkpoint.Nodes[id]; got != want {
			t.Fatalf("node %q = %q, want %q", id, got, want)
		}
	}
	if checkpoint.Edges["review"]["approved"] != EdgeArrived || checkpoint.Edges["review"]["rejected"] != EdgeNotTaken {
		t.Fatalf("review route = %v", checkpoint.Edges["review"])
	}
	if command == nil || command.NodeID != "ship" {
		t.Fatalf("command after the subprocess = %#v", command)
	}
	checkpoint = mustApply(t, checkpoint, definition, observed(command, ProgramSucceeded, 0))
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q", checkpoint.Status)
	}
	if err := ValidateCheckpoint(checkpoint, definition); err != nil {
		t.Fatalf("completed checkpoint is not structurally valid: %v", err)
	}
}

func TestSubprocessOnAnUntakenBranchSkipsItsWholeBody(t *testing.T) {
	inner := sequentialTemplate("work")
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "skip-flow", Start: "choose",
		Nodes: map[string]model.Node{
			"choose": {
				Type:      model.NodeTypeDecision,
				Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Which path?"},
				Next:      model.Next{"boxed": "box", "plain": "plain"},
			},
			"box": {
				Type:       model.NodeTypeSubprocess,
				Subprocess: &model.Subprocess{Start: inner.Start, Nodes: inner.Nodes},
				Next:       model.Next{model.DefaultOutcome: "finish"},
			},
			"plain":  programTask("finish", "plain"),
			"finish": programTask("end", "finish"),
			"end":    {Type: model.NodeTypeEnd},
		},
	}
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-skip", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = mustApply(t, checkpoint, definition, decided("choose", "plain"))
	for _, id := range []string{"box", "box/start", "box/work", "box/end"} {
		if checkpoint.Nodes[id] != NodeSkipped {
			t.Fatalf("node %q = %q, want skipped with its subprocess", id, checkpoint.Nodes[id])
		}
	}
	for from, outcomes := range map[string][]string{"box/start": {model.DefaultOutcome}, "box/work": {model.DefaultOutcome}} {
		for _, outcome := range outcomes {
			if checkpoint.Edges[from][outcome] != EdgeNotTaken {
				t.Fatalf("body edge %s/%s = %q", from, outcome, checkpoint.Edges[from][outcome])
			}
		}
	}
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	if command == nil || command.NodeID != "plain" {
		t.Fatalf("command = %#v", command)
	}
	checkpoint = mustApply(t, checkpoint, definition, observed(command, ProgramSucceeded, 0))
	checkpoint, command = advanceAndPlan(t, checkpoint, definition)
	checkpoint = mustApply(t, checkpoint, definition, observed(command, ProgramSucceeded, 0))
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q", checkpoint.Status)
	}
}

func TestNestedSubprocessesRunToCompletion(t *testing.T) {
	innermost := sequentialTemplate("leaf")
	middle := &model.Template{
		Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "inner"}},
			"inner": {
				Type:       model.NodeTypeSubprocess,
				Subprocess: &model.Subprocess{Start: innermost.Start, Nodes: innermost.Nodes},
				Next:       model.Next{model.DefaultOutcome: "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "nested", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "outer"}},
			"outer": {
				Type:       model.NodeTypeSubprocess,
				Subprocess: &model.Subprocess{Start: middle.Start, Nodes: middle.Nodes},
				Next:       model.Next{model.DefaultOutcome: "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-nested", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	if command == nil || command.NodeID != "outer/inner/leaf" {
		t.Fatalf("command = %#v", command)
	}
	if checkpoint.Nodes["outer"] != NodeRunning || checkpoint.Nodes["outer/inner"] != NodeRunning {
		t.Fatalf("outer %q, outer/inner %q", checkpoint.Nodes["outer"], checkpoint.Nodes["outer/inner"])
	}
	checkpoint = mustApply(t, checkpoint, definition, observed(command, ProgramSucceeded, 0))
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q, nodes %v", checkpoint.Status, checkpoint.Nodes)
	}
	for _, id := range []string{"outer", "outer/inner", "outer/inner/end", "outer/end", "end"} {
		if checkpoint.Nodes[id] != NodeDone {
			t.Fatalf("node %q = %q", id, checkpoint.Nodes[id])
		}
	}
}

func TestUnresolvedSubprocessReferenceIsNotExecutable(t *testing.T) {
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "by-ref", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "box"}},
			"box": {
				Type:       model.NodeTypeSubprocess,
				Subprocess: &model.Subprocess{TemplateRef: "other"},
				Next:       model.Next{model.DefaultOutcome: "end"},
			},
			"end": {Type: model.NodeTypeEnd},
		},
	}
	_, err := Prepare(tmpl, nil)
	var eligibility *EligibilityError
	if !errors.As(err, &eligibility) || !slices.ContainsFunc(eligibility.Diagnostics, func(d model.Diagnostic) bool {
		return d.Code == "unresolved_subprocess"
	}) {
		t.Fatalf("Prepare error = %v", err)
	}

	resolved, err := model.ResolveSubprocessRefs(tmpl, func(id, _ string) (*model.Template, string, error) {
		body := sequentialTemplate("work")
		body.ID = id
		return body, "sha256:0000000000000000000000000000000000000000000000000000000000000000", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	definition := mustPrepare(t, resolved, nil)
	if _, ok := definition.index["box/work"]; !ok {
		t.Fatalf("resolved body was not prepared: %v", preparedNodeIDs(definition))
	}
}

func mustApply(t *testing.T, checkpoint Checkpoint, definition *Definition, transition Transition) Checkpoint {
	t.Helper()
	next, err := Apply(checkpoint, definition, transition)
	if err != nil {
		t.Fatalf("apply %s: %v", transition.Kind, err)
	}
	return next
}
