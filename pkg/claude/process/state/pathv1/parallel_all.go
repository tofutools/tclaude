package pathv1

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

var ErrParallelAllNotReady = errors.New("path-v1 parallel all is not ready")

// AdvanceParallelAll installs the first ready all-reservation in stable ID
// order. The complete fold and all emitted records come only from the exact
// checkpoint; callers cannot select a partial candidate subset.
func AdvanceParallelAll(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
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
	ids := make([]ReservationID, 0)
	for id, reservation := range aggregate.Routing.Reservations {
		if reservation.State == ReservationOpen && reservation.JoinPolicy == JoinAll {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		projection, ready, reduceErr := reduceParallelAll(input, aggregate.View(), id)
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
		return newExecutionTransition(input.checkpoint, next, "parallel_all")
	}
	return nil, ErrParallelAllNotReady
}

// AdvanceParallelRoute folds one durable observation through its exclusive
// edge choice and local candidate closures. A selected all reservation stays
// open until every candidate is settled; an exclusive target activates in the
// same bounded checkpoint event sequence.
func AdvanceParallelRoute(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	observation, found, err := PendingExclusiveObservation(ctx, input)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("%w: no pending durable observation is routable", ErrExclusiveNotRoutable)
	}
	route, err := buildExclusiveRouteDraft(ctx, input, observation)
	if err != nil {
		return nil, err
	}
	routed, err := ReplayRoutePaths(route.view, route.command)
	if err != nil {
		return nil, err
	}
	post := route.view.Aggregate
	post.Routing = &routed.Routing
	parent := post.Routing.Paths[route.sourcePathID]
	var selected PathRecord
	losers := make([]PathRecord, 0)
	for _, childID := range parent.ProducedPathIDs {
		child := post.Routing.Paths[childID]
		if child.Kind == PathEdge && child.State == PathArrived {
			selected = child
		} else if child.Kind == PathImpossibleEdge && child.State == PathImpossible {
			losers = append(losers, child)
		}
	}
	if selected.ID == "" {
		return nil, fmt.Errorf("%w: parallel route has no selected arrival", ErrMutationInconsistent)
	}
	slices.SortFunc(losers, func(a, b PathRecord) int { return strings.Compare(a.ID, b.ID) })
	eventSeq := route.eventSeq
	for _, loser := range losers {
		eventSeq++
		_, next, closeErr := buildExclusiveSequenceClosure(input.binding, post, loser, eventSeq)
		if closeErr != nil {
			return nil, closeErr
		}
		post = next
	}
	selectedReservation := post.Routing.Reservations[selected.TargetReservationID]
	if selectedReservation.JoinPolicy == JoinExclusive {
		eventSeq++
		activation, next, activateErr := buildExclusiveSequenceActivation(input.binding, post, selected, eventSeq)
		if activateErr != nil {
			return nil, activateErr
		}
		post = next
		if node := input.template.Nodes[selectedReservation.NodeID]; node.Type == "end" {
			eventSeq++
			_, post, err = buildExclusiveSequenceEnd(input, post, activation, eventSeq)
			if err != nil {
				return nil, err
			}
		}
	}
	if report := ValidateAggregate(post); !report.Valid() {
		return nil, fmt.Errorf("%w: parallel route aggregate has %d diagnostics", ErrMutationInconsistent, len(report.Diagnostics)+report.Suppressed)
	}
	aggregate, err := checkpointAggregate(post)
	if err != nil {
		return nil, err
	}
	last, err := aggregateLogicalLastSeq(aggregate)
	if err != nil {
		return nil, err
	}
	next, err := advanceCheckpointV7To(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint), last)
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, "parallel_route")
}

// AdvanceParallelExclusiveArrival activates the first fully settled exclusive
// reservation created by a split or a preceding branch route.
func AdvanceParallelExclusiveArrival(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.parallel == nil {
		return nil, fmt.Errorf("%w: verified parallel input is required", ErrParallelInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	ids := make([]ReservationID, 0)
	for id, reservation := range aggregate.Routing.Reservations {
		if reservation.State == ReservationOpen && reservation.JoinPolicy == JoinExclusive {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		reservation := aggregate.Routing.Reservations[id]
		_, arrivals, _, foldErr := activationFold(aggregate.Routing, reservation)
		if foldErr != nil {
			return nil, foldErr
		}
		if len(arrivals) != 1 {
			continue
		}
		ready := true
		for _, candidate := range reservation.Candidates {
			if candidate.ID == aggregate.Routing.Paths[arrivals[0]].CandidateID {
				continue
			}
			key, keyErr := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
			if keyErr != nil {
				return nil, keyErr
			}
			if _, exists := aggregate.Routing.CandidateClosures[key]; !exists {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
		selected := aggregate.Routing.Paths[arrivals[0]]
		activation, post, activateErr := buildExclusiveSequenceActivation(input.binding, aggregate.View(), selected, eventSeq)
		if activateErr != nil {
			return nil, activateErr
		}
		if node := input.template.Nodes[reservation.NodeID]; node.Type == "end" {
			eventSeq++
			_, post, err = buildExclusiveSequenceEnd(input, post, activation, eventSeq)
			if err != nil {
				return nil, err
			}
		}
		projected, projectErr := checkpointAggregate(post)
		if projectErr != nil {
			return nil, projectErr
		}
		last, lastErr := aggregateLogicalLastSeq(projected)
		if lastErr != nil {
			return nil, lastErr
		}
		next, nextErr := advanceCheckpointV7To(input.checkpoint, projected, CurrentRunStatus(input.checkpoint), last)
		if nextErr != nil {
			return nil, nextErr
		}
		return newExecutionTransition(input.checkpoint, next, "parallel_exclusive_arrival")
	}
	return nil, ErrParallelAllNotReady
}

// AdvanceParallelEnd settles one live end-node output selected from a
// multi-path checkpoint.
func AdvanceParallelEnd(ctx context.Context, input *VerifiedExclusiveInput, sourcePathID PathID) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.checkpoint == nil || input.template == nil || input.parallel == nil {
		return nil, fmt.Errorf("%w: verified parallel input is required", ErrParallelInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	path, ok := aggregate.Routing.Paths[sourcePathID]
	if !ok || path.Kind != PathActivationOutput || path.State != PathLive {
		return nil, fmt.Errorf("%w: live end output is required", ErrExclusiveUnsupported)
	}
	activation := aggregate.Routing.Activations[path.SourceActivation.ID]
	reservation := aggregate.Routing.Reservations[activation.ReservationID]
	if input.template.Nodes[reservation.NodeID].Type != "end" {
		return nil, ErrExclusiveUnsupported
	}
	eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
	authority := CommandRecord{Identity: CommandIdentity{TargetReservationID: reservation.ID}}
	_, post, err := buildExclusiveSequenceEnd(input, aggregate.View(), authority, eventSeq)
	if err != nil {
		return nil, err
	}
	projected, err := checkpointAggregate(post)
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
	return newExecutionTransition(input.checkpoint, next, "parallel_end")
}

// AdvanceParallelPropagation advances exactly one immutable frontier entry.
// An unproved slot remains pending; absence is never converted to impossible.
func AdvanceParallelPropagation(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.parallel == nil {
		return nil, fmt.Errorf("%w: verified parallel input is required", ErrParallelInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	if seeded, seedErr := advanceParallelPropagationSeed(ctx, input, aggregate); seedErr == nil {
		return seeded, nil
	} else if !errors.Is(seedErr, ErrParallelAllNotReady) {
		return nil, seedErr
	}
	ids := make([]PropagationIntentID, 0)
	for id, intent := range aggregate.Routing.Propagation {
		if intent.State == PropagationPending {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		intent := aggregate.Routing.Propagation[id]
		if int(intent.Cursor) >= len(intent.Frontier) {
			return nil, fmt.Errorf("%w: pending propagation cursor is exhausted", ErrMutationInconsistent)
		}
		key := intent.Frontier[intent.Cursor]
		reservation, candidate, found := candidateForClosureKey(aggregate.Routing, key)
		if !found {
			return nil, fmt.Errorf("%w: propagation frontier candidate is absent", ErrMutationInconsistent)
		}
		entry, causes, terminal, foldErr := foldCandidateFromAggregate(aggregate.View(), reservation, candidate)
		if foldErr != nil {
			return nil, foldErr
		}
		if entry.FoldKind == CandidateFoldOpen {
			continue
		}
		eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
		before := Clone(aggregate.Routing)
		after := Clone(before)
		planIntents := []PropagationIntent(nil)
		if entry.FoldKind != "arrived" {
			causeDigest, digestErr := CauseSetIdentity(causes)
			if digestErr != nil {
				return nil, digestErr
			}
			after.CandidateClosures[key] = CandidateClosure{ID: entry.PathOrClosureID, Key: CandidateClosureKeyRecord{ID: key, ReservationID: reservation.ID, CandidateID: candidate.ID}, TerminalKind: terminal, CauseDigest: causeDigest, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
			_, arrivals, leafDigest, foldErr := activationFold(after, reservation)
			if foldErr != nil {
				return nil, foldErr
			}
			_, open, failed, skipped, canceled, impossible := parallelAllCandidateKinds(after, reservation)
			if reservation.JoinPolicy == JoinExclusive && !open && (failed || skipped || canceled || impossible) {
				reason := ScopeCloseCandidateNonSuccess
				if !failed && !skipped && !canceled {
					reason = ScopeCloseAllImpossible
				}
				if err := buildParallelAllClose(&after, reservation, arrivals, leafDigest, reason, eventSeq); err != nil {
					return nil, err
				}
			}
		}
		advanced := after.Propagation[id]
		advanced.Cursor++
		advanced.EventSeq = eventSeq
		if int(advanced.Cursor) == len(advanced.Frontier) {
			advanced.State = PropagationComplete
		}
		after.Propagation[id] = advanced
		planIntents = append(planIntents, advanced)
		planIntents, err = normalizePropagationIntents(planIntents)
		if err != nil {
			return nil, err
		}
		batch, batchErr := NewMutationBatch(&before, &after, eventSeq)
		if batchErr != nil {
			return nil, batchErr
		}
		current := aggregate.View()
		current.Routing = &before
		current.Commands = cloneMap(aggregate.Commands)
		replayView := MutationReplayView{Aggregate: current, Checkpoint: input.binding}
		plan := PropagateClosurePlan{TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation, InputDigest: intent.PlanDigest, CauseDigest: intent.RootCauseDigest, RootReservationID: intent.RootReservationID, RootCandidateID: intent.RootCandidateID, RootCauseDigest: intent.RootCauseDigest, Intents: planIntents, Batch: batch}
		payload, payloadErr := EncodePropagateClosurePayload(replayView, plan)
		if payloadErr != nil {
			return nil, payloadErr
		}
		identity := CommandIdentity{RunID: aggregate.RunID, Kind: CommandPropagateCandidateClosure, PayloadSchema: 1, TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation, InputDigest: intent.PlanDigest, CauseDigest: intent.RootCauseDigest, PlanDigest: payloadDigest(payload)}
		command, commandErr := observedCommand(identity, payload)
		if commandErr != nil {
			return nil, commandErr
		}
		if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
			return nil, err
		}
		result, replayErr := ReplayPropagateClosure(replayView, command)
		if replayErr != nil {
			return nil, replayErr
		}
		nextView := replayView.Aggregate
		nextView.Routing = &result.Routing
		projected, projectErr := checkpointAggregate(nextView)
		if projectErr != nil {
			return nil, projectErr
		}
		last, lastErr := aggregateLogicalLastSeq(projected)
		if lastErr != nil {
			return nil, lastErr
		}
		next, nextErr := advanceCheckpointV7To(input.checkpoint, projected, CurrentRunStatus(input.checkpoint), last)
		if nextErr != nil {
			return nil, nextErr
		}
		return newExecutionTransition(input.checkpoint, next, "parallel_propagation")
	}
	return nil, ErrParallelAllNotReady
}

func advanceParallelPropagationSeed(ctx context.Context, input *VerifiedExclusiveInput, aggregate AggregateCheckpoint) (*ExecutionTransition, error) {
	ids := make([]ReservationID, 0)
	for id, reservation := range aggregate.Routing.Reservations {
		if reservation.State == ReservationClosedNoActivation && reservation.CauseDigest != "" {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reservation := aggregate.Routing.Reservations[id]
		alreadySeeded := false
		for _, intent := range aggregate.Routing.Propagation {
			if intent.RootReservationID == reservation.ID && intent.RootCauseDigest == reservation.CauseDigest {
				alreadySeeded = true
				break
			}
		}
		if alreadySeeded {
			continue
		}
		edges := make([]EdgeKey, 0)
		for _, edge := range input.parallel.edges {
			if edge.FromNodeID == reservation.NodeID {
				edges = append(edges, edge)
			}
		}
		if len(edges) == 0 {
			continue
		}
		if input.template.Nodes[reservation.NodeID].Type == "parallel" {
			return nil, fmt.Errorf("%w: terminal propagation through an unactivated nested parallel fork is not enabled", ErrParallelUnsupported)
		}
		slices.SortFunc(edges, compareParallelEdgeTuple)
		eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
		before := Clone(aggregate.Routing)
		after := Clone(before)
		view := aggregate.View()
		authority := cloneExclusiveAuthority(view.Authority)
		outputScope, outputBranch := reservation.ScopeID, reservation.BranchEdgeID
		if reservation.IsReducing {
			scope := after.Scopes[reservation.ReducesScopeID]
			outputScope, outputBranch = scope.ParentScopeID, scope.ParentBranchEdgeID
		}
		pseudoSource := PathRecord{ScopeID: outputScope, BranchEdgeID: outputBranch}
		for _, edge := range edges {
			target, created, routeErr := exactRouteReservation(input, view, authority, after, pseudoSource, edge, eventSeq)
			if routeErr != nil {
				return nil, routeErr
			}
			if created {
				after.Reservations[target.ID] = target
				authority.Reservations[target.ID] = reservationAuthority(target)
			}
		}
		intents, seedErr := seedParallelAllPropagation(input, &after, reservation, eventSeq)
		if seedErr != nil {
			return nil, seedErr
		}
		if len(intents) == 0 {
			continue
		}
		intents, err := normalizePropagationIntents(intents)
		if err != nil {
			return nil, err
		}
		firstReservation, _, found := candidateForClosureKey(after, intents[0].Frontier[0])
		if !found {
			return nil, fmt.Errorf("%w: seeded propagation target is absent", ErrMutationInconsistent)
		}
		batch, batchErr := NewMutationBatch(&before, &after, eventSeq)
		if batchErr != nil {
			return nil, batchErr
		}
		current := aggregate.View()
		current.Authority, current.Routing = authority, &before
		current.Commands = cloneMap(aggregate.Commands)
		replayView := MutationReplayView{Aggregate: current, Checkpoint: input.binding}
		plan := PropagateClosurePlan{TargetReservationID: firstReservation.ID, TargetGeneration: firstReservation.Generation, InputDigest: intents[0].PlanDigest, CauseDigest: reservation.CauseDigest, RootReservationID: reservation.ID, RootCandidateID: intents[0].RootCandidateID, RootCauseDigest: reservation.CauseDigest, Intents: intents, Batch: batch}
		payload, payloadErr := EncodePropagateClosurePayload(replayView, plan)
		if payloadErr != nil {
			return nil, payloadErr
		}
		identity := CommandIdentity{RunID: aggregate.RunID, Kind: CommandPropagateCandidateClosure, PayloadSchema: 1, TargetReservationID: firstReservation.ID, TargetGeneration: firstReservation.Generation, InputDigest: plan.InputDigest, CauseDigest: reservation.CauseDigest, PlanDigest: payloadDigest(payload)}
		command, commandErr := observedCommand(identity, payload)
		if commandErr != nil {
			return nil, commandErr
		}
		if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
			return nil, err
		}
		result, replayErr := ReplayPropagateClosure(replayView, command)
		if replayErr != nil {
			return nil, replayErr
		}
		nextView := replayView.Aggregate
		nextView.Routing = &result.Routing
		projected, projectErr := checkpointAggregate(nextView)
		if projectErr != nil {
			return nil, projectErr
		}
		last, lastErr := aggregateLogicalLastSeq(projected)
		if lastErr != nil {
			return nil, lastErr
		}
		next, nextErr := advanceCheckpointV7To(input.checkpoint, projected, CurrentRunStatus(input.checkpoint), last)
		if nextErr != nil {
			return nil, nextErr
		}
		return newExecutionTransition(input.checkpoint, next, "parallel_propagation_seed")
	}
	return nil, ErrParallelAllNotReady
}

// AdvanceParallelTerminalClosure turns one settled terminal branch output
// into the complete authoritative downstream candidate-closure frontier. It
// never interprets an absent slot as terminal; reservations absent because no
// successful path ever routed to them are materialized from exact topology in
// the same indivisible propagation batch.
func AdvanceParallelTerminalClosure(ctx context.Context, input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.parallel == nil {
		return nil, fmt.Errorf("%w: verified parallel input is required", ErrParallelInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	terminalIDs := make([]PathID, 0)
	for id, path := range aggregate.Routing.Paths {
		if path.Kind == PathActivationOutput && path.State.TerminalNonSuccess() {
			terminalIDs = append(terminalIDs, id)
		}
	}
	slices.Sort(terminalIDs)
	for _, pathID := range terminalIDs {
		path := aggregate.Routing.Paths[pathID]
		cause, ok := aggregate.Routing.CauseRecords[path.TerminalCauseID]
		if !ok {
			return nil, fmt.Errorf("%w: terminal branch cause is absent", ErrMutationInconsistent)
		}
		causeDigest, digestErr := CauseSetIdentity([]CauseID{cause.ID})
		if digestErr != nil {
			return nil, digestErr
		}
		activation := aggregate.Routing.Activations[path.SourceActivation.ID]
		source := aggregate.Routing.Reservations[activation.ReservationID]
		rootCandidate := CandidateID("")
		for _, inputPathID := range activation.InputPathIDs {
			if candidateID := aggregate.Routing.Paths[inputPathID].CandidateID; candidateID != "" && (rootCandidate == "" || candidateID < rootCandidate) {
				rootCandidate = candidateID
			}
		}
		if rootCandidate == "" {
			continue
		}
		eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
		before := Clone(aggregate.Routing)
		after := Clone(before)
		view := aggregate.View()
		authority := cloneExclusiveAuthority(view.Authority)
		edges := make([]EdgeKey, 0)
		for _, edge := range input.parallel.edges {
			if edge.FromNodeID == source.NodeID {
				edges = append(edges, edge)
			}
		}
		slices.SortFunc(edges, compareParallelEdgeTuple)
		targets := make([]propagationFrontierTarget, 0, len(edges))
		for _, edge := range edges {
			reservation, created, routeErr := exactRouteReservation(input, view, authority, after, path, edge, eventSeq)
			if routeErr != nil {
				return nil, routeErr
			}
			if created {
				after.Reservations[reservation.ID] = reservation
				authority.Reservations[reservation.ID] = reservationAuthority(reservation)
			}
			candidate, found := routeCandidateForEdge(reservation, edge.ID, path.BranchEdgeID)
			if !found {
				return nil, fmt.Errorf("%w: terminal propagation target lacks exact edge candidate", ErrParallelInputInvalid)
			}
			key, keyErr := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
			if keyErr != nil {
				return nil, keyErr
			}
			if _, exists := after.CandidateClosures[key]; exists {
				continue
			}
			seeded := false
			for _, intent := range after.Propagation {
				if intent.RootReservationID == source.ID && intent.RootCandidateID == rootCandidate && intent.RootCauseDigest == causeDigest && slices.Contains(intent.Frontier, key) {
					seeded = true
					break
				}
			}
			if seeded {
				continue
			}
			foldView := view
			foldView.Authority, foldView.Routing = authority, &after
			entry, causes, _, foldErr := foldCandidateFromAggregate(foldView, reservation, candidate)
			if foldErr != nil {
				return nil, foldErr
			}
			if entry.FoldKind == CandidateFoldOpen || entry.FoldKind == "arrived" {
				if created {
					delete(after.Reservations, reservation.ID)
					delete(authority.Reservations, reservation.ID)
				}
				continue
			}
			closureDigest, closureErr := CauseSetIdentity(causes)
			if closureErr != nil || closureDigest != causeDigest {
				return nil, fmt.Errorf("%w: terminal propagation cause union drift", ErrMutationInconsistent)
			}
			targets = append(targets, propagationFrontierTarget{nodeID: edge.ToNodeID, reservationID: reservation.ID, candidateID: candidate.ID, key: key})
		}
		if len(targets) == 0 {
			continue
		}
		slices.SortFunc(targets, func(a, b propagationFrontierTarget) int { return strings.Compare(a.key, b.key) })
		frontier := make([]CandidateClosureKey, 0, len(targets))
		for _, target := range targets {
			if len(frontier) == 0 || frontier[len(frontier)-1] != target.key {
				frontier = append(frontier, target.key)
			}
		}
		if len(frontier) > MaxRoutingList {
			return nil, &OverBudgetError{Limit: "propagation_frontier", Value: len(frontier), Maximum: MaxRoutingList}
		}
		shard, shardErr := nextPropagationShard(after, causeDigest)
		if shardErr != nil {
			return nil, shardErr
		}
		planDigest, planErr := PropagationPlanIdentity(source.ID, rootCandidate, causeDigest, uint64(shard), frontier)
		if planErr != nil {
			return nil, planErr
		}
		intentID, intentErr := PropagationIntentIdentity(causeDigest, uint64(shard), planDigest)
		if intentErr != nil {
			return nil, intentErr
		}
		intent := PropagationIntent{ID: intentID, RootReservationID: source.ID, RootCandidateID: rootCandidate, RootCauseDigest: causeDigest, Shard: shard, Cursor: 0, Frontier: frontier, PlanDigest: planDigest, State: PropagationPending, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
		after.Propagation[intent.ID] = intent
		batch, batchErr := NewMutationBatch(&before, &after, eventSeq)
		if batchErr != nil {
			return nil, batchErr
		}
		first := targets[0]
		firstReservation := after.Reservations[first.reservationID]
		current := aggregate.View()
		current.Authority, current.Routing = authority, &before
		current.Commands = cloneMap(aggregate.Commands)
		replayView := MutationReplayView{Aggregate: current, Checkpoint: input.binding}
		plan := PropagateClosurePlan{SourcePathID: path.ID, TargetReservationID: first.reservationID, TargetGeneration: firstReservation.Generation, InputDigest: planDigest, CauseDigest: causeDigest, RootReservationID: source.ID, RootCandidateID: rootCandidate, RootCauseDigest: causeDigest, Intents: []PropagationIntent{intent}, Batch: batch}
		payload, payloadErr := EncodePropagateClosurePayload(replayView, plan)
		if payloadErr != nil {
			return nil, payloadErr
		}
		identity := CommandIdentity{RunID: aggregate.RunID, Kind: CommandPropagateCandidateClosure, PayloadSchema: 1, SourcePathID: path.ID, TargetReservationID: first.reservationID, TargetGeneration: firstReservation.Generation, InputDigest: planDigest, CauseDigest: causeDigest, PlanDigest: payloadDigest(payload)}
		command, commandErr := observedCommand(identity, payload)
		if commandErr != nil {
			return nil, commandErr
		}
		if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
			return nil, err
		}
		result, replayErr := ReplayPropagateClosure(replayView, command)
		if replayErr != nil {
			return nil, replayErr
		}
		nextView := replayView.Aggregate
		nextView.Routing = &result.Routing
		projected, projectErr := checkpointAggregate(nextView)
		if projectErr != nil {
			return nil, projectErr
		}
		last, lastErr := aggregateLogicalLastSeq(projected)
		if lastErr != nil {
			return nil, lastErr
		}
		next, nextErr := advanceCheckpointV7To(input.checkpoint, projected, CurrentRunStatus(input.checkpoint), last)
		if nextErr != nil {
			return nil, nextErr
		}
		return newExecutionTransition(input.checkpoint, next, "parallel_terminal_closure")
	}
	return nil, ErrParallelAllNotReady
}

func candidateForClosureKey(routing RoutingState, key CandidateClosureKey) (ActivationReservation, CandidateRecord, bool) {
	for _, reservation := range routing.Reservations {
		for _, candidate := range reservation.Candidates {
			candidateKey, err := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
			if err == nil && candidateKey == key {
				return reservation, candidate, true
			}
		}
	}
	return ActivationReservation{}, CandidateRecord{}, false
}

func foldCandidateFromAggregate(view AggregateView, reservation ActivationReservation, candidate CandidateRecord) (CandidateFoldEntry, []CauseID, TerminalKind, error) {
	report := InvariantReport{}
	index := &aggregateIndex{
		view: view, c: diagnosticCollector{report: &report},
		candidates: map[candidateKey]CandidateRecord{}, candidateByClosureKey: map[CandidateClosureKey]candidateKey{}, slots: map[PossibleSlotID]PossibleSlotRecord{},
		pathsBySlot: map[PossibleSlotID][]PathID{}, pathsByTarget: map[candidateKey][]PathID{}, openDescendants: map[candidateKey]bool{}, outputs: map[ActivationID]PathID{}, forkScopeByOutput: map[PathID]ScopeID{},
	}
	index.indexReservations()
	index.indexPaths()
	settled := make(map[PossibleSlotID]SlotSettlement, len(candidate.PossibleSlotIDs))
	for _, slotID := range candidate.PossibleSlotIDs {
		slot, ok := index.slots[slotID]
		if !ok {
			return CandidateFoldEntry{}, nil, "", fmt.Errorf("%w: candidate slot is absent", ErrMutationInconsistent)
		}
		if value, ok := index.slotSettlement(slot); ok {
			settled[slotID] = value
		}
	}
	return FoldCandidateSlots(reservation.ID, candidate, settled, index.openDescendants[candidateKey{reservation.ID, candidate.ID}])
}

func reduceParallelAll(input *VerifiedExclusiveInput, post AggregateView, reservationID ReservationID) (AggregateCheckpoint, bool, error) {
	reservation, ok := post.Routing.Reservations[reservationID]
	if !ok || reservation.State != ReservationOpen || reservation.JoinPolicy != JoinAll {
		return AggregateCheckpoint{}, false, fmt.Errorf("%w: open all reservation is required", ErrParallelInputInvalid)
	}
	fold, arrivals, leafDigest, err := activationFold(*post.Routing, reservation)
	if err != nil {
		return AggregateCheckpoint{}, false, err
	}
	arrived, open, failed, skipped, canceled, impossible := parallelAllCandidateKinds(*post.Routing, reservation)
	if open {
		return AggregateCheckpoint{}, false, nil
	}
	activate := arrived && !failed && !skipped && !canceled
	if !activate && !impossible && !failed && !skipped && !canceled {
		return AggregateCheckpoint{}, false, fmt.Errorf("%w: closed all fold has no terminal authority", ErrMutationInconsistent)
	}
	if CurrentLastLogSeq(input.checkpoint) >= math.MaxInt64 {
		return AggregateCheckpoint{}, false, &OverBudgetError{Limit: "log_entries", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
	}
	eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
	before := Clone(*post.Routing)
	after := Clone(before)
	if activate {
		if _, err := MutationCountAllActivate(len(reservation.Candidates)); err != nil {
			return AggregateCheckpoint{}, false, err
		}
		if err := buildParallelAllActivation(&after, reservation, arrivals, eventSeq); err != nil {
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
	var maximum int
	if !activate {
		maximum, err = MutationCountAllNonSuccess(len(arrivals), len(intents))
	} else {
		maximum, err = MutationCountAllActivate(len(reservation.Candidates))
	}
	if err != nil || len(batch.Mutations) > maximum {
		return AggregateCheckpoint{}, false, fmt.Errorf("%w: all mutation count %d exceeds exact bound %d", ErrMutationInconsistent, len(batch.Mutations), maximum)
	}
	current := post
	current.Routing = &before
	current.Commands = cloneMap(post.Commands)
	replayView := MutationReplayView{Aggregate: current, Checkpoint: input.binding}
	plan := ActivateGenerationPlan{ReservationID: reservation.ID, Generation: reservation.Generation, InputDigest: fold, CauseDigest: leafDigest, JoinPolicy: JoinAll, InputPathIDs: cloneSlice(arrivals), Candidates: cloneCandidates(reservation.Candidates), PossibleSlots: cloneSlice(reservation.PossibleSlots), Intents: intents, Batch: batch}
	payload, err := EncodeActivateGenerationPayload(replayView, plan)
	if err != nil {
		return AggregateCheckpoint{}, false, err
	}
	identity := CommandIdentity{RunID: post.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1, TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation, InputDigest: fold, CauseDigest: leafDigest, PlanDigest: payloadDigest(payload)}
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

type propagationFrontierTarget struct {
	nodeID        string
	reservationID ReservationID
	candidateID   CandidateID
	key           CandidateClosureKey
}

func seedParallelAllPropagation(input *VerifiedExclusiveInput, after *RoutingState, reservation ActivationReservation, eventSeq int64) ([]PropagationIntent, error) {
	closed := after.Reservations[reservation.ID]
	if closed.State != ReservationClosedNoActivation || closed.CauseDigest == "" {
		return nil, fmt.Errorf("%w: propagation seed requires closed all reservation", ErrMutationInconsistent)
	}
	rootCandidate := CandidateID("")
	for _, candidate := range reservation.Candidates {
		key, err := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
		if err != nil {
			return nil, err
		}
		if _, ok := after.CandidateClosures[key]; ok {
			rootCandidate = candidate.ID
			break
		}
	}
	if rootCandidate == "" {
		return nil, fmt.Errorf("%w: closed all reservation lacks terminal root candidate", ErrMutationInconsistent)
	}
	outputScope, outputBranch := reservation.ScopeID, reservation.BranchEdgeID
	if reservation.IsReducing {
		scope := after.Scopes[reservation.ReducesScopeID]
		outputScope, outputBranch = scope.ParentScopeID, scope.ParentBranchEdgeID
	}
	targets := make([]propagationFrontierTarget, 0)
	for _, edge := range input.parallel.edges {
		if edge.FromNodeID != reservation.NodeID {
			continue
		}
		for _, target := range after.Reservations {
			if target.State != ReservationOpen || target.NodeID != edge.ToNodeID || target.ScopeID != outputScope {
				continue
			}
			candidate, ok := routeCandidateForEdge(target, edge.ID, outputBranch)
			if !ok {
				continue
			}
			key, err := CandidateClosureKeyIdentity(target.ID, candidate.ID)
			if err != nil {
				return nil, err
			}
			targets = append(targets, propagationFrontierTarget{nodeID: edge.ToNodeID, reservationID: target.ID, candidateID: candidate.ID, key: key})
			break
		}
	}
	slices.SortFunc(targets, func(a, b propagationFrontierTarget) int {
		if n := strings.Compare(a.nodeID, b.nodeID); n != 0 {
			return n
		}
		if n := strings.Compare(a.reservationID, b.reservationID); n != 0 {
			return n
		}
		return strings.Compare(a.candidateID, b.candidateID)
	})
	frontier := make([]CandidateClosureKey, 0, len(targets))
	for _, target := range targets {
		if len(frontier) == 0 || frontier[len(frontier)-1] != target.key {
			frontier = append(frontier, target.key)
		}
	}
	// Frontier storage is a canonical identity set. Target traversal above is
	// topology/reservation/candidate ordered; opaque keys are sorted only for
	// the persisted set representation consumed by mutation validation.
	slices.Sort(frontier)
	if len(frontier) == 0 {
		return nil, nil
	}
	shards := (len(frontier) + MaxRoutingList - 1) / MaxRoutingList
	if shards > MaxPropagationShards {
		return nil, &OverBudgetError{Limit: "propagation_shards", Value: shards, Maximum: MaxPropagationShards}
	}
	intents := make([]PropagationIntent, 0, shards)
	for partIndex := 0; partIndex < shards; partIndex++ {
		start := partIndex * MaxRoutingList
		end := min(len(frontier), start+MaxRoutingList)
		part := cloneSlice(frontier[start:end])
		shard, shardErr := nextPropagationShard(*after, closed.CauseDigest)
		if shardErr != nil {
			return nil, shardErr
		}
		planDigest, err := PropagationPlanIdentity(reservation.ID, rootCandidate, closed.CauseDigest, uint64(shard), part)
		if err != nil {
			return nil, err
		}
		intentID, err := PropagationIntentIdentity(closed.CauseDigest, uint64(shard), planDigest)
		if err != nil {
			return nil, err
		}
		intent := PropagationIntent{ID: intentID, RootReservationID: reservation.ID, RootCandidateID: rootCandidate, RootCauseDigest: closed.CauseDigest, Shard: shard, Cursor: 0, Frontier: part, PlanDigest: planDigest, State: PropagationPending, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
		after.Propagation[intent.ID] = intent
		intents = append(intents, intent)
	}
	return intents, nil
}

func nextPropagationShard(routing RoutingState, causeDigest CauseDigest) (uint32, error) {
	used := make(map[uint32]struct{})
	for _, intent := range routing.Propagation {
		if intent.RootCauseDigest == causeDigest {
			used[intent.Shard] = struct{}{}
		}
	}
	for shard := uint32(0); shard < MaxPropagationShards; shard++ {
		if _, exists := used[shard]; !exists {
			return shard, nil
		}
	}
	return 0, &OverBudgetError{Limit: "propagation_shards", Value: len(used) + 1, Maximum: MaxPropagationShards}
}

func parallelAllCandidateKinds(routing RoutingState, reservation ActivationReservation) (arrived, open, failed, skipped, canceled, impossible bool) {
	for _, candidate := range reservation.Candidates {
		kind := CandidateFoldOpen
		for _, path := range routing.Paths {
			if path.Kind == PathEdge && path.State == PathArrived && path.TargetReservationID == reservation.ID && path.CandidateID == candidate.ID {
				kind = "arrived"
				break
			}
		}
		if kind == CandidateFoldOpen {
			key, _ := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
			if closure, ok := routing.CandidateClosures[key]; ok {
				kind = string(closure.TerminalKind)
			}
		}
		switch kind {
		case "arrived":
			arrived = true
		case CandidateFoldOpen:
			open = true
		case string(TerminalFailed):
			failed = true
		case string(TerminalSkipped):
			skipped = true
		case string(TerminalCanceled):
			canceled = true
		case string(TerminalImpossible):
			impossible = true
		}
	}
	return
}

func buildParallelAllActivation(after *RoutingState, reservation ActivationReservation, arrivals []PathID, eventSeq int64) error {
	inputDigest, err := InputSetIdentity(arrivals)
	if err != nil {
		return err
	}
	activationID, err := ActivationIdentity(reservation.RunID, reservation.ID, reservation.Generation, inputDigest)
	if err != nil {
		return err
	}
	outputID, err := ActivationOutputIdentity(activationID, reservation.Generation)
	if err != nil {
		return err
	}
	ref := ActivationRef{ID: activationID, Generation: reservation.Generation}
	inputs := make([]PathRecord, len(arrivals))
	for index, pathID := range arrivals {
		inputs[index] = after.Paths[pathID]
	}
	var lineage []CandidateLineageFrame
	lineageID := ""
	if reservation.IsReducing {
		scope, ok := after.Scopes[reservation.ReducesScopeID]
		forkOutput, outputOK := after.Paths[scope.ForkOutputPathID]
		if !ok || !outputOK || forkOutput.State != PathSplit {
			return fmt.Errorf("%w: reducing all lacks exact fork output", ErrMutationInconsistent)
		}
		lineage = cloneSlice(forkOutput.CandidateLineage)
		lineageID = forkOutput.CandidateLineageID
	} else {
		lineage, lineageID, err = PopConsumedLineage(inputs, reservation.ID)
		if err != nil {
			return err
		}
	}
	for _, pathID := range arrivals {
		path := after.Paths[pathID]
		path.State, path.ConsumedBy, path.UpdatedSeq = PathConsumed, &ref, eventSeq
		receiptID, receiptErr := DispositionReceiptIdentity(path.ID, PathArrived, PathConsumed, "all_input", MutationCommandPlaceholder, "", uint64(eventSeq))
		if receiptErr != nil {
			return receiptErr
		}
		path.Disposition = &DispositionReceipt{ID: receiptID, PathID: path.ID, FromState: PathArrived, ToState: PathConsumed, ReasonCode: "all_input", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
		after.Paths[path.ID] = path
	}
	outputScope, outputBranch, reduced := reservation.ScopeID, reservation.BranchEdgeID, ScopeID("")
	if reservation.IsReducing {
		scope, ok := after.Scopes[reservation.ReducesScopeID]
		if !ok || scope.State != ScopeOpen {
			return fmt.Errorf("%w: reducing all scope is not open", ErrMutationInconsistent)
		}
		outputScope, outputBranch, reduced = scope.ParentScopeID, scope.ParentBranchEdgeID, scope.ID
		scope.State, scope.CloseReason, scope.ClosedByCommandID, scope.EventSeq = ScopeClosedActivated, ScopeCloseAll, MutationCommandPlaceholder, eventSeq
		after.Scopes[scope.ID] = scope
	}
	after.Paths[outputID] = PathRecord{ID: outputID, Kind: PathActivationOutput, State: PathLive, SourceActivation: ref, ScopeID: outputScope, BranchEdgeID: outputBranch, CandidateLineage: lineage, CandidateLineageID: lineageID, LineageDepth: uint32(len(lineage)), CreatedSeq: eventSeq, UpdatedSeq: eventSeq}
	receiptID, err := ActivationReceiptIdentity(activationID, reservation.ID, inputDigest, outputID, MutationCommandPlaceholder, uint64(eventSeq))
	if err != nil {
		return err
	}
	receipt := ActivationReceipt{ID: receiptID, ActivationID: activationID, ReservationID: reservation.ID, InputSetDigest: inputDigest, OutputPathID: outputID, ScopeID: outputScope, BranchEdgeID: outputBranch, ReducedScopeID: reduced, JoinPolicy: JoinAll, Result: ReceiptActivated, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
	after.Activations[activationID] = ActivationRecord{ID: activationID, RunID: reservation.RunID, Ref: ref, ReservationID: reservation.ID, InputPathIDs: cloneSlice(arrivals), InputSetDigest: inputDigest, OutputPathID: outputID, Receipt: receipt, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
	updated := after.Reservations[reservation.ID]
	updated.State, updated.Activation, updated.CloseReceipt, updated.ClosedReason, updated.CauseDigest, updated.CommandID, updated.EventSeq = ReservationActivated, &ref, nil, "", "", MutationCommandPlaceholder, eventSeq
	after.Reservations[updated.ID] = updated
	return nil
}

func buildParallelAllClose(after *RoutingState, reservation ActivationReservation, arrivals []PathID, leafDigest CauseDigest, reason ScopeCloseReason, eventSeq int64) error {
	leafSet, ok := after.CauseSets[leafDigest]
	if !ok || len(leafSet.CauseIDs) == 0 {
		return fmt.Errorf("%w: closed all fold lacks complete leaf causes", ErrMutationInconsistent)
	}
	kinds := make([]TerminalKind, 0, len(leafSet.CauseIDs))
	for _, causeID := range leafSet.CauseIDs {
		cause, exists := after.CauseRecords[causeID]
		if !exists {
			return fmt.Errorf("%w: all leaf cause %q is absent", ErrMutationInconsistent, causeID)
		}
		kinds = append(kinds, cause.TerminalKind)
	}
	terminal, err := FoldTerminalKinds(kinds)
	if err != nil {
		return err
	}
	reasonCode := "join_all_impossible"
	if reason == ScopeCloseCandidateNonSuccess {
		reasonCode = "join_candidate_non_success"
	}
	joinCauseID, err := CauseIdentity("", terminal, reasonCode, "", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return err
	}
	finalIDs := append(cloneSlice(leafSet.CauseIDs), joinCauseID)
	slices.Sort(finalIDs)
	finalIDs = slices.Compact(finalIDs)
	finalDigest, err := CauseSetIdentity(finalIDs)
	if err != nil {
		return err
	}
	after.CauseRecords[joinCauseID] = CauseRecord{ID: joinCauseID, TerminalKind: terminal, DispositionReason: reasonCode, SourceCommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
	after.CauseSets[finalDigest] = CauseSetRecord{Digest: finalDigest, CauseIDs: finalIDs}
	for _, pathID := range arrivals {
		path := after.Paths[pathID]
		path.State, path.ConsumedBy, path.UpdatedSeq = PathConsumed, nil, eventSeq
		receiptID, receiptErr := DispositionReceiptIdentity(path.ID, PathArrived, PathConsumed, "join_non_success", MutationCommandPlaceholder, "", uint64(eventSeq))
		if receiptErr != nil {
			return receiptErr
		}
		path.Disposition = &DispositionReceipt{ID: receiptID, PathID: path.ID, FromState: PathArrived, ToState: PathConsumed, ReasonCode: "join_non_success", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
		after.Paths[path.ID] = path
	}
	inputDigest, err := InputSetIdentity(arrivals)
	if err != nil {
		return err
	}
	outputScope, outputBranch, reduced := reservation.ScopeID, reservation.BranchEdgeID, ScopeID("")
	if reservation.IsReducing {
		scope, exists := after.Scopes[reservation.ReducesScopeID]
		if !exists || scope.State != ScopeOpen {
			return fmt.Errorf("%w: reducing all scope is not open", ErrMutationInconsistent)
		}
		outputScope, outputBranch, reduced = scope.ParentScopeID, scope.ParentBranchEdgeID, scope.ID
		scope.State, scope.CloseReason, scope.ClosedByCommandID, scope.EventSeq = ScopeClosedNoActivation, reason, MutationCommandPlaceholder, eventSeq
		after.Scopes[scope.ID] = scope
	}
	receiptID, err := ActivationReceiptIdentity("", reservation.ID, inputDigest, "", MutationCommandPlaceholder, uint64(eventSeq))
	if err != nil {
		return err
	}
	receipt := ActivationReceipt{ID: receiptID, ReservationID: reservation.ID, InputSetDigest: inputDigest, ScopeID: outputScope, BranchEdgeID: outputBranch, ReducedScopeID: reduced, JoinPolicy: reservation.JoinPolicy, Result: ReceiptClosedNoActivation, CauseDigest: finalDigest, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq}
	updated := after.Reservations[reservation.ID]
	updated.State, updated.Activation, updated.CloseReceipt, updated.ClosedReason, updated.CauseDigest, updated.CommandID, updated.EventSeq = ReservationClosedNoActivation, nil, &receipt, string(reason), finalDigest, MutationCommandPlaceholder, eventSeq
	after.Reservations[updated.ID] = updated
	return nil
}
