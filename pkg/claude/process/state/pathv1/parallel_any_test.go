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

func TestParallelAnyRejectsUnavailableReducer(t *testing.T) {
	if _, err := AdvanceParallelAny(t.Context(), nil); !errors.Is(err, ErrParallelInputInvalid) {
		t.Fatalf("nil any input error = %v", err)
	}
}
