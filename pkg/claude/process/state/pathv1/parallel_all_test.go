package pathv1

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestParallelAllHappyPathPartialRestartAndExactScopePop(t *testing.T) {
	t.Parallel()
	source := parallelSplitSource(2)
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

	// Split arrivals are concurrently eligible. The deterministic executor may
	// claim them one at a time without changing that checkpoint truth.
	for range 2 {
		activate, activateErr := AdvanceParallelExclusiveArrival(t.Context(), input)
		if activateErr != nil {
			t.Fatal(activateErr)
		}
		input = verifyParallelTransition(t, source, activate)
	}

	for branch := 0; branch < 2; branch++ {
		live := liveParallelTaskPaths(t, input)
		if len(live) != 2-branch {
			t.Fatalf("live branch count = %d, want %d", len(live), 2-branch)
		}
		plan, planErr := PlanExclusiveAttempt(t.Context(), input, live[0], 1, nil)
		if planErr != nil {
			t.Fatal(planErr)
		}
		claim, claimErr := ClaimExclusiveAttempt(t.Context(), input, plan)
		if claimErr != nil {
			t.Fatal(claimErr)
		}
		input = verifyParallelTransition(t, source, claim)
		plan, recovered, recoverErr := RecoverExclusiveAttempt(t.Context(), input)
		if recoverErr != nil || !recovered {
			t.Fatalf("recover attempt = %v/%v", recovered, recoverErr)
		}
		observe, observeErr := ObserveExclusiveAttempt(t.Context(), input, plan, ExclusiveObservation{Outcome: "pass", Actor: "human:operator"}, false)
		if observeErr != nil {
			t.Fatal(observeErr)
		}
		input = verifyParallelTransition(t, source, observe)
		route1, routeErr := AdvanceParallelRoute(t.Context(), input)
		if routeErr != nil {
			t.Fatal(routeErr)
		}
		route2, routeErr := AdvanceParallelRoute(t.Context(), input)
		if routeErr != nil || route1.PostBinding() != route2.PostBinding() || string(route1.postBytes) != string(route2.postBytes) {
			t.Fatalf("restart route drift = %v", routeErr)
		}
		input = verifyParallelTransition(t, source, route1)
		if branch == 0 {
			if _, allErr := AdvanceParallelAll(t.Context(), input); !errors.Is(allErr, ErrParallelAllNotReady) {
				t.Fatalf("partial all fold = %v", allErr)
			}
		}
	}

	before, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	var reducing ActivationReservation
	for _, reservation := range before.Routing.Reservations {
		if reservation.JoinPolicy == JoinAll && reservation.IsReducing {
			reducing = reservation
		}
	}
	if reducing.ID == "" {
		t.Fatal("reducing all reservation is absent")
	}
	nonSuccessInput := failedReducerCandidateInput(t, source, input, reducing)
	closedTransition, closeErr := AdvanceParallelAll(t.Context(), nonSuccessInput)
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	closedInput := verifyParallelTransition(t, source, closedTransition)
	closedAggregate, closeErr := CurrentAggregateCheckpoint(closedInput.checkpoint)
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	closedReservation := closedAggregate.Routing.Reservations[reducing.ID]
	if closedReservation.State != ReservationClosedNoActivation || closedReservation.ClosedReason != string(ScopeCloseCandidateNonSuccess) || closedReservation.Activation != nil {
		t.Fatalf("terminal mixture did not close all reservation: %#v", closedReservation)
	}
	for _, path := range closedAggregate.Routing.Paths {
		if path.TargetReservationID == reducing.ID && path.State == PathConsumed && (path.ConsumedBy != nil || path.Disposition == nil || path.Disposition.ReasonCode != "join_non_success") {
			t.Fatalf("non-success arrival consumption = %#v", path)
		}
	}
	all, err := AdvanceParallelAll(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, all)
	after, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	closed := after.Routing.Scopes[reducing.ReducesScopeID]
	if closed.State != ScopeClosedActivated || closed.CloseReason != ScopeCloseAll {
		t.Fatalf("scope close = %#v", closed)
	}
	activated := after.Routing.Reservations[reducing.ID]
	if activated.State != ReservationActivated || activated.Activation == nil {
		t.Fatalf("all reservation = %#v", activated)
	}
	output := after.Routing.Activations[activated.Activation.ID].OutputPathID
	path := after.Routing.Paths[output]
	if path.ScopeID != closed.ParentScopeID || path.BranchEdgeID != closed.ParentBranchEdgeID {
		t.Fatalf("all output did not pop exactly one scope: %#v / %#v", path, closed)
	}
	if report := ValidateAggregate(after.View()); !report.Valid() {
		t.Fatalf("post-all aggregate diagnostics = %#v", report.Diagnostics)
	}
}

func TestParallelAllNestedNonSuccessSeedsAndReplaysDPE(t *testing.T) {
	t.Parallel()
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
	var inner, outer ActivationReservation
	for _, reservation := range aggregate.Routing.Reservations {
		if reservation.NodeID == "inner-merge" {
			inner = reservation
		}
		if reservation.NodeID == "outer-merge" {
			outer = reservation
		}
	}
	if inner.ID == "" || outer.ID == "" {
		t.Fatalf("nested reducers absent: inner=%#v outer=%#v", inner, outer)
	}
	input = failedReducerCandidateInput(t, source, input, inner)
	transition, err = AdvanceParallelAll(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	aggregate, _ = CurrentAggregateCheckpoint(input.checkpoint)
	pending := 0
	for _, intent := range aggregate.Routing.Propagation {
		if intent.State == PropagationPending {
			pending++
		}
	}
	if pending != 1 {
		t.Fatalf("pending DPE intents = %d, want 1", pending)
	}
	first, err := AdvanceParallelPropagation(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AdvanceParallelPropagation(t.Context(), input)
	if err != nil || first.PostBinding() != second.PostBinding() || string(first.postBytes) != string(second.postBytes) {
		t.Fatalf("DPE restart drift = %v", err)
	}
	input = verifyParallelTransition(t, source, first)
	aggregate, _ = CurrentAggregateCheckpoint(input.checkpoint)
	closedOuterCandidate := false
	for _, closure := range aggregate.Routing.CandidateClosures {
		if closure.Key.ReservationID == outer.ID && closure.TerminalKind == TerminalFailed {
			closedOuterCandidate = true
		}
	}
	if !closedOuterCandidate {
		t.Fatal("DPE did not materialize the exact outer candidate closure")
	}
	transition, err = AdvanceParallelAll(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	aggregate, _ = CurrentAggregateCheckpoint(input.checkpoint)
	if got := aggregate.Routing.Reservations[outer.ID]; got.State != ReservationClosedNoActivation || got.ClosedReason != string(ScopeCloseCandidateNonSuccess) {
		t.Fatalf("outer all did not close from propagated failure: %#v", got)
	}
}

func TestParallelTerminalFailureObservationRetryIsExact(t *testing.T) {
	source := parallelSplitSource(2)
	input, err := VerifyExecutionInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	root := livePathForNodeType(t, input, "parallel")
	transition, err := AdvanceParallelSplit(t.Context(), input, root)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	input = activateAllReadyExclusive(t, source, input)
	paths := liveParallelTaskPaths(t, input)
	plan, err := PlanExclusiveAttempt(t.Context(), input, paths[0], 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	transition, err = ClaimExclusiveAttempt(t.Context(), input, plan)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	plan, found, err := RecoverExclusiveAttempt(t.Context(), input)
	if err != nil || !found {
		t.Fatalf("recover task = %v/%v", found, err)
	}
	observation := ExclusiveObservation{Outcome: "fail", Actor: "human:operator", EvidenceRef: "artifact:terminal-failure"}
	transition, err = ObserveExclusiveAttempt(t.Context(), input, plan, observation, false)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	activation := aggregate.Routing.Activations[plan.Command().Identity.SourceActivationID]
	nodeID := aggregate.Routing.Reservations[activation.ReservationID].NodeID
	recorded, exact, err := ExactExclusiveAttemptObserved(t.Context(), input, nodeID, plan.Command().ID, observation)
	if err != nil || !exact || recorded.ID != plan.Command().ID {
		t.Fatalf("terminal retry = command %q exact %v err %v", recorded.ID, exact, err)
	}
}

func TestExactRouteReservationIgnoresWrongContextWithoutRecreatingClosedTarget(t *testing.T) {
	source := parallelSplitSource(2)
	input, err := VerifyExecutionInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	root := livePathForNodeType(t, input, "parallel")
	transition, err := AdvanceParallelSplit(t.Context(), input, root)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	routing := Clone(aggregate.Routing)
	var target ActivationReservation
	var edge EdgeKey
	for _, reservation := range routing.Reservations {
		if reservation.IsReducing || reservation.State != ReservationOpen || len(reservation.Candidates) == 0 {
			continue
		}
		candidate := reservation.Candidates[0]
		candidateEdge, ok := input.parallel.edges[EdgeID(candidate.MemberID)]
		if ok {
			target, edge = reservation, candidateEdge
			break
		}
	}
	if target.ID == "" {
		t.Fatal("split branch reservation not found")
	}
	closed := target
	closed.State = ReservationClosedNoActivation
	routing.Reservations[closed.ID] = closed
	wrong := target
	wrong.ID = ReservationID("wrong-context")
	wrong.ScopeID = ScopeID("other-scope")
	routing.Reservations[wrong.ID] = wrong
	view := aggregate.View()
	authority := cloneExclusiveAuthority(view.Authority)
	got, created, err := exactRouteReservation(input, view, authority, routing, PathRecord{ScopeID: target.ScopeID, BranchEdgeID: target.BranchEdgeID}, edge, 10)
	if err != nil || created || got.ID != closed.ID || got.State != ReservationClosedNoActivation {
		t.Fatalf("closed exact reservation lookup = %#v created=%v err=%v", got, created, err)
	}
}

func TestAdvanceParallelEndRequiresSealedParallelInput(t *testing.T) {
	if _, err := AdvanceParallelEnd(t.Context(), nil, "path"); !errors.Is(err, ErrParallelInputInvalid) {
		t.Fatalf("nil parallel end input error = %v", err)
	}
}

func activateAllReadyExclusive(t *testing.T, source []byte, input *VerifiedExclusiveInput) *VerifiedExclusiveInput {
	t.Helper()
	for {
		transition, err := AdvanceParallelExclusiveArrival(t.Context(), input)
		if errors.Is(err, ErrParallelAllNotReady) {
			return input
		}
		if err != nil {
			t.Fatal(err)
		}
		input = verifyParallelTransition(t, source, transition)
	}
}

func settleAndRouteFirstTask(t *testing.T, source []byte, input *VerifiedExclusiveInput) *VerifiedExclusiveInput {
	t.Helper()
	paths := liveParallelTaskPaths(t, input)
	plan, err := PlanExclusiveAttempt(t.Context(), input, paths[0], 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := ClaimExclusiveAttempt(t.Context(), input, plan)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	plan, found, err := RecoverExclusiveAttempt(t.Context(), input)
	if err != nil || !found {
		t.Fatalf("recover task = %v/%v", found, err)
	}
	transition, err = ObserveExclusiveAttempt(t.Context(), input, plan, ExclusiveObservation{Outcome: "pass", Actor: "human:operator"}, false)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	transition, err = AdvanceParallelRoute(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	return verifyParallelTransition(t, source, transition)
}

func livePathForNodeType(t *testing.T, input *VerifiedExclusiveInput, nodeType string) PathID {
	t.Helper()
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range aggregate.Routing.Paths {
		if path.Kind != PathActivationOutput || path.State != PathLive {
			continue
		}
		activation := aggregate.Routing.Activations[path.SourceActivation.ID]
		reservation := aggregate.Routing.Reservations[activation.ReservationID]
		if string(input.template.Nodes[reservation.NodeID].Type) == nodeType {
			return path.ID
		}
	}
	t.Fatalf("live %s path not found", nodeType)
	return ""
}

func nestedParallelAllSource() []byte {
	return []byte(strings.TrimSpace(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: nested-parallel-all
start: outer-fork
nodes:
  outer-fork:
    type: parallel
    next: {left: inner-fork, right: outer-right}
  inner-fork:
    type: parallel
    next: {left: inner-left, right: inner-right}
  inner-left:
    type: task
    performer: {kind: agent, prompt: inner-left}
    next: inner-merge
  inner-right:
    type: task
    performer: {kind: agent, prompt: inner-right}
    next: inner-merge
  inner-merge:
    type: task
    join: all
    performer: {kind: agent, prompt: inner-merge}
    next: outer-merge
  outer-right:
    type: task
    performer: {kind: agent, prompt: outer-right}
    next: outer-merge
  outer-merge:
    type: end
    join: all
`) + "\n")
}

func failedReducerCandidateInput(t *testing.T, source []byte, input *VerifiedExclusiveInput, reservation ActivationReservation) *VerifiedExclusiveInput {
	t.Helper()
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	arrivals := make([]PathID, 0)
	for _, path := range aggregate.Routing.Paths {
		if path.TargetReservationID == reservation.ID && path.State == PathArrived {
			arrivals = append(arrivals, path.ID)
		}
	}
	slices.Sort(arrivals)
	if len(arrivals) < 2 {
		t.Fatalf("reducer arrivals = %d", len(arrivals))
	}
	eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
	path := aggregate.Routing.Paths[arrivals[0]]
	command := makeTestCommand(t, CommandIdentity{RunID: aggregate.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1, TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation, InputDigest: "terminal-fold", CauseDigest: "terminal-cause", PlanDigest: "terminal-plan"}, CommandObserved)
	aggregate.Commands[command.ID] = command
	causeID, err := CauseIdentity(path.ID, TerminalFailed, "branch_failed", path.SourceActivation.ID, command.ID, "", uint64(eventSeq))
	if err != nil {
		t.Fatal(err)
	}
	path.State, path.UpdatedSeq, path.TerminalCauseID = PathFailed, eventSeq, causeID
	dispositionID, err := DispositionReceiptIdentity(path.ID, PathArrived, PathFailed, "branch_failed", command.ID, "", uint64(eventSeq))
	if err != nil {
		t.Fatal(err)
	}
	path.Disposition = &DispositionReceipt{ID: dispositionID, PathID: path.ID, FromState: PathArrived, ToState: PathFailed, ReasonCode: "branch_failed", CommandID: command.ID, EventSeq: eventSeq}
	aggregate.Routing.Paths[path.ID] = path
	aggregate.Routing.CauseRecords[causeID] = CauseRecord{ID: causeID, SourcePathID: path.ID, TerminalKind: TerminalFailed, DispositionReason: "branch_failed", SourceActivationID: path.SourceActivation.ID, SourceCommandID: command.ID, EventSeq: eventSeq}
	causeDigest, err := CauseSetIdentity([]CauseID{causeID})
	if err != nil {
		t.Fatal(err)
	}
	aggregate.Routing.CauseSets[causeDigest] = CauseSetRecord{Digest: causeDigest, CauseIDs: []CauseID{causeID}}
	closureKey, err := CandidateClosureKeyIdentity(reservation.ID, path.CandidateID)
	if err != nil {
		t.Fatal(err)
	}
	closureID, err := CandidateClosureIdentity(reservation.ID, path.CandidateID, TerminalFailed, causeDigest)
	if err != nil {
		t.Fatal(err)
	}
	aggregate.Routing.CandidateClosures[closureKey] = CandidateClosure{ID: closureID, Key: CandidateClosureKeyRecord{ID: closureKey, ReservationID: reservation.ID, CandidateID: path.CandidateID}, TerminalKind: TerminalFailed, CauseDigest: causeDigest, CommandID: command.ID, EventSeq: eventSeq}
	next, err := advanceCheckpointV7To(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint), uint64(eventSeq))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeCheckpointV7(next)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyExecutionInput(t.Context(), encoded, source)
	if err != nil {
		t.Fatal(err)
	}
	return verified
}

func verifyParallelTransition(t *testing.T, source []byte, transition *ExecutionTransition) *VerifiedExclusiveInput {
	t.Helper()
	input, err := VerifyExecutionInput(t.Context(), transition.postBytes, source)
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func liveParallelTaskPaths(t *testing.T, input *VerifiedExclusiveInput) []PathID {
	t.Helper()
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]PathID, 0)
	for _, path := range aggregate.Routing.Paths {
		if path.Kind != PathActivationOutput || path.State != PathLive {
			continue
		}
		activation := aggregate.Routing.Activations[path.SourceActivation.ID]
		reservation := aggregate.Routing.Reservations[activation.ReservationID]
		if input.template.Nodes[reservation.NodeID].Type == "task" {
			paths = append(paths, path.ID)
		}
	}
	slices.Sort(paths)
	return paths
}
