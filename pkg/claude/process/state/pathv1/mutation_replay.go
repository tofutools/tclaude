package pathv1

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
)

func EncodeRoutePathsPayload(view MutationReplayView, plan RoutePathsPlan) ([]byte, error) {
	return encodeMutationPayload(view, plan)
}

func EncodeActivateGenerationPayload(view MutationReplayView, plan ActivateGenerationPlan) ([]byte, error) {
	return encodeMutationPayload(view, plan)
}

func EncodePropagateClosurePayload(view MutationReplayView, plan PropagateClosurePlan) ([]byte, error) {
	return encodeMutationPayload(view, plan)
}

func EncodeSettleDetachedSinkPayload(view MutationReplayView, plan SettleDetachedSinkPlan) ([]byte, error) {
	return encodeMutationPayload(view, plan)
}

func encodeMutationPayload[T any](view MutationReplayView, plan T) ([]byte, error) {
	payload := mutationPayload[T]{
		TemplateRef:        view.Aggregate.TemplateRef,
		TemplateSourceHash: view.Aggregate.TemplateSourceHash,
		Checkpoint:         view.Checkpoint,
		Plan:               plan,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxCommandPayloadBytes {
		return nil, &OverBudgetError{Limit: "payload_bytes", Value: len(data), Maximum: MaxCommandPayloadBytes}
	}
	return data, nil
}

func (p RoutePathsPlan) Validate() error {
	if p.SettlementCommandID == "" || p.SourceActivationID == "" || p.SourceGeneration == 0 || p.SourcePathID == "" || p.Attempt == 0 || p.CauseDigest == "" || p.ResultCode == "" {
		return fmt.Errorf("%w: route plan lacks complete command bindings", ErrMutationInvalid)
	}
	if !sortedUnique(p.ProducedPathIDs) {
		return fmt.Errorf("%w: produced path IDs are not sorted and unique", ErrMutationInvalid)
	}
	return p.Batch.Validate()
}

func (p ActivateGenerationPlan) Validate() error {
	if p.ReservationID == "" || p.Generation == 0 || p.InputDigest == "" || p.CauseDigest == "" || !p.JoinPolicy.Valid() {
		return fmt.Errorf("%w: activation plan lacks complete command bindings", ErrMutationInvalid)
	}
	if !sortedUnique(p.InputPathIDs) || !sortedUnique(p.LosingCandidateIDs) || !sortedUnique(p.PreArrivedLoserPathIDs) {
		return fmt.Errorf("%w: activation plan ID lists are not sorted and unique", ErrMutationInvalid)
	}
	if len(p.Candidates) == 0 || !sortedCandidateRecords(p.Candidates) || !sortedSlotRecords(p.PossibleSlots) {
		return fmt.Errorf("%w: activation candidates/slots are empty or noncanonical", ErrMutationInvalid)
	}
	if err := p.Batch.Validate(); err != nil {
		return err
	}
	switch p.JoinPolicy {
	case JoinAny:
		if p.WinnerPathID == "" || len(p.InputPathIDs) != 1 || p.InputPathIDs[0] != p.WinnerPathID || len(p.LosingCandidateIDs) != len(p.Candidates)-1 {
			return fmt.Errorf("%w: any activation lacks one winner and every loser", ErrMutationInvalid)
		}
		want, err := MutationCountAny(len(p.Candidates), len(p.PreArrivedLoserPathIDs))
		if err != nil {
			return fmt.Errorf("%w: %v", ErrMutationInvalid, err)
		}
		if len(p.Batch.Mutations) != want {
			return fmt.Errorf("%w: any mutation count %d, want %d", ErrMutationInvalid, len(p.Batch.Mutations), want)
		}
	case JoinAll, JoinExclusive:
		if p.WinnerPathID != "" || len(p.LosingCandidateIDs) != 0 || len(p.PreArrivedLoserPathIDs) != 0 {
			return fmt.Errorf("%w: non-any plan carries any-only fields", ErrMutationInvalid)
		}
	}
	return nil
}

func (p PropagateClosurePlan) Validate() error {
	if p.TargetReservationID == "" || p.TargetGeneration == 0 || p.InputDigest == "" || p.CauseDigest == "" || p.RootReservationID == "" || p.RootCandidateID == "" || p.RootCauseDigest == "" || len(p.Intents) == 0 {
		return fmt.Errorf("%w: propagation plan lacks complete bindings/intents", ErrMutationInvalid)
	}
	if _, err := normalizePropagationIntents(p.Intents); err != nil {
		return err
	}
	return p.Batch.Validate()
}

func (p SettleDetachedSinkPlan) Validate() error {
	if p.SourcePathID == "" || p.ReservationID == "" || p.Generation == 0 || p.DetachmentSetID == "" || p.DetachmentID == "" || p.ResultCode == "" {
		return fmt.Errorf("%w: detached-sink plan lacks complete bindings", ErrMutationInvalid)
	}
	if p.ResultCode != "detached" {
		return fmt.Errorf("%w: detached-sink result %q, want detached", ErrMutationInvalid, p.ResultCode)
	}
	if err := p.Batch.Validate(); err != nil {
		return err
	}
	mutation, ok := findMutation(p.Batch, MutationPath, p.SourcePathID)
	if len(p.Batch.Mutations) != 2 || !ok || len(mutation.Before) != 0 || len(mutation.After) == 0 {
		return fmt.Errorf("%w: detached-sink command must own exactly its routed parent update and arrived-to-sink child create", ErrMutationInvalid)
	}
	return nil
}

func sortedCandidateRecords(values []CandidateRecord) bool {
	for index, value := range values {
		if value.ID == "" || !sortedUnique(value.PossibleSlotIDs) || (index > 0 && values[index-1].ID >= value.ID) {
			return false
		}
	}
	return true
}

func sortedSlotRecords(values []PossibleSlotRecord) bool {
	for index, value := range values {
		if value.ID == "" || (index > 0 && values[index-1].ID >= value.ID) {
			return false
		}
	}
	return true
}

func normalizePropagationIntents(values []PropagationIntent) ([]PropagationIntent, error) {
	out := make([]PropagationIntent, 0, len(values))
	owners := map[string][]byte{}
	for index, intent := range values {
		if intent.ID == "" || intent.RootReservationID == "" || intent.RootCandidateID == "" || intent.RootCauseDigest == "" || intent.PlanDigest == "" || !intent.State.Valid() || intent.Cursor > uint32(len(intent.Frontier)) || !sortedUnique(intent.Frontier) {
			return nil, fmt.Errorf("%w: invalid propagation intent %q", ErrMutationInvalid, intent.ID)
		}
		planDigest, err := PropagationPlanIdentity(intent.RootReservationID, intent.RootCandidateID, intent.RootCauseDigest, uint64(intent.Shard), intent.Frontier)
		if err != nil || planDigest != intent.PlanDigest {
			return nil, fmt.Errorf("%w: propagation intent %q plan digest mismatch", ErrMutationInvalid, intent.ID)
		}
		id, err := PropagationIntentIdentity(intent.RootCauseDigest, uint64(intent.Shard), intent.PlanDigest)
		if err != nil || id != intent.ID {
			return nil, fmt.Errorf("%w: propagation intent %q identity mismatch", ErrMutationInvalid, intent.ID)
		}
		encoded, _ := json.Marshal(intent)
		owner := fmt.Sprintf("%s/%d", intent.RootCauseDigest, intent.Shard)
		if previous, duplicate := owners[owner]; duplicate {
			if !bytes.Equal(previous, encoded) {
				return nil, fmt.Errorf("%w: duplicate propagation shard has different bytes", ErrMutationInconsistent)
			}
			if index == 0 || comparePropagationIntent(values[index-1], intent) != 0 {
				return nil, fmt.Errorf("%w: identical duplicate propagation shard is not adjacent", ErrMutationInvalid)
			}
			continue
		}
		owners[owner] = encoded
		if index > 0 && comparePropagationIntent(values[index-1], intent) >= 0 {
			return nil, fmt.Errorf("%w: propagation intents are not in deterministic order", ErrMutationInvalid)
		}
		out = append(out, intent)
	}
	return out, nil
}

func comparePropagationIntent(a, b PropagationIntent) int {
	if value := cmp.Compare(a.RootCauseDigest, b.RootCauseDigest); value != 0 {
		return value
	}
	if value := cmp.Compare(a.Shard, b.Shard); value != 0 {
		return value
	}
	if value := cmp.Compare(a.Cursor, b.Cursor); value != 0 {
		return value
	}
	return cmp.Compare(a.ID, b.ID)
}

func ValidateMutationCommand(view MutationReplayView, command CommandRecord) error {
	switch command.Identity.Kind {
	case CommandRoutePaths:
		return ValidateRoutePathsCommand(view, command)
	case CommandActivateGeneration:
		return ValidateActivateGenerationCommand(view, command)
	case CommandPropagateCandidateClosure:
		return ValidatePropagateClosureCommand(view, command)
	case CommandSettleDetachedSink:
		return ValidateSettleDetachedSinkCommand(view, command)
	default:
		return fmt.Errorf("%w: command kind %q is not a mutation replay command", ErrMutationInvalid, command.Identity.Kind)
	}
}

func ReplayMutationCommand(view MutationReplayView, command CommandRecord) (MutationReplayResult, error) {
	switch command.Identity.Kind {
	case CommandRoutePaths:
		return ReplayRoutePaths(view, command)
	case CommandActivateGeneration:
		return ReplayActivateGeneration(view, command)
	case CommandPropagateCandidateClosure:
		return ReplayPropagateClosure(view, command)
	case CommandSettleDetachedSink:
		return ReplaySettleDetachedSink(view, command)
	default:
		return MutationReplayResult{}, fmt.Errorf("%w: command kind %q is not replayable here", ErrMutationInvalid, command.Identity.Kind)
	}
}

func ValidateRoutePathsCommand(view MutationReplayView, command CommandRecord) error {
	payload, err := decodeMutationCommand[RoutePathsPlan](view, command, CommandRoutePaths, true)
	if err != nil {
		return err
	}
	if err := payload.Plan.Validate(); err != nil {
		return err
	}
	id, plan := command.Identity, payload.Plan
	if id.SourceActivationID != plan.SourceActivationID || id.SourceGeneration != plan.SourceGeneration || id.SourcePathID != plan.SourcePathID || id.Attempt != plan.Attempt || id.InputDigest != plan.SettlementCommandID || id.CauseDigest != plan.CauseDigest || id.ResultCode != plan.ResultCode {
		return fmt.Errorf("%w: route command identity differs from typed plan", ErrMutationInvalid)
	}
	settlement, ok := view.Aggregate.Commands[plan.SettlementCommandID]
	if !ok || settlement.Identity.Kind != CommandSettleAttempt || settlement.Identity.SourceActivationID != plan.SourceActivationID || settlement.Identity.SourceGeneration != plan.SourceGeneration || settlement.Identity.Attempt != plan.Attempt || (settlement.State != CommandObserved && settlement.State != CommandReconciled) {
		return fmt.Errorf("%w: route plan does not bind its exact observed settlement", ErrMutationInvalid)
	}
	outcome, exact := exactSettlementResult(plan.ResultCode)
	if !exact || settlement.Identity.ResultCode != outcome {
		return fmt.Errorf("%w: route result does not conserve settlement result", ErrMutationInvalid)
	}
	pre, err := payload.Plan.Batch.preState(view.Aggregate.Routing, command.ID)
	if err != nil {
		return err
	}
	path, ok := pre.Paths[plan.SourcePathID]
	if !ok || path.State != PathLive || path.SourceActivation.ID != plan.SourceActivationID || path.SourceActivation.Generation != plan.SourceGeneration {
		return fmt.Errorf("%w: route source is not the bound live activation output", ErrMutationInvalid)
	}
	mutation, ok := findMutation(plan.Batch, MutationPath, plan.SourcePathID)
	if !ok || len(mutation.Before) == 0 || len(mutation.After) == 0 {
		return fmt.Errorf("%w: route plan does not update its source path", ErrMutationInvalid)
	}
	var after PathRecord
	if err := decodeExactPayload(mutation.After, &after); err != nil {
		return err
	}
	if after.State != PathRouted && after.State != PathSplit && !after.State.TerminalNonSuccess() && after.State != PathEnded {
		return fmt.Errorf("%w: invalid routed source post-state %q", ErrMutationInvalid, after.State)
	}
	if !slices.Equal(after.ProducedPathIDs, plan.ProducedPathIDs) || after.Disposition == nil || after.Disposition.CommandID != MutationCommandPlaceholder || after.UpdatedSeq != plan.Batch.EventSeq {
		return fmt.Errorf("%w: route post-state differs from exact output/command/event plan", ErrMutationInvalid)
	}
	return validateRouteMutationSet(pre, plan, after)
}

func ValidateActivateGenerationCommand(view MutationReplayView, command CommandRecord) error {
	payload, err := decodeMutationCommand[ActivateGenerationPlan](view, command, CommandActivateGeneration, true)
	if err != nil {
		return err
	}
	plan := payload.Plan
	if err := plan.Validate(); err != nil {
		return err
	}
	id := command.Identity
	if id.TargetReservationID != plan.ReservationID || id.TargetGeneration != plan.Generation || id.InputDigest != plan.InputDigest || id.CauseDigest != plan.CauseDigest {
		return fmt.Errorf("%w: activation command identity differs from typed plan", ErrMutationInvalid)
	}
	pre, err := plan.Batch.preState(view.Aggregate.Routing, command.ID)
	if err != nil {
		return err
	}
	reservation, ok := pre.Reservations[plan.ReservationID]
	if !ok || reservation.State != ReservationOpen || reservation.Generation != plan.Generation || reservation.JoinPolicy != plan.JoinPolicy || !canonicalEqual(reservation.Candidates, plan.Candidates) || !canonicalEqual(reservation.PossibleSlots, plan.PossibleSlots) {
		return fmt.Errorf("%w: activation plan does not carry byte-exact open reservation candidates/slots", ErrMutationInvalid)
	}
	fold, arrivals, causeDigest, err := activationFold(pre, reservation)
	if err != nil {
		return err
	}
	if fold != plan.InputDigest || causeDigest != plan.CauseDigest {
		return fmt.Errorf("%w: activation input/cause digest differs from exact candidate fold", ErrMutationInvalid)
	}
	afterMutation, ok := findMutation(plan.Batch, MutationReservation, plan.ReservationID)
	if !ok || len(afterMutation.After) == 0 {
		return fmt.Errorf("%w: activation plan does not transition its reservation", ErrMutationInvalid)
	}
	var afterReservation ActivationReservation
	if err := decodeExactPayload(afterMutation.After, &afterReservation); err != nil {
		return err
	}
	switch afterReservation.State {
	case ReservationActivated:
		if plan.JoinPolicy != JoinAny && !slices.Equal(plan.InputPathIDs, arrivals) {
			return fmt.Errorf("%w: activation inputs differ from exact arrived candidate set", ErrMutationInvalid)
		}
	case ReservationClosedNoActivation:
		if !slices.Equal(plan.InputPathIDs, arrivals) {
			return fmt.Errorf("%w: close inputs differ from exact arrived candidate set", ErrMutationInvalid)
		}
	default:
		return fmt.Errorf("%w: reservation post-state is not terminal", ErrMutationInvalid)
	}
	if err := validateActivationMutationSet(pre, plan, reservation, afterReservation); err != nil {
		return err
	}
	if plan.JoinPolicy == JoinAny {
		return validateAnyPlan(pre, plan)
	}
	return nil
}

func activationFold(pre RoutingState, reservation ActivationReservation) (string, []PathID, CauseDigest, error) {
	entries := make([]CandidateFoldEntry, 0, len(reservation.Candidates))
	arrivals := make([]PathID, 0)
	causeIDs := make([]CauseID, 0)
	for _, candidate := range reservation.Candidates {
		candidateArrivals := make([]PathID, 0, 1)
		for _, path := range pre.Paths {
			if path.Kind == PathEdge && path.State == PathArrived && path.TargetReservationID == reservation.ID && path.CandidateID == candidate.ID {
				candidateArrivals = append(candidateArrivals, path.ID)
			}
		}
		slices.Sort(candidateArrivals)
		if len(candidateArrivals) > 1 {
			return "", nil, "", fmt.Errorf("%w: candidate %q has duplicate arrivals", ErrMutationInconsistent, candidate.ID)
		}
		if len(candidateArrivals) == 1 {
			entries = append(entries, CandidateFoldEntry{CandidateID: candidate.ID, FoldKind: "arrived", PathOrClosureID: candidateArrivals[0]})
			arrivals = append(arrivals, candidateArrivals[0])
			continue
		}
		key, err := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
		if err != nil {
			return "", nil, "", err
		}
		closure, ok := pre.CandidateClosures[key]
		if !ok {
			entries = append(entries, CandidateFoldEntry{CandidateID: candidate.ID, FoldKind: CandidateFoldOpen})
			continue
		}
		entries = append(entries, CandidateFoldEntry{CandidateID: candidate.ID, FoldKind: string(closure.TerminalKind), PathOrClosureID: closure.ID})
		set, ok := pre.CauseSets[closure.CauseDigest]
		if !ok {
			return "", nil, "", fmt.Errorf("%w: closure %q cause set missing", ErrMutationInconsistent, closure.ID)
		}
		causeIDs = append(causeIDs, set.CauseIDs...)
	}
	slices.Sort(arrivals)
	fold, err := CandidateFoldIdentity(entries)
	if err != nil {
		return "", nil, "", err
	}
	causeDigest, err := CauseSetIdentity(causeIDs)
	if err != nil {
		return "", nil, "", err
	}
	return fold, arrivals, causeDigest, nil
}

func validateAnyPlan(pre RoutingState, plan ActivateGenerationPlan) error {
	winner, ok := pre.Paths[plan.WinnerPathID]
	if !ok || winner.State != PathArrived || winner.TargetReservationID != plan.ReservationID {
		return fmt.Errorf("%w: any winner is not an arrived reservation path", ErrMutationInvalid)
	}
	arrivals := make([]PathRecord, 0)
	for _, path := range pre.Paths {
		if path.State == PathArrived && path.TargetReservationID == plan.ReservationID {
			arrivals = append(arrivals, path)
		}
	}
	slices.SortFunc(arrivals, func(a, b PathRecord) int {
		if value := cmp.Compare(a.ArrivedSeq, b.ArrivedSeq); value != 0 {
			return value
		}
		return cmp.Compare(a.ID, b.ID)
	})
	if len(arrivals) == 0 || arrivals[0].ID != winner.ID {
		return fmt.Errorf("%w: any winner is not minimum committed arrival", ErrMutationInvalid)
	}
	losers := make([]CandidateID, 0, len(plan.Candidates)-1)
	for _, candidate := range plan.Candidates {
		if candidate.ID != winner.CandidateID {
			losers = append(losers, candidate.ID)
		}
	}
	if !slices.Equal(losers, plan.LosingCandidateIDs) {
		return fmt.Errorf("%w: any losing candidate set is incomplete", ErrMutationInvalid)
	}
	preArrived := make([]PathID, 0)
	for _, arrival := range arrivals {
		if arrival.ID != winner.ID {
			preArrived = append(preArrived, arrival.ID)
		}
	}
	slices.Sort(preArrived)
	if !slices.Equal(preArrived, plan.PreArrivedLoserPathIDs) {
		return fmt.Errorf("%w: any pre-arrived loser set differs from committed pre-state", ErrMutationInvalid)
	}
	for _, candidateID := range losers {
		key, _ := DetachmentKeyIdentity(plan.ReservationID, candidateID)
		mutation, ok := findMutation(plan.Batch, MutationDetachment, key)
		if !ok || len(mutation.Before) != 0 || len(mutation.After) == 0 {
			return fmt.Errorf("%w: any loser %q lacks exact detachment create", ErrMutationInvalid, candidateID)
		}
		var detachment DetachmentRecord
		if err := decodeExactPayload(mutation.After, &detachment); err != nil {
			return err
		}
		if detachment.CandidateID != candidateID || detachment.WinnerPathID != winner.ID || detachment.CommandID != MutationCommandPlaceholder || detachment.EventSeq != plan.Batch.EventSeq || detachment.ActivatedSeq != plan.Batch.EventSeq {
			return fmt.Errorf("%w: any detachment bytes differ from winner event", ErrMutationInvalid)
		}
		setID, _ := DetachmentSetIdentity("", detachment.ID)
		setMutation, ok := findMutation(plan.Batch, MutationDetachmentSet, setID)
		if !ok || len(setMutation.Before) != 0 || len(setMutation.After) == 0 {
			return fmt.Errorf("%w: any loser %q lacks root detachment set", ErrMutationInvalid, candidateID)
		}
	}
	for _, pathID := range preArrived {
		mutation, ok := findMutation(plan.Batch, MutationPath, pathID)
		if !ok || len(mutation.Before) == 0 || len(mutation.After) == 0 {
			return fmt.Errorf("%w: pre-arrived loser %q is not atomically sunk", ErrMutationInvalid, pathID)
		}
		var sink PathRecord
		if err := decodeExactPayload(mutation.After, &sink); err != nil {
			return err
		}
		if sink.State != PathDetachedSink || sink.Disposition == nil || sink.DetachedSink == nil || sink.Disposition.CommandID != MutationCommandPlaceholder || sink.Disposition.ReasonCode != "pre_arrived_any_loser" || sink.DetachedSink.CommandID != MutationCommandPlaceholder || sink.DetachedSink.ReasonCode != "pre_arrived_any_loser" || sink.UpdatedSeq != plan.Batch.EventSeq {
			return fmt.Errorf("%w: pre-arrived loser %q sink bytes are incomplete", ErrMutationInvalid, pathID)
		}
	}
	return nil
}

func ValidatePropagateClosureCommand(view MutationReplayView, command CommandRecord) error {
	payload, err := decodeMutationCommand[PropagateClosurePlan](view, command, CommandPropagateCandidateClosure, true)
	if err != nil {
		return err
	}
	plan := payload.Plan
	if err := plan.Validate(); err != nil {
		return err
	}
	id := command.Identity
	if id.SourcePathID != plan.SourcePathID || id.TargetReservationID != plan.TargetReservationID || id.TargetGeneration != plan.TargetGeneration || id.InputDigest != plan.InputDigest || id.CauseDigest != plan.CauseDigest {
		return fmt.Errorf("%w: propagation command identity differs from typed plan", ErrMutationInvalid)
	}
	if plan.CauseDigest != plan.RootCauseDigest {
		return fmt.Errorf("%w: propagation cause differs from root cause union", ErrMutationInvalid)
	}
	pre, err := plan.Batch.preState(view.Aggregate.Routing, command.ID)
	if err != nil {
		return err
	}
	root, ok := pre.Reservations[plan.RootReservationID]
	if !ok || root.Generation != plan.TargetGeneration {
		return fmt.Errorf("%w: propagation root reservation/generation missing", ErrMutationInvalid)
	}
	if _, ok := slices.BinarySearchFunc(root.Candidates, plan.RootCandidateID, func(candidate CandidateRecord, id CandidateID) int { return cmp.Compare(candidate.ID, id) }); !ok {
		return fmt.Errorf("%w: propagation root candidate is not reserved", ErrMutationInvalid)
	}
	target, ok := pre.Reservations[plan.TargetReservationID]
	if !ok || target.Generation != plan.TargetGeneration {
		return fmt.Errorf("%w: propagation first target reservation/generation missing", ErrMutationInvalid)
	}
	if plan.SourcePathID != "" {
		if _, ok := pre.Paths[plan.SourcePathID]; !ok {
			return fmt.Errorf("%w: propagation source path missing", ErrMutationInvalid)
		}
	}
	intents, err := normalizePropagationIntents(plan.Intents)
	if err != nil {
		return err
	}
	if len(intents) > MaxPropagationShards {
		return fmt.Errorf("%w: propagation shard count %d exceeds %d", ErrMutationInvalid, len(intents), MaxPropagationShards)
	}
	for _, intent := range intents {
		if intent.RootReservationID != plan.RootReservationID || intent.RootCandidateID != plan.RootCandidateID || intent.RootCauseDigest != plan.RootCauseDigest || intent.EventSeq != plan.Batch.EventSeq {
			return fmt.Errorf("%w: propagation intent differs from root/command/event", ErrMutationInvalid)
		}
		mutation, ok := findMutation(plan.Batch, MutationPropagation, intent.ID)
		if !ok || !bytes.Equal(mutation.After, mustMarshal(intent)) {
			return fmt.Errorf("%w: propagation intent %q missing from exact batch", ErrMutationInvalid, intent.ID)
		}
		if len(mutation.Before) == 0 && intent.CommandID != MutationCommandPlaceholder {
			return fmt.Errorf("%w: created propagation intent lacks command sentinel", ErrMutationInvalid)
		}
		if len(mutation.Before) > 0 {
			var before PropagationIntent
			if err := decodeExactPayload(mutation.Before, &before); err != nil || intent.CommandID != before.CommandID {
				return fmt.Errorf("%w: resumed propagation intent changes command authority", ErrMutationInvalid)
			}
		}
	}
	return validatePropagationMutationSet(pre, plan, intents)
}

func ValidateSettleDetachedSinkCommand(view MutationReplayView, command CommandRecord) error {
	payload, err := decodeMutationCommand[SettleDetachedSinkPlan](view, command, CommandSettleDetachedSink, true)
	if err != nil {
		return err
	}
	plan := payload.Plan
	if err := plan.Validate(); err != nil {
		return err
	}
	id := command.Identity
	if id.SourcePathID != plan.SourcePathID || id.TargetReservationID != plan.ReservationID || id.TargetGeneration != plan.Generation || id.InputDigest != plan.DetachmentSetID || id.CauseDigest != plan.CauseDigest || id.ResultCode != plan.ResultCode {
		return fmt.Errorf("%w: detached-sink command identity differs from typed plan", ErrMutationInvalid)
	}
	pre, err := plan.Batch.preState(view.Aggregate.Routing, command.ID)
	if err != nil {
		return err
	}
	if _, exists := pre.Paths[plan.SourcePathID]; exists {
		return fmt.Errorf("%w: detached sink source is partially present before its atomic create", ErrMutationInconsistent)
	}
	reservation, ok := pre.Reservations[plan.ReservationID]
	if !ok || reservation.State != ReservationActivated || reservation.Generation != plan.Generation {
		return fmt.Errorf("%w: detached sink reservation is not closed by activation", ErrMutationInvalid)
	}
	childMutation, _ := findMutation(plan.Batch, MutationPath, plan.SourcePathID)
	var after PathRecord
	if err := decodeExactPayload(childMutation.After, &after); err != nil {
		return err
	}
	parentMutation, ok := findMutation(plan.Batch, MutationPath, after.ParentPathID)
	if !ok || len(parentMutation.Before) == 0 || len(parentMutation.After) == 0 {
		return fmt.Errorf("%w: detached sink lacks its atomic parent route transition", ErrMutationInvalid)
	}
	var parentAfter PathRecord
	if err := decodeExactPayload(parentMutation.After, &parentAfter); err != nil {
		return err
	}
	if parentAfter.State != PathRouted || !slices.Equal(parentAfter.ProducedPathIDs, []PathID{after.ID}) || parentAfter.Disposition == nil || parentAfter.Disposition.CommandID != MutationCommandPlaceholder || parentAfter.UpdatedSeq != plan.Batch.EventSeq {
		return fmt.Errorf("%w: detached sink parent route differs from the exact child event", ErrMutationInvalid)
	}
	key, _ := DetachmentKeyIdentity(plan.ReservationID, after.CandidateID)
	detachment, ok := pre.Detachments[key]
	if !ok || detachment.ID != plan.DetachmentID || !detachmentSetContainsExact(pre, plan.DetachmentSetID, plan.DetachmentID) {
		return fmt.Errorf("%w: detached sink lacks exact detachment/set authority", ErrMutationInvalid)
	}
	if after.ID != plan.SourcePathID || after.Kind != PathEdge || after.State != PathDetachedSink || after.SourceActivation.Generation != plan.Generation || after.TargetReservationID != plan.ReservationID || after.DetachmentSetID != plan.DetachmentSetID || after.Disposition == nil || after.DetachedSink == nil || after.Disposition.FromState != PathArrived || after.Disposition.CommandID != MutationCommandPlaceholder || after.Disposition.ReasonCode != "late_any_arrival" || after.DetachedSink.CommandID != MutationCommandPlaceholder || after.DetachedSink.DetachmentID != plan.DetachmentID || after.DetachedSink.ReasonCode != "late_any_arrival" || after.ArrivedSeq != plan.Batch.EventSeq || after.CreatedSeq != plan.Batch.EventSeq || after.UpdatedSeq != plan.Batch.EventSeq {
		return fmt.Errorf("%w: detached sink post-state differs from exact late-arrival transition", ErrMutationInvalid)
	}
	return nil
}

func decodeMutationCommand[T any](view MutationReplayView, command CommandRecord, kind CommandKindV1, bindPlanDigest bool) (mutationPayload[T], error) {
	var payload mutationPayload[T]
	// MutationBatch validation occurs inside each typed Plan.Validate before
	// any replay classification. Primitive command validation precedes decode.
	if err := ValidateCommand(command); err != nil {
		return payload, fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	if command.Identity.Kind != kind || command.State != CommandObserved {
		return payload, fmt.Errorf("%w: command must be observed %s", ErrMutationInvalid, kind)
	}
	if view.Aggregate.Routing == nil || view.Aggregate.Commands == nil {
		return payload, fmt.Errorf("%w: incomplete aggregate replay view", ErrMutationInvalid)
	}
	stored, ok := view.Aggregate.Commands[command.ID]
	if !ok || !canonicalEqual(stored, command) {
		return payload, fmt.Errorf("%w: command is not byte-exact in aggregate", ErrMutationInvalid)
	}
	if err := view.Checkpoint.Validate(); err != nil {
		return payload, fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	if err := decodeExactPayload(command.Payload, &payload); err != nil {
		return payload, fmt.Errorf("%w: typed command payload: %v", ErrMutationInvalid, err)
	}
	if payload.TemplateRef == "" || payload.TemplateSourceHash == "" || payload.TemplateRef != view.Aggregate.TemplateRef || payload.TemplateSourceHash != view.Aggregate.TemplateSourceHash || payload.Checkpoint != view.Checkpoint {
		return payload, fmt.Errorf("%w: template/checkpoint binding mismatch", ErrMutationInvalid)
	}
	if bindPlanDigest && command.Identity.PlanDigest != payloadDigest(command.Payload) {
		return payload, fmt.Errorf("%w: command plan digest does not bind exact typed payload", ErrMutationInvalid)
	}
	return payload, nil
}

func ReplayRoutePaths(view MutationReplayView, command CommandRecord) (MutationReplayResult, error) {
	if err := ValidateRoutePathsCommand(view, command); err != nil {
		return MutationReplayResult{}, err
	}
	var payload mutationPayload[RoutePathsPlan]
	_ = decodeExactPayload(command.Payload, &payload)
	return replayValidatedMutation(view, command.ID, payload.Plan.Batch)
}

func ReplayActivateGeneration(view MutationReplayView, command CommandRecord) (MutationReplayResult, error) {
	if err := ValidateActivateGenerationCommand(view, command); err != nil {
		return MutationReplayResult{}, err
	}
	var payload mutationPayload[ActivateGenerationPlan]
	_ = decodeExactPayload(command.Payload, &payload)
	return replayValidatedMutation(view, command.ID, payload.Plan.Batch)
}

func ReplayPropagateClosure(view MutationReplayView, command CommandRecord) (MutationReplayResult, error) {
	if err := ValidatePropagateClosureCommand(view, command); err != nil {
		return MutationReplayResult{}, err
	}
	var payload mutationPayload[PropagateClosurePlan]
	_ = decodeExactPayload(command.Payload, &payload)
	return replayValidatedMutation(view, command.ID, payload.Plan.Batch)
}

func ReplaySettleDetachedSink(view MutationReplayView, command CommandRecord) (MutationReplayResult, error) {
	if err := ValidateSettleDetachedSinkCommand(view, command); err != nil {
		return MutationReplayResult{}, err
	}
	var payload mutationPayload[SettleDetachedSinkPlan]
	_ = decodeExactPayload(command.Payload, &payload)
	return replayValidatedMutation(view, command.ID, payload.Plan.Batch)
}

func replayValidatedMutation(view MutationReplayView, commandID string, batch MutationBatch) (MutationReplayResult, error) {
	routing, disposition, err := batch.replay(view.Aggregate.Routing, commandID)
	if err != nil {
		return MutationReplayResult{}, err
	}
	post := view.Aggregate
	post.Routing = &routing
	encoded, err := Encode(&routing)
	if err != nil {
		return MutationReplayResult{}, err
	}
	post.CheckpointBytes = len(encoded)
	report := ValidateAggregate(post)
	if !report.Valid() {
		message := "aggregate invariant failed"
		if len(report.Diagnostics) > 0 {
			message = report.Diagnostics[0].Code + ": " + report.Diagnostics[0].Message
		}
		return MutationReplayResult{}, fmt.Errorf("%w: post-application %s", ErrMutationInconsistent, message)
	}
	return MutationReplayResult{Routing: routing, Disposition: disposition}, nil
}

func (b MutationBatch) preState(current *RoutingState, commandID string) (RoutingState, error) {
	if err := b.Validate(); err != nil {
		return RoutingState{}, err
	}
	if err := rejectRoutingSentinel(current); err != nil {
		return RoutingState{}, err
	}
	materialized, err := b.materialize(commandID)
	if err != nil {
		return RoutingState{}, err
	}
	digest, err := RoutingDigest(current)
	if err != nil {
		return RoutingState{}, err
	}
	switch digest {
	case b.BeforeDigest:
		if err := requireMutationSide(current, b.Mutations, true); err != nil {
			return RoutingState{}, err
		}
		if err := b.validateTemplateDigest(current); err != nil {
			return RoutingState{}, err
		}
		return Clone(*current), nil
	default:
		if err := requireMutationSide(current, materialized, false); err != nil {
			return RoutingState{}, err
		}
		pre := Clone(*current)
		for index := len(materialized) - 1; index >= 0; index-- {
			mutation := materialized[index]
			reverse := RecordMutation{Kind: mutation.Kind, Key: mutation.Key, Before: mutation.After, After: mutation.Before}
			if err := applyRecordMutation(&pre, reverse); err != nil {
				return RoutingState{}, err
			}
		}
		got, err := RoutingDigest(&pre)
		if err != nil {
			return RoutingState{}, err
		}
		if got != b.BeforeDigest {
			return RoutingState{}, fmt.Errorf("%w: reverse replay does not reproduce complete pre-state", ErrMutationInconsistent)
		}
		if err := b.validateTemplateDigest(&pre); err != nil {
			return RoutingState{}, err
		}
		return pre, nil
	}
}

func findMutation(batch MutationBatch, kind MutationRecordKind, key string) (RecordMutation, bool) {
	index, ok := slices.BinarySearchFunc(batch.Mutations, RecordMutation{Kind: kind, Key: key}, compareMutation)
	if !ok {
		return RecordMutation{}, false
	}
	return batch.Mutations[index], true
}

func canonicalEqual[T any](left, right T) bool {
	leftBytes, leftErr := json.Marshal(left)
	rightBytes, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && bytes.Equal(leftBytes, rightBytes)
}

func mustMarshal(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func detachmentSetContainsExact(st RoutingState, setID DetachmentSetID, detachmentID DetachmentID) bool {
	seen := map[DetachmentSetID]struct{}{}
	for setID != "" {
		if _, duplicate := seen[setID]; duplicate {
			return false
		}
		seen[setID] = struct{}{}
		set, ok := st.DetachmentSets[setID]
		if !ok {
			return false
		}
		if set.DetachmentID == detachmentID {
			return true
		}
		setID = set.ParentSetID
	}
	return false
}
