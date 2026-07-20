package pathv1

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	legacy "github.com/tofutools/tclaude/pkg/claude/process/state"
)

func TestPureExclusiveSimpleTaskPassFailConservesEveryRoute(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-task
start: work
nodes:
  work:
    type: task
    performer:
      kind: agent
      prompt: do the work
    next:
      pass: success
      fail: failure
  success:
    type: end
    result: completed
  failure:
    type: end
    result: failed
`)
	checkpoint := initializedExclusiveCheckpoint(t, source)

	for _, test := range []struct {
		outcome, selected string
	}{
		{outcome: "pass", selected: "success"},
		{outcome: "fail", selected: "failure"},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
			if err != nil {
				t.Fatal(err)
			}
			pathID := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
			observation := ExclusiveObservation{SourcePathID: pathID, Attempt: 1, Outcome: test.outcome}
			command, err := PlanExclusiveRoute(t.Context(), input, observation)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := ReduceExclusiveRoute(t.Context(), input, observation, command)
			if err != nil {
				t.Fatal(err)
			}
			if projection.ReplayDisposition() != ReplayApplied {
				t.Fatalf("replay disposition = %q", projection.ReplayDisposition())
			}
			routing := projection.Routing()
			parent := routing.Paths[pathID]
			if parent.State != PathRouted || len(parent.ProducedPathIDs) != 2 {
				t.Fatalf("parent = %#v", parent)
			}
			selected, impossible := 0, 0
			for _, childID := range parent.ProducedPathIDs {
				child := routing.Paths[childID]
				switch child.Kind {
				case PathEdge:
					selected++
					if child.State != PathArrived || child.Edge == nil || child.Edge.ToNodeID != test.selected {
						t.Fatalf("selected child = %#v", child)
					}
				case PathImpossibleEdge:
					impossible++
					if child.State != PathImpossible || child.ImpossibleCauseDigest == "" {
						t.Fatalf("impossible child = %#v", child)
					}
					set, ok := routing.CauseSets[child.ImpossibleCauseDigest]
					if !ok || len(set.CauseIDs) != 1 {
						t.Fatalf("impossible cause set = %#v", set)
					}
					cause := routing.CauseRecords[set.CauseIDs[0]]
					if cause.TerminalKind != TerminalImpossible || cause.SourceCommandID != command.ID || !strings.HasPrefix(cause.DispositionReason, "exclusive_unselected/") {
						t.Fatalf("impossible cause = %#v", cause)
					}
				default:
					t.Fatalf("unexpected child kind %q", child.Kind)
				}
			}
			if selected != 1 || impossible != 1 {
				t.Fatalf("selected=%d impossible=%d", selected, impossible)
			}

			// The same exact command against its complete post-state is an
			// idempotent replay through the canonical mutation reducer.
			post := projection.aggregate.View()
			replay, err := ReplayRoutePaths(MutationReplayView{Aggregate: post, Checkpoint: input.binding}, command)
			if err != nil || replay.Disposition != ReplayAlreadyApplied {
				t.Fatalf("idempotent replay = %q, %v", replay.Disposition, err)
			}
		})
	}
}

func TestPureExclusiveStartRoutesThroughCanonicalMutationReplay(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-start
start: start
nodes:
  start:
    type: start
    next: work
  work:
    type: task
    performer:
      kind: agent
      prompt: work
    next: done
  done:
    type: end
`)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	pathID := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	observation := ExclusiveObservation{SourcePathID: pathID, Attempt: 1, Outcome: "pass"}
	command, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := ReduceExclusiveRoute(t.Context(), input, observation, command)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Binding() != input.binding {
		t.Fatalf("projection binding = %#v, want original replay basis %#v", projection.Binding(), input.binding)
	}
	parent := projection.Routing().Paths[pathID]
	if len(parent.ProducedPathIDs) != 1 || projection.Routing().Paths[parent.ProducedPathIDs[0]].Kind != PathEdge {
		t.Fatalf("start route = %#v", parent)
	}
}

func TestPureExclusiveDecisionUsesExactVerdict(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-decision
start: choose
nodes:
  choose:
    type: decision
    performer: {kind: human, ask: choose}
    next: {ship: shipped, hold: held}
  shipped: {type: end}
  held: {type: end}
`)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	pathID := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	observation := ExclusiveObservation{SourcePathID: pathID, Attempt: 1, Outcome: "ship"}
	command, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := ReduceExclusiveRoute(t.Context(), input, observation, command)
	if err != nil {
		t.Fatal(err)
	}
	for _, childID := range projection.Routing().Paths[pathID].ProducedPathIDs {
		child := projection.Routing().Paths[childID]
		if child.Kind == PathEdge && child.Edge.ToNodeID != "shipped" {
			t.Fatalf("selected decision child = %#v", child)
		}
	}
	unknown := observation
	unknown.Outcome = "SHIP "
	if _, err := PlanExclusiveRoute(t.Context(), input, unknown); !errors.Is(err, ErrExclusiveUnsupported) {
		t.Fatalf("non-exact decision verdict error = %v", err)
	}
}

func TestPureExclusivePlansThreeWayFanOut(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-three-way
start: choose
nodes:
  choose:
    type: decision
    performer: {kind: human, ask: choose}
    next: {one: first, two: second, three: third}
  first: {type: end}
  second: {type: end}
  third: {type: end}
`)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{
		SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
		Attempt:      1, Outcome: "one",
	}
	sequence, err := PlanExclusiveRouteSequence(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	commands := sequence.Commands()
	if len(commands) != 7 { // route + two loser closures + two dead reservations + activation + end
		t.Fatalf("three-way command count = %d", len(commands))
	}
	projection, err := ReduceExclusiveRouteSequence(t.Context(), input, observation, commands)
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for _, reservation := range projection.Routing().Reservations {
		if reservation.State == ReservationClosedNoActivation {
			closed++
		}
	}
	if closed != 2 {
		t.Fatalf("three-way closed loser reservations = %d", closed)
	}
}

func TestPureExclusiveWaitRoutesOnlySatisfiedObservation(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-wait
start: wait
nodes:
  wait:
    type: wait
    wait: {signal: deploy/prod}
    next: done
  done: {type: end}
`)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID, Attempt: 1, Outcome: "satisfied"}
	command, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := ReduceExclusiveRoute(t.Context(), input, observation, command)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, effect := range projection.aggregate.SideEffects {
		if effect.Kind == SideEffectWait {
			found = effect.State == "satisfied" && effect.WaitKind == "signal"
		}
	}
	if !found {
		t.Fatalf("satisfied wait effect missing: %#v", projection.aggregate.SideEffects)
	}
}

func TestPureExclusiveRetryPendingAndFinalFailure(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-retry
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    retry: {maxAttempts: 2}
    next: {pass: done, fail: failed}
  done: {type: end}
  failed: {type: end, result: failed}
`)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	pathID := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	pending := ExclusiveObservation{SourcePathID: pathID, Attempt: 1, Outcome: "fail"}
	if got, err := ClassifyExclusiveObservation(t.Context(), input, pending); err != nil || got != ExclusiveRetryPending {
		t.Fatalf("pending retry = %q, %v", got, err)
	}
	if _, err := PlanExclusiveRoute(t.Context(), input, pending); !errors.Is(err, ErrExclusiveNotRoutable) {
		t.Fatalf("pending retry route error = %v", err)
	}
	final := pending
	final.Attempt = 2
	if got, err := ClassifyExclusiveObservation(t.Context(), input, final); err != nil || got != ExclusiveRouteReady {
		t.Fatalf("final failure = %q, %v", got, err)
	}
	if _, err := PlanExclusiveRoute(t.Context(), input, final); err != nil {
		t.Fatal(err)
	}
}

func TestPureExclusiveNoFailTargetUsesLegacyNonPassRetryBoundary(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-nonpass-retry
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    retry: {maxAttempts: 2}
    next: {pass: done}
  done: {type: end}
`)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	pathID := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	pending := ExclusiveObservation{SourcePathID: pathID, Attempt: 1, Outcome: "needs-work"}
	if got, err := ClassifyExclusiveObservation(t.Context(), input, pending); err != nil || got != ExclusiveRetryPending {
		t.Fatalf("non-pass pending retry = %q, %v", got, err)
	}
	final := pending
	final.Attempt = 2
	if got, err := ClassifyExclusiveObservation(t.Context(), input, final); err != nil || got != ExclusiveRouteReady {
		t.Fatalf("non-pass final disposition = %q, %v", got, err)
	}
	if _, err := PlanExclusiveRoute(t.Context(), input, final); !errors.Is(err, ErrExclusiveNotRoutable) {
		t.Fatalf("terminal failure route error = %v", err)
	}
}

func TestPureExclusiveExplicitFailEdgeKeepsReleasedNonPassRouting(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: explicit-fail-edge-nonpass
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    retry: {maxAttempts: 2}
    next: {pass: done, fail: failed}
  done: {type: end}
  failed: {type: end, result: failed}
`)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{
		SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
		Attempt:      1, Outcome: "needs-work",
	}
	if got, err := ClassifyExclusiveObservation(t.Context(), input, observation); err != nil || got != ExclusiveRouteReady {
		t.Fatalf("explicit-edge disposition changed = %q, %v", got, err)
	}
	command, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	var payload mutationPayload[RoutePathsPlan]
	if err := decodeExactPayload(command.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	selectedDone := false
	for _, producedPathID := range payload.Plan.ProducedPathIDs {
		mutation, ok := findMutation(payload.Plan.Batch, MutationPath, producedPathID)
		if !ok {
			continue
		}
		var path PathRecord
		if err := decodeExactPayload(mutation.After, &path); err != nil {
			t.Fatal(err)
		}
		if path.Edge != nil && path.Edge.ToNodeID == "done" && path.State != PathImpossible {
			selectedDone = true
		}
	}
	if !selectedDone {
		t.Fatal("released explicit-edge pass fallback changed")
	}
}

func TestPureExclusiveDecisionKeepsExactNeedsWorkEdgeAuthority(t *testing.T) {
	withEdge := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exact-needs-work-decision
start: choose
nodes:
  choose:
    type: decision
    performer: {kind: human, ask: Choose}
    next: {needs-work: revise, approve: done}
  revise: {type: end}
  done: {type: end}
`)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, withEdge), withEdge)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{
		SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
		Attempt:      1, Outcome: "needs-work",
	}
	if _, err := PlanExclusiveRoute(t.Context(), input, observation); err != nil {
		t.Fatalf("exact decision edge rejected: %v", err)
	}

	withoutEdge := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: missing-needs-work-decision
start: choose
nodes:
  choose:
    type: decision
    performer: {kind: human, ask: Choose}
    next: {approve: done}
  done: {type: end}
`)
	input, err = VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, withoutEdge), withoutEdge)
	if err != nil {
		t.Fatal(err)
	}
	observation.SourcePathID = input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	if _, err := PlanExclusiveRoute(t.Context(), input, observation); !errors.Is(err, ErrExclusiveUnsupported) {
		t.Fatalf("missing exact decision edge error = %v", err)
	}
}

func TestPureExclusiveAuditedBlockResolutionIsGenerationBound(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-block-resolution
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    next: {pass: done, fail: failed}
  done: {type: end}
  failed: {type: end, result: failed}
`)
	for _, test := range []struct {
		decision string
		want     ExclusiveDisposition
	}{
		{decision: "retry", want: ExclusiveResolvedRetry},
		{decision: "skip", want: ExclusiveResolvedSkip},
		{decision: "cancel", want: ExclusiveResolvedCancel},
	} {
		t.Run(test.decision, func(t *testing.T) {
			checkpoint, digest := resolvedExclusiveCheckpoint(t, initializedExclusiveCheckpoint(t, source), test.decision, 1)
			input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
			if err != nil {
				t.Fatal(err)
			}
			observation := ExclusiveObservation{
				SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
				Attempt:      1, Outcome: "fail", ResolutionDigest: digest,
			}
			if got, err := ClassifyExclusiveObservation(t.Context(), input, observation); err != nil || got != test.want {
				t.Fatalf("resolution disposition = %q, %v", got, err)
			}
			if _, err := PlanExclusiveRoute(t.Context(), input, observation); !errors.Is(err, ErrExclusiveNotRoutable) {
				t.Fatalf("resolved outcome route error = %v", err)
			}
			stale := observation
			stale.Attempt++
			if _, err := ClassifyExclusiveObservation(t.Context(), input, stale); !errors.Is(err, ErrMutationInvalid) {
				t.Fatalf("stale resolution error = %v", err)
			}
		})
	}
}

func TestPureExclusiveCompletedEndUsesCanonicalCompletionBasis(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-completion
start: start
nodes:
  start: {type: start, next: done}
  done: {type: end, result: completed}
`)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID, Attempt: 1, Outcome: "pass"}
	route, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	activation, err := PlanExclusiveActivation(t.Context(), input, observation, route, CommandRecord{}, CommandRecord{})
	if err != nil {
		t.Fatal(err)
	}
	end, err := PlanExclusiveEnd(t.Context(), input, observation, route, CommandRecord{}, CommandRecord{}, activation)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := ReduceExclusiveEnd(t.Context(), input, observation, route, CommandRecord{}, CommandRecord{}, activation, end)
	if err != nil {
		t.Fatal(err)
	}
	completionResult, err := AssessAggregateCompletion(ended.aggregate.View())
	if err != nil || completionResult.Result != "completed" {
		t.Fatalf("aggregate completion = %#v, %v", completionResult, err)
	}
	completionInput := ExclusiveCompletionInput{
		CheckpointJSON: []byte(`{"status":"running","lastLogSeq":7,"logChecksum":"sum-7","outstandingCommands":{}}`),
		RunStatus:      "running", LastLogSeq: 7, LogChecksum: "sum-7",
	}
	completion, err := PlanExclusiveCompletion(t.Context(), input, observation, route, CommandRecord{}, CommandRecord{}, activation, end, completionInput)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := ReduceExclusiveCompletion(t.Context(), input, observation, route, CommandRecord{}, CommandRecord{}, activation, end, completion, completionInput)
	if err != nil || recovery.Phase != CompletionReadyToClaim || !exactExclusiveCommand(recovery.Command, completion) || recovery.Result != "completed" {
		t.Fatalf("completion recovery = %#v, %v", recovery, err)
	}
	drifted := completionInput
	drifted.CheckpointJSON = bytes.Replace(drifted.CheckpointJSON, []byte("sum-7"), []byte("sum-x"), 1)
	if _, err := PlanExclusiveCompletion(t.Context(), input, observation, route, CommandRecord{}, CommandRecord{}, activation, end, drifted); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("completion checkpoint drift error = %v", err)
	}
}

func TestPureExclusiveDistinctPassFailTargetsCloseDeadReservation(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-distinct-completion
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    next: {pass: success, fail: failure}
  success: {type: end, result: completed}
  failure: {type: end, result: failed}
`)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID, Attempt: 1, Outcome: "pass"}
	route, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	closure, err := PlanExclusiveDeadPath(t.Context(), input, observation, route)
	if err != nil {
		t.Fatal(err)
	}
	dead, err := PlanExclusiveDeadReservation(t.Context(), input, observation, route, closure)
	if err != nil || dead.ID == "" {
		t.Fatalf("dead reservation command = %#v, %v", dead, err)
	}
	deadProjection, err := ReduceExclusiveDeadReservation(t.Context(), input, observation, route, closure, dead)
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for _, reservation := range deadProjection.Routing().Reservations {
		if reservation.State == ReservationClosedNoActivation {
			closed++
			if reservation.ClosedReason != string(ScopeCloseAllImpossible) || reservation.CloseReceipt == nil || reservation.CommandID != dead.ID {
				t.Fatalf("dead reservation = %#v", reservation)
			}
		}
	}
	if closed != 1 {
		t.Fatalf("closed dead reservations = %d", closed)
	}
	activation, err := PlanExclusiveActivation(t.Context(), input, observation, route, closure, dead)
	if err != nil {
		t.Fatal(err)
	}
	end, err := PlanExclusiveEnd(t.Context(), input, observation, route, closure, dead, activation)
	if err != nil {
		t.Fatal(err)
	}
	ended, err := ReduceExclusiveEnd(t.Context(), input, observation, route, closure, dead, activation, end)
	if err != nil {
		t.Fatal(err)
	}
	completion, err := AssessAggregateCompletion(ended.aggregate.View())
	if err != nil || completion.Result != "completed" {
		t.Fatalf("distinct-target completion = %#v, %v", completion, err)
	}
}

func TestPureExclusiveLocalMergeEliminatesImpossibleCandidate(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-local-merge
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    next: {pass: merge, fail: merge}
  merge: {type: end}
`)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID, Attempt: 1, Outcome: "pass"}
	route, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	closure, err := PlanExclusiveDeadPath(t.Context(), input, observation, route)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := ReduceExclusiveDeadPath(t.Context(), input, observation, route, closure)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Binding() != input.binding {
		t.Fatalf("closure projection binding = %#v, want original replay basis %#v", projection.Binding(), input.binding)
	}
	routing := projection.Routing()
	if len(routing.CandidateClosures) != 1 || len(routing.Propagation) != 1 {
		t.Fatalf("closures=%d propagation=%d", len(routing.CandidateClosures), len(routing.Propagation))
	}
	for _, candidateClosure := range routing.CandidateClosures {
		if candidateClosure.TerminalKind != TerminalImpossible || candidateClosure.CommandID != closure.ID {
			t.Fatalf("candidate closure = %#v", candidateClosure)
		}
		set := routing.CauseSets[candidateClosure.CauseDigest]
		if len(set.CauseIDs) != 1 || routing.CauseRecords[set.CauseIDs[0]].SourceCommandID != route.ID {
			t.Fatalf("closure provenance = %#v / %#v", set, routing.CauseRecords)
		}
	}
	for _, intent := range routing.Propagation {
		if intent.State != PropagationComplete || intent.Cursor != uint32(len(intent.Frontier)) || intent.CommandID != closure.ID {
			t.Fatalf("propagation intent = %#v", intent)
		}
	}
	activation, err := PlanExclusiveActivation(t.Context(), input, observation, route, closure, CommandRecord{})
	if err != nil {
		t.Fatal(err)
	}
	activated, err := ReduceExclusiveActivation(t.Context(), input, observation, route, closure, CommandRecord{}, activation)
	if err != nil {
		t.Fatal(err)
	}
	activatedRouting := activated.Routing()
	activatedCount, consumedCount := 0, 0
	for _, reservation := range activatedRouting.Reservations {
		if reservation.NodeID == "merge" && reservation.State == ReservationActivated {
			activatedCount++
		}
	}
	for _, path := range activatedRouting.Paths {
		if path.State == PathConsumed {
			consumedCount++
		}
	}
	if activatedCount != 1 || consumedCount != 1 {
		t.Fatalf("activated=%d consumed=%d", activatedCount, consumedCount)
	}

	post := projection.aggregate.View()
	replay, err := ReplayPropagateClosure(MutationReplayView{Aggregate: post, Checkpoint: input.binding}, closure)
	if err != nil || replay.Disposition != ReplayAlreadyApplied {
		t.Fatalf("idempotent closure replay = %q, %v", replay.Disposition, err)
	}
	drifted := cloneCommandRecord(closure)
	drifted.Payload = append(drifted.Payload, ' ')
	if _, err := ReduceExclusiveDeadPath(t.Context(), input, observation, route, drifted); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("closure drift error = %v", err)
	}
}

func TestVerifiedExclusiveInputIsDeeplyDetachedAndFailClosed(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-detached
start: task
nodes:
  task:
    type: task
    performer: {kind: agent, prompt: work}
    next: done
  done: {type: end}
`)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	originalCheckpoint := bytes.Clone(checkpoint)
	originalSource := bytes.Clone(source)
	input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	for index := range checkpoint {
		checkpoint[index] = 'x'
	}
	for index := range source {
		source[index] = 'y'
	}
	pathID := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	observation := ExclusiveObservation{SourcePathID: pathID, Attempt: 1, Outcome: "pass"}
	first, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	first.Payload[0] ^= 0xff
	second, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := VerifyExclusiveInput(t.Context(), originalCheckpoint, originalSource)
	if err != nil {
		t.Fatal(err)
	}
	want, err := PlanExclusiveRoute(t.Context(), baseline, observation)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, want) {
		t.Fatal("caller mutation changed sealed verified input")
	}

	wrongSource := bytes.Replace(originalSource, []byte("exclusive-detached"), []byte("exclusive-mismatch"), 1)
	if _, err := VerifyExclusiveInput(t.Context(), originalCheckpoint, wrongSource); !errors.Is(err, ErrExclusiveInputInvalid) {
		t.Fatalf("source mismatch error = %v", err)
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := VerifyExclusiveInput(canceled, originalCheckpoint, originalSource); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled verification error = %v", err)
	}
}

func TestPureExclusiveRejectsCommandAndMutationDrift(t *testing.T) {
	source := []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: exclusive-drift
start: task
nodes:
  task:
    type: task
    performer: {kind: agent, prompt: work}
    next: {pass: done, fail: failed}
  done: {type: end}
  failed: {type: end, result: failed}
`)
	checkpoint := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), checkpoint, source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID, Attempt: 1, Outcome: "pass"}
	command, err := PlanExclusiveRoute(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	drifted := cloneCommandRecord(command)
	drifted.Identity.ResultCode = "exclusive/fail"
	if _, err := ReduceExclusiveRoute(t.Context(), input, observation, drifted); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("identity drift error = %v", err)
	}
	drifted = cloneCommandRecord(command)
	drifted.Payload = append(drifted.Payload, ' ')
	if _, err := ReduceExclusiveRoute(t.Context(), input, observation, drifted); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("payload drift error = %v", err)
	}
}

func TestReleasedPathV1HostPreservesLegacyStateIsolation(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", ".."))
	// The daemon host must opt into the schema-7 executor; generic library
	// hosts remain legacy-safe by default.
	required := map[string][]string{
		"engine/host.go":              {"func (h *Host) EnableExclusiveV7()", "processexec.NewExclusiveV7"},
		"../agentd/process_engine.go": {"host.EnableExclusiveV7()", "LoadPathV1RunView"},
	}
	for name, fragments := range required {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range fragments {
			if !bytes.Contains(data, []byte(fragment)) {
				t.Errorf("release wiring %s is missing %q", name, fragment)
			}
		}
	}

	// The active v6 state still cannot carry a routing aggregate or accept a
	// schema-7 checkpoint through its ordinary decoder.
	if _, ok := reflect.TypeOf(legacy.State{}).FieldByName("Routing"); ok {
		t.Fatal("active schema-6 State gained routing")
	}
}

func initializedExclusiveCheckpoint(t *testing.T, source []byte) []byte {
	t.Helper()
	parsed, err := model.Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Diagnostics.HasErrors() {
		t.Fatalf("fixture template diagnostics: %#v", parsed.Diagnostics)
	}
	inits := make([]legacy.NodeInit, 0, len(parsed.Template.Nodes))
	for id, node := range parsed.Template.Nodes {
		status := legacy.NodeStatusPending
		if id == parsed.Template.Start {
			status = legacy.NodeStatusReady
		}
		inits = append(inits, legacy.NodeInit{ID: id, Type: node.Type, Status: status})
	}
	state := legacy.New("run-exclusive", parsed.Ref, parsed.Ref, inits)
	state.Status = legacy.RunStatusRunning
	legacyBytes, err := legacy.Encode(&state)
	if err != nil {
		t.Fatal(err)
	}
	needed, err := AssessUpgradeNeeded(t.Context(), legacyBytes, &state, parsed.Ref, parsed.SourceHash, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := BuildInitialization(t.Context(), needed, parsed.Template)
	if err != nil {
		t.Fatal(err)
	}
	data, err := EncodeCheckpointV7(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func resolvedExclusiveCheckpoint(t *testing.T, data []byte, decision string, attempt uint64) ([]byte, string) {
	t.Helper()
	checkpoint, err := DecodeCheckpointV7(data)
	if err != nil {
		t.Fatal(err)
	}
	event := &checkpoint.Initialize
	aggregate := &event.Aggregate
	genesis := aggregate.Authority.Genesis
	reservation := aggregate.Routing.Reservations[genesis.ReservationID]
	timestamp := "2026-07-15T12:00:00Z"
	resolution := BlockResolution{
		NodeID: reservation.NodeID, BlockedAttempt: attempt, Decision: decision,
		Actor: "human:operator", Reason: "reviewed poison", EvidenceRef: "ticket:TCL-504", Timestamp: timestamp,
	}
	digest, err := ValidateBlockResolution(resolution)
	if err != nil {
		t.Fatal(err)
	}
	record := PathV1AdminRecord{
		RunID: aggregate.RunID, EventSeq: event.EventSeq + 1, AdminType: "block_resolution_recorded",
		Actor: resolution.Actor, ReasonCode: "resolved_" + decision, EvidenceRef: resolution.EvidenceRef,
		Timestamp: timestamp, ResolutionDigest: digest,
	}
	record.ID, err = AdminRecordIdentity(record)
	if err != nil {
		t.Fatal(err)
	}
	aggregate.AdminRecords[record.ID] = record
	aggregate.AdminResolutions[record.ID] = resolution
	blockID, err := BlockIdentity(aggregate.RunID, genesis.ActivationID, attempt)
	if err != nil {
		t.Fatal(err)
	}
	aggregate.SideEffects[blockID] = SideEffectIdentity{
		Kind: SideEffectBlock, ID: blockID, RunID: aggregate.RunID,
		ActivationID: genesis.ActivationID, BlockedAttempt: attempt, State: "resolved_" + decision,
	}

	aggregateDigest, err := initializationAggregateDigest(aggregate.View(), event.UpgradeNeeded.Checkpoint.Digest)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(InitializeRoutingPayload{
		UpgradeNeeded: event.UpgradeNeeded, TemplateHash: event.TemplateHash,
		Genesis: aggregate.Authority.Genesis, AggregateDigest: aggregateDigest,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := CommandIdentity{
		RunID: aggregate.RunID, Kind: CommandInitializeRouting, PayloadSchema: 1,
		InputDigest: event.UpgradeNeeded.Checkpoint.Digest, PlanDigest: aggregateDigest,
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		t.Fatal(err)
	}
	oldCommandID := event.Command.ID
	for id, value := range aggregate.Routing.Reservations {
		if value.CommandID == oldCommandID {
			value.CommandID = command.ID
		}
		if value.CloseReceipt != nil && value.CloseReceipt.CommandID == oldCommandID {
			receipt := *value.CloseReceipt
			receipt.CommandID = command.ID
			receipt.ID, err = ActivationReceiptIdentity(receipt.ActivationID, receipt.ReservationID, receipt.InputSetDigest, receipt.OutputPathID, receipt.CommandID, uint64(receipt.EventSeq))
			if err != nil {
				t.Fatal(err)
			}
			value.CloseReceipt = &receipt
		}
		aggregate.Routing.Reservations[id] = value
	}
	for id, value := range aggregate.Routing.Activations {
		if value.CommandID == oldCommandID {
			value.CommandID = command.ID
		}
		if value.Receipt.CommandID == oldCommandID {
			value.Receipt.CommandID = command.ID
			value.Receipt.ID, err = ActivationReceiptIdentity(value.Receipt.ActivationID, value.Receipt.ReservationID, value.Receipt.InputSetDigest, value.Receipt.OutputPathID, value.Receipt.CommandID, uint64(value.Receipt.EventSeq))
			if err != nil {
				t.Fatal(err)
			}
		}
		aggregate.Routing.Activations[id] = value
	}
	delete(aggregate.Commands, event.Command.ID)
	aggregate.Commands[command.ID] = command
	admin := event.AdminRecord
	delete(aggregate.AdminRecords, admin.ID)
	admin.ID = ""
	admin.EvidenceRef = command.ID
	admin.ID, err = AdminRecordIdentity(admin)
	if err != nil {
		t.Fatal(err)
	}
	aggregate.AdminRecords[admin.ID] = admin
	event.Command = command
	event.AdminRecord = admin
	event.AggregateDigest = aggregateDigest
	checkpoint.Digest, err = initializeEventDigest(*event)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeCheckpointV7(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	return encoded, digest
}
