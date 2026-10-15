package engine

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// agentReviewLoopTemplate is reviewLoopTemplate with its decision delegated
// to an agent.
func agentReviewLoopTemplate(bound model.LoopBound) *model.Template {
	tmpl := reviewLoopTemplate(bound)
	decide := tmpl.Nodes["decide"]
	decide.Performer = &model.Performer{Kind: model.PerformerAgent, Role: "reviewer", Prompt: "Review the change and decide."}
	tmpl.Nodes["decide"] = decide
	return tmpl
}

func reportedVerdict(command *Command, verdict string) Transition {
	transition := observed(command, ProgramSucceeded, 0)
	transition.Observation.Verdict = verdict
	return transition
}

// runUntilDecider succeeds every task command until the agent decider's
// command is planned, and returns it.
func runUntilDecider(t *testing.T, checkpoint Checkpoint, definition *Definition) (Checkpoint, *Command) {
	t.Helper()
	for {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
			t.Fatalf("classify after plan: %v", err)
		}
		if command == nil || command.NodeID == "decide" {
			return checkpoint, command
		}
		var err error
		if checkpoint, err = Apply(checkpoint, definition, observed(command, ProgramSucceeded, 0)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgentDeciderIsPlannedAsACommandNotAnObligation(t *testing.T) {
	definition := mustPrepare(t, agentReviewLoopTemplate(model.LoopBound{MaxIterations: 3, OnExhausted: "escalate"}), nil)
	checkpoint, err := Initialize("run-agent-decide", definition)
	if err != nil {
		t.Fatal(err)
	}
	for round := 1; round <= 2; round++ {
		var decider *Command
		checkpoint, decider = runUntilDecider(t, checkpoint, definition)
		if decider == nil || decider.Kind != CommandAgent || decider.Attempt != round || decider.Round != round || decider.Rounds != 3 {
			t.Fatalf("round %d decider command = %#v", round, decider)
		}
		if decider.Agent == nil || decider.Agent.Role != "reviewer" {
			t.Fatalf("decider agent = %#v", decider.Agent)
		}
		if len(checkpoint.AwaitingDecisions) != 0 {
			t.Fatalf("an agent decider must not park on a person: %#v", checkpoint.AwaitingDecisions)
		}
		// A person cannot decide for the agent.
		if _, err := Apply(checkpoint, definition, decidedAt("decide", "approved", round)); !errors.Is(err, ErrStaleDecision) {
			t.Fatalf("human verdict on an agent decision err = %v", err)
		}
		checkpoint = roundTrip(t, checkpoint, definition)
		if round == 1 {
			checkpoint, err = Apply(checkpoint, definition, reportedVerdict(decider, "changes"))
			if err != nil {
				t.Fatal(err)
			}
			if got := checkpoint.Iterations["decide"]["changes"]; got != 1 {
				t.Fatalf("iterations = %d", got)
			}
			checkpoint = roundTrip(t, checkpoint, definition)
			continue
		}
		checkpoint, err = Apply(checkpoint, definition, reportedVerdict(decider, "approved"))
		if err != nil {
			t.Fatal(err)
		}
	}
	checkpoint, ran := succeedUntilQuiet(t, checkpoint, definition)
	if len(ran) != 1 || ran[0].NodeID != "merge" || checkpoint.Status != RunCompleted {
		t.Fatalf("after approval ran %#v, status %q", ran, checkpoint.Status)
	}
}

func TestAgentDeciderReportsAreChecked(t *testing.T) {
	definition := mustPrepare(t, agentReviewLoopTemplate(model.LoopBound{MaxIterations: 2}), nil)
	checkpoint, err := Initialize("run-agent-decide-checks", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, decider := runUntilDecider(t, checkpoint, definition)
	refused := func(name string, transition Transition, want error) {
		t.Helper()
		if _, err := Apply(checkpoint, definition, transition); !errors.Is(err, want) {
			t.Fatalf("%s: err = %v, want %v", name, err, want)
		}
	}
	refused("no verdict", observed(decider, ProgramSucceeded, 0), ErrInvalidDecisionVerdict)
	refused("unknown verdict", reportedVerdict(decider, "maybe"), ErrInvalidDecisionVerdict)
	failedWithVerdict := reportedVerdict(decider, "approved")
	failedWithVerdict.Observation.Outcome, failedWithVerdict.Observation.ExitCode = ProgramFailed, 1
	refused("failure with a verdict", failedWithVerdict, ErrInvalidTransition)

	// Round two of two: going back again has no onExhausted to follow, so the
	// verdict is refused and the decider's command stays outstanding for
	// another report.
	checkpoint, err = Apply(checkpoint, definition, reportedVerdict(decider, "changes"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, decider = runUntilDecider(t, checkpoint, definition)
	refused("exhausted", reportedVerdict(decider, "changes"), ErrLoopExhausted)
	if _, ok := findCommand(checkpoint, decider.ID, decider.NodeID); !ok {
		t.Fatal("the refused report consumed the decider's command")
	}
	if _, err := Apply(checkpoint, definition, reportedVerdict(decider, "escalate")); err != nil {
		t.Fatal(err)
	}
}

func TestFailedAgentDeciderFailsTheRun(t *testing.T) {
	definition := mustPrepare(t, agentReviewLoopTemplate(model.LoopBound{MaxIterations: 3, OnExhausted: "escalate"}), nil)
	checkpoint, err := Initialize("run-agent-decide-fail", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, decider := runUntilDecider(t, checkpoint, definition)
	checkpoint, err = Apply(checkpoint, definition, observedWithError(decider, "could not review"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
		t.Fatal(err)
	}
	if checkpoint.Status != RunFailed || checkpoint.Nodes["decide"] != NodeFailed {
		t.Fatalf("status %q, decide %q", checkpoint.Status, checkpoint.Nodes["decide"])
	}
}

func TestHumanAndAgentDecidersPlanTheSameRounds(t *testing.T) {
	// The loop's tasks get the same commands whoever decides.
	bound := model.LoopBound{MaxIterations: 3, OnExhausted: "escalate"}
	human := mustPrepare(t, reviewLoopTemplate(bound), nil)
	agent := mustPrepare(t, agentReviewLoopTemplate(bound), nil)
	humanRun, err := Initialize("run-same", human)
	if err != nil {
		t.Fatal(err)
	}
	agentRun, err := Initialize("run-same", agent)
	if err != nil {
		t.Fatal(err)
	}
	humanRun, _ = succeedUntilQuiet(t, humanRun, human)
	agentRun, decider := runUntilDecider(t, agentRun, agent)
	if humanRun, err = Apply(humanRun, human, decidedAt("decide", "changes", 0)); err != nil {
		t.Fatal(err)
	}
	if agentRun, err = Apply(agentRun, agent, reportedVerdict(decider, "changes")); err != nil {
		t.Fatal(err)
	}
	_, humanRan := succeedUntilQuiet(t, humanRun, human)
	var agentRan []Command
	for {
		var command *Command
		agentRun, command = advanceAndPlan(t, agentRun, agent)
		if command == nil || command.NodeID == "decide" {
			break
		}
		agentRan = append(agentRan, *command)
		if agentRun, err = Apply(agentRun, agent, observed(command, ProgramSucceeded, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(humanRan, agentRan) {
		t.Fatalf("human round 2 = %#v\nagent round 2 = %#v", humanRan, agentRan)
	}
}

// TestAgentReviewGateSendsTheWorkBack: an agent do stage and an agent review
// gate. A failed review re-runs the do stage under the compound's budget,
// exactly as a failed program gate does.
func TestAgentReviewGateSendsTheWorkBack(t *testing.T) {
	tmpl := sequentialTemplate("task")
	node := tmpl.Nodes["task"]
	node.Performer = &model.Performer{Kind: model.PerformerAgent, Role: "dev", Prompt: "Do it"}
	node.Retry = &model.RetryPolicy{MaxAttempts: 2, OnFail: model.RetryModeFeedbackSameSession}
	node.Review = &model.Step{ID: "rev", Performer: model.Performer{Kind: model.PerformerAgent, Role: "reviewer", Prompt: "Review it"}}
	tmpl.Nodes["task"] = node
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-agent-gate", definition)
	if err != nil {
		t.Fatal(err)
	}
	var trail []string
	step := func(succeed bool) {
		t.Helper()
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		if command == nil {
			t.Fatalf("nothing planned after %v", trail)
		}
		if command.Kind != CommandAgent || command.Agent == nil {
			t.Fatalf("%s planned %#v", command.NodeID, command)
		}
		trail = append(trail, fmt.Sprintf("%s#%d", command.NodeID, command.Attempt))
		transition := observed(command, ProgramSucceeded, 0)
		if !succeed {
			transition = observedWithError(command, "needs tests")
		}
		if checkpoint, err = Apply(checkpoint, definition, transition); err != nil {
			t.Fatal(err)
		}
		if err := ValidateCheckpoint(checkpoint, definition); err != nil {
			t.Fatal(err)
		}
	}
	step(true)  // do
	step(false) // review sends it back
	step(true)  // do, attempt 2
	step(true)  // review
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if want := "task.do#1 task.review#1 task.do#2 task.review#2"; strings.Join(trail, " ") != want {
		t.Fatalf("trail = %v, want %s", trail, want)
	}
	if checkpoint.Status != RunCompleted {
		t.Fatalf("status = %q", checkpoint.Status)
	}
}
