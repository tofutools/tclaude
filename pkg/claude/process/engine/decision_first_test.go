package engine

import (
	"errors"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// decisionFirstTemplate is a loop entered at its decision:
//
//	start -> implement -> review -{approved}-> merge -> end
//	                             -{changes}-> fix -> review (max rounds)
//	                             -{escalate}-> escalated (end, canceled)
func decisionFirstTemplate(bound model.LoopBound) *model.Template {
	review := humanDecision("Approve?", model.Next{"approved": "merge", "changes": "fix", "escalate": "escalated"})
	review.Loops = map[string]model.LoopBound{"changes": bound}
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "decision-first",
		Start:      "start",
		Nodes: map[string]model.Node{
			"start":     {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "implement"}},
			"implement": programTask("review", "implement"),
			"review":    review,
			"fix":       programTask("review", "fix"),
			"merge":     programTask("end", "merge"),
			"end":       {Type: model.NodeTypeEnd},
			"escalated": {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	}
}

func commandNodes(commands []Command) []string {
	ids := make([]string, 0, len(commands))
	for _, command := range commands {
		ids = append(ids, command.NodeID)
	}
	return ids
}

func TestDecisionFirstLoopApprovedAtOnceSkipsTheRepeatedWork(t *testing.T) {
	definition := mustPrepare(t, decisionFirstTemplate(model.LoopBound{MaxIterations: 3, OnExhausted: "escalate"}), nil)
	checkpoint, err := Initialize("run-df-approve", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, ran := succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 1 || ran[0].NodeID != "implement" {
		t.Fatalf("before the decision = %v", commandNodes(ran))
	}
	if checkpoint.Nodes["review"] != NodeReady || checkpoint.Nodes["fix"] != NodePending {
		t.Fatalf("review = %q, fix = %q", checkpoint.Nodes["review"], checkpoint.Nodes["fix"])
	}
	checkpoint = roundTrip(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("review", "approved", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, ran = succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 1 || ran[0].NodeID != "merge" {
		t.Fatalf("after approval = %v", commandNodes(ran))
	}
	if checkpoint.Status != RunCompleted || checkpoint.Nodes["fix"] != NodeSkipped || checkpoint.Nodes["escalated"] != NodeSkipped {
		t.Fatalf("status = %q, nodes = %v", checkpoint.Status, checkpoint.Nodes)
	}
	roundTrip(t, checkpoint, definition)
}

func TestDecisionFirstLoopRunsRoundsUntilApproved(t *testing.T) {
	definition := mustPrepare(t, decisionFirstTemplate(model.LoopBound{MaxIterations: 3, OnExhausted: "escalate"}), nil)
	checkpoint, err := Initialize("run-df-rounds", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	for round := 2; round <= 3; round++ {
		checkpoint, err = Apply(checkpoint, definition, decidedAt("review", "changes", round-2))
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		checkpoint = roundTrip(t, checkpoint, definition)
		if checkpoint.Nodes["fix"] != NodeReady || checkpoint.Nodes["review"] != NodePending || checkpoint.Nodes["merge"] != NodePending {
			t.Fatalf("round %d nodes = %v", round, checkpoint.Nodes)
		}
		var ran []Command
		checkpoint, ran = succeedUntilQuiet(t, checkpoint, definition)
		if len(ran) != 1 || ran[0].NodeID != "fix" || ran[0].Round != round || ran[0].Rounds != 3 {
			t.Fatalf("round %d commands = %#v", round, ran)
		}
		if checkpoint.Nodes["review"] != NodeReady {
			t.Fatalf("round %d review = %q", round, checkpoint.Nodes["review"])
		}
		if r, ok := definition.RoundOf(checkpoint, "review"); !ok || r.Round != round {
			t.Fatalf("round of review = %#v", r)
		}
		checkpoint = roundTrip(t, checkpoint, definition)
	}
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("review", "approved", 2)); err != nil {
		t.Fatal(err)
	}
	checkpoint, ran := succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 1 || ran[0].NodeID != "merge" || checkpoint.Status != RunCompleted {
		t.Fatalf("after approval = %v, status = %q", commandNodes(ran), checkpoint.Status)
	}
}

func TestDecisionFirstLoopExhaustionFollowsOnExhausted(t *testing.T) {
	definition := mustPrepare(t, decisionFirstTemplate(model.LoopBound{MaxIterations: 2, OnExhausted: "escalate"}), nil)
	checkpoint, err := Initialize("run-df-exhaust", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("review", "changes", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	before := checkpoint
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("review", "changes", 1)); err != nil {
		t.Fatal(err)
	}
	if step, ok := definition.LoopStepOf("review", "changes", before, checkpoint); !ok || !step.Exhausted {
		t.Fatalf("loop step = %#v %v", step, ok)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint.Status != RunCanceled || checkpoint.Nodes["escalated"] != NodeDone || checkpoint.Nodes["merge"] != NodeSkipped {
		t.Fatalf("status = %q, nodes = %v", checkpoint.Status, checkpoint.Nodes)
	}
}

// A decision-first loop on a branch not taken is skipped whole, the work it
// would repeat included.
func TestDecisionFirstLoopOnABranchNotTakenIsSkipped(t *testing.T) {
	tmpl := decisionFirstTemplate(model.LoopBound{MaxIterations: 3, OnExhausted: "escalate"})
	tmpl.Nodes["start"] = humanDecision("Build?", model.Next{"build": "implement", "skip": "end"})
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-df-skip", definition)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("start", "skip", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	for _, id := range []string{"implement", "review", "fix", "merge"} {
		if checkpoint.Nodes[id] != NodeSkipped {
			t.Fatalf("%s = %q, nodes = %v", id, checkpoint.Nodes[id], checkpoint.Nodes)
		}
	}
	if checkpoint.Status != RunCompleted {
		t.Fatalf("status = %q", checkpoint.Status)
	}
}

// A decision-first loop inside an ordinary loop starts counting again with
// every round of the outer one.
func TestDecisionFirstLoopInsideALoopRestartsWithIt(t *testing.T) {
	tmpl := decisionFirstTemplate(model.LoopBound{MaxIterations: 3, OnExhausted: "escalate"})
	tmpl.Nodes["merge"] = programTask("final", "merge")
	final := humanDecision("Done?", model.Next{"done": "end", "again": "implement"})
	final.Loops = map[string]model.LoopBound{"again": {MaxIterations: 2}}
	tmpl.Nodes["final"] = final
	tmpl.Nodes["review"] = func() model.Node {
		review := tmpl.Nodes["review"]
		review.Next = model.Next{"approved": "merge", "changes": "fix"}
		review.Loops = map[string]model.LoopBound{"changes": {MaxIterations: 3}}
		return review
	}()
	delete(tmpl.Nodes, "escalated")
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-df-nested", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("review", "changes", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("review", "approved", 1)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("final", "again", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint = roundTrip(t, checkpoint, definition)
	if _, counted := checkpoint.Iterations["review"]; counted {
		t.Fatalf("inner rounds carried into the outer round: %v", checkpoint.Iterations)
	}
	checkpoint, ran := succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 1 || ran[0].NodeID != "implement" || checkpoint.Nodes["review"] != NodeReady || checkpoint.Nodes["fix"] != NodePending {
		t.Fatalf("outer round 2 = %v, nodes = %v", commandNodes(ran), checkpoint.Nodes)
	}
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("review", "approved", 2)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("final", "done", 1)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted || checkpoint.Nodes["fix"] != NodeSkipped {
		t.Fatalf("status = %q, nodes = %v", checkpoint.Status, checkpoint.Nodes)
	}
}

func TestDecisionFirstLoopSendBackGoesToTheDecision(t *testing.T) {
	definition := mustPrepare(t, decisionFirstTemplate(model.LoopBound{MaxIterations: 3, OnExhausted: "escalate"}), nil)
	checkpoint, err := Initialize("run-df-back", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("review", "changes", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if _, err := Apply(checkpoint, definition, Transition{Kind: TransitionSentBack, Back: &SendBack{NodeID: "fix"}}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("send back to the repeated work err = %v", err)
	}
	checkpoint, err = Apply(checkpoint, definition, Transition{Kind: TransitionSentBack, Back: &SendBack{NodeID: "implement"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
		t.Fatal(err)
	}
	if _, counted := checkpoint.Iterations["review"]; counted || checkpoint.Nodes["fix"] != NodePending {
		t.Fatalf("iterations = %v, nodes = %v", checkpoint.Iterations, checkpoint.Nodes)
	}
	checkpoint, ran := succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 1 || ran[0].NodeID != "implement" || checkpoint.Nodes["review"] != NodeReady {
		t.Fatalf("after send-back = %v, nodes = %v", commandNodes(ran), checkpoint.Nodes)
	}
}

// tracksTemplate runs two decision-first loops alongside each other and joins
// them:
//
//	start -> fork -{a}-> review-a -{approved}-> signoff (join: all) -> end
//	                              -{changes}-> fix-a -> review-a
//	              -{b}-> review-b -{approved}->
//	                              -{changes}-> fix-b -> review-b
func tracksTemplate() *model.Template {
	review := func(fix string) model.Node {
		node := humanDecision("Approve?", model.Next{"approved": "signoff", "changes": fix})
		node.Loops = map[string]model.LoopBound{"changes": {MaxIterations: 3}}
		return node
	}
	return &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "tracks", Start: "start",
		Nodes: map[string]model.Node{
			"start":    {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":     {Type: model.NodeTypeParallel, Next: model.Next{"a": "review-a", "b": "review-b"}},
			"review-a": review("fix-a"),
			"fix-a":    programTask("review-a", "fix-a"),
			"review-b": review("fix-b"),
			"fix-b":    programTask("review-b", "fix-b"),
			"signoff":  joinAllTask("end", "signoff"),
			"end":      {Type: model.NodeTypeEnd},
		},
	}
}

// runCommands plans and succeeds every program command until none is ready.
func runCommands(t *testing.T, checkpoint Checkpoint, definition *Definition) (Checkpoint, []string) {
	t.Helper()
	var ran []string
	for {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		if command == nil {
			return roundTrip(t, checkpoint, definition), ran
		}
		ran = append(ran, command.NodeID)
		var err error
		if checkpoint, err = Apply(checkpoint, definition, observed(command, ProgramSucceeded, 0)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDecisionFirstTracksRunAlongsideAndJoin(t *testing.T) {
	definition := mustPrepare(t, tracksTemplate(), nil)
	checkpoint, err := Initialize("run-tracks", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = runCommands(t, checkpoint, definition)
	if len(checkpoint.AwaitingDecisions) != 2 {
		t.Fatalf("awaiting = %#v", checkpoint.AwaitingDecisions)
	}
	apply := func(transition Transition) {
		t.Helper()
		if checkpoint, err = Apply(checkpoint, definition, transition); err != nil {
			t.Fatal(err)
		}
	}
	// Track a asks for changes; track b approves while a's work runs.
	apply(decidedAt("review-a", "changes", 0))
	apply(decidedAt("review-b", "approved", 0))
	if checkpoint.Nodes["signoff"] != NodePending || checkpoint.Nodes["fix-b"] != NodeSkipped {
		t.Fatalf("after b approved: nodes = %v", checkpoint.Nodes)
	}
	var ran []string
	checkpoint, ran = runCommands(t, checkpoint, definition)
	if len(ran) != 1 || ran[0] != "fix-a" || checkpoint.Nodes["review-a"] != NodeReady || checkpoint.Nodes["signoff"] != NodePending {
		t.Fatalf("round 2 of a = %v, nodes = %v", ran, checkpoint.Nodes)
	}
	// A second round of changes in track a; the join still waits.
	apply(decidedAt("review-a", "changes", 1))
	checkpoint, ran = runCommands(t, checkpoint, definition)
	if len(ran) != 1 || ran[0] != "fix-a" || checkpoint.Nodes["signoff"] != NodePending {
		t.Fatalf("round 3 of a = %v, nodes = %v", ran, checkpoint.Nodes)
	}
	if r, _ := definition.RoundOf(checkpoint, "review-a"); r.Round != 3 {
		t.Fatalf("round of review-a = %#v", r)
	}
	// Its rounds are used up: going back again is refused, approving joins.
	if _, err := Apply(checkpoint, definition, decidedAt("review-a", "changes", 2)); !errors.Is(err, ErrLoopExhausted) {
		t.Fatalf("exhausted err = %v", err)
	}
	apply(decidedAt("review-a", "approved", 2))
	checkpoint, ran = runCommands(t, checkpoint, definition)
	if len(ran) != 1 || ran[0] != "signoff" || checkpoint.Status != RunCompleted {
		t.Fatalf("after both approved = %v, status = %q, nodes = %v", ran, checkpoint.Status, checkpoint.Nodes)
	}
}
