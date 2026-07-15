package pathv1

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// ExclusiveSequenceCursor is the durable recovery position in a canonical
// exclusive route sequence. Applied is always a prefix length: commands may
// only be resumed in the order returned by ExclusiveRouteSequence.Commands.
type ExclusiveSequenceCursor struct {
	Applied uint32
	Total   uint32
}

// ExclusiveRouteSequenceCommandBound returns the worst-case number of
// commands for one exclusive route: one route, N-1 loser propagations, up to
// N-1 dead-reservation activations, one winner activation, and one end route.
func ExclusiveRouteSequenceCommandBound(outgoing int) (int, error) {
	if _, err := MutationCountExclusive(outgoing); err != nil {
		return 0, err
	}
	maximum := 2*outgoing + 1
	if maximum > MaxRoutingLogEntries {
		return 0, &OverBudgetError{Limit: "log_entries", Value: maximum, Maximum: MaxRoutingLogEntries}
	}
	return maximum, nil
}

func (c ExclusiveSequenceCursor) Complete() bool {
	return c.Applied == c.Total
}

// ExclusiveRouteSequence is a sealed, bounded sequence of existing path-v1
// route, propagation, and activation commands. It carries no new mutation
// authority: every state is produced by the canonical mutation reducers.
type ExclusiveRouteSequence struct {
	commands []CommandRecord
	final    AggregateCheckpoint
	binding  CheckpointBinding
}

func (s *ExclusiveRouteSequence) Commands() []CommandRecord {
	if s == nil {
		return nil
	}
	return cloneExclusiveCommandSlice(s.commands)
}

func (s *ExclusiveRouteSequence) Cursor() ExclusiveSequenceCursor {
	if s == nil {
		return ExclusiveSequenceCursor{}
	}
	return ExclusiveSequenceCursor{Total: uint32(len(s.commands))}
}

// ExclusiveSequenceRecovery describes the exact prefix already made durable.
// NextCommand is zero when the sequence is complete. Projection returns the
// aggregate after Applied commands, or the verified input aggregate at zero.
type ExclusiveSequenceRecovery struct {
	cursor     ExclusiveSequenceCursor
	next       CommandRecord
	projection AggregateCheckpoint
	binding    CheckpointBinding
}

func (r *ExclusiveSequenceRecovery) Cursor() ExclusiveSequenceCursor {
	if r == nil {
		return ExclusiveSequenceCursor{}
	}
	return r.cursor
}

func (r *ExclusiveSequenceRecovery) NextCommand() CommandRecord {
	if r == nil {
		return CommandRecord{}
	}
	return cloneCommandRecord(r.next)
}

func (r *ExclusiveSequenceRecovery) Projection() *ExclusiveProjection {
	if r == nil {
		return nil
	}
	return &ExclusiveProjection{aggregate: r.projection, binding: r.binding}
}

// PlanExclusiveRouteSequence selects exactly one outgoing edge and closes all
// loser candidates in stable path-ID order before closing fully impossible
// loser reservations in stable reservation-ID order. The selected reservation
// is activated last; a directly reached end node is routed after activation.
func PlanExclusiveRouteSequence(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation) (*ExclusiveRouteSequence, error) {
	draft, err := buildExclusiveRouteSequence(ctx, input, observation, -1)
	if err != nil {
		return nil, err
	}
	return &ExclusiveRouteSequence{
		commands: cloneExclusiveCommandSlice(draft.commands),
		final:    draft.final,
		binding:  input.binding,
	}, nil
}

// RecoverExclusiveRouteSequence validates an already-durable command prefix
// against a freshly derived plan. Exact prefix equality makes replay
// idempotent and rejects reordering, omission, insertion, or payload drift.
func RecoverExclusiveRouteSequence(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, applied []CommandRecord) (*ExclusiveSequenceRecovery, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(applied) > MaxRoutingLogEntries {
		return nil, &OverBudgetError{Limit: "log_entries", Value: len(applied), Maximum: MaxRoutingLogEntries}
	}
	maximum, err := exclusiveRouteSequenceInputBound(input, observation.SourcePathID)
	if err != nil {
		return nil, err
	}
	if len(applied) > maximum {
		return nil, fmt.Errorf("%w: exclusive sequence cursor %d exceeds command bound %d", ErrMutationInvalid, len(applied), maximum)
	}
	draft, err := buildExclusiveRouteSequence(ctx, input, observation, len(applied))
	if err != nil {
		return nil, err
	}
	if len(applied) > len(draft.commands) {
		return nil, fmt.Errorf("%w: exclusive sequence cursor %d exceeds total %d", ErrMutationInvalid, len(applied), len(draft.commands))
	}
	for index := range applied {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !exactExclusiveCommand(draft.commands[index], applied[index]) {
			return nil, fmt.Errorf("%w: exclusive sequence command %d differs from deterministic prefix", ErrMutationInvalid, index)
		}
	}
	recovery := &ExclusiveSequenceRecovery{
		cursor:     ExclusiveSequenceCursor{Applied: uint32(len(applied)), Total: uint32(len(draft.commands))},
		projection: draft.captured, binding: input.binding,
	}
	if len(applied) < len(draft.commands) {
		recovery.next = cloneCommandRecord(draft.commands[len(applied)])
	}
	return recovery, nil
}

func exclusiveRouteSequenceInputBound(input *VerifiedExclusiveInput, sourcePathID PathID) (int, error) {
	if input == nil || input.checkpoint == nil || input.template == nil || sourcePathID == "" {
		return 0, fmt.Errorf("%w: complete sealed sequence input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return 0, err
	}
	source, ok := aggregate.Routing.Paths[sourcePathID]
	if !ok || source.Kind != PathActivationOutput {
		return 0, fmt.Errorf("%w: sequence source path is absent", ErrMutationInconsistent)
	}
	activation, ok := aggregate.Routing.Activations[source.SourceActivation.ID]
	if !ok {
		return 0, fmt.Errorf("%w: sequence source activation is absent", ErrMutationInconsistent)
	}
	reservation, ok := aggregate.Routing.Reservations[activation.ReservationID]
	if !ok {
		return 0, fmt.Errorf("%w: sequence source reservation is absent", ErrMutationInconsistent)
	}
	node, ok := input.template.Nodes[reservation.NodeID]
	if !ok {
		return 0, fmt.Errorf("%w: sequence source node is absent", ErrExclusiveInputInvalid)
	}
	return ExclusiveRouteSequenceCommandBound(len(node.Next))
}

// ReduceExclusiveRouteSequence accepts only the complete canonical sequence.
// Prefixes are intentionally handled by RecoverExclusiveRouteSequence so a
// caller must persist and resume an explicit cursor rather than silently
// treating partial loser closure as completion.
func ReduceExclusiveRouteSequence(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, commands []CommandRecord) (*ExclusiveProjection, error) {
	recovery, err := RecoverExclusiveRouteSequence(ctx, input, observation, commands)
	if err != nil {
		return nil, err
	}
	if !recovery.cursor.Complete() {
		return nil, fmt.Errorf("%w: exclusive sequence is partial at %d of %d", ErrExclusiveNotRoutable, recovery.cursor.Applied, recovery.cursor.Total)
	}
	projection := recovery.Projection()
	projection.command = cloneCommandRecord(commands[len(commands)-1])
	projection.dispose = ReplayApplied
	return projection, nil
}

type exclusiveRouteSequenceDraft struct {
	commands []CommandRecord
	final    AggregateCheckpoint
	captured AggregateCheckpoint
	route    exclusiveRouteDraft
}

func buildExclusiveRouteSequence(ctx context.Context, input *VerifiedExclusiveInput, observation ExclusiveObservation, captureAt int) (exclusiveRouteSequenceDraft, error) {
	if err := ctx.Err(); err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}
	route, err := buildExclusiveRouteDraft(ctx, input, observation)
	if err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}
	maximum, err := ExclusiveRouteSequenceCommandBound(len(route.outgoing))
	if err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}
	if maximum > MaxRoutingLogEntries {
		return exclusiveRouteSequenceDraft{}, &OverBudgetError{Limit: "log_entries", Value: maximum, Maximum: MaxRoutingLogEntries}
	}
	last := CurrentLastLogSeq(input.checkpoint)
	if last > math.MaxInt64 || uint64(maximum) > uint64(math.MaxInt64)-last {
		return exclusiveRouteSequenceDraft{}, &OverBudgetError{Limit: "log_entries", Value: math.MaxInt, Maximum: math.MaxInt - 1}
	}

	commands := make([]CommandRecord, 0, maximum)
	var captured AggregateCheckpoint
	if captureAt == 0 {
		captured, err = CurrentAggregateCheckpoint(input.checkpoint)
		if err != nil {
			return exclusiveRouteSequenceDraft{}, err
		}
	}
	routed, err := ReplayRoutePaths(route.view, route.command)
	if err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}
	post := route.view.Aggregate
	post.Routing = &routed.Routing
	if err := validateExclusiveSequenceStep(post, route); err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}
	commands = append(commands, cloneCommandRecord(route.command))
	if err := captureExclusiveSequenceProjection(&captured, captureAt, len(commands), post); err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}

	parent := post.Routing.Paths[route.sourcePathID]
	losers := make([]PathRecord, 0, len(parent.ProducedPathIDs)-1)
	var selected PathRecord
	for _, childID := range parent.ProducedPathIDs {
		child := post.Routing.Paths[childID]
		switch {
		case child.Kind == PathEdge && child.State == PathArrived:
			if selected.ID != "" {
				return exclusiveRouteSequenceDraft{}, fmt.Errorf("%w: exclusive route has multiple winners", ErrMutationInconsistent)
			}
			selected = child
		case child.Kind == PathImpossibleEdge && child.State == PathImpossible:
			losers = append(losers, child)
		}
	}
	if selected.ID == "" || len(losers) != len(route.outgoing)-1 {
		return exclusiveRouteSequenceDraft{}, fmt.Errorf("%w: exclusive route winner/loser partition is incomplete", ErrMutationInconsistent)
	}
	slices.SortFunc(losers, func(a, b PathRecord) int { return compareString(string(a.ID), string(b.ID)) })

	eventSeq := route.eventSeq
	loserReservations := make(map[ReservationID]struct{}, len(losers))
	for _, loser := range losers {
		if err := ctx.Err(); err != nil {
			return exclusiveRouteSequenceDraft{}, err
		}
		eventSeq++
		command, next, buildErr := buildExclusiveSequenceClosure(input.binding, post, loser, eventSeq)
		if buildErr != nil {
			return exclusiveRouteSequenceDraft{}, buildErr
		}
		post = next
		if err := validateExclusiveSequenceStep(post, route); err != nil {
			return exclusiveRouteSequenceDraft{}, err
		}
		commands = append(commands, cloneCommandRecord(command))
		if err := captureExclusiveSequenceProjection(&captured, captureAt, len(commands), post); err != nil {
			return exclusiveRouteSequenceDraft{}, err
		}
		if loser.TargetReservationID != selected.TargetReservationID {
			loserReservations[loser.TargetReservationID] = struct{}{}
		}
	}

	reservationIDs := make([]ReservationID, 0, len(loserReservations))
	for reservationID := range loserReservations {
		reservationIDs = append(reservationIDs, reservationID)
	}
	slices.Sort(reservationIDs)
	for _, reservationID := range reservationIDs {
		if err := ctx.Err(); err != nil {
			return exclusiveRouteSequenceDraft{}, err
		}
		command, next, required, buildErr := buildExclusiveSequenceDeadReservation(input.binding, post, reservationID, eventSeq+1)
		if buildErr != nil {
			return exclusiveRouteSequenceDraft{}, buildErr
		}
		if !required {
			continue
		}
		eventSeq++
		post = next
		if err := validateExclusiveSequenceStep(post, route); err != nil {
			return exclusiveRouteSequenceDraft{}, err
		}
		commands = append(commands, cloneCommandRecord(command))
		if err := captureExclusiveSequenceProjection(&captured, captureAt, len(commands), post); err != nil {
			return exclusiveRouteSequenceDraft{}, err
		}
	}

	eventSeq++
	activation, post, err := buildExclusiveSequenceActivation(input.binding, post, selected, eventSeq)
	if err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}
	if err := validateExclusiveSequenceStep(post, route); err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}
	commands = append(commands, cloneCommandRecord(activation))
	if err := captureExclusiveSequenceProjection(&captured, captureAt, len(commands), post); err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}

	reservation := post.Routing.Reservations[activation.Identity.TargetReservationID]
	if node, ok := input.template.Nodes[reservation.NodeID]; ok && node.Type == model.NodeTypeEnd {
		eventSeq++
		end, ended, endErr := buildExclusiveSequenceEnd(input, post, activation, eventSeq)
		if endErr != nil {
			return exclusiveRouteSequenceDraft{}, endErr
		}
		post = ended
		if err := validateExclusiveSequenceStep(post, route); err != nil {
			return exclusiveRouteSequenceDraft{}, err
		}
		commands = append(commands, cloneCommandRecord(end))
		if err := captureExclusiveSequenceProjection(&captured, captureAt, len(commands), post); err != nil {
			return exclusiveRouteSequenceDraft{}, err
		}
	}

	if len(commands) > maximum {
		return exclusiveRouteSequenceDraft{}, &OverBudgetError{Limit: "log_entries", Value: len(commands), Maximum: maximum}
	}
	if err := validateExclusiveSequenceComplete(post, route, losers, selected); err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}
	final, err := checkpointAggregate(post)
	if err != nil {
		return exclusiveRouteSequenceDraft{}, err
	}
	if captureAt < 0 {
		captured = final
	}
	return exclusiveRouteSequenceDraft{commands: commands, final: final, captured: captured, route: route}, nil
}

func captureExclusiveSequenceProjection(dst *AggregateCheckpoint, captureAt, cursor int, view AggregateView) error {
	if captureAt != cursor {
		return nil
	}
	aggregate, err := checkpointAggregate(view)
	if err != nil {
		return err
	}
	*dst = aggregate
	return nil
}

func cloneExclusiveCommandSlice(values []CommandRecord) []CommandRecord {
	cloned := make([]CommandRecord, len(values))
	for index := range values {
		cloned[index] = cloneCommandRecord(values[index])
	}
	return cloned
}

func validateExclusiveSequenceStep(post AggregateView, route exclusiveRouteDraft) error {
	if err := validateExclusiveSequenceConservation(post, route); err != nil {
		return err
	}
	report := ValidateAggregate(post)
	if !report.Valid() {
		return fmt.Errorf("%w: exclusive sequence aggregate has %d diagnostics", ErrMutationInconsistent, len(report.Diagnostics)+report.Suppressed)
	}
	return nil
}

func validateExclusiveSequenceConservation(post AggregateView, route exclusiveRouteDraft) error {
	parent, ok := post.Routing.Paths[route.sourcePathID]
	if !ok || parent.State != PathRouted || !slices.Equal(parent.ProducedPathIDs, materializedProducedIDs(post, route.sourcePathID)) {
		return fmt.Errorf("%w: sequence route parent does not own exact materialized children", ErrMutationInconsistent)
	}
	selected, impossible := 0, 0
	seenEdges := make(map[EdgeID]struct{}, len(route.outgoing))
	for _, childID := range parent.ProducedPathIDs {
		child, exists := post.Routing.Paths[childID]
		if !exists || child.ParentPathID != parent.ID || child.Edge == nil {
			return fmt.Errorf("%w: sequence routed child backlink is incomplete", ErrMutationInconsistent)
		}
		if _, duplicate := seenEdges[child.Edge.ID]; duplicate {
			return fmt.Errorf("%w: sequence routed edge is duplicated", ErrMutationInconsistent)
		}
		seenEdges[child.Edge.ID] = struct{}{}
		switch child.Kind {
		case PathEdge:
			selected++
			if child.Edge.ID != route.selectedEdge || (child.State != PathArrived && child.State != PathConsumed) {
				return fmt.Errorf("%w: selected edge differs from exact sequence plan", ErrMutationInconsistent)
			}
		case PathImpossibleEdge:
			impossible++
			if child.Edge.ID == route.selectedEdge || child.State != PathImpossible || child.ImpossibleCauseDigest == "" {
				return fmt.Errorf("%w: sequence loser lacks exact provenance", ErrMutationInconsistent)
			}
		default:
			return fmt.Errorf("%w: sequence route produced child kind %q", ErrMutationInconsistent, child.Kind)
		}
	}
	if len(seenEdges) != len(route.outgoing) || selected != 1 || impossible != len(route.outgoing)-1 {
		return fmt.Errorf("%w: sequence token count selected=%d impossible=%d outgoing=%d", ErrMutationInconsistent, selected, impossible, len(route.outgoing))
	}
	return nil
}

func validateExclusiveSequenceComplete(post AggregateView, route exclusiveRouteDraft, losers []PathRecord, selected PathRecord) error {
	if err := validateExclusiveSequenceStep(post, route); err != nil {
		return err
	}
	for _, loser := range losers {
		reservation, ok := post.Routing.Reservations[loser.TargetReservationID]
		if !ok {
			return fmt.Errorf("%w: loser reservation %q is absent", ErrMutationInconsistent, loser.TargetReservationID)
		}
		key, err := CandidateClosureKeyIdentity(reservation.ID, loser.CandidateID)
		if err != nil {
			return err
		}
		closure, ok := post.Routing.CandidateClosures[key]
		if !ok || closure.TerminalKind != TerminalImpossible || closure.CauseDigest != loser.ImpossibleCauseDigest {
			return fmt.Errorf("%w: loser %q lacks its authoritative candidate closure", ErrMutationInconsistent, loser.ID)
		}
		if reservation.ID == selected.TargetReservationID {
			continue
		}
		allClosed := true
		for _, candidate := range reservation.Candidates {
			candidateKey, keyErr := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
			if keyErr != nil {
				return keyErr
			}
			if _, exists := post.Routing.CandidateClosures[candidateKey]; !exists {
				allClosed = false
				break
			}
		}
		if allClosed && reservation.State != ReservationClosedNoActivation {
			return fmt.Errorf("%w: fully impossible loser reservation %q remains open", ErrMutationInconsistent, reservation.ID)
		}
	}
	selectedReservation := post.Routing.Reservations[selected.TargetReservationID]
	if selectedReservation.State != ReservationActivated {
		return fmt.Errorf("%w: selected reservation was not activated", ErrMutationInconsistent)
	}
	return nil
}

func compareString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func buildExclusiveSequenceClosure(binding CheckpointBinding, post AggregateView, impossible PathRecord, eventSeq int64) (CommandRecord, AggregateView, error) {
	reservation, ok := post.Routing.Reservations[impossible.TargetReservationID]
	if !ok || reservation.JoinPolicy != JoinExclusive || reservation.State != ReservationOpen {
		return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: loser reservation is absent or closed", ErrMutationInconsistent)
	}
	candidate, ok := candidateForID(reservation, impossible.CandidateID)
	if !ok || impossible.Kind != PathImpossibleEdge || impossible.State != PathImpossible {
		return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: loser candidate is not an impossible reserved edge", ErrMutationInconsistent)
	}
	set, ok := post.Routing.CauseSets[impossible.ImpossibleCauseDigest]
	if !ok || len(set.CauseIDs) == 0 {
		return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: loser lacks complete cause provenance", ErrMutationInconsistent)
	}
	kinds := make([]TerminalKind, len(set.CauseIDs))
	for index, causeID := range set.CauseIDs {
		cause, exists := post.Routing.CauseRecords[causeID]
		if !exists || cause.SourceCommandID == "" {
			return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: loser cause %q is absent or unauthoritative", ErrMutationInconsistent, causeID)
		}
		kinds[index] = cause.TerminalKind
	}
	settled := make(map[PossibleSlotID]SlotSettlement, len(candidate.PossibleSlotIDs))
	for _, slotID := range candidate.PossibleSlotIDs {
		settled[slotID] = SlotSettlement{CauseIDs: cloneSlice(set.CauseIDs), CauseKinds: cloneSlice(kinds)}
	}
	entry, causeIDs, terminal, err := FoldCandidateSlots(reservation.ID, candidate, settled, false)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	if entry.FoldKind == CandidateFoldOpen || entry.FoldKind == "arrived" || !slices.Equal(causeIDs, set.CauseIDs) {
		return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: loser candidate does not fold to its exact causes", ErrMutationInconsistent)
	}
	closureKey, err := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	if _, exists := post.Routing.CandidateClosures[closureKey]; exists {
		return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: loser candidate %q is already closed", ErrMutationInconsistent, candidate.ID)
	}
	before := Clone(*post.Routing)
	after := Clone(before)
	after.CandidateClosures[closureKey] = CandidateClosure{
		ID: entry.PathOrClosureID, Key: CandidateClosureKeyRecord{ID: closureKey, ReservationID: reservation.ID, CandidateID: candidate.ID},
		TerminalKind: terminal, CauseDigest: set.Digest, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	frontier := []CandidateClosureKey{closureKey}
	planDigest, err := PropagationPlanIdentity(reservation.ID, candidate.ID, set.Digest, 0, frontier)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	intentID, err := PropagationIntentIdentity(set.Digest, 0, planDigest)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	intent := PropagationIntent{
		ID: intentID, RootReservationID: reservation.ID, RootCandidateID: candidate.ID, RootCauseDigest: set.Digest,
		Shard: 0, Cursor: 1, Frontier: frontier, PlanDigest: planDigest, State: PropagationComplete,
		CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	after.Propagation[intent.ID] = intent
	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	current := post
	current.Routing = &before
	current.Commands = cloneMap(post.Commands)
	replayView := MutationReplayView{Aggregate: current, Checkpoint: binding}
	plan := PropagateClosurePlan{
		SourcePathID: impossible.ID, TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
		InputDigest: planDigest, CauseDigest: set.Digest,
		RootReservationID: reservation.ID, RootCandidateID: candidate.ID, RootCauseDigest: set.Digest,
		Intents: []PropagationIntent{intent}, Batch: batch,
	}
	payload, err := EncodePropagateClosurePayload(replayView, plan)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	identity := CommandIdentity{
		RunID: post.RunID, Kind: CommandPropagateCandidateClosure, PayloadSchema: 1,
		SourcePathID: impossible.ID, TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
		InputDigest: planDigest, CauseDigest: set.Digest, PlanDigest: payloadDigest(payload),
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	result, err := ReplayPropagateClosure(replayView, command)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	next := replayView.Aggregate
	next.Routing = &result.Routing
	return command, next, nil
}

func buildExclusiveSequenceDeadReservation(binding CheckpointBinding, post AggregateView, reservationID ReservationID, eventSeq int64) (CommandRecord, AggregateView, bool, error) {
	reservation, ok := post.Routing.Reservations[reservationID]
	if !ok || reservation.State != ReservationOpen || reservation.JoinPolicy != JoinExclusive {
		return CommandRecord{}, AggregateView{}, false, fmt.Errorf("%w: loser reservation %q is unavailable", ErrMutationInconsistent, reservationID)
	}
	for _, candidate := range reservation.Candidates {
		key, err := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
		if err != nil {
			return CommandRecord{}, AggregateView{}, false, err
		}
		if _, exists := post.Routing.CandidateClosures[key]; !exists {
			return CommandRecord{}, post, false, nil
		}
	}
	fold, arrivals, leafDigest, err := activationFold(*post.Routing, reservation)
	if err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	if len(arrivals) != 0 {
		return CommandRecord{}, AggregateView{}, false, fmt.Errorf("%w: fully closed loser reservation has an arrival", ErrMutationInconsistent)
	}
	leafSet, ok := post.Routing.CauseSets[leafDigest]
	if !ok || len(leafSet.CauseIDs) == 0 {
		return CommandRecord{}, AggregateView{}, false, fmt.Errorf("%w: loser reservation fold lacks complete causes", ErrMutationInconsistent)
	}
	kinds := make([]TerminalKind, 0, len(leafSet.CauseIDs))
	for _, causeID := range leafSet.CauseIDs {
		cause, exists := post.Routing.CauseRecords[causeID]
		if !exists {
			return CommandRecord{}, AggregateView{}, false, fmt.Errorf("%w: loser reservation cause %q is absent", ErrMutationInconsistent, causeID)
		}
		kinds = append(kinds, cause.TerminalKind)
	}
	terminal, err := FoldTerminalKinds(kinds)
	if err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	joinCauseID, err := CauseIdentity("", terminal, "join_all_impossible", "", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	finalCauseIDs := append(cloneSlice(leafSet.CauseIDs), joinCauseID)
	slices.Sort(finalCauseIDs)
	finalDigest, err := CauseSetIdentity(finalCauseIDs)
	if err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	inputDigest, err := InputSetIdentity(nil)
	if err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	receiptID, err := ActivationReceiptIdentity("", reservation.ID, inputDigest, "", MutationCommandPlaceholder, uint64(eventSeq))
	if err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	before := Clone(*post.Routing)
	after := Clone(before)
	after.CauseRecords[joinCauseID] = CauseRecord{
		ID: joinCauseID, TerminalKind: terminal, DispositionReason: "join_all_impossible",
		SourceCommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	after.CauseSets[finalDigest] = CauseSetRecord{Digest: finalDigest, CauseIDs: finalCauseIDs}
	dead := after.Reservations[reservation.ID]
	dead.State = ReservationClosedNoActivation
	dead.CloseReceipt = &ActivationReceipt{
		ID: receiptID, ReservationID: reservation.ID, InputSetDigest: inputDigest,
		ScopeID: reservation.ScopeID, BranchEdgeID: reservation.BranchEdgeID,
		JoinPolicy: reservation.JoinPolicy, Result: ReceiptClosedNoActivation,
		CauseDigest: finalDigest, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	dead.ClosedReason = string(ScopeCloseAllImpossible)
	dead.CauseDigest = finalDigest
	dead.CommandID = MutationCommandPlaceholder
	dead.EventSeq = eventSeq
	after.Reservations[dead.ID] = dead
	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	current := post
	current.Routing = &before
	current.Commands = cloneMap(post.Commands)
	replayView := MutationReplayView{Aggregate: current, Checkpoint: binding}
	plan := ActivateGenerationPlan{
		ReservationID: reservation.ID, Generation: reservation.Generation, InputDigest: fold, CauseDigest: leafDigest,
		JoinPolicy: JoinExclusive, InputPathIDs: []PathID{}, Candidates: cloneCandidates(reservation.Candidates),
		PossibleSlots: cloneSlice(reservation.PossibleSlots), Batch: batch,
	}
	payload, err := EncodeActivateGenerationPayload(replayView, plan)
	if err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	identity := CommandIdentity{
		RunID: post.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1,
		TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
		InputDigest: fold, CauseDigest: leafDigest, PlanDigest: payloadDigest(payload),
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	result, err := ReplayActivateGeneration(replayView, command)
	if err != nil {
		return CommandRecord{}, AggregateView{}, false, err
	}
	next := replayView.Aggregate
	next.Routing = &result.Routing
	return command, next, true, nil
}

func buildExclusiveSequenceActivation(binding CheckpointBinding, post AggregateView, selected PathRecord, eventSeq int64) (CommandRecord, AggregateView, error) {
	reservation, ok := post.Routing.Reservations[selected.TargetReservationID]
	if !ok || reservation.State != ReservationOpen || reservation.JoinPolicy != JoinExclusive {
		return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: selected reservation is unavailable", ErrMutationInconsistent)
	}
	fold, arrivals, causeDigest, err := activationFold(*post.Routing, reservation)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	if len(arrivals) != 1 || arrivals[0] != selected.ID {
		return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: exclusive activation must conserve exactly the winner", ErrMutationInconsistent)
	}
	for _, candidate := range reservation.Candidates {
		if candidate.ID == selected.CandidateID {
			continue
		}
		key, err := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
		if err != nil {
			return CommandRecord{}, AggregateView{}, err
		}
		if _, exists := post.Routing.CandidateClosures[key]; !exists {
			return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: selected reservation retains open loser candidate %q", ErrExclusiveNotRoutable, candidate.ID)
		}
	}
	inputDigest, err := InputSetIdentity(arrivals)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	activationID, err := ActivationIdentity(post.RunID, reservation.ID, reservation.Generation, inputDigest)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	outputID, err := ActivationOutputIdentity(activationID, reservation.Generation)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	ref := ActivationRef{ID: activationID, Generation: reservation.Generation}
	before := Clone(*post.Routing)
	after := Clone(before)
	frames, lineageID, err := PopConsumedLineage([]PathRecord{selected}, reservation.ID)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	consumed := after.Paths[selected.ID]
	consumed.State = PathConsumed
	consumed.ConsumedBy = &ref
	consumed.UpdatedSeq = eventSeq
	dispositionID, err := DispositionReceiptIdentity(consumed.ID, PathArrived, PathConsumed, "exclusive_input", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	consumed.Disposition = &DispositionReceipt{
		ID: dispositionID, PathID: consumed.ID, FromState: PathArrived, ToState: PathConsumed,
		ReasonCode: "exclusive_input", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	after.Paths[consumed.ID] = consumed
	after.Paths[outputID] = PathRecord{
		ID: outputID, Kind: PathActivationOutput, State: PathLive, SourceActivation: ref,
		ScopeID: reservation.ScopeID, BranchEdgeID: reservation.BranchEdgeID,
		CandidateLineage: frames, CandidateLineageID: lineageID, LineageDepth: uint32(len(frames)),
		CreatedSeq: eventSeq, UpdatedSeq: eventSeq,
	}
	receiptID, err := ActivationReceiptIdentity(activationID, reservation.ID, inputDigest, outputID, MutationCommandPlaceholder, uint64(eventSeq))
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	receipt := ActivationReceipt{
		ID: receiptID, ActivationID: activationID, ReservationID: reservation.ID, InputSetDigest: inputDigest,
		OutputPathID: outputID, ScopeID: reservation.ScopeID, BranchEdgeID: reservation.BranchEdgeID,
		JoinPolicy: JoinExclusive, Result: ReceiptActivated, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	after.Activations[activationID] = ActivationRecord{
		ID: activationID, RunID: post.RunID, Ref: ref, ReservationID: reservation.ID,
		InputPathIDs: cloneSlice(arrivals), InputSetDigest: inputDigest, OutputPathID: outputID,
		Receipt: receipt, CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	activated := after.Reservations[reservation.ID]
	activated.State = ReservationActivated
	activated.Activation = &ref
	activated.CommandID = MutationCommandPlaceholder
	activated.EventSeq = eventSeq
	after.Reservations[activated.ID] = activated
	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	current := post
	current.Routing = &before
	current.Commands = cloneMap(post.Commands)
	replayView := MutationReplayView{Aggregate: current, Checkpoint: binding}
	plan := ActivateGenerationPlan{
		ReservationID: reservation.ID, Generation: reservation.Generation, InputDigest: fold, CauseDigest: causeDigest,
		JoinPolicy: JoinExclusive, InputPathIDs: cloneSlice(arrivals), Candidates: cloneCandidates(reservation.Candidates),
		PossibleSlots: cloneSlice(reservation.PossibleSlots), Batch: batch,
	}
	payload, err := EncodeActivateGenerationPayload(replayView, plan)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	identity := CommandIdentity{
		RunID: post.RunID, Kind: CommandActivateGeneration, PayloadSchema: 1,
		TargetReservationID: reservation.ID, TargetGeneration: reservation.Generation,
		InputDigest: fold, CauseDigest: causeDigest, PlanDigest: payloadDigest(payload),
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	result, err := ReplayActivateGeneration(replayView, command)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	next := replayView.Aggregate
	next.Routing = &result.Routing
	return command, next, nil
}

func buildExclusiveSequenceEnd(input *VerifiedExclusiveInput, post AggregateView, activation CommandRecord, eventSeq int64) (CommandRecord, AggregateView, error) {
	reservation := post.Routing.Reservations[activation.Identity.TargetReservationID]
	node, ok := input.template.Nodes[reservation.NodeID]
	if !ok || node.Type != model.NodeTypeEnd {
		return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: activated target is not an end node", ErrExclusiveUnsupported)
	}
	result := strings.ToLower(strings.TrimSpace(node.Result))
	if result != "" && result != "pass" && result != "success" && result != "completed" && result != "complete" {
		return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: terminal result %q requires non-success authority outside this slice", ErrExclusiveUnsupported, node.Result)
	}
	activationRecord := post.Routing.Activations[reservation.Activation.ID]
	output := post.Routing.Paths[activationRecord.OutputPathID]
	if output.Kind != PathActivationOutput || output.State != PathLive {
		return CommandRecord{}, AggregateView{}, fmt.Errorf("%w: end activation output is not live", ErrMutationInconsistent)
	}
	before := Clone(*post.Routing)
	after := Clone(before)
	ended := after.Paths[output.ID]
	ended.State = PathEnded
	ended.UpdatedSeq = eventSeq
	dispositionID, err := DispositionReceiptIdentity(ended.ID, PathLive, PathEnded, "completed", MutationCommandPlaceholder, "", uint64(eventSeq))
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	ended.Disposition = &DispositionReceipt{
		ID: dispositionID, PathID: ended.ID, FromState: PathLive, ToState: PathEnded,
		ReasonCode: "completed", CommandID: MutationCommandPlaceholder, EventSeq: eventSeq,
	}
	after.Paths[ended.ID] = ended
	batch, err := NewMutationBatch(&before, &after, eventSeq)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	endObservation := ExclusiveObservation{SourcePathID: output.ID, Attempt: 1, Outcome: "pass"}
	current := post
	current.Commands = cloneMap(post.Commands)
	current.SideEffects = cloneMap(post.SideEffects)
	perform, settle, effect, err := observedAttemptCommands(current, reservation.NodeID, node, output, endObservation)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	if err := insertExactCommand(current.Commands, perform); err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	if err := insertExactCommand(current.Commands, settle); err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	current.SideEffects[effect.ID] = effect
	current.Routing = &before
	replayView := MutationReplayView{Aggregate: current, Checkpoint: input.binding}
	emptyCause, err := CauseSetIdentity(nil)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	plan := RoutePathsPlan{
		SettlementCommandID: settle.ID, SourceActivationID: output.SourceActivation.ID,
		SourceGeneration: output.SourceActivation.Generation, SourcePathID: output.ID,
		Attempt: 1, CauseDigest: emptyCause, ResultCode: "pass", ProducedPathIDs: []PathID{}, Batch: batch,
	}
	payload, err := EncodeRoutePathsPayload(replayView, plan)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	identity := CommandIdentity{
		RunID: post.RunID, Kind: CommandRoutePaths, PayloadSchema: 1,
		SourceActivationID: output.SourceActivation.ID, SourceGeneration: output.SourceActivation.Generation,
		SourcePathID: output.ID, Attempt: 1, InputDigest: settle.ID,
		CauseDigest: emptyCause, PlanDigest: payloadDigest(payload), ResultCode: "pass",
	}
	command, err := observedCommand(identity, payload)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	if err := insertExactCommand(replayView.Aggregate.Commands, command); err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	replayed, err := ReplayRoutePaths(replayView, command)
	if err != nil {
		return CommandRecord{}, AggregateView{}, err
	}
	next := replayView.Aggregate
	next.Routing = &replayed.Routing
	return command, next, nil
}
