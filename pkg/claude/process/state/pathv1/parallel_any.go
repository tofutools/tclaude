package pathv1

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
)

var (
	ErrParallelAnyNotReady          = errors.New("path-v1 parallel any is not ready")
	ErrParallelDetachedSinkNotReady = errors.New("path-v1 detached sink is not ready")
)

// AdvanceParallelAny installs the first ready any reservation in stable ID
// order. A successful fold chooses the minimum immutable
// (ArrivedSeq, PathID), and the single activate-generation event also closes
// the reservation/scope and records every losing candidate detachment.
func AdvanceParallelAny(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.checkpoint == nil || input.parallel == nil {
		return nil, fmt.Errorf("%w: verified parallel input is required", ErrParallelInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	if transition, interned, internErr := advanceReducerDetachmentIntern(ctx, input); internErr != nil {
		return nil, internErr
	} else if interned {
		return transition, nil
	}
	ids := make([]ReservationID, 0)
	for id, reservation := range aggregate.Routing.Reservations {
		if reservation.State == ReservationOpen && reservation.JoinPolicy == JoinAny {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		projection, ready, reduceErr := reduceParallelAny(input, aggregate.View(), id)
		if reduceErr != nil {
			return nil, reduceErr
		}
		if !ready {
			continue
		}
		last, lastErr := aggregateLogicalLastSeq(projection)
		if lastErr != nil {
			return nil, lastErr
		}
		next, nextErr := advanceCheckpointV7To(input.checkpoint, projection, CurrentRunStatus(input.checkpoint), last)
		if nextErr != nil {
			return nil, nextErr
		}
		return newExecutionTransition(input.checkpoint, next, TransitionParallelAny)
	}
	return nil, ErrParallelAnyNotReady
}

func reduceParallelAny(input *VerifiedExclusiveInput, post AggregateView, reservationID ReservationID) (AggregateCheckpoint, bool, error) {
	reservation, ok := post.Routing.Reservations[reservationID]
	if !ok || reservation.State != ReservationOpen || reservation.JoinPolicy != JoinAny {
		return AggregateCheckpoint{}, false, fmt.Errorf("%w: open any reservation is required", ErrParallelInputInvalid)
	}
	fold, arrivals, leafDigest, err := activationFold(*post.Routing, reservation)
	if err != nil {
		return AggregateCheckpoint{}, false, err
	}
	_, open, failed, skipped, canceled, impossible := parallelAllCandidateKinds(*post.Routing, reservation)
	activate := len(arrivals) > 0
	if !activate && open {
		return AggregateCheckpoint{}, false, nil
	}
	if !activate && !failed && !skipped && !canceled && !impossible {
		return AggregateCheckpoint{}, false, fmt.Errorf("%w: closed any fold has no terminal authority", ErrMutationInconsistent)
	}
	if CurrentLastLogSeq(input.checkpoint) >= math.MaxInt64 {
		return AggregateCheckpoint{}, false, &OverBudgetError{Limit: "log_entries", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
	}
	eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
	before := Clone(*post.Routing)
	after := Clone(before)
	var winner PathID
	losingCandidates := []CandidateID(nil)
	preArrivedLosers := []PathID(nil)
	if activate {
		winner, losingCandidates, preArrivedLosers, err = buildParallelAnyActivation(&after, reservation, arrivals, eventSeq)
		if err != nil {
			return AggregateCheckpoint{}, false, err
		}
	} else {
		reason := ScopeCloseAllImpossible
		if failed || skipped || canceled {
			reason = ScopeCloseCandidateNonSuccess
		}
		if err := buildParallelAllClose(&after, reservation, arrivals, leafDigest, reason, eventSeq); err != nil {
			return AggregateCheckpoint{}, false, err
		}
	}
	intents := []PropagationIntent(nil)
	if !activate {
		intents, err = seedParallelAllPropagation(input, &after, reservation, eventSeq)
		if err != nil {
			return AggregateCheckpoint{}, false, err
		}
	}
	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return AggregateCheckpoint{}, false, err
	}
	if activate {
		maximum, boundErr := MutationCountAny(len(reservation.Candidates), len(preArrivedLosers))
		if boundErr != nil || len(batch.Mutations) > maximum {
			return AggregateCheckpoint{}, false, fmt.Errorf("%w: any mutation count %d exceeds %d: %v", ErrMutationInconsistent, len(batch.Mutations), maximum, boundErr)
		}
	} else {
		maximum, boundErr := MutationCountAllNonSuccess(len(arrivals), len(intents))
		if boundErr != nil || len(batch.Mutations) > maximum {
			return AggregateCheckpoint{}, false, fmt.Errorf("%w: closed any mutation count %d exceeds %d: %v", ErrMutationInconsistent, len(batch.Mutations), maximum, boundErr)
		}
	}
	current := post
	current.Routing = &before
	current.Commands = cloneMap(post.Commands)
	replayView := MutationReplayView{Aggregate: current, Checkpoint: input.binding}
	plan := ActivateGenerationPlan{
		ReservationID: reservation.ID, Generation: reservation.Generation, InputDigest: fold, CauseDigest: leafDigest,
		JoinPolicy: JoinAny, InputPathIDs: cloneSlice(arrivals), Candidates: cloneCandidates(reservation.Candidates),
		PossibleSlots: cloneSlice(reservation.PossibleSlots), Intents: intents, Batch: batch,
	}
	if activate {
		plan.InputPathIDs = []PathID{winner}
		plan.WinnerPathID = winner
		plan.LosingCandidateIDs = losingCandidates
		plan.PreArrivedLoserPathIDs = preArrivedLosers
	}
	payload, err := EncodeActivateGenerationPayload(replayView, plan)
	if err != nil {
		return AggregateCheckpoint{}, false, err
	}
	identity := CommandIdentity{
		RunID: post.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1,
		TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
		InputDigest: fold, CauseDigest: leafDigest, PlanDigest: payloadDigest(payload),
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return AggregateCheckpoint{}, false, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return AggregateCheckpoint{}, false, err
	}
	result, err := ReplayActivateGeneration(replayView, command)
	if err != nil {
		return AggregateCheckpoint{}, false, err
	}
	next := replayView.Aggregate
	next.Routing = &result.Routing
	checkpoint, err := checkpointAggregate(next)
	return checkpoint, true, err
}

func buildParallelAnyActivation(after *RoutingState, reservation ActivationReservation, arrivals []PathID, eventSeq int64) (PathID, []CandidateID, []PathID, error) {
	if len(arrivals) == 0 {
		return "", nil, nil, fmt.Errorf("%w: any activation lacks an arrival", ErrMutationInconsistent)
	}
	ordered := make([]PathRecord, 0, len(arrivals))
	for _, pathID := range arrivals {
		path, ok := after.Paths[pathID]
		if !ok || path.State != PathArrived || path.TargetReservationID != reservation.ID {
			return "", nil, nil, fmt.Errorf("%w: any arrival %q is unavailable", ErrMutationInconsistent, pathID)
		}
		path, _, err := inheritPathDetachments(after, path)
		if err != nil {
			return "", nil, nil, err
		}
		after.Paths[path.ID] = path
		ordered = append(ordered, path)
	}
	slices.SortFunc(ordered, func(a, b PathRecord) int {
		if n := cmp.Compare(a.ArrivedSeq, b.ArrivedSeq); n != 0 {
			return n
		}
		return cmp.Compare(a.ID, b.ID)
	})
	winner := ordered[0]
	inputDigest, err := InputSetIdentity([]PathID{winner.ID})
	if err != nil {
		return "", nil, nil, err
	}
	activationID, err := ActivationIdentity(reservation.RunID, reservation.ID, reservation.Generation, inputDigest)
	if err != nil {
		return "", nil, nil, err
	}
	outputID, err := ActivationOutputIdentity(activationID, reservation.Generation)
	if err != nil {
		return "", nil, nil, err
	}
	ref := ActivationRef{ID: activationID, Generation: reservation.Generation}
	outputScope, outputBranch, reduced := reservation.ScopeID, reservation.BranchEdgeID, ScopeID("")
	lineage, lineageID := []CandidateLineageFrame(nil), ""
	outputDetachments := winner.DetachmentSetID
	if reservation.IsReducing {
		scope, ok := after.Scopes[reservation.ReducesScopeID]
		forkOutput, outputOK := after.Paths[scope.ForkOutputPathID]
		if !ok || !outputOK || forkOutput.State != PathSplit || scope.State != ScopeOpen {
			return "", nil, nil, fmt.Errorf("%w: reducing any lacks exact open fork scope", ErrMutationInconsistent)
		}
		lineage = cloneSlice(forkOutput.CandidateLineage)
		lineageID = forkOutput.CandidateLineageID
		outputScope, outputBranch, reduced = scope.ParentScopeID, scope.ParentBranchEdgeID, scope.ID
		scope.State, scope.CloseReason, scope.ClosedByCommandID, scope.EventSeq = ScopeClosedActivated, ScopeCloseAny, MutationCommandPlaceholder, eventSeq
		after.Scopes[scope.ID] = scope
	} else {
		lineage, lineageID, err = PopConsumedLineage([]PathRecord{winner}, reservation.ID)
		if err != nil {
			return "", nil, nil, err
		}
	}
	winner.State, winner.ConsumedBy, winner.UpdatedSeq = PathConsumed, &ref, eventSeq
	winnerReceipt, err := DispositionReceiptIdentity(winner.ID, PathArrived, PathConsumed, "any_winner", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return "", nil, nil, err
	}
	winner.Disposition = &DispositionReceipt{ID: winnerReceipt, PathID: winner.ID, FromState: PathArrived, ToState: PathConsumed, ReasonCode: "any_winner", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
	after.Paths[winner.ID] = winner

	preArrivedByCandidate := make(map[CandidateID]PathRecord, len(ordered)-1)
	preArrivedLosers := make([]PathID, 0, len(ordered)-1)
	for _, path := range ordered[1:] {
		preArrivedByCandidate[path.CandidateID] = path
		preArrivedLosers = append(preArrivedLosers, path.ID)
	}
	slices.Sort(preArrivedLosers)
	losingCandidates := make([]CandidateID, 0, len(reservation.Candidates)-1)
	for _, candidate := range reservation.Candidates {
		if candidate.ID == winner.CandidateID {
			continue
		}
		losingCandidates = append(losingCandidates, candidate.ID)
		key, keyErr := DetachmentKeyIdentity(reservation.ID, candidate.ID)
		if keyErr != nil {
			return "", nil, nil, keyErr
		}
		detachmentID, detachErr := DetachmentIdentity(reservation.ID, candidate.ID, winner.ID, uint64(eventSeq))
		if detachErr != nil {
			return "", nil, nil, detachErr
		}
		if _, exists := after.Detachments[key]; exists {
			return "", nil, nil, fmt.Errorf("%w: any loser detachment already exists", ErrMutationInconsistent)
		}
		detachment := DetachmentRecord{
			ID: detachmentID, Key: DetachmentKeyRecord{ID: key, ReservationID: reservation.ID, CandidateID: candidate.ID},
			ReservationID: reservation.ID, CandidateID: candidate.ID, WinnerPathID: winner.ID, JoinActivation: ref,
			ReasonCode: "any_loser", CommandID: MutationCommandPlaceholder, ActivatedSeq: eventSeq, EventSeq: eventSeq, Actor: "system",
		}
		after.Detachments[key] = detachment
		parentSet := DetachmentSetID("")
		if path, ok := preArrivedByCandidate[candidate.ID]; ok {
			parentSet = path.DetachmentSetID
		}
		setID, setErr := DetachmentSetIdentity(parentSet, detachment.ID)
		if setErr != nil {
			return "", nil, nil, setErr
		}
		if _, exists := after.DetachmentSets[setID]; exists {
			return "", nil, nil, fmt.Errorf("%w: any loser detachment set already exists", ErrMutationInconsistent)
		}
		after.DetachmentSets[setID] = DetachmentSetRecord{ID: setID, ParentSetID: parentSet, DetachmentID: detachment.ID}
		if path, ok := preArrivedByCandidate[candidate.ID]; ok {
			path.State, path.UpdatedSeq, path.DetachmentSetID = PathDetachedSink, eventSeq, setID
			dispositionID, dispositionErr := DispositionReceiptIdentity(path.ID, PathArrived, PathDetachedSink, "pre_arrived_any_loser", MutationCommandPlaceholder, "", uint64(eventSeq))
			if dispositionErr != nil {
				return "", nil, nil, dispositionErr
			}
			path.Disposition = &DispositionReceipt{ID: dispositionID, PathID: path.ID, FromState: PathArrived, ToState: PathDetachedSink, ReasonCode: "pre_arrived_any_loser", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
			path.DetachedSink = &DetachedSinkReceipt{DetachmentID: detachment.ID, CommandID: MutationCommandPlaceholder, ReasonCode: "pre_arrived_any_loser", EventSeq: eventSeq}
			after.Paths[path.ID] = path
		}
	}
	slices.Sort(losingCandidates)
	after.Paths[outputID] = PathRecord{
		ID: outputID, Kind: PathActivationOutput, State: PathLive, SourceActivation: ref,
		ScopeID: outputScope, BranchEdgeID: outputBranch, CandidateLineage: lineage,
		CandidateLineageID: lineageID, LineageDepth: uint32(len(lineage)), DetachmentSetID: outputDetachments,
		CreatedSeq: eventSeq, UpdatedSeq: eventSeq,
	}
	receiptID, err := ActivationReceiptIdentity(activationID, reservation.ID, inputDigest, outputID, MutationCommandPlaceholder, uint64(eventSeq))
	if err != nil {
		return "", nil, nil, err
	}
	receipt := ActivationReceipt{
		ID: receiptID, ActivationID: activationID, ReservationID: reservation.ID, InputSetDigest: inputDigest,
		OutputPathID: outputID, ScopeID: outputScope, BranchEdgeID: outputBranch, ReducedScopeID: reduced,
		JoinPolicy: JoinAny, Result: ReceiptActivated, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	after.Activations[activationID] = ActivationRecord{
		ID: activationID, RunID: reservation.RunID, Ref: ref, ReservationID: reservation.ID,
		InputPathIDs: []PathID{winner.ID}, InputSetDigest: inputDigest, OutputPathID: outputID,
		Receipt: receipt, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	updated := after.Reservations[reservation.ID]
	updated.State, updated.Activation, updated.CloseReceipt, updated.ClosedReason, updated.CauseDigest, updated.CommandID, updated.EventSeq = ReservationActivated, &ref, nil, "", "", MutationCommandPlaceholder, eventSeq
	after.Reservations[updated.ID] = updated
	return winner.ID, losingCandidates, preArrivedLosers, nil
}

// AdvanceParallelDetachedSink settles the first pending observation whose
// selected edge returns a detached candidate to its already-closed any
// reservation. The arrival sequence and sink disposition are one event and
// the route produces no runnable output.
func AdvanceParallelDetachedSink(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	observation, found, err := PendingExclusiveObservation(ctx, input)
	if err != nil {
		return nil, err
	}
	if !found || input == nil || input.parallel == nil {
		return nil, ErrParallelDetachedSinkNotReady
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	view := aggregate.View()
	source, ok := view.Routing.Paths[observation.SourcePathID]
	if !ok || source.Kind != PathActivationOutput || source.State != PathLive {
		return nil, fmt.Errorf("%w: detached sink source is not live", ErrMutationInconsistent)
	}
	activation, ok := view.Routing.Activations[source.SourceActivation.ID]
	if !ok || activation.Ref != source.SourceActivation {
		return nil, fmt.Errorf("%w: detached sink source activation is absent", ErrMutationInconsistent)
	}
	sourceReservation := view.Routing.Reservations[activation.ReservationID]
	node, ok := input.template.Nodes[sourceReservation.NodeID]
	if !ok {
		return nil, fmt.Errorf("%w: detached sink source node is absent", ErrParallelInputInvalid)
	}
	observation.Outcome, err = canonicalExclusiveOutcome(node, observation.Outcome)
	if err != nil {
		return nil, err
	}
	if disposition, classifyErr := classifyExclusiveObservation(view, input.template, observation, true); classifyErr != nil || disposition != ExclusiveRouteReady {
		if classifyErr != nil {
			return nil, classifyErr
		}
		return nil, ErrParallelDetachedSinkNotReady
	}
	outgoing, err := exactOutgoingEdges(view.TemplateRef, sourceReservation.NodeID, node.Next)
	if err != nil {
		return nil, err
	}
	selectedIndex, err := resolveExclusiveEdge(node, observation.Outcome, outgoing)
	if err != nil {
		return nil, err
	}
	edge := outgoing[selectedIndex]
	var target ActivationReservation
	var candidate CandidateRecord
	for _, reservationID := range sortedMapKeys(view.Routing.Reservations) {
		reservation := view.Routing.Reservations[reservationID]
		if reservation.NodeID != edge.ToNodeID || reservation.ScopeID != source.ScopeID || reservation.JoinPolicy != JoinAny || reservation.State != ReservationActivated {
			continue
		}
		candidateValue, candidateOK := routeCandidateForEdge(reservation, edge.ID, source.BranchEdgeID)
		if !candidateOK {
			continue
		}
		detached, detachedErr := DetachedFrom(view.Routing, source, reservation.ID)
		if detachedErr != nil {
			return nil, detachedErr
		}
		if !detached {
			continue
		}
		target, candidate = reservation, candidateValue
		break
	}
	if target.ID == "" {
		return nil, ErrParallelDetachedSinkNotReady
	}
	detachmentKey, err := DetachmentKeyIdentity(target.ID, candidate.ID)
	if err != nil {
		return nil, err
	}
	detachment, ok := view.Routing.Detachments[detachmentKey]
	if !ok {
		return nil, fmt.Errorf("%w: closed any candidate lacks detachment authority", ErrMutationInconsistent)
	}
	if CurrentLastLogSeq(input.checkpoint) >= math.MaxInt64 {
		return nil, &OverBudgetError{Limit: "log_entries", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
	}
	eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
	before := Clone(*view.Routing)
	after := Clone(before)
	parent, _, err := inheritPathDetachments(&after, source)
	if err != nil {
		return nil, err
	}
	if !detachmentSetContainsExact(after, parent.DetachmentSetID, detachment.ID) {
		return nil, fmt.Errorf("%w: inherited path does not contain exact any detachment", ErrMutationInconsistent)
	}
	lineage, lineageID, err := AppendCandidateLineage(parent, target.ID, candidate.ID)
	if err != nil {
		return nil, err
	}
	pathID, err := EdgePathIdentity(parent.SourceActivation.ID, parent.ID, edge.ID, target.ID, candidate.ID)
	if err != nil {
		return nil, err
	}
	if _, exists := after.Paths[pathID]; exists {
		return nil, fmt.Errorf("%w: detached sink path already exists", ErrMutationInconsistent)
	}
	arrivalID, err := ArrivalIdentity(pathID, target.ID, candidate.ID)
	if err != nil {
		return nil, err
	}
	parent.State, parent.ProducedPathIDs, parent.UpdatedSeq = PathRouted, []PathID{pathID}, eventSeq
	parentReceipt, err := DispositionReceiptIdentity(parent.ID, PathLive, PathRouted, "late_detached_route", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return nil, err
	}
	parent.Disposition = &DispositionReceipt{ID: parentReceipt, PathID: parent.ID, FromState: PathLive, ToState: PathRouted, ReasonCode: "late_detached_route", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
	after.Paths[parent.ID] = parent
	sinkReceipt, err := DispositionReceiptIdentity(pathID, PathArrived, PathDetachedSink, "late_any_arrival", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return nil, err
	}
	after.Paths[pathID] = PathRecord{
		ID: pathID, Kind: PathEdge, State: PathDetachedSink, ParentPathID: parent.ID,
		SourceActivation: parent.SourceActivation, Edge: cloneEdge(&edge), TargetReservationID: target.ID,
		CandidateID: candidate.ID, ScopeID: parent.ScopeID, BranchEdgeID: parent.BranchEdgeID,
		CandidateLineage: lineage, CandidateLineageID: lineageID, LineageDepth: uint32(len(lineage)),
		ArrivalID: arrivalID, ArrivedSeq: eventSeq, DetachmentSetID: parent.DetachmentSetID,
		Disposition:  &DispositionReceipt{ID: sinkReceipt, PathID: pathID, FromState: PathArrived, ToState: PathDetachedSink, ReasonCode: "late_any_arrival", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq},
		DetachedSink: &DetachedSinkReceipt{DetachmentID: detachment.ID, CommandID: MutationCommandPlaceholder, ReasonCode: "late_any_arrival", EventSeq: eventSeq},
		CreatedSeq:   eventSeq, UpdatedSeq: eventSeq,
	}
	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return nil, err
	}
	current := view
	current.Commands = cloneMap(view.Commands)
	current.SideEffects = cloneMap(view.SideEffects)
	perform, settle, effect, err := observedAttemptCommands(current, sourceReservation.NodeID, node, source, observation, false)
	if err != nil {
		return nil, err
	}
	if err := insertExactCommand(current.Commands, perform); err != nil {
		return nil, err
	}
	if err := insertExactCommand(current.Commands, settle); err != nil {
		return nil, err
	}
	current.SideEffects[effect.ID] = effect
	current.Routing = &before
	replayView := MutationReplayView{Aggregate: current, Checkpoint: input.binding}
	emptyCause, err := CauseSetIdentity(nil)
	if err != nil {
		return nil, err
	}
	plan := SettleDetachedSinkPlan{
		SettlementCommandID: settle.ID, SourceActivationID: source.SourceActivation.ID,
		SourceGeneration: source.SourceActivation.Generation, SourceAttempt: observation.Attempt,
		SettlementResultCode: settle.Identity.ResultCode,
		SourcePathID:         pathID, ReservationID: target.ID, Generation: target.Generation,
		DetachmentSetID: parent.DetachmentSetID, DetachmentID: detachment.ID,
		CauseDigest: emptyCause, ResultCode: "detached", Batch: batch,
	}
	payload, err := EncodeSettleDetachedSinkPayload(replayView, plan)
	if err != nil {
		return nil, err
	}
	identity := CommandIdentity{
		RunID: view.RunID, Kind: CommandSettleDetachedSink, PayloadSchema: 1,
		SourceActivationID: source.SourceActivation.ID, SourceGeneration: source.SourceActivation.Generation, Attempt: observation.Attempt,
		SourcePathID: pathID, TargetReservationID: target.ID, TargetGeneration: target.Generation,
		InputDigest: settle.ID, CauseDigest: emptyCause, PlanDigest: payloadDigest(payload), ResultCode: "detached",
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return nil, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return nil, err
	}
	result, err := ReplaySettleDetachedSink(replayView, command)
	if err != nil {
		return nil, err
	}
	nextView := replayView.Aggregate
	nextView.Routing = &result.Routing
	projected, err := checkpointAggregate(nextView)
	if err != nil {
		return nil, err
	}
	last, err := aggregateLogicalLastSeq(projected)
	if err != nil {
		return nil, err
	}
	next, err := advanceCheckpointV7To(input.checkpoint, projected, CurrentRunStatus(input.checkpoint), last)
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, TransitionParallelDetachedSink)
}
