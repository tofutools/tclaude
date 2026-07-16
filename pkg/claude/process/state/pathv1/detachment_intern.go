package pathv1

import (
	"context"
	"fmt"
	"math"
	"slices"
)

// advanceReducerDetachmentIntern creates at most one immutable set node for
// the first ready reducer in stable reservation/arrival order. It deliberately
// leaves every path unchanged; callers repeat the transition until activation
// can inherit its inputs without adding mutations to the reducer event.
func advanceReducerDetachmentIntern(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, false, err
	}
	reservation, pathID, record, found, selectErr := nextReducerDetachmentIntern(aggregate.Routing)
	if selectErr != nil {
		return nil, false, selectErr
	}
	if found {
		if CurrentLastLogSeq(input.checkpoint) >= math.MaxInt64 {
			return nil, false, &OverBudgetError{Limit: "log_entries", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
		}
		eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
		before := Clone(aggregate.Routing)
		after := Clone(before)
		after.DetachmentSets[record.ID] = record
		batch, batchErr := NewMutationBatch(&before, &after, eventSeq)
		if batchErr != nil {
			return nil, false, batchErr
		}
		if len(batch.Mutations) != 1 || batch.Mutations[0].Kind != MutationDetachmentSet {
			return nil, false, fmt.Errorf("%w: detachment-set intern must create exactly one record", ErrMutationInconsistent)
		}
		current := aggregate.View()
		current.Routing = &before
		current.Commands = cloneMap(current.Commands)
		replayView := MutationReplayView{Aggregate: current, Checkpoint: input.binding}
		plan := InternDetachmentSetPlan{ReservationID: reservation.ID, Generation: reservation.Generation, SourcePathID: pathID, Record: record, Batch: batch}
		payload, payloadErr := EncodeInternDetachmentSetPayload(replayView, plan)
		if payloadErr != nil {
			return nil, false, payloadErr
		}
		identity := CommandIdentity{
			RunID: aggregate.RunID, Kind: CommandInternDetachmentSet, PayloadSchema: 1,
			SourcePathID: pathID, TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
			InputDigest: record.ID, PlanDigest: payloadDigest(payload),
		}
		command, commandErr := observedCommand(identity, payload)
		if commandErr != nil {
			return nil, false, commandErr
		}
		if insertErr := insertExactCommand(replayView.Aggregate.Commands, command); insertErr != nil {
			return nil, false, insertErr
		}
		result, replayErr := ReplayInternDetachmentSet(replayView, command)
		if replayErr != nil {
			return nil, false, replayErr
		}
		nextAggregate := replayView.Aggregate
		nextAggregate.Routing = &result.Routing
		projection, checkpointErr := checkpointAggregate(nextAggregate)
		if checkpointErr != nil {
			return nil, false, checkpointErr
		}
		next, advanceErr := advanceCheckpointV7To(input.checkpoint, projection, CurrentRunStatus(input.checkpoint), uint64(eventSeq))
		if advanceErr != nil {
			return nil, false, advanceErr
		}
		transition, transitionErr := newExecutionTransition(input.checkpoint, next, "parallel_detachment_intern")
		return transition, true, transitionErr
	}
	return nil, false, nil
}

func nextReducerDetachmentIntern(routing RoutingState) (ActivationReservation, PathID, DetachmentSetRecord, bool, error) {
	ids := make([]ReservationID, 0)
	for id, reservation := range routing.Reservations {
		if reservation.State == ReservationOpen && (reservation.JoinPolicy == JoinAny || reservation.JoinPolicy == JoinAll) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		reservation := routing.Reservations[id]
		_, arrivals, _, err := activationFold(routing, reservation)
		if err != nil {
			return ActivationReservation{}, "", DetachmentSetRecord{}, false, err
		}
		ready := len(arrivals) > 0
		if reservation.JoinPolicy == JoinAll {
			_, open, _, _, _, _ := parallelAllCandidateKinds(routing, reservation)
			// Both activated and closed-no-activation all events consume every
			// arrived input, so either closed fold must be fully interned first.
			ready = !open && len(arrivals) > 0
		}
		if !ready {
			continue
		}
		slices.Sort(arrivals)
		for _, pathID := range arrivals {
			_, records, deriveErr := derivePathDetachmentInheritance(&routing, routing.Paths[pathID])
			if deriveErr != nil {
				return ActivationReservation{}, "", DetachmentSetRecord{}, false, deriveErr
			}
			if len(records) > 0 {
				return reservation, pathID, records[0], true, nil
			}
		}
	}
	return ActivationReservation{}, "", DetachmentSetRecord{}, false, nil
}
