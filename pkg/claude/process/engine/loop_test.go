package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// reviewLoopTemplate is the canonical rework loop:
//
//	start -> implement -> review -> decide -{approved}-> merge -> end
//	                                       -{changes}-> implement (max rounds)
//	                                       -{escalate}-> escalated (end, canceled)
func reviewLoopTemplate(bound model.LoopBound) *model.Template {
	decide := humanDecision("Ship it?", model.Next{"approved": "merge", "changes": "implement", "escalate": "escalated"})
	decide.Loops = map[string]model.LoopBound{"changes": bound}
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "review-loop",
		Start:      "start",
		Nodes: map[string]model.Node{
			"start":     {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "implement"}},
			"implement": programTask("review", "implement"),
			"review":    programTask("decide", "review"),
			"decide":    decide,
			"merge":     programTask("end", "merge"),
			"end":       {Type: model.NodeTypeEnd},
			"escalated": {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	}
}

// succeedUntilQuiet plans and succeeds every command until the run waits on a
// decision or ends, classifying every state on the way. It returns the
// commands it ran.
func succeedUntilQuiet(t *testing.T, checkpoint Checkpoint, definition *Definition) (Checkpoint, []Command) {
	t.Helper()
	var ran []Command
	for {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
			t.Fatalf("classify after plan: %v", err)
		}
		if command == nil {
			return checkpoint, ran
		}
		ran = append(ran, *command)
		var err error
		checkpoint, err = Apply(checkpoint, definition, observed(command, ProgramSucceeded, 0))
		if err != nil {
			t.Fatal(err)
		}
		if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
			t.Fatalf("classify after %s: %v", command.NodeID, err)
		}
	}
}

func roundTrip(t *testing.T, checkpoint Checkpoint, definition *Definition) Checkpoint {
	t.Helper()
	data, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeCheckpoint(data, definition)
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, data)
	}
	return decoded
}

func TestReviewLoopRunsRoundsUntilApproved(t *testing.T) {
	definition := mustPrepare(t, reviewLoopTemplate(model.LoopBound{MaxIterations: 3, OnExhausted: "escalate"}), nil)
	checkpoint, err := Initialize("run-loop", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, ran := succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 2 || ran[0].Round != 1 || ran[0].Rounds != 3 {
		t.Fatalf("round 1 commands = %#v", ran)
	}
	for round := 2; round <= 3; round++ {
		// The verdict of the previous round's window is stale once it moved on.
		if round == 3 {
			if _, err := Apply(checkpoint, definition, decidedAt("decide", "changes", 0)); !errors.Is(err, ErrStaleDecision) {
				t.Fatalf("stale verdict err = %v", err)
			}
		}
		checkpoint, err = Apply(checkpoint, definition, decidedAt("decide", "changes", round-2))
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
			t.Fatalf("classify round %d: %v", round, err)
		}
		checkpoint = roundTrip(t, checkpoint, definition)
		if got := checkpoint.Iterations["decide"]["changes"]; got != round-1 {
			t.Fatalf("iterations = %d, want %d", got, round-1)
		}
		// The merge stays reachable while the loop is live.
		if status := checkpoint.Nodes["merge"]; status != NodePending {
			t.Fatalf("merge = %q during the loop", status)
		}
		checkpoint, ran = succeedUntilQuiet(t, checkpoint, definition)
		if len(ran) != 2 || ran[0].NodeID != "implement" || ran[0].Attempt != round || ran[0].Round != round {
			t.Fatalf("round %d commands = %#v", round, ran)
		}
	}
	if got := definition.RequiredDecisionAttempt(checkpoint, "decide"); got != 2 {
		t.Fatalf("decision window = %d, want 2", got)
	}
	checkpoint, err = Apply(checkpoint, definition, decidedAt("decide", "approved", 2))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, ran = succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 1 || ran[0].NodeID != "merge" || ran[0].Round != 0 {
		t.Fatalf("after approval = %#v", ran)
	}
	if checkpoint.Status != RunCompleted || checkpoint.Nodes["escalated"] != NodeSkipped {
		t.Fatalf("status = %q, escalated = %q", checkpoint.Status, checkpoint.Nodes["escalated"])
	}
}

func TestReviewLoopExhaustionFollowsOnExhausted(t *testing.T) {
	definition := mustPrepare(t, reviewLoopTemplate(model.LoopBound{MaxIterations: 2, OnExhausted: "escalate"}), nil)
	checkpoint, err := Initialize("run-exhaust", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	before := checkpoint
	checkpoint, err = Apply(checkpoint, definition, decidedAt("decide", "changes", 0))
	if err != nil {
		t.Fatal(err)
	}
	if step, ok := definition.LoopStepOf("decide", "changes", before, checkpoint); !ok || !step.Iterated || step.Loop.Round != 2 {
		t.Fatalf("loop step = %#v %v", step, ok)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	before = checkpoint
	checkpoint, err = Apply(checkpoint, definition, decidedAt("decide", "changes", 1))
	if err != nil {
		t.Fatal(err)
	}
	if step, ok := definition.LoopStepOf("decide", "changes", before, checkpoint); !ok || !step.Exhausted || step.Loop.Round != 2 {
		t.Fatalf("loop step = %#v %v", step, ok)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint.Status != RunCanceled || checkpoint.Nodes["escalated"] != NodeDone {
		t.Fatalf("status = %q, escalated = %q", checkpoint.Status, checkpoint.Nodes["escalated"])
	}
}

func TestReviewLoopWithoutOnExhaustedRefusesTheExtraRound(t *testing.T) {
	definition := mustPrepare(t, reviewLoopTemplate(model.LoopBound{MaxIterations: 2}), nil)
	checkpoint, err := Initialize("run-refuse", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("decide", "changes", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	_, err = Apply(checkpoint, definition, decidedAt("decide", "changes", 1))
	if !errors.Is(err, ErrLoopExhausted) || !errors.Is(err, ErrInvalidDecisionVerdict) {
		t.Fatalf("exhausted verdict err = %v", err)
	}
	// The decision is still open, and another outcome settles it.
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("decide", "approved", 1)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("status = %q", checkpoint.Status)
	}
}

// gatedLoopTemplate leaves the loop body from a decision inside it, so that
// exit must stay open while the loop can still go round.
//
//	start -> implement -> gate -{ok}-> decide -{done}-> end
//	                           -{abort}-> aborted       -{again}-> implement
func gatedLoopTemplate() *model.Template {
	decide := humanDecision("Again?", model.Next{"done": "end", "again": "implement"})
	decide.Loops = map[string]model.LoopBound{"again": {MaxIterations: 3}}
	return &model.Template{
		APIVersion: model.APIVersion,
		Kind:       model.Kind,
		ID:         "gated-loop",
		Start:      "start",
		Nodes: map[string]model.Node{
			"start":     {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "implement"}},
			"implement": programTask("gate", "implement"),
			"gate":      humanDecision("Keep going?", model.Next{"ok": "decide", "abort": "aborted"}),
			"decide":    decide,
			"end":       {Type: model.NodeTypeEnd},
			"aborted":   {Type: model.NodeTypeEnd, Result: "canceled"},
		},
	}
}

func TestLoopExitStaysOpenWhileTheLoopIsLive(t *testing.T) {
	definition := mustPrepare(t, gatedLoopTemplate(), nil)
	checkpoint, err := Initialize("run-gated", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("gate", "ok", 0)); err != nil {
		t.Fatal(err)
	}
	if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
		t.Fatal(err)
	}
	if status, edge := checkpoint.Nodes["aborted"], checkpoint.Edges["gate"]["abort"]; status != NodePending || edge != EdgeUnresolved {
		t.Fatalf("aborted = %q via %q while the loop is live", status, edge)
	}
	checkpoint = roundTrip(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("decide", "again", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	// Round two takes the exit the first round passed by.
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("gate", "abort", 1)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint.Status != RunCanceled || checkpoint.Nodes["decide"] != NodeSkipped || checkpoint.Nodes["end"] != NodeSkipped {
		t.Fatalf("status = %q, nodes = %#v", checkpoint.Status, checkpoint.Nodes)
	}
}

func TestLoopExitClosesWhenTheLoopFinishes(t *testing.T) {
	definition := mustPrepare(t, gatedLoopTemplate(), nil)
	checkpoint, err := Initialize("run-gated-done", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("gate", "ok", 0)); err != nil {
		t.Fatal(err)
	}
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("decide", "done", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted || checkpoint.Nodes["aborted"] != NodeSkipped ||
		checkpoint.Edges["gate"]["abort"] != EdgeNotTaken {
		t.Fatalf("status = %q, aborted = %q", checkpoint.Status, checkpoint.Nodes["aborted"])
	}
}

// TestNestedLoopRestartsWithItsOuterRound checks an inner loop's count starts
// over with every outer round.
func TestNestedLoopRestartsWithItsOuterRound(t *testing.T) {
	inner := humanDecision("Polish again?", model.Next{"polished": "outer", "again": "polish"})
	inner.Loops = map[string]model.LoopBound{"again": {MaxIterations: 2}}
	outer := humanDecision("Start over?", model.Next{"ship": "end", "redo": "draft"})
	outer.Loops = map[string]model.LoopBound{"redo": {MaxIterations: 2}}
	definition := mustPrepare(t, &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "nested-loop", Start: "draft",
		Nodes: map[string]model.Node{
			"draft":  programTask("polish", "draft"),
			"polish": programTask("inner", "polish"),
			"inner":  inner,
			"outer":  outer,
			"end":    {Type: model.NodeTypeEnd},
		},
	}, nil)
	checkpoint, err := Initialize("run-nested", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = succeedUntilQuiet(t, checkpoint, definition)
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("inner", "again", 0)); err != nil {
		t.Fatal(err)
	}
	checkpoint, ran := succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 1 || ran[0].NodeID != "polish" || ran[0].Round != 2 || ran[0].Rounds != 2 {
		t.Fatalf("inner round 2 = %#v", ran)
	}
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("inner", "polished", 1)); err != nil {
		t.Fatal(err)
	}
	if checkpoint, err = Apply(checkpoint, definition, decidedAt("outer", "redo", 0)); err != nil {
		t.Fatal(err)
	}
	if _, ok := checkpoint.Iterations["inner"]; ok {
		t.Fatalf("inner count survived the outer round: %#v", checkpoint.Iterations)
	}
	checkpoint = roundTrip(t, checkpoint, definition)
	checkpoint, ran = succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 2 || ran[1].NodeID != "polish" || ran[1].Round != 1 || ran[0].Round != 2 {
		t.Fatalf("outer round 2 = %#v", ran)
	}
	// The inner loop has its rounds again.
	if _, err = Apply(checkpoint, definition, decidedAt("inner", "again", 2)); err != nil {
		t.Fatal(err)
	}
}

func TestLoopCheckpointBoundaryRefusesBadIterations(t *testing.T) {
	definition := mustPrepare(t, reviewLoopTemplate(model.LoopBound{MaxIterations: 3}), nil)
	checkpoint, err := Initialize("run-bad-iterations", definition)
	if err != nil {
		t.Fatal(err)
	}
	for _, iterations := range []map[string]map[string]int{
		{"decide": {"changes": 3}},
		{"decide": {"approved": 1}},
		{"review": {"changes": 1}},
	} {
		bad := cloneCheckpoint(checkpoint)
		bad.Iterations = iterations
		if err := ValidateCheckpoint(bad, definition); !errors.Is(err, ErrInvalidCheckpoint) {
			t.Fatalf("iterations %v: err = %v", iterations, err)
		}
	}
}

func TestUnboundedCycleNamesTheFix(t *testing.T) {
	tmpl := reviewLoopTemplate(model.LoopBound{MaxIterations: 3})
	decide := tmpl.Nodes["decide"]
	decide.Loops = nil
	tmpl.Nodes["decide"] = decide
	diagnostics := CheckEligibility(tmpl)
	if len(diagnostics) == 0 || diagnostics[0].Code != "graph_cycle" || !strings.Contains(diagnostics[0].Message, "maxIterations") {
		t.Fatalf("diagnostics = %#v", diagnostics)
	}
}
