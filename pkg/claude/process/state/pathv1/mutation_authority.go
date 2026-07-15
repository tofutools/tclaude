package pathv1

import (
	"fmt"
	"slices"
)

type mutationTarget struct {
	kind MutationRecordKind
	key  string
}

func validateExactMutationTargets(batch MutationBatch, allowed map[mutationTarget]struct{}) error {
	if len(batch.Mutations) != len(allowed) {
		return fmt.Errorf("%w: batch has %d mutations, exact authorized set has %d", ErrMutationInvalid, len(batch.Mutations), len(allowed))
	}
	for _, mutation := range batch.Mutations {
		if _, ok := allowed[mutationTarget{kind: mutation.Kind, key: mutation.Key}]; !ok {
			return fmt.Errorf("%w: mutation %s/%s is outside the exact authorized set", ErrMutationInvalid, mutation.Kind, mutation.Key)
		}
	}
	return nil
}

func validateRouteMutationSet(pre RoutingState, plan RoutePathsPlan, sourceAfter PathRecord) error {
	allowed := map[mutationTarget]struct{}{{kind: MutationPath, key: plan.SourcePathID}: {}}
	producedScopes := map[ScopeID]struct{}{}
	producedReservations := map[ReservationID]struct{}{}
	for _, pathID := range plan.ProducedPathIDs {
		mutation, ok := findMutation(plan.Batch, MutationPath, pathID)
		if !ok || len(mutation.Before) != 0 || len(mutation.After) == 0 {
			return fmt.Errorf("%w: produced path %q is not an exact create", ErrMutationInvalid, pathID)
		}
		var path PathRecord
		if err := decodeExactPayload(mutation.After, &path); err != nil {
			return err
		}
		if path.ParentPathID != plan.SourcePathID {
			return fmt.Errorf("%w: produced path %q is not a child of the routed source", ErrMutationInvalid, pathID)
		}
		if path.Kind == PathImpossibleEdge {
			if path.ImpossibleCauseDigest == "" {
				return fmt.Errorf("%w: produced impossible path %q lacks its exact cause set", ErrMutationInvalid, pathID)
			}
			if err := authorizeCauseSet(pre, plan.Batch, allowed, path.ImpossibleCauseDigest); err != nil {
				return err
			}
		}
		allowed[mutationTarget{kind: MutationPath, key: pathID}] = struct{}{}
		if _, exists := pre.Scopes[path.ScopeID]; !exists && path.ScopeID != "" {
			producedScopes[path.ScopeID] = struct{}{}
		}
		if _, exists := pre.Reservations[path.TargetReservationID]; !exists && path.TargetReservationID != "" {
			producedReservations[path.TargetReservationID] = struct{}{}
		}
	}
	for scopeID := range producedScopes {
		mutation, ok := findMutation(plan.Batch, MutationScope, scopeID)
		if !ok || len(mutation.Before) != 0 || len(mutation.After) == 0 {
			return fmt.Errorf("%w: produced scope %q is not an exact create", ErrMutationInvalid, scopeID)
		}
		var scope ScopeRecord
		if err := decodeExactPayload(mutation.After, &scope); err != nil {
			return err
		}
		if scope.ForkOutputPathID != plan.SourcePathID {
			return fmt.Errorf("%w: produced scope %q is not owned by the routed source", ErrMutationInvalid, scopeID)
		}
		allowed[mutationTarget{kind: MutationScope, key: scopeID}] = struct{}{}
		if _, exists := pre.Reservations[scope.JoinReservationID]; !exists && scope.JoinReservationID != "" {
			producedReservations[scope.JoinReservationID] = struct{}{}
		}
	}
	for reservationID := range producedReservations {
		mutation, ok := findMutation(plan.Batch, MutationReservation, reservationID)
		if !ok || len(mutation.Before) != 0 || len(mutation.After) == 0 {
			return fmt.Errorf("%w: produced reservation %q is not an exact create", ErrMutationInvalid, reservationID)
		}
		allowed[mutationTarget{kind: MutationReservation, key: reservationID}] = struct{}{}
	}
	if sourceAfter.ImpossibleCauseDigest != "" {
		if err := authorizeCauseSet(pre, plan.Batch, allowed, sourceAfter.ImpossibleCauseDigest); err != nil {
			return err
		}
	}
	if sourceAfter.TerminalCauseID != "" {
		if _, exists := pre.CauseRecords[sourceAfter.TerminalCauseID]; !exists {
			mutation, ok := findMutation(plan.Batch, MutationCauseRecord, sourceAfter.TerminalCauseID)
			if !ok || len(mutation.Before) != 0 || len(mutation.After) == 0 {
				return fmt.Errorf("%w: routed terminal cause %q is not an exact create", ErrMutationInvalid, sourceAfter.TerminalCauseID)
			}
			allowed[mutationTarget{kind: MutationCauseRecord, key: sourceAfter.TerminalCauseID}] = struct{}{}
		}
	}
	if err := validateExactMutationTargets(plan.Batch, allowed); err != nil {
		return err
	}
	return validateRouteEventSeq(plan.Batch)
}

func authorizeCauseSet(pre RoutingState, batch MutationBatch, allowed map[mutationTarget]struct{}, digest CauseDigest) error {
	set, exists := pre.CauseSets[digest]
	if !exists {
		mutation, ok := findMutation(batch, MutationCauseSet, digest)
		if !ok || len(mutation.Before) != 0 || len(mutation.After) == 0 {
			return fmt.Errorf("%w: cause set %q is not an exact create", ErrMutationInvalid, digest)
		}
		if err := decodeExactPayload(mutation.After, &set); err != nil {
			return err
		}
		allowed[mutationTarget{kind: MutationCauseSet, key: digest}] = struct{}{}
	}
	wantDigest, err := CauseSetIdentity(set.CauseIDs)
	if err != nil || wantDigest != digest {
		return fmt.Errorf("%w: cause set %q bytes do not match its digest", ErrMutationInvalid, digest)
	}
	for _, causeID := range set.CauseIDs {
		if _, exists := pre.CauseRecords[causeID]; exists {
			continue
		}
		mutation, ok := findMutation(batch, MutationCauseRecord, causeID)
		if !ok || len(mutation.Before) != 0 || len(mutation.After) == 0 {
			return fmt.Errorf("%w: cause record %q is not an exact create", ErrMutationInvalid, causeID)
		}
		allowed[mutationTarget{kind: MutationCauseRecord, key: causeID}] = struct{}{}
	}
	return nil
}

func validateRouteEventSeq(batch MutationBatch) error {
	want := batch.EventSeq
	for _, mutation := range batch.Mutations {
		if len(mutation.After) == 0 {
			continue
		}
		switch mutation.Kind {
		case MutationPath:
			var record PathRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.UpdatedSeq != want || (len(mutation.Before) == 0 && record.CreatedSeq != want) || (record.Disposition != nil && record.Disposition.CommandID == MutationCommandPlaceholder && record.Disposition.EventSeq != want) {
				return routeEventSeqError(mutation, want)
			}
		case MutationScope:
			var record ScopeRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if len(mutation.Before) != 0 || record.EventSeq != want {
				return routeEventSeqError(mutation, want)
			}
		case MutationReservation:
			var record ActivationReservation
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if len(mutation.Before) != 0 || record.EventSeq != want || (record.CloseReceipt != nil && record.CloseReceipt.EventSeq != want) {
				return routeEventSeqError(mutation, want)
			}
		case MutationCauseRecord:
			var record CauseRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if len(mutation.Before) != 0 || record.EventSeq != want || record.SourceCommandID != MutationCommandPlaceholder {
				return routeEventSeqError(mutation, want)
			}
		}
	}
	return nil
}

func routeEventSeqError(mutation RecordMutation, want int64) error {
	return fmt.Errorf("%w: route-owned post record %s/%s is not coupled to reducer event %d", ErrMutationInvalid, mutation.Kind, mutation.Key, want)
}

func validateActivationMutationSet(pre RoutingState, plan ActivateGenerationPlan, before, after ActivationReservation) error {
	allowed := map[mutationTarget]struct{}{{kind: MutationReservation, key: plan.ReservationID}: {}}
	pathIDs := append([]PathID(nil), plan.InputPathIDs...)
	pathIDs = append(pathIDs, plan.PreArrivedLoserPathIDs...)
	if after.State == ReservationActivated {
		if after.Activation == nil {
			return fmt.Errorf("%w: activated reservation lacks activation reference", ErrMutationInvalid)
		}
		mutation, ok := findMutation(plan.Batch, MutationActivation, after.Activation.ID)
		if !ok || len(mutation.Before) != 0 || len(mutation.After) == 0 {
			return fmt.Errorf("%w: activation %q is not an exact create", ErrMutationInvalid, after.Activation.ID)
		}
		var activation ActivationRecord
		if err := decodeExactPayload(mutation.After, &activation); err != nil {
			return err
		}
		if activation.ReservationID != plan.ReservationID || !slices.Equal(activation.InputPathIDs, plan.InputPathIDs) {
			return fmt.Errorf("%w: activation record differs from exact reservation inputs", ErrMutationInvalid)
		}
		allowed[mutationTarget{kind: MutationActivation, key: activation.ID}] = struct{}{}
		if activation.OutputPathID != "" {
			pathIDs = append(pathIDs, activation.OutputPathID)
		}
	}
	for _, pathID := range pathIDs {
		if _, ok := findMutation(plan.Batch, MutationPath, pathID); !ok {
			return fmt.Errorf("%w: activation-owned path %q is missing", ErrMutationInvalid, pathID)
		}
		allowed[mutationTarget{kind: MutationPath, key: pathID}] = struct{}{}
	}
	for _, candidateID := range plan.LosingCandidateIDs {
		key, _ := DetachmentKeyIdentity(plan.ReservationID, candidateID)
		mutation, ok := findMutation(plan.Batch, MutationDetachment, key)
		if !ok || len(mutation.Before) != 0 || len(mutation.After) == 0 {
			return fmt.Errorf("%w: losing candidate %q lacks its exact detachment", ErrMutationInvalid, candidateID)
		}
		var detachment DetachmentRecord
		if err := decodeExactPayload(mutation.After, &detachment); err != nil {
			return err
		}
		allowed[mutationTarget{kind: MutationDetachment, key: key}] = struct{}{}
		setID, _ := DetachmentSetIdentity("", detachment.ID)
		if _, ok := findMutation(plan.Batch, MutationDetachmentSet, setID); !ok {
			return fmt.Errorf("%w: detachment %q lacks its exact root set", ErrMutationInvalid, detachment.ID)
		}
		allowed[mutationTarget{kind: MutationDetachmentSet, key: setID}] = struct{}{}
	}
	if before.IsReducing {
		mutation, ok := findMutation(plan.Batch, MutationScope, before.ReducesScopeID)
		if !ok || len(mutation.Before) == 0 || len(mutation.After) == 0 {
			return fmt.Errorf("%w: reducing activation lacks its exact scope transition", ErrMutationInvalid)
		}
		allowed[mutationTarget{kind: MutationScope, key: before.ReducesScopeID}] = struct{}{}
	}
	if after.CauseDigest != "" {
		if err := authorizeCauseSet(pre, plan.Batch, allowed, after.CauseDigest); err != nil {
			return err
		}
	}
	if err := validateExactMutationTargets(plan.Batch, allowed); err != nil {
		return err
	}
	return validateActivationEventSeq(plan.Batch)
}

func validateActivationEventSeq(batch MutationBatch) error {
	want := batch.EventSeq
	for _, mutation := range batch.Mutations {
		if len(mutation.After) == 0 {
			continue
		}
		switch mutation.Kind {
		case MutationPath:
			var record PathRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.UpdatedSeq != want || (len(mutation.Before) == 0 && record.CreatedSeq != want) {
				return activationEventSeqError(mutation, want)
			}
			if record.Disposition != nil && record.Disposition.CommandID == MutationCommandPlaceholder && record.Disposition.EventSeq != want {
				return activationEventSeqError(mutation, want)
			}
			if record.DetachedSink != nil && record.DetachedSink.CommandID == MutationCommandPlaceholder && record.DetachedSink.EventSeq != want {
				return activationEventSeqError(mutation, want)
			}
		case MutationScope:
			var record ScopeRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.ClosedByCommandID == MutationCommandPlaceholder && record.EventSeq != want {
				return activationEventSeqError(mutation, want)
			}
		case MutationReservation:
			var record ActivationReservation
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.CommandID != MutationCommandPlaceholder || record.EventSeq != want {
				return activationEventSeqError(mutation, want)
			}
			if record.CloseReceipt != nil && (record.CloseReceipt.CommandID != MutationCommandPlaceholder || record.CloseReceipt.EventSeq != want) {
				return activationEventSeqError(mutation, want)
			}
		case MutationActivation:
			var record ActivationRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.CommandID != MutationCommandPlaceholder || record.EventSeq != want || record.Receipt.CommandID != MutationCommandPlaceholder || record.Receipt.EventSeq != want {
				return activationEventSeqError(mutation, want)
			}
		case MutationCandidateClosure:
			var record CandidateClosure
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.CommandID == MutationCommandPlaceholder && record.EventSeq != want {
				return activationEventSeqError(mutation, want)
			}
		case MutationCauseRecord:
			var record CauseRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.SourceCommandID == MutationCommandPlaceholder && record.EventSeq != want {
				return activationEventSeqError(mutation, want)
			}
		case MutationDetachment:
			var record DetachmentRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.CommandID != MutationCommandPlaceholder || record.EventSeq != want || record.ActivatedSeq != want {
				return activationEventSeqError(mutation, want)
			}
		case MutationPropagation:
			var record PropagationIntent
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.CommandID == MutationCommandPlaceholder && record.EventSeq != want {
				return activationEventSeqError(mutation, want)
			}
		}
	}
	return nil
}

func activationEventSeqError(mutation RecordMutation, want int64) error {
	return fmt.Errorf("%w: activation-owned post record %s/%s is not coupled to reducer event %d", ErrMutationInvalid, mutation.Kind, mutation.Key, want)
}

func validatePropagationMutationSet(pre RoutingState, plan PropagateClosurePlan, intents []PropagationIntent) error {
	allowed := make(map[mutationTarget]struct{})
	processed := make(map[CandidateClosureKey]struct{})
	for _, intent := range intents {
		mutation, ok := findMutation(plan.Batch, MutationPropagation, intent.ID)
		if !ok || len(mutation.After) == 0 {
			return fmt.Errorf("%w: propagation intent %q lacks its exact transition", ErrMutationInvalid, intent.ID)
		}
		start := uint32(0)
		if len(mutation.Before) > 0 {
			var before PropagationIntent
			if err := decodeExactPayload(mutation.Before, &before); err != nil {
				return err
			}
			start = before.Cursor
			if start > intent.Cursor || !slices.Equal(before.Frontier, intent.Frontier) {
				return fmt.Errorf("%w: propagation intent %q cursor/frontier regresses", ErrMutationInvalid, intent.ID)
			}
		}
		for _, key := range intent.Frontier[start:intent.Cursor] {
			processed[key] = struct{}{}
		}
		allowed[mutationTarget{kind: MutationPropagation, key: intent.ID}] = struct{}{}
	}

	reservations := make(map[ReservationID]struct{})
	for key := range processed {
		found := false
		for _, reservation := range pre.Reservations {
			for _, candidate := range reservation.Candidates {
				candidateKey, _ := CandidateClosureKeyIdentity(reservation.ID, candidate.ID)
				if candidateKey == key {
					reservations[reservation.ID] = struct{}{}
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("%w: processed closure key %q has no reserved candidate", ErrMutationInvalid, key)
		}
	}

	requiredPathIDs := make(map[PathID]struct{})
	detachmentIDs := make(map[DetachmentID]struct{})
	causeDigests := make(map[CauseDigest]struct{})
	for _, mutation := range plan.Batch.Mutations {
		switch mutation.Kind {
		case MutationPropagation:
			// Exact intent IDs were populated above.
		case MutationCandidateClosure:
			if _, ok := processed[mutation.Key]; !ok {
				return unauthorizedPropagationMutation(mutation)
			}
			if len(mutation.Before) != 0 || len(mutation.After) == 0 {
				return fmt.Errorf("%w: propagation cannot rewrite immutable candidate closure %q", ErrMutationInvalid, mutation.Key)
			}
			var closure CandidateClosure
			if err := decodeExactPayload(mutation.After, &closure); err != nil {
				return err
			}
			causeDigests[closure.CauseDigest] = struct{}{}
			allowed[mutationTarget{kind: mutation.Kind, key: mutation.Key}] = struct{}{}
		case MutationReservation:
			if _, ok := reservations[mutation.Key]; !ok {
				return unauthorizedPropagationMutation(mutation)
			}
			var reservation ActivationReservation
			if err := decodeMutationSide(mutation, &reservation); err != nil {
				return err
			}
			causeDigests[reservation.CauseDigest] = struct{}{}
			if reservation.State == ReservationClosedNoActivation {
				for _, path := range pre.Paths {
					if path.TargetReservationID == reservation.ID && path.State == PathArrived {
						requiredPathIDs[path.ID] = struct{}{}
					}
				}
			}
			allowed[mutationTarget{kind: mutation.Kind, key: mutation.Key}] = struct{}{}
		case MutationActivation:
			var activation ActivationRecord
			if err := decodeMutationSide(mutation, &activation); err != nil {
				return err
			}
			if _, ok := reservations[activation.ReservationID]; !ok {
				return unauthorizedPropagationMutation(mutation)
			}
			for _, pathID := range activation.InputPathIDs {
				requiredPathIDs[pathID] = struct{}{}
			}
			if activation.OutputPathID != "" {
				requiredPathIDs[activation.OutputPathID] = struct{}{}
			}
			causeDigests[activation.Receipt.CauseDigest] = struct{}{}
			allowed[mutationTarget{kind: mutation.Kind, key: mutation.Key}] = struct{}{}
		case MutationScope:
			var before, after ScopeRecord
			if len(mutation.Before) == 0 || len(mutation.After) == 0 || decodeExactPayload(mutation.Before, &before) != nil || decodeExactPayload(mutation.After, &after) != nil || before.State == after.State {
				return unauthorizedPropagationMutation(mutation)
			}
			authorized := false
			for reservationID := range reservations {
				reservation := pre.Reservations[reservationID]
				if reservation.IsReducing && reservation.ReducesScopeID == mutation.Key {
					authorized = true
				}
			}
			if !authorized {
				return unauthorizedPropagationMutation(mutation)
			}
			allowed[mutationTarget{kind: mutation.Kind, key: mutation.Key}] = struct{}{}
		case MutationDetachment:
			var detachment DetachmentRecord
			if err := decodeMutationSide(mutation, &detachment); err != nil {
				return err
			}
			if _, ok := reservations[detachment.ReservationID]; !ok {
				return unauthorizedPropagationMutation(mutation)
			}
			detachmentIDs[detachment.ID] = struct{}{}
			allowed[mutationTarget{kind: mutation.Kind, key: mutation.Key}] = struct{}{}
		}
	}

	for _, mutation := range plan.Batch.Mutations {
		switch mutation.Kind {
		case MutationPath:
			if _, required := requiredPathIDs[mutation.Key]; !required {
				return unauthorizedPropagationMutation(mutation)
			}
			var path PathRecord
			if err := decodeMutationSide(mutation, &path); err != nil {
				return err
			}
			causeDigests[path.ImpossibleCauseDigest] = struct{}{}
			allowed[mutationTarget{kind: mutation.Kind, key: mutation.Key}] = struct{}{}
		case MutationDetachmentSet:
			var set DetachmentSetRecord
			if err := decodeMutationSide(mutation, &set); err != nil {
				return err
			}
			if _, ok := detachmentIDs[set.DetachmentID]; !ok {
				return unauthorizedPropagationMutation(mutation)
			}
			allowed[mutationTarget{kind: mutation.Kind, key: mutation.Key}] = struct{}{}
		}
	}
	for pathID := range requiredPathIDs {
		if _, ok := findMutation(plan.Batch, MutationPath, pathID); !ok {
			return fmt.Errorf("%w: propagation-required path mutation %q is missing", ErrMutationInvalid, pathID)
		}
	}

	causeIDs := make(map[CauseID]struct{})
	for _, mutation := range plan.Batch.Mutations {
		if mutation.Kind != MutationCauseSet {
			continue
		}
		if _, ok := causeDigests[CauseDigest(mutation.Key)]; !ok {
			return unauthorizedPropagationMutation(mutation)
		}
		var set CauseSetRecord
		if err := decodeMutationSide(mutation, &set); err != nil {
			return err
		}
		for _, causeID := range set.CauseIDs {
			causeIDs[causeID] = struct{}{}
		}
		allowed[mutationTarget{kind: mutation.Kind, key: mutation.Key}] = struct{}{}
	}
	for _, mutation := range plan.Batch.Mutations {
		if mutation.Kind != MutationCauseRecord {
			continue
		}
		if _, ok := causeIDs[CauseID(mutation.Key)]; !ok {
			return unauthorizedPropagationMutation(mutation)
		}
		allowed[mutationTarget{kind: mutation.Kind, key: mutation.Key}] = struct{}{}
	}
	if err := validateExactMutationTargets(plan.Batch, allowed); err != nil {
		return err
	}
	return validatePropagationEventSeq(plan.Batch)
}

func validatePropagationEventSeq(batch MutationBatch) error {
	want := batch.EventSeq
	for _, mutation := range batch.Mutations {
		if len(mutation.After) == 0 {
			continue
		}
		switch mutation.Kind {
		case MutationPath:
			var record PathRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.UpdatedSeq != want || (len(mutation.Before) == 0 && record.CreatedSeq != want) || (record.Disposition != nil && record.Disposition.CommandID == MutationCommandPlaceholder && record.Disposition.EventSeq != want) || (record.DetachedSink != nil && record.DetachedSink.CommandID == MutationCommandPlaceholder && record.DetachedSink.EventSeq != want) {
				return propagationEventSeqError(mutation, want)
			}
		case MutationScope:
			var record ScopeRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.EventSeq != want || record.ClosedByCommandID != MutationCommandPlaceholder {
				return propagationEventSeqError(mutation, want)
			}
		case MutationReservation:
			var record ActivationReservation
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.EventSeq != want || record.CommandID != MutationCommandPlaceholder || (record.CloseReceipt != nil && (record.CloseReceipt.EventSeq != want || record.CloseReceipt.CommandID != MutationCommandPlaceholder)) {
				return propagationEventSeqError(mutation, want)
			}
		case MutationActivation:
			var record ActivationRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if len(mutation.Before) != 0 || record.EventSeq != want || record.CommandID != MutationCommandPlaceholder || record.Receipt.EventSeq != want || record.Receipt.CommandID != MutationCommandPlaceholder {
				return propagationEventSeqError(mutation, want)
			}
		case MutationCandidateClosure:
			var record CandidateClosure
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if len(mutation.Before) != 0 || record.EventSeq != want || record.CommandID != MutationCommandPlaceholder {
				return propagationEventSeqError(mutation, want)
			}
		case MutationCauseRecord:
			var record CauseRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if len(mutation.Before) != 0 || record.EventSeq != want || record.SourceCommandID != MutationCommandPlaceholder {
				return propagationEventSeqError(mutation, want)
			}
		case MutationCauseSet:
			if len(mutation.Before) != 0 {
				return fmt.Errorf("%w: propagation cannot rewrite immutable cause set %q", ErrMutationInvalid, mutation.Key)
			}
		case MutationDetachment:
			var record DetachmentRecord
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if len(mutation.Before) != 0 || record.EventSeq != want || record.ActivatedSeq != want || record.CommandID != MutationCommandPlaceholder {
				return propagationEventSeqError(mutation, want)
			}
		case MutationDetachmentSet:
			if len(mutation.Before) != 0 {
				return fmt.Errorf("%w: propagation cannot rewrite immutable detachment set %q", ErrMutationInvalid, mutation.Key)
			}
		case MutationPropagation:
			var record PropagationIntent
			if err := decodeExactPayload(mutation.After, &record); err != nil {
				return err
			}
			if record.EventSeq != want {
				return propagationEventSeqError(mutation, want)
			}
		}
	}
	return nil
}

func propagationEventSeqError(mutation RecordMutation, want int64) error {
	return fmt.Errorf("%w: propagation-owned post record %s/%s is not coupled to reducer event %d", ErrMutationInvalid, mutation.Kind, mutation.Key, want)
}

func decodeMutationSide[T any](mutation RecordMutation, value *T) error {
	data := mutation.After
	if len(data) == 0 {
		data = mutation.Before
	}
	return decodeExactPayload(data, value)
}

func unauthorizedPropagationMutation(mutation RecordMutation) error {
	return fmt.Errorf("%w: propagation mutation %s/%s is not reachable from an advanced frontier key", ErrMutationInvalid, mutation.Kind, mutation.Key)
}
