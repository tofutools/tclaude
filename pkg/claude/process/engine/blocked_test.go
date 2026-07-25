package engine

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// resolve builds one operator resolution transition.
func resolve(nodeID string, attempt int, action ResolutionAction) Transition {
	return Transition{
		Kind:       TransitionBlockedResolved,
		Resolution: &BlockedResolution{NodeID: nodeID, Attempt: attempt, Action: action},
	}
}

// exhaust plans and fails one task until its budget runs out, returning the
// checkpoint that disposition left behind.
func exhaust(t *testing.T, checkpoint Checkpoint, definition *Definition, nodeID string, attempts int) Checkpoint {
	t.Helper()
	for range attempts {
		var command *Command
		checkpoint, command = advanceAndPlan(t, checkpoint, definition)
		if command == nil || command.NodeID != nodeID {
			t.Fatalf("expected a command for %q; got %#v", nodeID, command)
		}
		next, err := Apply(checkpoint, definition, observedWithError(command, "program exited 1"))
		if err != nil {
			t.Fatal(err)
		}
		checkpoint = next
	}
	return checkpoint
}

func observedWithError(command *Command, message string) Transition {
	transition := observed(command, ProgramFailed, 1)
	transition.Observation.Error = message
	return transition
}

// TestExplicitRetryExhaustionBlocksTheBranch is the central disposition change:
// an author who asked for retries gets a parked branch and a live run, not a
// failed one.
func TestExplicitRetryExhaustionBlocksTheBranch(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 2}), nil)
	checkpoint, err := Initialize("run-blocks", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = exhaust(t, checkpoint, definition, "task", 2)

	if checkpoint.Status != RunRunning {
		t.Fatalf("an exhausted retry-authored branch doomed the run: %q", checkpoint.Status)
	}
	if checkpoint.Nodes["task"] != NodeBlocked {
		t.Fatalf("task status = %q, want blocked", checkpoint.Nodes["task"])
	}
	if len(checkpoint.Blocked) != 1 || checkpoint.Blocked[0].NodeID != "task" {
		t.Fatalf("blocked outbox = %#v", checkpoint.Blocked)
	}
	if checkpoint.Blocked[0].Reason != "program exited 1" {
		t.Fatalf("blocked reason = %q, want the exhausted observation's error", checkpoint.Blocked[0].Reason)
	}
	// The exact blocked identity is the node plus its unmoved attempt counter.
	if got := checkpoint.Attempts["task"]; got != 2 {
		t.Fatalf("blocked attempt = %d, want 2", got)
	}
	// A parked branch is not a doomed one, and it is not plannable either.
	if Draining(checkpoint) {
		t.Fatal("a blocked branch reported the run as draining")
	}
	if Runnable(checkpoint, definition) {
		t.Fatal("a blocked-only run reported pushable work")
	}
	quiescent, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(quiescent, checkpoint) {
		t.Fatalf("a blocked run advanced: %#v", quiescent)
	}
	if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
		t.Fatalf("a blocked sequential run failed the strict classifier: %v", err)
	}
}

// TestExhaustionDispositionFollowsTheAuthoredPolicy pins the one thing
// maxAttempts alone cannot express: an explicit maxAttempts: 1 parks, and no
// policy at all stays fail-fast, even though both allow exactly one attempt.
func TestExhaustionDispositionFollowsTheAuthoredPolicy(t *testing.T) {
	for _, test := range []struct {
		name   string
		retry  *model.RetryPolicy
		status NodeStatus
		run    RunStatus
	}{
		{name: "explicit single attempt blocks", retry: &model.RetryPolicy{MaxAttempts: 1},
			status: NodeBlocked, run: RunRunning},
		{name: "no authored policy stays fail-fast", retry: nil,
			status: NodeFailed, run: RunFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			definition := mustPrepare(t, retryTemplate(test.retry), nil)
			checkpoint, err := Initialize("run-disposition", definition)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint = exhaust(t, checkpoint, definition, "task", 1)
			if checkpoint.Nodes["task"] != test.status || checkpoint.Status != test.run {
				t.Fatalf("task = %q and run = %q, want %q and %q",
					checkpoint.Nodes["task"], checkpoint.Status, test.status, test.run)
			}
			if blocked := len(checkpoint.Blocked) > 0; blocked != (test.status == NodeBlocked) {
				t.Fatalf("blocked outbox = %#v for status %q", checkpoint.Blocked, test.status)
			}
			if err := ClassifyCheckpoint(checkpoint, definition); err != nil {
				t.Fatalf("strict classifier: %v", err)
			}
		})
	}
}

// TestRetryResolutionRaisesTheCeilingWithoutReusingAnAttempt is the retry
// contract: one fresh authored-size window, a strictly advancing attempt
// counter, and an ordinary re-block once that window is spent too.
func TestRetryResolutionRaisesTheCeilingWithoutReusingAnAttempt(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 2}), nil)
	checkpoint, err := Initialize("run-reretry", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = exhaust(t, checkpoint, definition, "task", 2)
	firstIDs := map[string]struct{}{}

	resolved, err := Apply(checkpoint, definition, resolve("task", 2, ResolveRetry))
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Nodes["task"] != NodeReady || len(resolved.Blocked) != 0 {
		t.Fatalf("state after retry = %#v", resolved)
	}
	// Attempts is untouched: the operator bought headroom, not a reset.
	if got := resolved.Attempts["task"]; got != 2 {
		t.Fatalf("retry moved the attempt counter to %d, want 2", got)
	}
	if got := resolved.AttemptCeilings["task"]; got != 4 {
		t.Fatalf("raised ceiling = %d, want attempts(2) + authored budget(2)", got)
	}
	// Ordinary planning mints 3 then 4, and both are identities no earlier
	// attempt ever used.
	for attempt := 3; attempt <= 4; attempt++ {
		var command *Command
		resolved, command = advanceAndPlan(t, resolved, definition)
		if command == nil || command.Attempt != attempt {
			t.Fatalf("planned %#v, want attempt %d", command, attempt)
		}
		if _, dup := firstIDs[command.ID]; dup {
			t.Fatalf("attempt %d reused an earlier command identity", attempt)
		}
		firstIDs[command.ID] = struct{}{}
		resolved, err = Apply(resolved, definition, observedWithError(command, "still failing"))
		if err != nil {
			t.Fatal(err)
		}
	}
	// The raised window is spent, so the branch parks again — at the new exact
	// attempt, which is what the next resolution has to name.
	if resolved.Nodes["task"] != NodeBlocked || resolved.Status != RunRunning {
		t.Fatalf("state after the raised window = %#v", resolved)
	}
	if got := resolved.Attempts["task"]; got != 4 {
		t.Fatalf("re-blocked attempt = %d, want 4", got)
	}
	if _, err := Apply(resolved, definition, resolve("task", 2, ResolveRetry)); !errors.Is(err, ErrStaleResolution) {
		t.Fatalf("the superseded attempt was still resolvable: %v", err)
	}
	if err := ClassifyCheckpoint(resolved, definition); err != nil {
		t.Fatalf("strict classifier: %v", err)
	}
}

// TestSkipResolutionSettlesTheSoleRouteAndActivatesDownstream proves an
// operator skip is ordinary routing afterwards: the authored route arrives and
// the next node activates exactly as a successful program would have left it —
// including a downstream decision, which takes its own obligation.
func TestSkipResolutionSettlesTheSoleRouteAndActivatesDownstream(t *testing.T) {
	tmpl := sequentialTemplate("task")
	task := tmpl.Nodes["task"]
	task.Retry = &model.RetryPolicy{MaxAttempts: 1}
	task.Next = model.Next{model.DefaultOutcome: "choose"}
	tmpl.Nodes["task"] = task
	tmpl.Nodes["choose"] = humanDecision("Proceed?", model.Next{"go": "end", "stop": "end"})
	definition := mustPrepare(t, tmpl, nil)

	checkpoint, err := Initialize("run-skip", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = exhaust(t, checkpoint, definition, "task", 1)

	skipped, err := Apply(checkpoint, definition, resolve("task", 1, ResolveSkip))
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped.Blocked) != 0 {
		t.Fatalf("skip left the obligation behind: %#v", skipped.Blocked)
	}
	if skipped.Nodes["task"] != NodeDone {
		t.Fatalf("skipped task status = %q, want done", skipped.Nodes["task"])
	}
	if skipped.Edges["task"][model.DefaultOutcome] != EdgeArrived {
		t.Fatalf("skip did not settle the sole authored route: %#v", skipped.Edges["task"])
	}
	// Downstream activated normally, and a downstream decision parked on a human
	// with its own obligation rather than being silently skipped over.
	if skipped.Nodes["choose"] != NodeReady {
		t.Fatalf("downstream decision status = %q, want ready", skipped.Nodes["choose"])
	}
	if len(skipped.AwaitingDecisions) != 1 || skipped.AwaitingDecisions[0].NodeID != "choose" {
		t.Fatalf("downstream decision obligation = %#v", skipped.AwaitingDecisions)
	}
	// The attempt counter still records the attempts that really ran.
	if got := skipped.Attempts["task"]; got != 1 {
		t.Fatalf("skip changed the attempt counter to %d", got)
	}
	if err := ClassifyCheckpoint(skipped, definition); err != nil {
		t.Fatalf("strict classifier: %v", err)
	}
	decided, err := Apply(skipped, definition,
		Transition{Kind: TransitionDecisionRecorded, Decision: &DecisionRecord{NodeID: "choose", Verdict: "go"}})
	if err != nil {
		t.Fatal(err)
	}
	decided, _ = advanceAndPlan(t, decided, definition)
	if decided.Status != RunCompleted {
		t.Fatalf("a skipped branch could not complete its run: %q", decided.Status)
	}
}

// TestCancelResolutionDrainsInFlightSiblingsToCanceled is the honest-drain
// property: cancelling dooms the run at once, but a sibling's already-dispatched
// command survives, its real observation is still accepted, and only then does
// the run finish canceled.
func TestCancelResolutionDrainsInFlightSiblingsToCanceled(t *testing.T) {
	tmpl := fanOutTemplate("left", "right")
	left := tmpl.Nodes["left"]
	left.Retry = &model.RetryPolicy{MaxAttempts: 1}
	tmpl.Nodes["left"] = left
	definition := mustPrepare(t, tmpl, nil)

	checkpoint, err := Initialize("run-cancel", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, leftCommand := advanceAndPlan(t, checkpoint, definition)
	checkpoint, rightCommand := advanceAndPlan(t, checkpoint, definition)
	if leftCommand.NodeID != "left" || rightCommand.NodeID != "right" {
		t.Fatalf("planned %q then %q", leftCommand.NodeID, rightCommand.NodeID)
	}
	checkpoint, err = Apply(checkpoint, definition, observedWithError(leftCommand, "left failed"))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["left"] != NodeBlocked || checkpoint.Nodes["right"] != NodeRunning {
		t.Fatalf("state while left is parked and right runs = %#v", checkpoint.Nodes)
	}

	canceled, err := Apply(checkpoint, definition, resolve("left", 1, ResolveCancel))
	if err != nil {
		t.Fatal(err)
	}
	if canceled.Nodes["left"] != NodeCanceled {
		t.Fatalf("canceled node status = %q", canceled.Nodes["left"])
	}
	if len(canceled.Blocked) != 0 {
		t.Fatalf("cancel left a resolvable obligation behind: %#v", canceled.Blocked)
	}
	if !Draining(canceled) {
		t.Fatal("a canceled run did not report as draining")
	}
	// The sibling's command is untouched: its program may still be running.
	if len(canceled.Commands) != 1 || canceled.Commands[0].NodeID != "right" {
		t.Fatalf("cancel disturbed the sibling outbox: %#v", canceled.Commands)
	}
	if canceled.Status != RunRunning {
		t.Fatalf("run status = %q while a command still drains, want running", canceled.Status)
	}
	if Runnable(canceled, definition) {
		t.Fatal("a doomed run reported pushable work")
	}
	// The sibling's REAL observation is still accepted, and it is the transition
	// that finalizes the run.
	drained, err := Apply(canceled, definition, observed(rightCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if drained.Status != RunCanceled {
		t.Fatalf("drained run status = %q, want canceled", drained.Status)
	}
	if len(drained.Commands) != 0 {
		t.Fatalf("drained run kept a command: %#v", drained.Commands)
	}
}

// TestBlockedBranchDoesNotStopItsSiblings is why blocked is not doom: an
// unaffected parallel branch keeps being planned and observed normally.
func TestBlockedBranchDoesNotStopItsSiblings(t *testing.T) {
	tmpl := fanOutTemplate("left", "right")
	left := tmpl.Nodes["left"]
	left.Retry = &model.RetryPolicy{MaxAttempts: 1}
	tmpl.Nodes["left"] = left
	definition := mustPrepare(t, tmpl, nil)

	checkpoint, err := Initialize("run-sibling", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, leftCommand := advanceAndPlan(t, checkpoint, definition)
	checkpoint, err = Apply(checkpoint, definition, observedWithError(leftCommand, "left failed"))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["left"] != NodeBlocked {
		t.Fatalf("left status = %q, want blocked", checkpoint.Nodes["left"])
	}
	// The sibling is still plannable while the other branch sits parked.
	if !Runnable(checkpoint, definition) {
		t.Fatal("a blocked branch stopped its sibling from being planned")
	}
	checkpoint, rightCommand := advanceAndPlan(t, checkpoint, definition)
	if rightCommand == nil || rightCommand.NodeID != "right" {
		t.Fatalf("planned %#v, want the unaffected sibling", rightCommand)
	}
	checkpoint, err = Apply(checkpoint, definition, observed(rightCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Status != RunRunning || checkpoint.Nodes["right"] != NodeDone {
		t.Fatalf("state after the sibling succeeded = %#v", checkpoint.Nodes)
	}
	// The join is join: all and still owes the parked branch, so nothing
	// downstream activated.
	if checkpoint.Nodes["join"] != NodePending {
		t.Fatalf("join status = %q, want pending while a branch is parked", checkpoint.Nodes["join"])
	}
	// Resolving the parked branch with skip lets the whole run finish.
	checkpoint, err = Apply(checkpoint, definition, resolve("left", 1, ResolveSkip))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, joinCommand := advanceAndPlan(t, checkpoint, definition)
	if joinCommand == nil || joinCommand.NodeID != "join" {
		t.Fatalf("planned %#v, want the join task", joinCommand)
	}
	checkpoint, err = Apply(checkpoint, definition, observed(joinCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, _ = advanceAndPlan(t, checkpoint, definition)
	if checkpoint.Status != RunCompleted {
		t.Fatalf("run status = %q, want completed", checkpoint.Status)
	}
}

// TestBlockedJoinAnyLoserHoldsTheRunOpen is the end/join integration: a join:
// any winner reaches the end while the losing branch is parked, and the run
// must not terminate around a resolution that is still on offer.
func TestBlockedJoinAnyLoserHoldsTheRunOpen(t *testing.T) {
	tmpl := joinAnyTemplate("fast", "slow")
	slow := tmpl.Nodes["slow"]
	slow.Retry = &model.RetryPolicy{MaxAttempts: 1}
	tmpl.Nodes["slow"] = slow
	definition := mustPrepare(t, tmpl, nil)

	checkpoint, err := Initialize("run-join-any-blocked", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, first := advanceAndPlan(t, checkpoint, definition)
	checkpoint, second := advanceAndPlan(t, checkpoint, definition)
	fast, slowCommand := first, second
	if fast.NodeID != "fast" {
		fast, slowCommand = second, first
	}
	// The winner settles the reducer and its whole downstream route.
	checkpoint, err = Apply(checkpoint, definition, observed(fast, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, joinCommand := advanceAndPlan(t, checkpoint, definition)
	if joinCommand == nil || joinCommand.NodeID != "join" {
		t.Fatalf("planned %#v, want the won join reducer", joinCommand)
	}
	checkpoint, err = Apply(checkpoint, definition, observed(joinCommand, ProgramSucceeded, 0))
	if err != nil {
		t.Fatal(err)
	}
	// Now the loser exhausts and parks.
	checkpoint, err = Apply(checkpoint, definition, observedWithError(slowCommand, "slow failed"))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["slow"] != NodeBlocked {
		t.Fatalf("slow status = %q, want blocked", checkpoint.Nodes["slow"])
	}
	// The end node is ready, but a parked branch is live work, so it is not the
	// engine's to advance and the run stays open.
	advancedCheckpoint, err := AdvanceUntilQuiescent(checkpoint, definition)
	if err != nil {
		t.Fatal(err)
	}
	if advancedCheckpoint.Status != RunRunning {
		t.Fatalf("the run terminated around a parked branch: %q", advancedCheckpoint.Status)
	}
	if advancedCheckpoint.Nodes["end"] == NodeDone {
		t.Fatal("the end node completed while a blocked branch was still resolvable")
	}
	// Resolving the loser releases the end.
	resolved, err := Apply(advancedCheckpoint, definition, resolve("slow", 1, ResolveSkip))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = AdvanceUntilQuiescent(resolved, definition)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != RunCompleted {
		t.Fatalf("run status = %q after the loser was resolved, want completed", resolved.Status)
	}
}

// TestLaterDoomDropsBlockedObligations covers the honesty rule at the other
// end: once some other branch dooms the run, an already-parked branch must stop
// offering a resolution nothing could act on.
func TestLaterDoomDropsBlockedObligations(t *testing.T) {
	tmpl := fanOutTemplate("left", "right")
	// Only left is retryable. Right has no policy at all, so its failure is
	// fail-fast and dooms the run while left is already parked.
	left := tmpl.Nodes["left"]
	left.Retry = &model.RetryPolicy{MaxAttempts: 1}
	tmpl.Nodes["left"] = left
	definition := mustPrepare(t, tmpl, nil)

	checkpoint, err := Initialize("run-later-doom", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, leftCommand := advanceAndPlan(t, checkpoint, definition)
	checkpoint, rightCommand := advanceAndPlan(t, checkpoint, definition)
	checkpoint, err = Apply(checkpoint, definition, observedWithError(leftCommand, "left failed"))
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Nodes["left"] != NodeBlocked || len(checkpoint.Blocked) != 1 {
		t.Fatalf("state after left parked = %#v", checkpoint)
	}
	doomedCheckpoint, err := Apply(checkpoint, definition, observedWithError(rightCommand, "right failed"))
	if err != nil {
		t.Fatal(err)
	}
	if len(doomedCheckpoint.Blocked) != 0 {
		t.Fatalf("a doomed run kept a resolvable obligation: %#v", doomedCheckpoint.Blocked)
	}
	// The parked branch loses its parked STATUS too: it is simply the exhausted
	// failure it always was.
	if doomedCheckpoint.Nodes["left"] != NodeFailed {
		t.Fatalf("left status = %q after the run was doomed, want failed", doomedCheckpoint.Nodes["left"])
	}
	if doomedCheckpoint.Status != RunFailed {
		t.Fatalf("run status = %q, want failed once the last command drained", doomedCheckpoint.Status)
	}
	if _, err := Apply(doomedCheckpoint, definition, resolve("left", 1, ResolveRetry)); !errors.Is(err, ErrStaleResolution) {
		t.Fatalf("a doomed run still accepted a resolution: %v", err)
	}
}

// TestBlockedResolutionsFailClosed is the exact-identity table: everything that
// is not the run's current parked branch and attempt is refused, and the
// refused checkpoint is left byte-identical.
func TestBlockedResolutionsFailClosed(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 1}), nil)
	checkpoint, err := Initialize("run-fail-closed", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = exhaust(t, checkpoint, definition, "task", 1)
	before := cloneCheckpoint(checkpoint)

	for _, test := range []struct {
		name       string
		transition Transition
		want       error
	}{
		{name: "wrong attempt", transition: resolve("task", 2, ResolveRetry), want: ErrStaleResolution},
		{name: "attempt zero", transition: resolve("task", 0, ResolveRetry), want: ErrStaleResolution},
		{name: "unknown node", transition: resolve("nope", 1, ResolveRetry), want: ErrStaleResolution},
		{name: "node that is not blocked", transition: resolve("end", 1, ResolveSkip), want: ErrStaleResolution},
		{name: "unknown action", transition: resolve("task", 1, "reroute"), want: ErrInvalidTransition},
		{name: "resolution carrying a second payload", transition: Transition{
			Kind:       TransitionBlockedResolved,
			Resolution: &BlockedResolution{NodeID: "task", Attempt: 1, Action: ResolveRetry},
			Decision:   &DecisionRecord{NodeID: "task", Verdict: "x"},
		}, want: ErrInvalidTransition},
		{name: "missing resolution payload",
			transition: Transition{Kind: TransitionBlockedResolved}, want: ErrInvalidTransition},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Apply(checkpoint, definition, test.transition); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if !reflect.DeepEqual(checkpoint, before) {
				t.Fatalf("a refused resolution mutated the checkpoint: %#v", checkpoint)
			}
		})
	}

	// A resolution that DID commit cannot be replayed: the obligation is gone.
	resolved, err := Apply(checkpoint, definition, resolve("task", 1, ResolveSkip))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(resolved, definition, resolve("task", 1, ResolveSkip)); !errors.Is(err, ErrStaleResolution) {
		t.Fatalf("a duplicate resolution was accepted: %v", err)
	}
}

// TestOperatorRetryHasNoInventedLifetimeLimit pins the absence of a cap. Each
// raise is one audited operator action against a branch that really parked, so
// a lifetime limit would only ever strand a legitimately blocked branch. The
// one arithmetic bound is integer overflow, which is not policy: a checkpoint
// carrying an absurd attempt still LOADS — the boundary makes no claim about
// which values the reducer could have produced — and it is the raise itself
// that refuses, leaving the branch parked and the state untouched.
func TestOperatorRetryHasNoInventedLifetimeLimit(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 2}), nil)
	checkpoint, err := Initialize("run-no-cap", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = exhaust(t, checkpoint, definition, "task", 2)

	// Many consecutive operator retries, each opening one authored window. The
	// ceiling simply keeps rising; nothing refuses on grounds of "too many".
	for round := range 50 {
		resolved, err := Apply(checkpoint, definition, resolve("task", checkpoint.Attempts["task"], ResolveRetry))
		if err != nil {
			t.Fatalf("operator retry %d was refused: %v", round+1, err)
		}
		checkpoint = exhaust(t, resolved, definition, "task", 2)
		if got, want := checkpoint.AttemptCeilings["task"], (round+2)*2; got != want {
			t.Fatalf("ceiling after retry %d = %d, want %d", round+1, got, want)
		}
	}
	if got := checkpoint.Attempts["task"]; got != 102 {
		t.Fatalf("attempt counter = %d, want 102 after 51 authored windows", got)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpoint(encoded, definition); err != nil {
		t.Fatalf("a much-retried run did not cold-load: %v", err)
	}

	// Overflow is the one arithmetic refusal. The absurd counter loads fine —
	// the boundary bounds references and canonical shape, not plausibility.
	overflowing := cloneCheckpoint(checkpoint)
	overflowing.Attempts["task"] = math.MaxInt
	overflowing.AttemptCeilings["task"] = math.MaxInt
	if err := ValidateCheckpoint(overflowing, definition); err != nil {
		t.Fatalf("the load boundary invented a plausibility limit: %v", err)
	}
	before := cloneCheckpoint(overflowing)
	_, err = Apply(overflowing, definition, resolve("task", math.MaxInt, ResolveRetry))
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("error = %v, want a refused overflowing raise", err)
	}
	if !reflect.DeepEqual(overflowing, before) {
		t.Fatal("a refused raise mutated the checkpoint")
	}
}

// TestBlockedStateSurvivesColdLoad covers restart: the parked branch, its exact
// attempt, its reason, and any raised ceiling round-trip through the durable
// encoding, and a resolution formed after the reload commits normally.
func TestBlockedStateSurvivesColdLoad(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 1}), nil)
	checkpoint, err := Initialize("run-cold-blocked", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = exhaust(t, checkpoint, definition, "task", 1)
	// Raise the ceiling once so the sparse override is part of the round trip,
	// then park again at the new exact attempt.
	checkpoint, err = Apply(checkpoint, definition, resolve("task", 1, ResolveRetry))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint = exhaust(t, checkpoint, definition, "task", 1)

	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := DecodeCheckpoint(encoded, definition)
	if err != nil {
		t.Fatalf("a blocked checkpoint did not cold-load: %v", err)
	}
	if !reflect.DeepEqual(loaded, checkpoint) {
		t.Fatalf("cold load changed the checkpoint:\n got %#v\nwant %#v", loaded, checkpoint)
	}
	if loaded.Nodes["task"] != NodeBlocked || loaded.Attempts["task"] != 2 ||
		loaded.AttemptCeilings["task"] != 2 {
		t.Fatalf("cold-loaded blocked state = %#v", loaded)
	}
	// Nothing about loading re-plans or re-mints anything.
	if Runnable(loaded, definition) {
		t.Fatal("a cold-loaded blocked run reported pushable work")
	}
	resolved, err := Apply(loaded, definition, resolve("task", 2, ResolveSkip))
	if err != nil {
		t.Fatalf("a resolution after cold load was refused: %v", err)
	}
	if resolved.Nodes["task"] != NodeDone {
		t.Fatalf("post-restart skip left task = %q", resolved.Nodes["task"])
	}
}

// TestBlockedReasonIsBoundedInDurableState keeps the obligation a pointer to a
// branch rather than a place to store program output. The untruncated text
// stays in the attempt's own program_observed evidence.
func TestBlockedReasonIsBoundedInDurableState(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 1}), nil)
	checkpoint, err := Initialize("run-bounded-reason", definition)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, command := advanceAndPlan(t, checkpoint, definition)
	// Multi-byte runes, so a naive byte cut would produce invalid UTF-8.
	checkpoint, err = Apply(checkpoint, definition,
		observedWithError(command, strings.Repeat("ü", MaxBlockedReasonBytes)))
	if err != nil {
		t.Fatal(err)
	}
	reason := checkpoint.Blocked[0].Reason
	if len(reason) > MaxBlockedReasonBytes {
		t.Fatalf("durable reason is %d bytes, want at most %d", len(reason), MaxBlockedReasonBytes)
	}
	if !utf8.ValidString(reason) {
		t.Fatal("the truncated reason is not valid UTF-8")
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeCheckpoint(encoded, definition); err != nil {
		t.Fatalf("a bounded reason did not survive the load boundary: %v", err)
	}
}

// TestBlockedCheckpointsFailClosedAtTheLoadBoundary covers the untrusted-bytes
// direction: a forged blocked outbox, a stranded blocked node with no
// obligation, and a ceiling on a node that could never have earned one are all
// refused before they reach the reducer.
func TestBlockedCheckpointsFailClosedAtTheLoadBoundary(t *testing.T) {
	definition := mustPrepare(t, retryTemplate(&model.RetryPolicy{MaxAttempts: 2}), nil)
	valid, err := Initialize("run-boundary", definition)
	if err != nil {
		t.Fatal(err)
	}
	valid = exhaust(t, valid, definition, "task", 2)

	for _, test := range []struct {
		name   string
		mutate func(Checkpoint) Checkpoint
	}{
		{name: "blocked node with no obligation", mutate: func(c Checkpoint) Checkpoint {
			c.Blocked = nil
			return c
		}},
		{name: "obligation with no blocked node", mutate: func(c Checkpoint) Checkpoint {
			c.Nodes["task"] = NodeFailed
			return c
		}},
		{name: "duplicate obligation", mutate: func(c Checkpoint) Checkpoint {
			c.Blocked = append(c.Blocked, BlockedObligation{NodeID: "task"})
			return c
		}},
		{name: "obligation naming a node that is not a task", mutate: func(c Checkpoint) Checkpoint {
			c.Nodes["task"] = NodeFailed
			c.Blocked = []BlockedObligation{{NodeID: "end"}}
			c.Nodes["end"] = NodeBlocked
			return c
		}},
		{name: "blocked obligations on a terminal run", mutate: func(c Checkpoint) Checkpoint {
			c.Status = RunFailed
			return c
		}},
		{name: "ceiling that does not rise above the authored budget", mutate: func(c Checkpoint) Checkpoint {
			c.AttemptCeilings = map[string]int{"task": 2}
			return c
		}},
		{name: "ceiling on an unknown node", mutate: func(c Checkpoint) Checkpoint {
			c.AttemptCeilings = map[string]int{"end": 5}
			return c
		}},
		{name: "attempt past its raised ceiling", mutate: func(c Checkpoint) Checkpoint {
			c.AttemptCeilings = map[string]int{"task": 3}
			c.Attempts = map[string]int{"task": 4}
			return c
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			broken := test.mutate(cloneCheckpoint(valid))
			if err := ValidateCheckpoint(broken, definition); !errors.Is(err, ErrInvalidCheckpoint) {
				t.Fatalf("error = %v, want an invalid checkpoint", err)
			}
		})
	}
}
