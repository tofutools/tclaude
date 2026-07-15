package pathv1

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
)

// materialize structurally replaces the reserved command sentinel and
// recomputes every identity that transitively contains command-owned fields.
// It never performs textual substitution.
func (b MutationBatch) materialize(commandID string) ([]RecordMutation, error) {
	if !lowerHexDigest(commandID) || commandID == MutationCommandPlaceholder {
		return nil, fmt.Errorf("%w: invalid materialization command ID", ErrMutationInvalid)
	}
	causeIDs := map[string]string{}
	for _, mutation := range b.Mutations {
		if mutation.Kind != MutationCauseRecord || len(mutation.After) == 0 {
			continue
		}
		var value CauseRecord
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return nil, err
		}
		if value.SourceCommandID != MutationCommandPlaceholder {
			continue
		}
		seq, err := eventUint(value.EventSeq)
		if err != nil {
			return nil, err
		}
		actual, err := CauseIdentity(value.SourcePathID, value.TerminalKind, value.DispositionReason, value.SourceActivationID, commandID, value.AdminRecordID, seq)
		if err != nil {
			return nil, err
		}
		causeIDs[value.ID] = actual
	}

	causeDigests := map[string]string{}
	causeSetMembers := map[string][]CauseID{}
	for _, mutation := range b.Mutations {
		if mutation.Kind != MutationCauseSet || len(mutation.After) == 0 {
			continue
		}
		var value CauseSetRecord
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return nil, err
		}
		changed := false
		for index, id := range value.CauseIDs {
			if actual, ok := causeIDs[id]; ok {
				value.CauseIDs[index] = actual
				changed = true
			}
		}
		if !changed {
			continue
		}
		slices.Sort(value.CauseIDs)
		actual, err := CauseSetIdentity(value.CauseIDs)
		if err != nil {
			return nil, err
		}
		causeSetMembers[value.Digest] = cloneSlice(value.CauseIDs)
		causeDigests[value.Digest] = actual
	}

	pathIDs := map[string]string{}
	for _, mutation := range b.Mutations {
		if mutation.Kind != MutationPath || len(mutation.After) == 0 {
			continue
		}
		var value PathRecord
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return nil, err
		}
		actualCause, changed := causeDigests[value.ImpossibleCauseDigest]
		if !changed || value.Kind != PathImpossibleEdge || value.Edge == nil {
			continue
		}
		actual, err := ImpossibleEdgePathIdentity(actualCause, value.Edge.ID, value.TargetReservationID)
		if err != nil {
			return nil, err
		}
		pathIDs[value.ID] = actual
	}

	materialized := make([]RecordMutation, 0, len(b.Mutations))
	for _, mutation := range b.Mutations {
		actual := mutation
		if len(mutation.After) > 0 {
			key, data, err := materializeRecord(mutation.Kind, mutation.Key, mutation.After, commandID, causeIDs, causeDigests, causeSetMembers, pathIDs)
			if err != nil {
				return nil, err
			}
			if key != mutation.Key && len(mutation.Before) != 0 {
				return nil, fmt.Errorf("%w: materialization changes update key %s/%s", ErrMutationInvalid, mutation.Kind, mutation.Key)
			}
			actual.Key = key
			actual.After = data
		}
		if bytes.Contains(actual.After, []byte(MutationCommandPlaceholder)) {
			return nil, fmt.Errorf("%w: sentinel survives %s/%s materialization", ErrMutationInvalid, mutation.Kind, mutation.Key)
		}
		materialized = append(materialized, actual)
	}
	slices.SortFunc(materialized, compareMutation)
	for index := 1; index < len(materialized); index++ {
		if compareMutation(materialized[index-1], materialized[index]) == 0 {
			return nil, fmt.Errorf("%w: materialized mutations collide at %s/%s", ErrMutationInvalid, materialized[index].Kind, materialized[index].Key)
		}
	}
	return materialized, nil
}

func materializeRecord(kind MutationRecordKind, key string, data []byte, commandID string, causeIDs, causeDigests map[string]string, causeSetMembers map[string][]CauseID, pathIDs map[string]string) (string, []byte, error) {
	marshal := func(key string, value any) (string, []byte, error) {
		data, err := json.Marshal(value)
		return key, data, err
	}
	switch kind {
	case MutationPath:
		var value PathRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return "", nil, err
		}
		if actual, ok := pathIDs[value.ID]; ok {
			old := value.ID
			value.ID, key = actual, actual
			if value.Disposition != nil && value.Disposition.PathID == old {
				value.Disposition.PathID = actual
			}
		}
		if actual, ok := pathIDs[value.ParentPathID]; ok {
			value.ParentPathID = actual
		}
		for index, id := range value.ProducedPathIDs {
			if actual, ok := pathIDs[id]; ok {
				value.ProducedPathIDs[index] = actual
			}
		}
		slices.Sort(value.ProducedPathIDs)
		if actual, ok := causeDigests[value.ImpossibleCauseDigest]; ok {
			value.ImpossibleCauseDigest = actual
		}
		if actual, ok := causeIDs[value.TerminalCauseID]; ok {
			value.TerminalCauseID = actual
		}
		if value.Disposition != nil && value.Disposition.CommandID == MutationCommandPlaceholder {
			value.Disposition.CommandID = commandID
			seq, err := eventUint(value.Disposition.EventSeq)
			if err != nil {
				return "", nil, err
			}
			value.Disposition.ID, err = DispositionReceiptIdentity(value.Disposition.PathID, value.Disposition.FromState, value.Disposition.ToState, value.Disposition.ReasonCode, commandID, value.Disposition.AdminRecordID, seq)
			if err != nil {
				return "", nil, err
			}
		}
		if value.DetachedSink != nil && value.DetachedSink.CommandID == MutationCommandPlaceholder {
			value.DetachedSink.CommandID = commandID
		}
		return marshal(key, value)
	case MutationScope:
		var value ScopeRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return "", nil, err
		}
		if value.ClosedByCommandID == MutationCommandPlaceholder {
			value.ClosedByCommandID = commandID
		}
		return marshal(key, value)
	case MutationReservation:
		var value ActivationReservation
		if err := decodeExactPayload(data, &value); err != nil {
			return "", nil, err
		}
		if actual, ok := causeDigests[value.CauseDigest]; ok {
			value.CauseDigest = actual
		}
		if value.CommandID == MutationCommandPlaceholder {
			value.CommandID = commandID
		}
		if value.CloseReceipt != nil {
			if actual, ok := causeDigests[value.CloseReceipt.CauseDigest]; ok {
				value.CloseReceipt.CauseDigest = actual
			}
			if value.CloseReceipt.CommandID == MutationCommandPlaceholder {
				value.CloseReceipt.CommandID = commandID
				seq, err := eventUint(value.CloseReceipt.EventSeq)
				if err != nil {
					return "", nil, err
				}
				value.CloseReceipt.ID, err = ActivationReceiptIdentity(value.CloseReceipt.ActivationID, value.CloseReceipt.ReservationID, value.CloseReceipt.InputSetDigest, value.CloseReceipt.OutputPathID, commandID, seq)
				if err != nil {
					return "", nil, err
				}
			}
		}
		return marshal(key, value)
	case MutationActivation:
		var value ActivationRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return "", nil, err
		}
		if value.CommandID == MutationCommandPlaceholder {
			value.CommandID = commandID
		}
		if value.Receipt.CommandID == MutationCommandPlaceholder {
			value.Receipt.CommandID = commandID
			seq, err := eventUint(value.Receipt.EventSeq)
			if err != nil {
				return "", nil, err
			}
			value.Receipt.ID, err = ActivationReceiptIdentity(value.Receipt.ActivationID, value.Receipt.ReservationID, value.Receipt.InputSetDigest, value.Receipt.OutputPathID, commandID, seq)
			if err != nil {
				return "", nil, err
			}
		}
		return marshal(key, value)
	case MutationCandidateClosure:
		var value CandidateClosure
		if err := decodeExactPayload(data, &value); err != nil {
			return "", nil, err
		}
		if actual, ok := causeDigests[value.CauseDigest]; ok {
			value.CauseDigest = actual
			value.ID, _ = CandidateClosureIdentity(value.Key.ReservationID, value.Key.CandidateID, value.TerminalKind, actual)
		}
		if value.CommandID == MutationCommandPlaceholder {
			value.CommandID = commandID
		}
		return marshal(key, value)
	case MutationCauseRecord:
		var value CauseRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return "", nil, err
		}
		if value.SourceCommandID == MutationCommandPlaceholder {
			value.SourceCommandID = commandID
			value.ID, key = causeIDs[value.ID], causeIDs[value.ID]
		}
		if actual, ok := pathIDs[value.SourcePathID]; ok {
			value.SourcePathID = actual
		}
		return marshal(key, value)
	case MutationCauseSet:
		var value CauseSetRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return "", nil, err
		}
		if actual, ok := causeSetMembers[value.Digest]; ok {
			value.CauseIDs = cloneSlice(actual)
		}
		if actual, ok := causeDigests[value.Digest]; ok {
			value.Digest, key = actual, actual
		}
		return marshal(key, value)
	case MutationDetachmentSet:
		var value DetachmentSetRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return "", nil, err
		}
		return marshal(key, value)
	case MutationDetachment:
		var value DetachmentRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return "", nil, err
		}
		if value.CommandID == MutationCommandPlaceholder {
			value.CommandID = commandID
		}
		return marshal(key, value)
	case MutationPropagation:
		var value PropagationIntent
		if err := decodeExactPayload(data, &value); err != nil {
			return "", nil, err
		}
		if actual, ok := causeDigests[value.RootCauseDigest]; ok {
			value.RootCauseDigest = actual
			value.PlanDigest, _ = PropagationPlanIdentity(value.RootReservationID, value.RootCandidateID, actual, uint64(value.Shard), value.Frontier)
			value.ID, _ = PropagationIntentIdentity(actual, uint64(value.Shard), value.PlanDigest)
			key = value.ID
		}
		if value.CommandID == MutationCommandPlaceholder {
			value.CommandID = commandID
		}
		return marshal(key, value)
	default:
		return "", nil, fmt.Errorf("invalid mutation kind %q", kind)
	}
}
