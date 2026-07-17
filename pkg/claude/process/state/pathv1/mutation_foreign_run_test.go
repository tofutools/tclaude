package pathv1

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

type mutationCommandFixture struct {
	view    MutationReplayView
	command CommandRecord
}

func TestMutationCommandsRejectForeignRunBeforeReplay(t *testing.T) {
	factories := map[CommandKindV1]func(*testing.T) mutationCommandFixture{
		CommandRoutePaths:                routeMutationFixture,
		CommandActivateGeneration:        activateMutationFixture,
		CommandPropagateCandidateClosure: propagateMutationFixture,
		CommandSettleDetachedSink:        detachedSinkMutationFixture,
		CommandInternDetachmentSet:       internDetachmentSetMutationFixture,
	}
	if len(factories) != len(mutationCommandHandlers) {
		t.Fatalf("fixture kinds = %d, registered mutation kinds = %d", len(factories), len(mutationCommandHandlers))
	}
	kinds := make([]CommandKindV1, 0, len(mutationCommandHandlers))
	for kind := range mutationCommandHandlers {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	for _, kind := range kinds {
		handler := mutationCommandHandlers[kind]
		factory, ok := factories[kind]
		if !ok {
			t.Fatalf("registered mutation kind %q lacks a foreign-run fixture", kind)
		}
		t.Run(string(kind), func(t *testing.T) {
			fixture := factory(t)
			if fixture.command.Identity.Kind != kind {
				t.Fatalf("fixture command kind = %q, want %q", fixture.command.Identity.Kind, kind)
			}
			if report := ValidateAggregate(fixture.view.Aggregate); !report.Valid() {
				t.Fatalf("same-run fixture aggregate diagnostics = %#v", report.Diagnostics)
			}
			if err := handler.validate(fixture.view, fixture.command); err != nil {
				t.Fatalf("same-run direct validation: %v", err)
			}
			if result, err := ReplayMutationCommand(fixture.view, fixture.command); err != nil || result.Disposition != ReplayAlreadyApplied {
				t.Fatalf("same-run replay = %q, %v", result.Disposition, err)
			}

			foreign := fixture.command
			foreign.Identity.RunID = "foreign-run"
			foreign = commandWithPayload(t, foreign.Identity, foreign.State, foreign.Payload)
			if err := ValidateCommand(foreign); err != nil {
				t.Fatalf("foreign command is not independently valid: %v", err)
			}
			foreignView := fixture.view
			foreignView.Aggregate.Commands = cloneMap(fixture.view.Aggregate.Commands)
			delete(foreignView.Aggregate.Commands, fixture.command.ID)
			foreignView.Aggregate.Commands[foreign.ID] = foreign
			if !canonicalEqual(foreignView.Aggregate.Commands[foreign.ID], foreign) {
				t.Fatal("foreign command is not byte-exact in aggregate")
			}
			before, err := Encode(foreignView.Aggregate.Routing)
			if err != nil {
				t.Fatal(err)
			}
			wantError := fmt.Sprintf("%s: command run differs from aggregate", ErrMutationInvalid)
			assertForeignRunError(t, wantError, handler.validate(foreignView, foreign))
			assertForeignRunError(t, wantError, ValidateMutationCommand(foreignView, foreign))
			result, replayErr := ReplayMutationCommand(foreignView, foreign)
			assertForeignRunError(t, wantError, replayErr)
			if !canonicalEqual(result, MutationReplayResult{}) {
				t.Fatalf("foreign replay returned a mutation result: %#v", result)
			}
			after, err := Encode(foreignView.Aggregate.Routing)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("foreign replay mutated routing before rejection")
			}
		})
	}
}

func assertForeignRunError(t *testing.T, want string, err error) {
	t.Helper()
	if !errors.Is(err, ErrMutationInvalid) || errors.Is(err, ErrMutationInconsistent) || err.Error() != want {
		t.Fatalf("foreign-run error = %v, want %q classified only as invalid", err, want)
	}
}

func mutationFixtureFromTransition(t *testing.T, input *VerifiedExclusiveInput, source []byte, transition *ExecutionTransition, kind CommandKindV1) mutationCommandFixture {
	t.Helper()
	before, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if report := ValidateAggregate(before.View()); !report.Valid() {
		t.Fatalf("pre-transition aggregate diagnostics = %#v", report.Diagnostics)
	}
	afterInput := verifyParallelTransition(t, source, transition)
	after, err := CurrentAggregateCheckpoint(afterInput.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	var command CommandRecord
	for id, candidate := range after.Commands {
		if _, existed := before.Commands[id]; !existed && candidate.Identity.Kind == kind {
			if command.ID != "" {
				t.Fatalf("transition created multiple %q commands", kind)
			}
			command = candidate
		}
	}
	if command.ID == "" {
		t.Fatalf("transition did not create %q command", kind)
	}
	view := after.View()
	view.Commands = cloneMap(view.Commands)
	return mutationCommandFixture{
		view:    MutationReplayView{Aggregate: view, Checkpoint: input.binding},
		command: command,
	}
}

func routeMutationFixture(t *testing.T) mutationCommandFixture {
	t.Helper()
	postView, childID, _ := validOpenArrivalFixture(t)
	parentID := postView.Authority.Genesis.OutputPathID
	delete(postView.Commands, postView.Routing.Paths[parentID].Disposition.CommandID)

	before := Clone(*postView.Routing)
	delete(before.Paths, childID)
	parent := before.Paths[parentID]
	parent.State = PathLive
	parent.ProducedPathIDs = nil
	parent.Disposition = nil
	parent.UpdatedSeq = 1
	before.Paths[parentID] = parent

	afterTemplate := Clone(*postView.Routing)
	afterParent := afterTemplate.Paths[parentID]
	afterParent.Disposition.CommandID = MutationCommandPlaceholder
	afterParent.Disposition.ReasonCode = "exclusive_route"
	afterParent.Disposition.ID, _ = DispositionReceiptIdentity(parentID, PathLive, PathRouted, "exclusive_route", MutationCommandPlaceholder, "", 2)
	afterTemplate.Paths[parentID] = afterParent
	batch, err := NewMutationBatch(&before, &afterTemplate, 2)
	if err != nil {
		t.Fatal(err)
	}

	settlement := makeTestCommand(t, CommandIdentity{
		RunID: postView.RunID, Kind: CommandSettleAttempt, PayloadSchema: 1,
		SourceActivationID: parent.SourceActivation.ID, SourceGeneration: 1, Attempt: 1,
		InputDigest: "perform-command", PlanDigest: "observed-result", ResultCode: "pass",
	}, CommandObserved)
	postView.Commands[settlement.ID] = settlement
	postView.Routing = &before
	replayView := MutationReplayView{
		Aggregate:  postView,
		Checkpoint: CheckpointBinding{Generation: 7, Digest: strings.Repeat("c", 64)},
	}
	plan := RoutePathsPlan{
		SettlementCommandID: settlement.ID,
		SourceActivationID:  parent.SourceActivation.ID,
		SourceGeneration:    1,
		SourcePathID:        parentID,
		Attempt:             1,
		CauseDigest:         "route-cause",
		ResultCode:          "exclusive/pass",
		ProducedPathIDs:     []PathID{childID},
		Batch:               batch,
	}
	payload, err := EncodeRoutePathsPayload(replayView, plan)
	if err != nil {
		t.Fatal(err)
	}
	identity := CommandIdentity{
		RunID: postView.RunID, Kind: CommandRoutePaths, PayloadSchema: 1,
		SourceActivationID: parent.SourceActivation.ID, SourceGeneration: 1, SourcePathID: parentID, Attempt: 1,
		InputDigest: settlement.ID, CauseDigest: plan.CauseDigest, PlanDigest: payloadDigest(payload), ResultCode: plan.ResultCode,
	}
	command := commandWithPayload(t, identity, CommandObserved, payload)
	replayView.Aggregate.Commands[command.ID] = command
	result, err := ReplayRoutePaths(replayView, command)
	if err != nil || result.Disposition != ReplayApplied {
		t.Fatalf("construct route fixture = %q, %v", result.Disposition, err)
	}
	replayView.Aggregate.Routing = &result.Routing
	return mutationCommandFixture{view: replayView, command: command}
}

func activateMutationFixture(t *testing.T) mutationCommandFixture {
	t.Helper()
	source := parallelDirectAnySource(2)
	input, err := VerifyExecutionInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	root := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	split, err := AdvanceParallelSplit(t.Context(), input, root)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, split)
	transition, err := AdvanceParallelAny(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	return mutationFixtureFromTransition(t, input, source, transition, CommandActivateGeneration)
}

func propagateMutationFixture(t *testing.T) mutationCommandFixture {
	t.Helper()
	source := nestedParallelAllSource()
	input, err := VerifyExecutionInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	root := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	transition, err := AdvanceParallelSplit(t.Context(), input, root)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	input = activateAllReadyExclusive(t, source, input)
	innerFork := livePathForNodeType(t, input, "parallel")
	transition, err = AdvanceParallelSplit(t.Context(), input, innerFork)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	input = activateAllReadyExclusive(t, source, input)
	for len(liveParallelTaskPaths(t, input)) > 0 {
		input = settleAndRouteFirstTask(t, source, input)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	var inner ActivationReservation
	for _, reservation := range aggregate.Routing.Reservations {
		if reservation.NodeID == "inner-merge" {
			inner = reservation
			break
		}
	}
	if inner.ID == "" {
		t.Fatal("inner reducer is absent")
	}
	input = failedReducerCandidateInput(t, source, input, inner)
	transition, err = AdvanceParallelAll(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	transition, err = AdvanceParallelPropagation(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	return mutationFixtureFromTransition(t, input, source, transition, CommandPropagateCandidateClosure)
}

func detachedSinkMutationFixture(t *testing.T) mutationCommandFixture {
	t.Helper()
	source := parallelSlowAnySource()
	input, _ := advanceToSlowAnyWinner(t, source)
	transition, err := AdvanceParallelExclusiveArrival(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	slow := livePathForNode(t, input, "slow")
	input = observeParallelTask(t, source, input, slow, "pass")
	transition, err = AdvanceParallelRoute(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	r2 := livePathForNode(t, input, "r2")
	input = observeParallelTask(t, source, input, r2, "pass")
	transition, err = AdvanceParallelDetachedSink(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	return mutationFixtureFromTransition(t, input, source, transition, CommandSettleDetachedSink)
}

func internDetachmentSetMutationFixture(t *testing.T) mutationCommandFixture {
	t.Helper()
	source := nestedDetachedReducerSource(JoinAny)
	input, err := VerifyExecutionInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	root := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	transition, err := AdvanceParallelSplit(t.Context(), input, root)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	transition, err = AdvanceParallelExclusiveArrival(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	middleFork := livePathForNode(t, input, "middle-fork")
	transition, err = AdvanceParallelSplit(t.Context(), input, middleFork)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	transition, err = AdvanceParallelExclusiveArrival(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	innerFork := livePathForNode(t, input, "inner-fork")
	transition, err = AdvanceParallelSplit(t.Context(), input, innerFork)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	input = activateSpecificAnyReducer(t, source, input, "middle-merge")
	input = activateSpecificAnyReducer(t, source, input, "outer-merge")
	transition, err = AdvanceParallelAny(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	if transition.Kind() != "parallel_detachment_intern" {
		t.Fatalf("transition kind = %q, want parallel_detachment_intern", transition.Kind())
	}
	return mutationFixtureFromTransition(t, input, source, transition, CommandInternDetachmentSet)
}
