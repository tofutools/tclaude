package pathv1

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/quick"
)

func TestExclusiveMultiWayDistinctTargetsSequenceAndRecovery(t *testing.T) {
	const fanout = 5
	source := exclusiveFanoutTemplate(fanout, false)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{
		SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
		Attempt:      1, Outcome: exclusiveOutcome(2),
	}
	sequence, err := PlanExclusiveRouteSequence(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	commands := sequence.Commands()
	if len(commands) != 2*fanout+1 {
		t.Fatalf("command count = %d, want %d", len(commands), 2*fanout+1)
	}
	if commands[0].Identity.Kind != CommandRoutePaths || commands[len(commands)-1].Identity.Kind != CommandRoutePaths {
		t.Fatalf("sequence endpoints = %q/%q", commands[0].Identity.Kind, commands[len(commands)-1].Identity.Kind)
	}
	loserPaths := make([]PathID, 0, fanout-1)
	for _, command := range commands[1:fanout] {
		if command.Identity.Kind != CommandPropagateCandidateClosure {
			t.Fatalf("loser command kind = %q", command.Identity.Kind)
		}
		loserPaths = append(loserPaths, command.Identity.SourcePathID)
	}
	if !slices.IsSorted(loserPaths) {
		t.Fatalf("loser order = %#v", loserPaths)
	}
	deadReservations := make([]ReservationID, 0, fanout-1)
	for _, command := range commands[fanout : 2*fanout-1] {
		if command.Identity.Kind != CommandActivateGeneration {
			t.Fatalf("dead-reservation command kind = %q", command.Identity.Kind)
		}
		deadReservations = append(deadReservations, command.Identity.TargetReservationID)
	}
	if !slices.IsSorted(deadReservations) {
		t.Fatalf("dead reservation order = %#v", deadReservations)
	}

	second, err := PlanExclusiveRouteSequence(t.Context(), input, observation)
	if err != nil || !reflect.DeepEqual(commands, second.Commands()) {
		t.Fatalf("deterministic replan differs: %v", err)
	}
	for cursor := 0; cursor <= len(commands); cursor++ {
		recovery, recoverErr := RecoverExclusiveRouteSequence(t.Context(), input, observation, commands[:cursor])
		if recoverErr != nil {
			t.Fatalf("recover cursor %d: %v", cursor, recoverErr)
		}
		if got := recovery.Cursor(); got.Applied != uint32(cursor) || got.Total != uint32(len(commands)) || got.Complete() != (cursor == len(commands)) {
			t.Fatalf("cursor %d = %#v", cursor, got)
		}
		if cursor < len(commands) && !exactExclusiveCommand(recovery.NextCommand(), commands[cursor]) {
			t.Fatalf("cursor %d next command drifted", cursor)
		}
		if cursor > 0 {
			result, replayErr := replayExclusiveSequenceCommand(recovery.Projection().aggregate.View(), input.binding, commands[cursor-1])
			if replayErr != nil || result != ReplayAlreadyApplied {
				t.Fatalf("cursor %d idempotent replay = %q, %v", cursor, result, replayErr)
			}
		}
	}
	if _, err := ReduceExclusiveRouteSequence(t.Context(), input, observation, commands[:len(commands)-1]); !errors.Is(err, ErrExclusiveNotRoutable) {
		t.Fatalf("partial reduce error = %v", err)
	}
	drifted := cloneExclusiveCommandSlice(commands[:2])
	drifted[1].Payload = append(drifted[1].Payload, ' ')
	if _, err := RecoverExclusiveRouteSequence(t.Context(), input, observation, drifted); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("payload drift error = %v", err)
	}
	reordered := cloneExclusiveCommandSlice(commands[:3])
	reordered[1], reordered[2] = reordered[2], reordered[1]
	if _, err := RecoverExclusiveRouteSequence(t.Context(), input, observation, reordered); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("reordered prefix error = %v", err)
	}

	projection, err := ReduceExclusiveRouteSequence(t.Context(), input, observation, commands)
	if err != nil {
		t.Fatal(err)
	}
	routing := projection.Routing()
	closed, impossible, arrivedOrConsumed := 0, 0, 0
	for _, reservation := range routing.Reservations {
		if reservation.State == ReservationClosedNoActivation {
			closed++
		}
	}
	for _, path := range routing.Paths {
		switch path.Kind {
		case PathImpossibleEdge:
			impossible++
			set := routing.CauseSets[path.ImpossibleCauseDigest]
			if len(set.CauseIDs) != 1 || routing.CauseRecords[set.CauseIDs[0]].SourceCommandID != commands[0].ID {
				t.Fatalf("loser provenance = %#v / %#v", set, routing.CauseRecords)
			}
		case PathEdge:
			if path.State == PathArrived || path.State == PathConsumed {
				arrivedOrConsumed++
			}
		}
	}
	if closed != fanout-1 || impossible != fanout-1 || arrivedOrConsumed != 1 || len(routing.CandidateClosures) != fanout-1 {
		t.Fatalf("closed=%d impossible=%d selected=%d closures=%d", closed, impossible, arrivedOrConsumed, len(routing.CandidateClosures))
	}
}

func TestExclusiveMultiWaySharedTargetClosesEveryCandidateBeforeActivation(t *testing.T) {
	const fanout = 6
	source := exclusiveFanoutTemplate(fanout, true)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{
		SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
		Attempt:      1, Outcome: exclusiveOutcome(4),
	}
	sequence, err := PlanExclusiveRouteSequence(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	commands := sequence.Commands()
	if len(commands) != fanout+2 { // route + losers + selected activation + end
		t.Fatalf("shared-target command count = %d", len(commands))
	}
	for _, command := range commands[1:fanout] {
		if command.Identity.Kind != CommandPropagateCandidateClosure {
			t.Fatalf("shared-target loser kind = %q", command.Identity.Kind)
		}
	}
	projection, err := ReduceExclusiveRouteSequence(t.Context(), input, observation, commands)
	if err != nil {
		t.Fatal(err)
	}
	routing := projection.Routing()
	if len(routing.CandidateClosures) != fanout-1 {
		t.Fatalf("shared-target closures = %d", len(routing.CandidateClosures))
	}
	for _, reservation := range routing.Reservations {
		if reservation.NodeID != "done" {
			continue
		}
		if reservation.State != ReservationActivated || len(reservation.Candidates) != fanout {
			t.Fatalf("shared target reservation = %#v", reservation)
		}
	}
}

func TestExclusiveMultiWayConservationProperty(t *testing.T) {
	property := func(raw uint8, shared bool) bool {
		fanout := 3 + int(raw%14)
		source := exclusiveFanoutTemplate(fanout, shared)
		input, err := VerifyExclusiveInput(context.Background(), initializedExclusiveCheckpoint(t, source), source)
		if err != nil {
			return false
		}
		observation := ExclusiveObservation{
			SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
			Attempt:      1, Outcome: exclusiveOutcome(fanout / 2),
		}
		sequence, err := PlanExclusiveRouteSequence(context.Background(), input, observation)
		if err != nil || len(sequence.commands) > 2*fanout+1 {
			return false
		}
		routing := Clone(sequence.final.Routing)
		impossible := 0
		for _, path := range routing.Paths {
			if path.Kind == PathImpossibleEdge && path.State == PathImpossible {
				impossible++
			}
		}
		return impossible == fanout-1 && len(routing.CandidateClosures) == fanout-1
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 16}); err != nil {
		t.Fatal(err)
	}
}

func TestExclusiveMultiWayBoundsCancellationAndConcurrentPlanning(t *testing.T) {
	maximum, err := ExclusiveRouteSequenceCommandBound(MaxExclusiveOutgoing)
	if err != nil || maximum > MaxRoutingLogEntries {
		t.Fatalf("maximum command bound = %d, %v", maximum, err)
	}
	if _, err := ExclusiveRouteSequenceCommandBound(MaxExclusiveOutgoing + 1); err == nil {
		t.Fatal("exclusive operational-bound fan-out accepted")
	}
	source := exclusiveFanoutTemplate(8, false)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{
		SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
		Attempt:      1, Outcome: exclusiveOutcome(3),
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PlanExclusiveRouteSequence(canceled, input, observation); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled sequence error = %v", err)
	}
	if _, err := RecoverExclusiveRouteSequence(t.Context(), input, observation, make([]CommandRecord, MaxRoutingLogEntries+1)); err == nil {
		t.Fatal("oversized recovery prefix accepted")
	}
	want, err := PlanExclusiveRouteSequence(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	const workers = 12
	var group sync.WaitGroup
	errs := make(chan error, workers)
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			got, planErr := PlanExclusiveRouteSequence(context.Background(), input, observation)
			if planErr != nil {
				errs <- planErr
				return
			}
			if !reflect.DeepEqual(got.Commands(), want.Commands()) {
				errs <- errors.New("concurrent sequence differs")
			}
		}()
	}
	group.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestExclusiveMultiWayPlansCompleteOperationalBoundary(t *testing.T) {
	source := exclusiveFanoutTemplate(MaxExclusiveOutgoing, false)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{
		SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
		Attempt:      1, Outcome: exclusiveOutcome(0),
	}
	sequence, err := PlanExclusiveRouteSequence(t.Context(), input, observation)
	if err != nil {
		t.Fatal(err)
	}
	commands := sequence.Commands()
	if len(commands) != 2*MaxExclusiveOutgoing+1 {
		t.Fatalf("boundary sequence commands = %d, want %d", len(commands), 2*MaxExclusiveOutgoing+1)
	}
	command := commands[0]
	var payload mutationPayload[RoutePathsPlan]
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if got, want := len(payload.Plan.Batch.Mutations), 4*MaxExclusiveOutgoing-1; got != want {
		t.Fatalf("boundary route mutations = %d, want %d", got, want)
	}
	routing := sequence.final.Routing
	if len(routing.CandidateClosures) != MaxExclusiveOutgoing-1 {
		t.Fatalf("boundary loser closures = %d", len(routing.CandidateClosures))
	}
	closed := 0
	for _, reservation := range routing.Reservations {
		if reservation.State == ReservationClosedNoActivation {
			closed++
		}
	}
	if closed != MaxExclusiveOutgoing-1 {
		t.Fatalf("boundary closed loser reservations = %d", closed)
	}

	overSource := exclusiveFanoutTemplate(MaxExclusiveOutgoing+1, true)
	overInput, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, overSource), overSource)
	if err != nil {
		t.Fatal(err)
	}
	overObservation := ExclusiveObservation{
		SourcePathID: overInput.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
		Attempt:      1, Outcome: exclusiveOutcome(0),
	}
	if _, err := PlanExclusiveRouteSequence(t.Context(), overInput, overObservation); err == nil {
		t.Fatal("over-operational-bound fan-out planned")
	}
}

func TestExclusiveMultiWayCancellationAfterPlanningStarts(t *testing.T) {
	source := exclusiveFanoutTemplate(MaxExclusiveOutgoing, false)
	input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
	if err != nil {
		t.Fatal(err)
	}
	observation := ExclusiveObservation{
		SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
		Attempt:      1, Outcome: exclusiveOutcome(0),
	}
	ctx := &cancelAfterErrChecksContext{Context: context.Background(), remaining: 24, done: make(chan struct{})}
	if _, err := PlanExclusiveRouteSequence(ctx, input, observation); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-plan cancellation error = %v", err)
	}
	if calls := ctx.calls.Load(); calls < 24 {
		t.Fatalf("planner checked cancellation only %d times", calls)
	}
}

type cancelAfterErrChecksContext struct {
	context.Context
	remaining int64
	calls     atomic.Int64
	done      chan struct{}
	once      sync.Once
}

func (c *cancelAfterErrChecksContext) Done() <-chan struct{} { return c.done }

func (c *cancelAfterErrChecksContext) Err() error {
	c.calls.Add(1)
	if atomic.AddInt64(&c.remaining, -1) <= 0 {
		c.once.Do(func() { close(c.done) })
		return context.Canceled
	}
	return nil
}

func FuzzExclusiveMultiWaySequenceRejectsPrefixMutation(f *testing.F) {
	f.Add([]byte(" "))
	f.Add([]byte(`{"drift":true}`))
	f.Fuzz(func(t *testing.T, mutation []byte) {
		if len(mutation) > 256 {
			t.Skip()
		}
		source := exclusiveFanoutTemplate(4, false)
		input, err := VerifyExclusiveInput(t.Context(), initializedExclusiveCheckpoint(t, source), source)
		if err != nil {
			t.Fatal(err)
		}
		observation := ExclusiveObservation{
			SourcePathID: input.checkpoint.Initialize.Aggregate.Authority.Genesis.OutputPathID,
			Attempt:      1, Outcome: exclusiveOutcome(1),
		}
		sequence, err := PlanExclusiveRouteSequence(t.Context(), input, observation)
		if err != nil {
			t.Fatal(err)
		}
		prefix := sequence.Commands()[:2]
		if len(mutation) == 0 {
			mutation = []byte(" ")
		}
		prefix[1].Payload = append(prefix[1].Payload, mutation...)
		if _, err := RecoverExclusiveRouteSequence(t.Context(), input, observation, prefix); !errors.Is(err, ErrMutationInvalid) {
			t.Fatalf("mutated prefix error = %v", err)
		}
	})
}

func replayExclusiveSequenceCommand(post AggregateView, binding CheckpointBinding, command CommandRecord) (ReplayDisposition, error) {
	view := MutationReplayView{Aggregate: post, Checkpoint: binding}
	switch command.Identity.Kind {
	case CommandRoutePaths:
		result, err := ReplayRoutePaths(view, command)
		return result.Disposition, err
	case CommandPropagateCandidateClosure:
		result, err := ReplayPropagateClosure(view, command)
		return result.Disposition, err
	case CommandActivateGeneration:
		result, err := ReplayActivateGeneration(view, command)
		return result.Disposition, err
	default:
		return "", fmt.Errorf("unexpected sequence command %q", command.Identity.Kind)
	}
}

func exclusiveFanoutTemplate(fanout int, shared bool) []byte {
	var source strings.Builder
	source.WriteString("apiVersion: tclaude.dev/v1alpha1\nkind: ProcessTemplate\nid: exclusive-multiway\nstart: choose\nnodes:\n  choose:\n    type: decision\n    performer: {kind: human, ask: choose}\n    next:\n")
	for index := 0; index < fanout; index++ {
		target := fmt.Sprintf("target_%03d", index)
		if shared {
			target = "done"
		}
		fmt.Fprintf(&source, "      %s: %s\n", exclusiveOutcome(index), target)
	}
	if shared {
		source.WriteString("  done: {type: end}\n")
	} else {
		for index := 0; index < fanout; index++ {
			fmt.Fprintf(&source, "  target_%03d: {type: end}\n", index)
		}
	}
	return []byte(source.String())
}

func exclusiveOutcome(index int) string {
	return fmt.Sprintf("outcome_%03d", index)
}
