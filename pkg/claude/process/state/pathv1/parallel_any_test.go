package pathv1

import (
	"cmp"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"
)

func TestParallelAnyDeterministicActivationIsRestartExact(t *testing.T) {
	source := parallelDirectAnySource(3)
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
	before, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	reservation := reservationForNode(t, before.View(), "merge")
	arrivals := arrivedPathsForReservation(before.View(), reservation.ID)
	if len(arrivals) != 3 {
		t.Fatalf("arrivals = %d, want 3", len(arrivals))
	}

	first, err := AdvanceParallelAny(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AdvanceParallelAny(t.Context(), input)
	if err != nil || first.PostBinding() != second.PostBinding() || string(first.postBytes) != string(second.postBytes) {
		t.Fatalf("any restart drift = %v", err)
	}
	input = verifyParallelTransition(t, source, first)
	after, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	reservation = after.Routing.Reservations[reservation.ID]
	if reservation.State != ReservationActivated || reservation.Activation == nil {
		t.Fatalf("any reservation = %#v", reservation)
	}
	winnerID := after.Routing.Activations[reservation.Activation.ID].InputPathIDs[0]
	slices.SortFunc(arrivals, compareArrivalTuple)
	if winnerID != arrivals[0].ID {
		t.Fatalf("winner = %q, want minimum tuple %q", winnerID, arrivals[0].ID)
	}
	if scope := after.Routing.Scopes[reservation.ReducesScopeID]; scope.State != ScopeClosedActivated || scope.CloseReason != ScopeCloseAny || scope.EventSeq != reservation.EventSeq {
		t.Fatalf("any scope close = %#v", scope)
	}
	if len(after.Routing.Detachments) != len(arrivals)-1 {
		t.Fatalf("detachments = %d, want %d", len(after.Routing.Detachments), len(arrivals)-1)
	}
	for _, arrival := range arrivals[1:] {
		path := after.Routing.Paths[arrival.ID]
		key, _ := DetachmentKeyIdentity(reservation.ID, path.CandidateID)
		detachment := after.Routing.Detachments[key]
		if path.State != PathDetachedSink || path.DetachedSink == nil || path.DetachedSink.DetachmentID != detachment.ID || path.UpdatedSeq != reservation.EventSeq || detachment.ActivatedSeq != reservation.EventSeq {
			t.Fatalf("loser was not sunk in winner event: path=%#v detachment=%#v", path, detachment)
		}
	}
	if report := ValidateAggregate(after.View()); !report.Valid() {
		t.Fatalf("post-any diagnostics = %#v", report.Diagnostics)
	}
}

func TestParallelAnyWinnerTupleProperty(t *testing.T) {
	source := parallelDirectAnySource(4)
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
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	reservation := reservationForNode(t, aggregate.View(), "merge")
	arrivals := arrivedPathsForReservation(aggregate.View(), reservation.ID)
	rng := rand.New(rand.NewSource(448))
	for iteration := 0; iteration < 250; iteration++ {
		after := Clone(aggregate.Routing)
		for _, arrival := range arrivals {
			path := after.Paths[arrival.ID]
			path.ArrivedSeq = int64(rng.Intn(7) + 2)
			after.Paths[path.ID] = path
		}
		want := arrivedPathsForReservation(AggregateView{Routing: &after}, reservation.ID)
		slices.SortFunc(want, compareArrivalTuple)
		winner, _, _, buildErr := buildParallelAnyActivation(&after, reservation, parallelPathIDs(arrivals), int64(iteration+20))
		if buildErr != nil {
			t.Fatalf("iteration %d: %v", iteration, buildErr)
		}
		if winner != want[0].ID {
			t.Fatalf("iteration %d winner = %q, want %q", iteration, winner, want[0].ID)
		}
	}
}

func TestNestedReducerInternsDetachedAncestorBeforeActivation(t *testing.T) {
	for _, policy := range []JoinPolicy{JoinAll, JoinAny} {
		t.Run(string(policy), func(t *testing.T) {
			source := nestedDetachedReducerSource(policy)
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
			beforeIntern, err := EncodeCheckpointV7(input.checkpoint)
			if err != nil {
				t.Fatal(err)
			}

			advance := AdvanceParallelAll
			if policy == JoinAny {
				advance = AdvanceParallelAny
			}
			first, err := advance(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			second, err := advance(t.Context(), input)
			if err != nil || first.Kind() != "parallel_detachment_intern" || first.PostBinding() != second.PostBinding() || string(first.postBytes) != string(second.postBytes) {
				t.Fatalf("intern restart drift: kind=%q err=%v", first.Kind(), err)
			}
			if _, _, _, err := ValidateExecutionTransitionForAppend(t.Context(), beforeIntern, source, first); err != nil {
				t.Fatalf("intern CAS validation: %v", err)
			}
			input = verifyParallelTransition(t, source, first)

			activate1, err := advance(t.Context(), input)
			if err != nil {
				t.Fatal(err)
			}
			activate2, err := advance(t.Context(), input)
			if err != nil || activate1.Kind() == "parallel_detachment_intern" || activate1.PostBinding() != activate2.PostBinding() || string(activate1.postBytes) != string(activate2.postBytes) {
				t.Fatalf("activation restart drift: kind=%q err=%v", activate1.Kind(), err)
			}
			if _, _, _, err := ValidateExecutionTransitionForAppend(t.Context(), beforeIntern, source, activate1); !errors.Is(err, ErrMutationInconsistent) {
				t.Fatalf("stale activation error = %v", err)
			}
			input = verifyParallelTransition(t, source, activate1)
			after, err := CurrentAggregateCheckpoint(input.checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			inner := reservationForNode(t, after.View(), "inner-merge")
			if inner.State != ReservationActivated || inner.Activation == nil {
				t.Fatalf("inner reducer = %#v", inner)
			}
			output := after.Routing.Paths[after.Routing.Activations[inner.Activation.ID].OutputPathID]
			members, err := VerifyDetachmentSet(&after.Routing, output.DetachmentSetID)
			if err != nil || len(members) != 2 {
				t.Fatalf("inner output detachments = %v, %v", members, err)
			}
			if report := ValidateAggregate(after.View()); !report.Valid() {
				t.Fatalf("nested post-state diagnostics = %#v", report.Diagnostics)
			}
		})
	}
}

func TestNestedAllNonSuccessInternsBeforeConsumingArrivals(t *testing.T) {
	source := nestedDetachedReducerSource(JoinAll)
	input, err := VerifyExecutionInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	root := input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID
	for _, nodeID := range []string{"outer-fork", "middle-fork", "inner-fork"} {
		pathID := root
		if nodeID != "outer-fork" {
			transition, activateErr := AdvanceParallelExclusiveArrival(t.Context(), input)
			if activateErr != nil {
				t.Fatal(activateErr)
			}
			input = verifyParallelTransition(t, source, transition)
			pathID = livePathForNode(t, input, nodeID)
		}
		transition, splitErr := AdvanceParallelSplit(t.Context(), input, pathID)
		if splitErr != nil {
			t.Fatal(splitErr)
		}
		input = verifyParallelTransition(t, source, transition)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	inner := reservationForNode(t, aggregate.View(), "inner-merge")
	input = failedReducerCandidateInput(t, source, input, inner)
	input = activateSpecificAnyReducer(t, source, input, "middle-merge")
	input = activateSpecificAnyReducer(t, source, input, "outer-merge")
	transition, err := AdvanceParallelAll(t.Context(), input)
	if err != nil || transition.Kind() != "parallel_detachment_intern" {
		t.Fatalf("non-success intern = %q, %v", transition.Kind(), err)
	}
	input = verifyParallelTransition(t, source, transition)
	transition, err = AdvanceParallelAll(t.Context(), input)
	if err != nil || transition.Kind() != "parallel_all" {
		t.Fatalf("non-success close = %q, %v", transition.Kind(), err)
	}
	input = verifyParallelTransition(t, source, transition)
	after, _ := CurrentAggregateCheckpoint(input.checkpoint)
	inner = after.Routing.Reservations[inner.ID]
	if inner.State != ReservationClosedNoActivation {
		t.Fatalf("inner all close = %#v", inner)
	}
	for _, path := range after.Routing.Paths {
		if path.TargetReservationID != inner.ID || path.State != PathConsumed {
			continue
		}
		if _, err := VerifyDetachmentSet(&after.Routing, path.DetachmentSetID); err != nil {
			t.Fatalf("consumed arrival detachment set: %v", err)
		}
	}
	if report := ValidateAggregate(after.View()); !report.Valid() {
		t.Fatalf("non-success diagnostics = %#v", report.Diagnostics)
	}
}

func TestNonReducingAllInternsDetachedArrivalBeforeActivation(t *testing.T) {
	policy := JoinAll
	source := nonReducingDetachedJoinSource()
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
	transition, err = AdvanceParallelSplit(t.Context(), input, livePathForNode(t, input, "middle-fork"))
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	transition, err = AdvanceParallelExclusiveArrival(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	transition, err = AdvanceParallelSplit(t.Context(), input, livePathForNode(t, input, "inner-fork"))
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	for index := 0; index < 2; index++ {
		transition, err = AdvanceParallelExclusiveArrival(t.Context(), input)
		if err != nil {
			t.Fatal(err)
		}
		input = verifyParallelTransition(t, source, transition)
	}
	localSource := livePathForNode(t, input, "local-source")
	input = observeParallelTask(t, source, input, localSource, "direct")
	transition, err = AdvanceParallelRoute(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	input = activateSpecificAnyReducer(t, source, input, "middle-merge")
	input = activateSpecificAnyReducer(t, source, input, "outer-merge")
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := reservationForNode(t, aggregate.View(), "local-left")
	if ordinary.IsReducing || ordinary.State != ReservationOpen {
		t.Fatalf("ordinary reservation = %#v", ordinary)
	}
	authority := aggregate.Authority.Reservations[ordinary.ID]
	if ordinary.JoinPolicy != policy || authority.JoinPolicy != policy {
		t.Fatalf("template-derived ordinary authority = routing %q, authority %q", ordinary.JoinPolicy, authority.JoinPolicy)
	}
	next, err := advanceCheckpointV7To(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint), CurrentLastLogSeq(input.checkpoint))
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeCheckpointV7(next)
	if err != nil {
		t.Fatal(err)
	}
	input, err = VerifyExecutionInput(t.Context(), encoded, source)
	if err != nil {
		t.Fatal(err)
	}

	advance := AdvanceParallelAll
	first, err := advance(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := advance(t.Context(), input)
	if err != nil || first.Kind() != "parallel_detachment_intern" || first.PostBinding() != second.PostBinding() || string(first.postBytes) != string(second.postBytes) {
		t.Fatalf("ordinary intern restart drift: kind=%q err=%v", first.Kind(), err)
	}
	if _, _, _, err := ValidateExecutionTransitionForAppend(t.Context(), encoded, source, first); err != nil {
		t.Fatalf("ordinary intern CAS validation: %v", err)
	}
	input = verifyParallelTransition(t, source, first)
	activation, err := advance(t.Context(), input)
	if err != nil || activation.Kind() == "parallel_detachment_intern" {
		t.Fatalf("ordinary activation = %q, %v", activation.Kind(), err)
	}
	input = verifyParallelTransition(t, source, activation)
	after, _ := CurrentAggregateCheckpoint(input.checkpoint)
	ordinary = after.Routing.Reservations[ordinary.ID]
	if ordinary.State != ReservationActivated || ordinary.Activation == nil {
		t.Fatalf("ordinary join activation = %#v", ordinary)
	}
	output := after.Routing.Paths[after.Routing.Activations[ordinary.Activation.ID].OutputPathID]
	members, err := VerifyDetachmentSet(&after.Routing, output.DetachmentSetID)
	wantMembers := make([]DetachmentID, 0, len(after.Routing.Detachments))
	for _, detachment := range after.Routing.Detachments {
		wantMembers = append(wantMembers, detachment.ID)
	}
	slices.Sort(wantMembers)
	if err != nil || !slices.Equal(members, wantMembers) {
		t.Fatalf("ordinary output detachments = %v, want %v: %v", members, wantMembers, err)
	}
}

func activateSpecificAnyReducer(t *testing.T, source []byte, input *VerifiedExclusiveInput, nodeID string) *VerifiedExclusiveInput {
	t.Helper()
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	reservation := reservationForNode(t, aggregate.View(), nodeID)
	projection, ready, err := reduceParallelAny(input, aggregate.View(), reservation.ID)
	if err != nil || !ready {
		t.Fatalf("specific any %s = ready %v, %v", nodeID, ready, err)
	}
	last, err := aggregateLogicalLastSeq(projection)
	if err != nil {
		t.Fatal(err)
	}
	next, err := advanceCheckpointV7To(input.checkpoint, projection, CurrentRunStatus(input.checkpoint), last)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := newExecutionTransition(input.checkpoint, next, "parallel_any")
	if err != nil {
		t.Fatal(err)
	}
	return verifyParallelTransition(t, source, transition)
}

func TestParallelAnySlowLoserActivatesOrdinaryReservationThenSinksLate(t *testing.T) {
	source := parallelSlowAnySource()
	input, anyReservation := advanceToSlowAnyWinner(t, source)

	activation, err := AdvanceParallelExclusiveArrival(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, activation)
	slow := livePathForNode(t, input, "slow")
	input = observeParallelTask(t, source, input, slow, "pass")
	routed, err := AdvanceParallelRoute(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, routed)
	r2 := livePathForNode(t, input, "r2")
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	r2Path := aggregate.Routing.Paths[r2]
	if detached, detachErr := DetachedFrom(&aggregate.Routing, r2Path, anyReservation.ID); detachErr != nil || !detached {
		t.Fatalf("slow output lacks R detachment: %v/%v", detached, detachErr)
	}
	r2Activation := aggregate.Routing.Activations[r2Path.SourceActivation.ID]
	r2Reservation := aggregate.Routing.Reservations[r2Activation.ReservationID]
	if detached, detachErr := DetachedFrom(&aggregate.Routing, r2Path, r2Reservation.ID); detachErr != nil || detached {
		t.Fatalf("R detachment suppressed ordinary R2: %v/%v", detached, detachErr)
	}

	input = observeParallelTask(t, source, input, r2, "pass")
	first, err := AdvanceParallelDetachedSink(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := AdvanceParallelDetachedSink(t.Context(), input)
	if err != nil || first.PostBinding() != second.PostBinding() || string(first.postBytes) != string(second.postBytes) {
		t.Fatalf("late sink restart drift = %v", err)
	}
	input = verifyParallelTransition(t, source, first)
	aggregate, err = CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	late := make([]PathRecord, 0, 1)
	for _, path := range aggregate.Routing.Paths {
		if path.TargetReservationID == anyReservation.ID && path.State == PathDetachedSink && path.DetachedSink != nil && path.DetachedSink.ReasonCode == "late_any_arrival" {
			late = append(late, path)
		}
	}
	if len(late) != 1 || late[0].ArrivedSeq == 0 || late[0].ArrivedSeq != late[0].UpdatedSeq {
		t.Fatalf("late detached sinks = %#v", late)
	}
	if aggregate.Routing.Reservations[anyReservation.ID].State != ReservationActivated {
		t.Fatal("late loser reactivated closed any reservation")
	}
	if report := ValidateAggregate(aggregate.View()); !report.Valid() {
		t.Fatalf("late sink diagnostics = %#v", report.Diagnostics)
	}
}

func TestParallelAnyFailureBeforeSinkRemainsOwnedAndFailsAggregate(t *testing.T) {
	source := parallelSlowAnySource()
	input, anyReservation := advanceToSlowAnyWinner(t, source)
	activation, err := AdvanceParallelExclusiveArrival(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, activation)
	slow := livePathForNode(t, input, "slow")
	input = observeParallelTask(t, source, input, slow, "pass")
	routed, err := AdvanceParallelRoute(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, routed)
	r2 := livePathForNode(t, input, "r2")
	input = observeParallelTask(t, source, input, r2, "fail")

	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	failed := aggregate.Routing.Paths[r2]
	if failed.State != PathFailed || failed.TerminalCauseID == "" {
		t.Fatalf("slow failure = %#v", failed)
	}
	if detached, detachErr := DetachedFrom(&aggregate.Routing, failed, anyReservation.ID); detachErr != nil || !detached {
		t.Fatalf("failed loser lost ownership: %v/%v", detached, detachErr)
	}
	winnerEnd := livePathForNode(t, input, "merge")
	ended, err := AdvanceParallelEnd(t.Context(), input, winnerEnd)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, ended)
	aggregate, err = CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	completion, err := AssessAggregateCompletion(aggregate.View())
	if err != nil {
		t.Fatal(err)
	}
	if completion.Result != "failed" || completion.TerminalCauseDigest == "" {
		t.Fatalf("completion = %#v", completion)
	}
}

func advanceToSlowAnyWinner(t *testing.T, source []byte) (*VerifiedExclusiveInput, ActivationReservation) {
	t.Helper()
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
	before, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	reservation := reservationForNode(t, before.View(), "merge")
	transition, err := AdvanceParallelAny(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, transition)
	after, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	return input, after.Routing.Reservations[reservation.ID]
}

func observeParallelTask(t *testing.T, source []byte, input *VerifiedExclusiveInput, pathID PathID, outcome string) *VerifiedExclusiveInput {
	t.Helper()
	plan, err := PlanExclusiveAttempt(t.Context(), input, pathID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := ClaimExclusiveAttempt(t.Context(), input, plan)
	if err != nil {
		t.Fatal(err)
	}
	input = verifyParallelTransition(t, source, claim)
	plan, found, err := RecoverExclusiveAttempt(t.Context(), input)
	if err != nil || !found {
		t.Fatalf("recover attempt = %v/%v", found, err)
	}
	observe, err := ObserveExclusiveAttempt(t.Context(), input, plan, ExclusiveObservation{Outcome: outcome, Actor: "human:operator"}, false)
	if err != nil {
		t.Fatal(err)
	}
	return verifyParallelTransition(t, source, observe)
}

func reservationForNode(t *testing.T, view AggregateView, nodeID string) ActivationReservation {
	t.Helper()
	for _, reservation := range view.Routing.Reservations {
		if reservation.NodeID == nodeID {
			return reservation
		}
	}
	t.Fatalf("reservation for node %q not found", nodeID)
	return ActivationReservation{}
}

func arrivedPathsForReservation(view AggregateView, reservationID ReservationID) []PathRecord {
	paths := make([]PathRecord, 0)
	for _, path := range view.Routing.Paths {
		if path.TargetReservationID == reservationID && path.State == PathArrived {
			paths = append(paths, path)
		}
	}
	return paths
}

func compareArrivalTuple(a, b PathRecord) int {
	if value := cmp.Compare(a.ArrivedSeq, b.ArrivedSeq); value != 0 {
		return value
	}
	return cmp.Compare(a.ID, b.ID)
}

func parallelPathIDs(paths []PathRecord) []PathID {
	ids := make([]PathID, len(paths))
	for index := range paths {
		ids[index] = paths[index].ID
	}
	return ids
}

func livePathForNode(t *testing.T, input *VerifiedExclusiveInput, nodeID string) PathID {
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
		if aggregate.Routing.Reservations[activation.ReservationID].NodeID == nodeID {
			return path.ID
		}
	}
	t.Fatalf("live path for node %q not found", nodeID)
	return ""
}

func parallelDirectAnySource(n int) []byte {
	var out strings.Builder
	out.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: parallel-direct-any\nstart: fork\nnodes:\n  fork:\n    type: parallel\n    next:\n")
	for index := 0; index < n; index++ {
		fmt.Fprintf(&out, "      branch-%04d: merge\n", index)
	}
	out.WriteString("  merge:\n    type: end\n    join: any\n    result: completed\n")
	return []byte(out.String())
}

func parallelSlowAnySource() []byte {
	return []byte(strings.TrimSpace(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: parallel-slow-any
start: fork
nodes:
  fork:
    type: parallel
    next: {quick: merge, slow: slow}
  slow:
    type: task
    performer: {kind: agent, prompt: slow}
    next: r2
  r2:
    type: task
    performer: {kind: agent, prompt: r2}
    next: merge
  merge:
    type: end
    join: any
    result: completed
`) + "\n")
}

func nestedDetachedReducerSource(policy JoinPolicy) []byte {
	return []byte(fmt.Sprintf(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: nested-detached-%s
start: outer-fork
nodes:
  outer-fork:
    type: parallel
    next: {quick: outer-merge, nested: middle-fork}
  middle-fork:
    type: parallel
    next: {quick: middle-merge, nested: inner-fork}
  inner-fork:
    type: parallel
    next: {left: inner-merge, right: inner-merge}
  inner-merge:
    type: task
    join: %s
    performer: {kind: agent, prompt: inner}
    next: middle-merge
  middle-merge:
    type: task
    join: any
    performer: {kind: agent, prompt: middle}
    next: outer-merge
  outer-merge:
    type: end
    join: any
    result: completed
`, policy, policy))
}

func nonReducingDetachedJoinSource() []byte {
	return []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: non-reducing-detached-all
start: outer-fork
nodes:
  outer-fork:
    type: parallel
    next: {quick: outer-merge, nested: middle-fork}
  middle-fork:
    type: parallel
    next: {quick: middle-merge, nested: inner-fork}
  inner-fork:
    type: parallel
    next: {left: local-source, right: local-right}
  local-source:
    type: task
    performer: {kind: agent, prompt: source}
    next: {direct: local-left, alternate: local-left}
  local-left:
    type: task
    join: all
    performer: {kind: agent, prompt: left}
    next: inner-merge
  local-right:
    type: task
    performer: {kind: agent, prompt: right}
    next: inner-merge
  inner-merge:
    type: task
    join: all
    performer: {kind: agent, prompt: inner}
    next: middle-merge
  middle-merge:
    type: task
    join: any
    performer: {kind: agent, prompt: middle}
    next: outer-merge
  outer-merge:
    type: end
    join: any
    result: completed
`)
}

func TestParallelAnyRejectsUnavailableReducer(t *testing.T) {
	if _, err := AdvanceParallelAny(t.Context(), nil); !errors.Is(err, ErrParallelInputInvalid) {
		t.Fatalf("nil any input error = %v", err)
	}
}
