package engine

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// TestAttemptsAreMonotonicAndMintDistinctCommandIdentities is the core identity
// property: the first attempt of a node is 1, every retry advances the counter
// by exactly one, and each attempt's deterministic command id differs from
// every earlier one so no two attempts of a node ever share an identity.
func TestAttemptsAreMonotonicAndMintDistinctCommandIdentities(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 3}), nil)
	checkpoint, err := Initialize("run-attempts", definition)
	if err != nil {
		t.Fatal(err)
	}
	if len(checkpoint.Attempts) != 0 {
		t.Fatalf("a pre-execution checkpoint carries attempts: %#v", checkpoint.Attempts)
	}

	seen := map[string]int{}
	for attempt := 1; attempt <= 3; attempt++ {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		if command == nil {
			t.Fatalf("attempt %d was not planned", attempt)
		}
		if command.Attempt != attempt {
			t.Fatalf("command attempt = %d, want %d", command.Attempt, attempt)
		}
		if got := checkpoint.Attempts["task"]; got != attempt {
			t.Fatalf("checkpoint attempt = %d, want %d", got, attempt)
		}
		if previous, dup := seen[command.ID]; dup {
			t.Fatalf("attempt %d reused the command id of attempt %d: %q", attempt, previous, command.ID)
		}
		seen[command.ID] = attempt
		if checkpoint.Nodes["task"] != NodeRunning || len(checkpoint.Commands) != 1 {
			t.Fatalf("state while attempt %d runs = %#v", attempt, checkpoint)
		}

		checkpoint, err = Apply(checkpoint, definition, observed(command, ProgramFailed, 1))
		if err != nil {
			t.Fatal(err)
		}
		if got := checkpoint.Attempts["task"]; got != attempt {
			t.Fatalf("a failed observation moved the counter to %d, want %d", got, attempt)
		}
		if len(checkpoint.Commands) != 0 {
			t.Fatalf("the failed attempt's command survived: %#v", checkpoint.Commands)
		}
		if attempt < 3 {
			// Within budget: only this node is re-readied, and the run stays live.
			if checkpoint.Nodes["task"] != NodeReady || checkpoint.Status != RunRunning {
				t.Fatalf("state after failed attempt %d = %#v", attempt, checkpoint)
			}
		}
	}
	// The third failure exhausted the authored budget, so this slice keeps
	// today's failure disposition.
	if checkpoint.Nodes["task"] != NodeFailed || checkpoint.Status != RunFailed {
		t.Fatalf("exhausted checkpoint = %#v", checkpoint)
	}
	if got := checkpoint.Attempts["task"]; got != 3 {
		t.Fatalf("exhausted attempt counter = %d, want 3", got)
	}
	quiescent, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(quiescent, checkpoint) {
		t.Fatalf("an exhausted run advanced: %#v", quiescent)
	}
	if _, plannable := nextPlannableTask(checkpoint, definition); plannable {
		t.Fatal("an exhausted run is still plannable")
	}
}

// TestSuccessAfterRetryCompletesNormally proves a retried branch is ordinary
// afterwards: the winning attempt settles its edges and the run reaches its end.
func TestSuccessAfterRetryCompletesNormally(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 2}), nil)
	checkpoint, err := Initialize("run-recovers", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, first := advanceAndPlan(t, checkpoint, definition)
	checkpoint, err = Apply(checkpoint, definition, observed(first, ProgramFailed, 1))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, second := advanceAndPlan(t, checkpoint, definition)
	if second.Attempt != 2 || second.ID == first.ID {
		t.Fatalf("retry command = %#v, first = %#v", second, first)
	}
	checkpoint, err = Apply(checkpoint, definition, observed(second, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["task"] != NodeDone || checkpoint.Edges["task"][model.DefaultOutcome] != EdgeArrived {
		t.Fatalf("state after the successful retry = %#v", checkpoint)
	}
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q, want completed", checkpoint.Status)
	}
	if got := checkpoint.Attempts["task"]; got != 2 {
		t.Fatalf("completed attempt counter = %d, want 2", got)
	}
	// The strict sequential classifier still holds across a retried run.
	if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
		t.Fatalf("retried run failed the strict classifier: %v", err)
	}
}

// TestOlderAttemptObservationsAreStale is the whole point of binding the
// attempt into the command identity: a delayed, duplicated, or forged
// observation of a superseded attempt matches no outbox entry and leaves the
// checkpoint byte-identical.
func TestOlderAttemptObservationsAreStale(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 3}), nil)
	checkpoint, err := Initialize("run-stale-attempt", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, first := advanceAndPlan(t, checkpoint, definition)
	checkpoint, err = Apply(checkpoint, definition, observed(first, ProgramFailed, 1))
	if err != nil {
		t.Fatal(err)
	}
	retrying, second := advanceAndPlan(t, checkpoint, definition)
	before := cloneCheckpoint(retrying)

	for _, test := range []struct {
		name       string
		transition Transition
	}{
		// The attempt-1 program reporting late, after attempt 2 was already
		// planned. Both outcomes must be refused: a delayed success is exactly as
		// dangerous as a delayed failure.
		{name: "delayed failure", transition: observed(first, ProgramFailed, 1)},
		{name: "delayed success", transition: observed(first, ProgramSucceeded, 0)},
		{name: "forged next attempt", transition: observed(&Command{
			ID: "cmd_17_run-stale-attempt_4_task_3_program", NodeID: "task", Attempt: 3,
		}, ProgramSucceeded, 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Apply(retrying, definition, test.transition); !errors.Is(err, ErrStaleObservation) {
				t.Fatalf("error = %v, want ErrStaleObservation", err)
			}
			if !reflect.DeepEqual(retrying, before) {
				t.Fatalf("a refused observation mutated state\n got: %#v\nwant: %#v", retrying, before)
			}
		})
	}

	// The current attempt still binds, and a duplicate of it is stale afterwards.
	settled, err := Apply(retrying, definition, observed(second, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(settled, definition, observed(second, ProgramSucceeded, 0)); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("duplicate current-attempt error = %v", err)
	}
}

// TestReplanningAnAlreadyCommittedAttemptIsRefused proves the planning
// transition is bound to the counter, not merely to node status: replaying the
// exact command that was already committed for attempt N — even once the node
// is ready again for N+1 — fails closed rather than re-running that identity.
func TestReplanningAnAlreadyCommittedAttemptIsRefused(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 2}), nil)
	checkpoint, err := Initialize("run-replan", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, first := advanceAndPlan(t, checkpoint, definition)
	readyAgain, err := Apply(checkpoint, definition, observed(first, ProgramFailed, 1))
	if err != nil {
		t.Fatal(err)
	}
	if readyAgain.Nodes["task"] != NodeReady {
		t.Fatalf("node status = %q, want ready", readyAgain.Nodes["task"])
	}
	before := cloneCheckpoint(readyAgain)
	replay := Transition{Kind: TransitionCommandPlanned, Command: first}
	if _, err := Apply(readyAgain, definition, replay); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("replayed plan error = %v", err)
	}
	if !reflect.DeepEqual(readyAgain, before) {
		t.Fatalf("a refused plan mutated state: %#v", readyAgain)
	}

	// And a plan past the authored budget is refused even if a caller hands the
	// reducer the state to do it.
	exhausted, second := advanceAndPlan(t, readyAgain, definition)
	exhausted, err = Apply(exhausted, definition, observed(second, ProgramFailed, 1))
	if err != nil {
		t.Fatal(err)
	}
	overBudget := cloneCheckpoint(exhausted)
	overBudget.Nodes["task"] = NodeReady
	overBudget.Status = RunRunning
	third := programCommand(overBudget.RunID, definition.nodes[definition.index["task"]], 3)
	if _, err := Apply(overBudget, definition, Transition{Kind: TransitionCommandPlanned, Command: &third}); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("over-budget plan error = %v", err)
	}
}

// TestRetryingBranchLeavesItsSiblingUntouched is the fan-out property: a branch
// burning through its own attempts must not clear, renumber, or stall the
// sibling command that is still running beside it.
func TestRetryingBranchLeavesItsSiblingUntouched(t *testing.T) {
	tmpl := fanOutTemplate("left", "right")
	left := tmpl.Nodes["left"]
	left.Retry = &model.RetryPolicy{MaxAttempts: 3}
	tmpl.Nodes["left"] = left
	definition := mustPrepare(t, tmpl, nil)

	checkpoint, err := Initialize("run-branch-retry", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, leftFirst := advanceAndPlan(t, checkpoint, definition)
	checkpoint, rightCommand := advanceAndPlan(t, checkpoint, definition)
	if leftFirst.NodeID != "left" || rightCommand.NodeID != "right" {
		t.Fatalf("planned %q then %q, want left then right", leftFirst.NodeID, rightCommand.NodeID)
	}
	siblingBefore, _ := findCommandForTest(checkpoint, "right")

	// Two failed attempts on the left branch, with a fresh plan in between.
	checkpoint, err = Apply(checkpoint, definition, observed(leftFirst, ProgramFailed, 1))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, leftSecond := advanceAndPlan(t, checkpoint, definition)
	if leftSecond.NodeID != "left" || leftSecond.Attempt != 2 {
		t.Fatalf("left retry = %#v", leftSecond)
	}
	checkpoint, err = Apply(checkpoint, definition, observed(leftSecond, ProgramFailed, 1))
	if err != nil {
		t.Fatal(err)
	}

	sibling, ok := findCommandForTest(checkpoint, "right")
	if !ok || !reflect.DeepEqual(sibling, siblingBefore) {
		t.Fatalf("the sibling command changed\n got: %#v\nwant: %#v", sibling, siblingBefore)
	}
	if checkpoint.Nodes["right"] != NodeRunning {
		t.Fatalf("sibling status = %q, want running", checkpoint.Nodes["right"])
	}
	if got := checkpoint.Attempts["right"]; got != 1 {
		t.Fatalf("sibling attempt counter = %d, want 1", got)
	}
	if got := checkpoint.Attempts["left"]; got != 2 {
		t.Fatalf("retrying branch attempt counter = %d, want 2", got)
	}
	// The sibling is still individually observable, and settling it does not
	// disturb the branch that is mid-retry.
	settled, err := Apply(checkpoint, definition, observed(rightCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatalf("the sibling could not settle while its neighbour retried: %v", err)
	}
	if settled.Nodes["right"] != NodeDone || settled.Nodes["left"] != NodeReady {
		t.Fatalf("state after the sibling settled = %#v", settled)
	}
	if got := settled.Attempts["left"]; got != 2 {
		t.Fatalf("the sibling's settlement renumbered the retrying branch to %d", got)
	}
}

// TestColdLoadPreservesTheExactCurrentAttempt covers restart: the counter and
// the outstanding attempt's exact command survive an encode/decode round trip,
// and nothing about loading re-plans or re-mints them.
func TestColdLoadPreservesTheExactCurrentAttempt(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 4}), nil)
	checkpoint, err := Initialize("run-cold", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, first := advanceAndPlan(t, checkpoint, definition)
	checkpoint, err = Apply(checkpoint, definition, observed(first, ProgramFailed, 1))
	if err != nil {
		t.Fatal(err)
	}
	running, second := advanceAndPlan(t, checkpoint, definition)

	encoded, err := json.Marshal(running)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := DecodeCheckpoint(encoded, definition)
	if err != nil {
		t.Fatalf("cold load: %v", err)
	}
	if !reflect.DeepEqual(loaded, running) {
		t.Fatalf("cold load changed state\n got: %#v\nwant: %#v", loaded, running)
	}
	if loaded.Attempts["task"] != 2 || len(loaded.Commands) != 1 || loaded.Commands[0].ID != second.ID {
		t.Fatalf("cold-loaded attempt state = %#v", loaded)
	}
	// A cold load never re-issues the outstanding attempt: the node is running,
	// so nothing is plannable and the outbox is left exactly as it was.
	replanned, planned, _, err := AdvanceAndPlan(loaded, definition)
	if err != nil {
		t.Fatal(err)
	}
	if planned != nil {
		t.Fatalf("a cold load auto-reissued the outstanding attempt: %#v", planned)
	}
	if !reflect.DeepEqual(replanned.Commands, loaded.Commands) || replanned.Attempts["task"] != 2 {
		t.Fatalf("a cold load disturbed the outstanding attempt: %#v", replanned)
	}
}

// TestBoundaryRefusesImpossibleAttemptCommandPairings pins what the load
// boundary owes: attempt counters that the pinned definition could never have
// produced, and outbox entries that disagree with the counter, are refused
// there — with no hash, no replay, and no second validation pass.
func TestBoundaryRefusesImpossibleAttemptCommandPairings(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 2}), nil)
	base, err := Initialize("run-boundary", definition)
	if err != nil {
		t.Fatal(err)
	}
	task := definition.nodes[definition.index["task"]]
	running := func(attempt int, command Command) Checkpoint {
		checkpoint := cloneCheckpoint(base)
		checkpoint.Nodes["task"] = NodeRunning
		checkpoint.Attempts = map[string]int{"task": attempt}
		checkpoint.Commands = []Command{command}
		return checkpoint
	}

	// The honest shape this is all measured against.
	if err := ValidateCheckpoint(running(2, programCommand(base.RunID, task, 2)), definition); err != nil {
		t.Fatalf("a valid retried state was refused: %v", err)
	}

	for _, test := range []struct {
		name       string
		checkpoint func() Checkpoint
	}{
		{
			name: "attempt counter names an unknown node",
			checkpoint: func() Checkpoint {
				checkpoint := cloneCheckpoint(base)
				checkpoint.Attempts = map[string]int{"nowhere": 1}
				return checkpoint
			},
		},
		{
			name: "attempt counter names a node that never executes",
			checkpoint: func() Checkpoint {
				checkpoint := cloneCheckpoint(base)
				checkpoint.Attempts = map[string]int{"start": 1}
				return checkpoint
			},
		},
		{
			name: "attempt below the first attempt",
			checkpoint: func() Checkpoint {
				checkpoint := cloneCheckpoint(base)
				checkpoint.Attempts = map[string]int{"task": 0}
				return checkpoint
			},
		},
		{
			name: "attempt past the authored budget",
			checkpoint: func() Checkpoint {
				checkpoint := cloneCheckpoint(base)
				checkpoint.Attempts = map[string]int{"task": 3}
				return checkpoint
			},
		},
		{
			name: "command of a superseded attempt",
			checkpoint: func() Checkpoint {
				return running(2, programCommand(base.RunID, task, 1))
			},
		},
		{
			name: "command ahead of the counter",
			checkpoint: func() Checkpoint {
				return running(1, programCommand(base.RunID, task, 2))
			},
		},
		{
			name: "command id and attempt field disagree",
			checkpoint: func() Checkpoint {
				command := programCommand(base.RunID, task, 2)
				command.Attempt = 1
				return running(2, command)
			},
		},
		{
			name: "outstanding command with no attempt counter at all",
			checkpoint: func() Checkpoint {
				checkpoint := running(1, programCommand(base.RunID, task, 1))
				checkpoint.Attempts = nil
				return checkpoint
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkpoint := test.checkpoint()
			if err := ValidateCheckpoint(checkpoint, definition); !errors.Is(err, ErrInvalidCheckpoint) {
				t.Fatalf("validator error = %v, want ErrInvalidCheckpoint", err)
			}
			encoded, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeCheckpoint(encoded, definition); !errors.Is(err, ErrInvalidCheckpoint) {
				t.Fatalf("decode error = %v, want ErrInvalidCheckpoint", err)
			}
		})
	}
}

// TestCheckpointOmitsTheAttemptCounterUntilSomethingRuns keeps the counter
// sparse on the wire: a run that has planned nothing encodes no attempts key at
// all, and one that has encodes only the nodes that actually executed.
func TestCheckpointOmitsTheAttemptCounterUntilSomethingRuns(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 2}), nil)
	checkpoint, err := Initialize("run-sparse", definition)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encoded); !json.Valid(encoded) || strings.Contains(got, `"attempts"`) {
		t.Fatalf("pre-execution checkpoint carries an attempts key: %s", got)
	}
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	encoded, err = json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(encoded); !strings.Contains(got, `"attempts":{"task":1}`) {
		t.Fatalf("running checkpoint attempts = %s", got)
	}
}

func findCommandForTest(checkpoint Checkpoint, nodeID string) (Command, bool) {
	for _, command := range checkpoint.Commands {
		if command.NodeID == nodeID {
			return command, true
		}
	}
	return Command{}, false
}
