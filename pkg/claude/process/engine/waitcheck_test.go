package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

var checkStart = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

// checkWaitTemplate is start → hold (a wait for check) → after → end, or with
// onLimit timedOut, hold → after (met) / late (timedOut).
func checkWaitTemplate(check model.WaitCheck) *model.Template {
	tmpl := sequentialTemplate("hold", "after")
	hold := model.Node{Type: model.NodeTypeWait, Wait: &model.WaitConfig{Check: &check}, Next: model.Next{model.DefaultOutcome: "after"}}
	if check.TimesOut() {
		hold.Next = model.Next{model.WaitOutcomeMet: "after", model.WaitOutcomeTimedOut: "late"}
		tmpl.Nodes["late"] = programTask("end", "late")
	}
	tmpl.Nodes["hold"] = hold
	return tmpl
}

func mustCheckWait(t *testing.T, check model.WaitCheck) (*Definition, Checkpoint) {
	t.Helper()
	tmpl := checkWaitTemplate(check)
	if diagnostics := CheckEligibility(tmpl); len(diagnostics) > 0 {
		t.Fatalf("check wait is executable, got %#v", diagnostics)
	}
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-check", definition)
	if err != nil {
		t.Fatal(err)
	}
	return definition, checkpoint
}

// timerDue observes a check wait's timer as the daemon would: due at dueAt.
func timerDue(command *Command, startedAt, dueAt time.Time) Transition {
	return Transition{Kind: TransitionProgramObserved, Observation: &ProgramObservation{
		CommandID: command.ID, NodeID: command.NodeID, Outcome: ProgramSucceeded,
		Wait: &WaitObservation{How: WaitHowElapsed, StartedAt: startedAt, DueAt: dueAt},
	}}
}

func mustApplyT(t *testing.T, checkpoint Checkpoint, definition *Definition, transition Transition) Checkpoint {
	t.Helper()
	next, err := Apply(checkpoint, definition, transition)
	if err != nil {
		t.Fatalf("apply %s: %v", transition.Kind, err)
	}
	return next
}

// A program check: the first check is made at once; "not yet" (exit 1) waits
// the interval, backing off; exit 0 ends the wait along its route.
func TestCheckWaitProgramAnswersUntilYes(t *testing.T) {
	definition, checkpoint := mustCheckWait(t, model.WaitCheck{
		Performer: &model.Performer{Kind: model.PerformerProgram, Run: "./ready.sh"},
		Every:     "5m", Backoff: model.WaitBackoffExponential, MaxEvery: "8m", Timeout: "30s", MaxChecks: 10,
	})
	checkpoint, timer := advanceAndPlan(t, checkpoint, definition)
	if timer == nil || timer.Kind != CommandWait || timer.Wait == nil || !timer.Wait.Now {
		t.Fatalf("the first check is made straight away: %#v", timer)
	}
	checkpoint = mustApplyT(t, checkpoint, definition, timerDue(timer, checkStart, checkStart))
	checkpoint, check := advanceAndPlan(t, checkpoint, definition)
	if check == nil || check.Kind != CommandProgram || check.Program.Run != "./ready.sh" || check.Program.Timeout != "30s" {
		t.Fatalf("check = %#v", check)
	}
	intervals := []string{"5m0s", "8m0s", "8m0s"} // 5m, then ×2 capped at 8m
	for i, want := range intervals {
		checkpoint = mustApplyT(t, checkpoint, definition, observed(check, ProgramFailed, 1))
		checkpoint, timer = advanceAndPlan(t, checkpoint, definition)
		if timer == nil || timer.Kind != CommandWait || timer.Wait.Duration != want || timer.Wait.Now {
			t.Fatalf("timer after check %d = %#v, want %s", i+1, timer, want)
		}
		checkpoint = mustApplyT(t, checkpoint, definition, timerDue(timer, checkStart, checkStart))
		checkpoint, check = advanceAndPlan(t, checkpoint, definition)
	}
	if state := checkpoint.WaitChecks["hold"]; state.Checks != 3 || state.Last != model.WaitCheckNo {
		t.Fatalf("state = %#v", state)
	}
	// Mid-check, the checkpoint loads cold: its check command is the one the
	// state says is out.
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpoint(encoded, definition); err != nil {
		t.Fatalf("a checking wait's checkpoint must load: %v", err)
	}
	if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
		t.Fatalf("classify: %v", err)
	}
	checkpoint = mustApplyT(t, checkpoint, definition, observed(check, ProgramSucceeded, 0))
	if checkpoint.Nodes["hold"] != NodeDone {
		t.Fatalf("a yes ends the wait: %#v", checkpoint.Nodes)
	}
	if _, next := advanceAndPlan(t, checkpoint, definition); next == nil || next.NodeID != "after" {
		t.Fatalf("the run goes on to after, got %#v", next)
	}
}

// Reaching maxChecks fails the step, naming it; with onLimit timedOut it
// takes the timedOut route instead.
func TestCheckWaitLimitFailsOrTimesOut(t *testing.T) {
	for _, onLimit := range []string{"", model.WaitOnLimitTimedOut} {
		definition, checkpoint := mustCheckWait(t, model.WaitCheck{
			Performer: &model.Performer{Kind: model.PerformerProgram, Run: "./ready.sh"},
			Every:     "1m", MaxChecks: 2, OnLimit: onLimit,
		})
		var command *Command
		for range 2 {
			checkpoint, command = advanceAndPlan(t, checkpoint, definition)
			checkpoint = mustApplyT(t, checkpoint, definition, timerDue(command, checkStart, checkStart))
			checkpoint, command = advanceAndPlan(t, checkpoint, definition)
			checkpoint = mustApplyT(t, checkpoint, definition, observed(command, ProgramFailed, 1))
		}
		if onLimit == "" {
			if checkpoint.Status != RunFailed || checkpoint.Failure == nil || checkpoint.Failure.NodeID != "hold" ||
				!strings.Contains(checkpoint.Failure.Reason, "not met after 2 checks") {
				t.Fatalf("fail: %#v %#v", checkpoint.Status, checkpoint.Failure)
			}
			continue
		}
		if _, next := advanceAndPlan(t, checkpoint, definition); next == nil || next.NodeID != "late" {
			t.Fatalf("timedOut goes to late, got %#v (%#v)", next, checkpoint.Nodes)
		}
	}
}

// within bounds the wait: each timer is capped at the start plus within, and
// one that comes due there ends the wait.
func TestCheckWaitWithinCapsTheTimerAndEndsIt(t *testing.T) {
	definition, checkpoint := mustCheckWait(t, model.WaitCheck{
		Performer: &model.Performer{Kind: model.PerformerProgram, Run: "./ready.sh"},
		Every:     "5m", Within: "7m", OnLimit: model.WaitOnLimitTimedOut,
	})
	checkpoint, timer := advanceAndPlan(t, checkpoint, definition)
	checkpoint = mustApplyT(t, checkpoint, definition, timerDue(timer, checkStart, checkStart))
	checkpoint, check := advanceAndPlan(t, checkpoint, definition)
	checkpoint = mustApplyT(t, checkpoint, definition, observed(check, ProgramFailed, 1))
	checkpoint, timer = advanceAndPlan(t, checkpoint, definition)
	if want := checkStart.Add(7 * time.Minute).Format(time.RFC3339); timer.Wait.Until != want {
		t.Fatalf("the timer is capped at within: %#v, want until %s", timer.Wait, want)
	}
	checkpoint = mustApplyT(t, checkpoint, definition, timerDue(timer, checkStart.Add(time.Minute), checkStart.Add(7*time.Minute)))
	if checkpoint.Nodes["hold"] != NodeDone || checkpoint.Nodes["late"] != NodeReady {
		t.Fatalf("within reached takes timedOut: %#v", checkpoint.Nodes)
	}
}

// Failed checks in a row (an error, not "no") fail the step at
// maxFailedChecks; a "no" in between starts the count again.
func TestCheckWaitFailedChecksInARow(t *testing.T) {
	definition, checkpoint := mustCheckWait(t, model.WaitCheck{
		Performer: &model.Performer{Kind: model.PerformerProgram, Run: "./ready.sh"},
		Every:     "1m", MaxChecks: 20, MaxFailedChecks: 2,
	})
	answers := []int{2, 1, 2, 2}
	for i, exit := range answers {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		checkpoint = mustApplyT(t, checkpoint, definition, timerDue(command, checkStart, checkStart))
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		transition := observed(command, ProgramFailed, exit)
		transition.Observation.Error = "exit status 2"
		checkpoint = mustApplyT(t, checkpoint, definition, transition)
		if i < len(answers)-1 && checkpoint.Status != RunRunning {
			t.Fatalf("after answer %d the run still waits: %#v", i, checkpoint.Failure)
		}
	}
	if checkpoint.Status != RunFailed || !strings.Contains(checkpoint.Failure.Reason, "2 checks in a row failed") {
		t.Fatalf("failure = %#v", checkpoint.Failure)
	}
}

// An agent check answers by its verdict.
func TestCheckWaitAgentAnswersByVerdict(t *testing.T) {
	definition, checkpoint := mustCheckWait(t, model.WaitCheck{
		Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "Is it ready?", Anyone: true},
		Every:     "10m", MaxChecks: 3,
	})
	checkpoint, timer := advanceAndPlan(t, checkpoint, definition)
	checkpoint = mustApplyT(t, checkpoint, definition, timerDue(timer, checkStart, checkStart))
	checkpoint, check := advanceAndPlan(t, checkpoint, definition)
	if check.Kind != CommandAgent || check.Agent == nil || len(check.Verdicts) != 2 {
		t.Fatalf("agent check = %#v", check)
	}
	yes := observed(check, ProgramSucceeded, 0)
	yes.Observation.Verdict = model.WaitCheckYes
	checkpoint = mustApplyT(t, checkpoint, definition, yes)
	if checkpoint.Nodes["hold"] != NodeDone {
		t.Fatalf("yes ends it: %#v", checkpoint.Nodes)
	}
}

// A rule check needs no check command: it is evaluated when a timer comes
// due, and as soon as the run makes it hold, here a step on another branch.
func TestCheckWaitRuleHoldsAsSoonAsAnotherBranchMakesIt(t *testing.T) {
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "rule-wait", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"work": "work", "hold": "hold"}},
			"work":  programTask("join", "build"),
			"hold": {Type: model.NodeTypeWait, Next: model.Next{model.DefaultOutcome: "join"},
				Wait: &model.WaitConfig{Check: &model.WaitCheck{When: "steps.work.result == succeeded", Every: "1h", MaxChecks: 5}}},
			"join": {Type: model.NodeTypeEnd, Join: model.JoinAll},
		},
	}
	if diagnostics := CheckEligibility(tmpl); len(diagnostics) > 0 {
		t.Fatalf("eligible: %#v", diagnostics)
	}
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-rule-wait", definition)
	if err != nil {
		t.Fatal(err)
	}
	commands := map[string]*Command{}
	for range 2 {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		commands[command.NodeID] = command
	}
	if commands["hold"] == nil || commands["hold"].Kind != CommandWait || commands["work"] == nil {
		t.Fatalf("planned = %#v", commands)
	}
	// The first check: work has not finished, so it waits an hour.
	checkpoint = mustApplyT(t, checkpoint, definition, timerDue(commands["hold"], checkStart, checkStart))
	checkpoint, timer := advanceAndPlan(t, checkpoint, definition)
	if timer == nil || timer.Kind != CommandWait || timer.Wait.Duration != "1h0m0s" {
		t.Fatalf("next timer = %#v", timer)
	}
	// work succeeds: the wait ends without waiting out its hour.
	checkpoint = mustApplyT(t, checkpoint, definition, observed(commands["work"], ProgramSucceeded, 0))
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Nodes["hold"] != NodeDone || checkpoint.Status != RunCompleted {
		t.Fatalf("the rule held: %#v %s", checkpoint.Nodes, checkpoint.Status)
	}
	if len(checkpoint.Commands) != 0 {
		t.Fatalf("the hour's timer is withdrawn: %#v", checkpoint.Commands)
	}
}

// A check that answers "not yet" after another branch doomed the run ends
// the wait, so the run can finish instead of waiting on a node nothing plans.
func TestCheckWaitInADoomedRunEnds(t *testing.T) {
	tmpl := &model.Template{
		APIVersion: model.APIVersion, Kind: model.Kind, ID: "doomed-wait", Start: "start",
		Nodes: map[string]model.Node{
			"start": {Type: model.NodeTypeStart, Next: model.Next{model.DefaultOutcome: "fork"}},
			"fork":  {Type: model.NodeTypeParallel, Next: model.Next{"work": "work", "hold": "hold"}},
			"work":  programTask("join", "build"),
			"hold": {Type: model.NodeTypeWait, Next: model.Next{model.DefaultOutcome: "join"},
				Wait: &model.WaitConfig{Check: &model.WaitCheck{
					Performer: &model.Performer{Kind: model.PerformerProgram, Run: "./ready.sh"}, Every: "1m", MaxChecks: 5}}},
			"join": {Type: model.NodeTypeEnd, Join: model.JoinAll},
		},
	}
	definition := mustPrepare(t, tmpl, nil)
	checkpoint, err := Initialize("run-doomed-wait", definition)
	if err != nil {
		t.Fatal(err)
	}
	commands := map[string]*Command{}
	for range 2 {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		commands[command.NodeID] = command
	}
	checkpoint = mustApplyT(t, checkpoint, definition, timerDue(commands["hold"], checkStart, checkStart))
	checkpoint, check := advanceAndPlan(t, checkpoint, definition)
	if check == nil || check.Kind != CommandProgram {
		t.Fatalf("check = %#v", check)
	}
	checkpoint = mustApplyT(t, checkpoint, definition, observed(commands["work"], ProgramFailed, 2))
	checkpoint = mustApplyT(t, checkpoint, definition, observed(check, ProgramFailed, 1))
	if checkpoint.Status != RunFailed || checkpoint.Failure == nil || checkpoint.Failure.NodeID != "work" {
		t.Fatalf("the run finishes failed, naming work: %s %#v %#v", checkpoint.Status, checkpoint.Failure, checkpoint.Nodes)
	}
}

// An agent check's answer other than yes or no is refused, the check still
// out, so the agent can answer again; it is not a failed check.
func TestCheckWaitRefusesAVerdictOtherThanYesOrNo(t *testing.T) {
	definition, checkpoint := mustCheckWait(t, model.WaitCheck{
		Performer: &model.Performer{Kind: model.PerformerAgent, Prompt: "Is it ready?", Anyone: true},
		Every:     "10m", MaxChecks: 3,
	})
	checkpoint, timer := advanceAndPlan(t, checkpoint, definition)
	checkpoint = mustApplyT(t, checkpoint, definition, timerDue(timer, checkStart, checkStart))
	checkpoint, check := advanceAndPlan(t, checkpoint, definition)
	maybe := observed(check, ProgramSucceeded, 0)
	maybe.Observation.Verdict = "maybe"
	if _, err := Apply(checkpoint, definition, maybe); !errors.Is(err, ErrInvalidDecisionVerdict) {
		t.Fatalf("maybe is refused, got %v", err)
	}
}

// within is measured to the second the timer's until says: a start with a
// fraction of a second still ends at the timer that is due at the limit.
func TestCheckWaitWithinMatchesTheTimersSecond(t *testing.T) {
	definition, checkpoint := mustCheckWait(t, model.WaitCheck{
		Performer: &model.Performer{Kind: model.PerformerProgram, Run: "./ready.sh"},
		Every:     "5m", Within: "5m", OnLimit: model.WaitOnLimitTimedOut,
	})
	started := checkStart.Add(400 * time.Millisecond)
	checkpoint, timer := advanceAndPlan(t, checkpoint, definition)
	checkpoint = mustApplyT(t, checkpoint, definition, timerDue(timer, started, started))
	checkpoint, check := advanceAndPlan(t, checkpoint, definition)
	checkpoint = mustApplyT(t, checkpoint, definition, observed(check, ProgramFailed, 1))
	checkpoint, timer = advanceAndPlan(t, checkpoint, definition)
	due, err := time.Parse(time.RFC3339, timer.Wait.Until)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = mustApplyT(t, checkpoint, definition, timerDue(timer, started, due))
	if checkpoint.Nodes["late"] != NodeReady {
		t.Fatalf("the limit is reached at the timer's until: %#v", checkpoint.Nodes)
	}
}
