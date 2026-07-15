package pathv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

func (v CheckpointBinding) Validate() error {
	if v.Generation == 0 {
		return fmt.Errorf("checkpoint generation must be positive")
	}
	if !lowerHexDigest(v.Digest) {
		return fmt.Errorf("checkpoint digest is not lowercase SHA-256")
	}
	return nil
}

func lowerHexDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return false
	}
	return value == hex.EncodeToString(decoded)
}

// RoutingDigest hashes the one canonical RoutingState encoding used by the
// dormant checkpoint substrate. It does not define a path-v1 identity.
func RoutingDigest(st *RoutingState) (string, error) {
	data, err := Encode(st)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func (b MutationBatch) Validate() error {
	if b.EventSeq < 0 {
		return fmt.Errorf("%w: negative event sequence", ErrMutationInvalid)
	}
	if b.LogEntries != 1 {
		return fmt.Errorf("%w: mutation batch must contain exactly one log entry", ErrMutationInvalid)
	}
	if !lowerHexDigest(b.BeforeDigest) || !lowerHexDigest(b.AfterDigest) || b.BeforeDigest == b.AfterDigest {
		return fmt.Errorf("%w: invalid or equal routing digests", ErrMutationInvalid)
	}
	usage := Usage{Mutations: len(b.Mutations), LogEntries: b.LogEntries}
	if err := usage.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	if len(b.Mutations) == 0 {
		return fmt.Errorf("%w: empty mutation batch", ErrMutationInvalid)
	}
	for index, mutation := range b.Mutations {
		if mutation.Key == "" || !mutation.Kind.valid() {
			return fmt.Errorf("%w: mutation %d has invalid kind/key", ErrMutationInvalid, index)
		}
		if index > 0 && compareMutation(b.Mutations[index-1], mutation) >= 0 {
			return fmt.Errorf("%w: mutations are not strictly ordered and unique", ErrMutationInvalid)
		}
		if len(mutation.Before) == 0 && len(mutation.After) == 0 {
			return fmt.Errorf("%w: mutation %s/%s has no pre/post record", ErrMutationInvalid, mutation.Kind, mutation.Key)
		}
		if len(mutation.Before) > 0 {
			if bytes.Contains(mutation.Before, []byte(MutationCommandPlaceholder)) {
				return fmt.Errorf("%w: mutation %s/%s pre-state contains command placeholder", ErrMutationInvalid, mutation.Kind, mutation.Key)
			}
			if err := validateMutationRecord(mutation.Kind, mutation.Key, mutation.Before); err != nil {
				return fmt.Errorf("%w: mutation %s/%s before: %v", ErrMutationInvalid, mutation.Kind, mutation.Key, err)
			}
		}
		if len(mutation.After) > 0 {
			if err := validateMutationRecord(mutation.Kind, mutation.Key, mutation.After); err != nil {
				return fmt.Errorf("%w: mutation %s/%s after: %v", ErrMutationInvalid, mutation.Kind, mutation.Key, err)
			}
		}
		if len(mutation.Before) > 0 && bytes.Equal(mutation.Before, mutation.After) {
			return fmt.Errorf("%w: mutation %s/%s does not change bytes", ErrMutationInvalid, mutation.Kind, mutation.Key)
		}
	}
	if err := b.validateOwnedSentinels(); err != nil {
		return err
	}
	return nil
}

func (v MutationRecordKind) valid() bool {
	switch v {
	case MutationPath, MutationScope, MutationReservation, MutationActivation,
		MutationCandidateClosure, MutationCauseRecord, MutationCauseSet,
		MutationDetachmentSet, MutationDetachment, MutationPropagation:
		return true
	}
	return false
}

func compareMutation(a, b RecordMutation) int {
	if a.Kind < b.Kind {
		return -1
	}
	if a.Kind > b.Kind {
		return 1
	}
	if a.Key < b.Key {
		return -1
	}
	if a.Key > b.Key {
		return 1
	}
	return 0
}

func validateMutationRecord(kind MutationRecordKind, key string, data []byte) error {
	switch kind {
	case MutationPath:
		var value PathRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return err
		}
		return requireMutationKey(key, value.ID)
	case MutationScope:
		var value ScopeRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return err
		}
		return requireMutationKey(key, value.ID)
	case MutationReservation:
		var value ActivationReservation
		if err := decodeExactPayload(data, &value); err != nil {
			return err
		}
		return requireMutationKey(key, value.ID)
	case MutationActivation:
		var value ActivationRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return err
		}
		return requireMutationKey(key, value.ID)
	case MutationCandidateClosure:
		var value CandidateClosure
		if err := decodeExactPayload(data, &value); err != nil {
			return err
		}
		return requireMutationKey(key, value.Key.ID)
	case MutationCauseRecord:
		var value CauseRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return err
		}
		return requireMutationKey(key, value.ID)
	case MutationCauseSet:
		var value CauseSetRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return err
		}
		return requireMutationKey(key, value.Digest)
	case MutationDetachmentSet:
		var value DetachmentSetRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return err
		}
		return requireMutationKey(key, value.ID)
	case MutationDetachment:
		var value DetachmentRecord
		if err := decodeExactPayload(data, &value); err != nil {
			return err
		}
		return requireMutationKey(key, value.Key.ID)
	case MutationPropagation:
		var value PropagationIntent
		if err := decodeExactPayload(data, &value); err != nil {
			return err
		}
		return requireMutationKey(key, value.ID)
	default:
		return fmt.Errorf("invalid mutation kind %q", kind)
	}
}

func requireMutationKey(key, id string) error {
	if id == "" || key != id {
		return fmt.Errorf("record key %q differs from ID %q", key, id)
	}
	return nil
}

// NewMutationBatch builds the exact deterministic map delta between two
// routing states. The result is validated before it is returned.
func NewMutationBatch(before, after *RoutingState, eventSeq int64) (MutationBatch, error) {
	if before == nil || after == nil {
		return MutationBatch{}, fmt.Errorf("%w: nil routing state", ErrMutationInvalid)
	}
	beforeDigest, err := RoutingDigest(before)
	if err != nil {
		return MutationBatch{}, err
	}
	afterDigest, err := RoutingDigest(after)
	if err != nil {
		return MutationBatch{}, err
	}
	mutations := make([]RecordMutation, 0)
	appendMutationDiff(&mutations, MutationPath, before.Paths, after.Paths)
	appendMutationDiff(&mutations, MutationScope, before.Scopes, after.Scopes)
	appendMutationDiff(&mutations, MutationReservation, before.Reservations, after.Reservations)
	appendMutationDiff(&mutations, MutationActivation, before.Activations, after.Activations)
	appendMutationDiff(&mutations, MutationCandidateClosure, before.CandidateClosures, after.CandidateClosures)
	appendMutationDiff(&mutations, MutationCauseRecord, before.CauseRecords, after.CauseRecords)
	appendMutationDiff(&mutations, MutationCauseSet, before.CauseSets, after.CauseSets)
	appendMutationDiff(&mutations, MutationDetachmentSet, before.DetachmentSets, after.DetachmentSets)
	appendMutationDiff(&mutations, MutationDetachment, before.Detachments, after.Detachments)
	appendMutationDiff(&mutations, MutationPropagation, before.Propagation, after.Propagation)
	slices.SortFunc(mutations, compareMutation)
	batch := MutationBatch{EventSeq: eventSeq, LogEntries: 1, BeforeDigest: beforeDigest, AfterDigest: afterDigest, Mutations: mutations}
	if err := batch.Validate(); err != nil {
		return MutationBatch{}, err
	}
	return batch, nil
}

func appendMutationDiff[T any](out *[]RecordMutation, kind MutationRecordKind, before, after map[string]T) {
	keys := make(map[string]struct{}, len(before)+len(after))
	for key := range before {
		keys[key] = struct{}{}
	}
	for key := range after {
		keys[key] = struct{}{}
	}
	for key := range keys {
		left, leftOK := before[key]
		right, rightOK := after[key]
		var leftBytes, rightBytes []byte
		if leftOK {
			leftBytes, _ = json.Marshal(left)
		}
		if rightOK {
			rightBytes, _ = json.Marshal(right)
		}
		if bytes.Equal(leftBytes, rightBytes) {
			continue
		}
		*out = append(*out, RecordMutation{Kind: kind, Key: key, Before: leftBytes, After: rightBytes})
	}
}

func (b MutationBatch) replay(current *RoutingState, commandID string) (RoutingState, ReplayDisposition, error) {
	if err := b.Validate(); err != nil {
		return RoutingState{}, "", err
	}
	if err := rejectRoutingSentinel(current); err != nil {
		return RoutingState{}, "", err
	}
	materialized, err := b.materialize(commandID)
	if err != nil {
		return RoutingState{}, "", err
	}
	digest, err := RoutingDigest(current)
	if err != nil {
		return RoutingState{}, "", err
	}
	switch digest {
	case b.BeforeDigest:
		if err := requireMutationSide(current, b.Mutations, true); err != nil {
			return RoutingState{}, "", err
		}
		if err := b.validateTemplateDigest(current); err != nil {
			return RoutingState{}, "", err
		}
		next := Clone(*current)
		for _, mutation := range materialized {
			if err := applyRecordMutation(&next, mutation); err != nil {
				return RoutingState{}, "", err
			}
		}
		return next, ReplayApplied, nil
	}
	if err := requireMutationSide(current, materialized, false); err != nil {
		return RoutingState{}, "", err
	}
	pre := Clone(*current)
	for index := len(materialized) - 1; index >= 0; index-- {
		mutation := materialized[index]
		reverse := RecordMutation{Kind: mutation.Kind, Key: mutation.Key, Before: mutation.After, After: mutation.Before}
		if err := applyRecordMutation(&pre, reverse); err != nil {
			return RoutingState{}, "", err
		}
	}
	preDigest, err := RoutingDigest(&pre)
	if err != nil {
		return RoutingState{}, "", err
	}
	if preDigest != b.BeforeDigest {
		return RoutingState{}, "", fmt.Errorf("%w: post-state contains unplanned or different state", ErrMutationInconsistent)
	}
	if err := b.validateTemplateDigest(&pre); err != nil {
		return RoutingState{}, "", err
	}
	return Clone(*current), ReplayAlreadyApplied, nil
}

func rejectRoutingSentinel(st *RoutingState) error {
	data, err := Encode(st)
	if err != nil {
		return err
	}
	if bytes.Contains(data, []byte(MutationCommandPlaceholder)) {
		return fmt.Errorf("%w: command placeholder appears in durable routing state", ErrMutationInconsistent)
	}
	return nil
}

func (b MutationBatch) validateOwnedSentinels() error {
	for _, mutation := range b.Mutations {
		if len(mutation.After) == 0 {
			continue
		}
		switch mutation.Kind {
		case MutationPath:
			var before, after PathRecord
			if len(mutation.Before) > 0 {
				_ = decodeExactPayload(mutation.Before, &before)
			}
			_ = decodeExactPayload(mutation.After, &after)
			if after.Disposition != nil && !canonicalEqual(before.Disposition, after.Disposition) {
				if after.Disposition.CommandID != MutationCommandPlaceholder {
					return fmt.Errorf("%w: command-owned path disposition lacks sentinel", ErrMutationInvalid)
				}
				seq, err := eventUint(after.Disposition.EventSeq)
				want := ""
				if err == nil {
					want, err = DispositionReceiptIdentity(after.Disposition.PathID, after.Disposition.FromState, after.Disposition.ToState, after.Disposition.ReasonCode, MutationCommandPlaceholder, after.Disposition.AdminRecordID, seq)
				}
				if err != nil || want != after.Disposition.ID {
					return fmt.Errorf("%w: sentinel disposition identity does not recompute", ErrMutationInvalid)
				}
			}
			if after.DetachedSink != nil && !canonicalEqual(before.DetachedSink, after.DetachedSink) && after.DetachedSink.CommandID != MutationCommandPlaceholder {
				return fmt.Errorf("%w: command-owned detached sink lacks sentinel", ErrMutationInvalid)
			}
		case MutationScope:
			var before, after ScopeRecord
			if len(mutation.Before) > 0 {
				_ = decodeExactPayload(mutation.Before, &before)
			}
			_ = decodeExactPayload(mutation.After, &after)
			openCreate := len(mutation.Before) == 0 && after.State == ScopeOpen
			if before.State != after.State && !openCreate && after.ClosedByCommandID != MutationCommandPlaceholder {
				return fmt.Errorf("%w: command-owned scope close lacks sentinel", ErrMutationInvalid)
			}
		case MutationReservation:
			var before, after ActivationReservation
			if len(mutation.Before) > 0 {
				_ = decodeExactPayload(mutation.Before, &before)
			}
			_ = decodeExactPayload(mutation.After, &after)
			openCreate := len(mutation.Before) == 0 && after.State == ReservationOpen
			if before.State != after.State && !openCreate && after.CommandID != MutationCommandPlaceholder {
				return fmt.Errorf("%w: command-owned reservation transition lacks sentinel", ErrMutationInvalid)
			}
			if after.CloseReceipt != nil && !canonicalEqual(before.CloseReceipt, after.CloseReceipt) {
				if after.CloseReceipt.CommandID != MutationCommandPlaceholder {
					return fmt.Errorf("%w: command-owned close receipt lacks sentinel", ErrMutationInvalid)
				}
				seq, err := eventUint(after.CloseReceipt.EventSeq)
				want := ""
				if err == nil {
					want, err = ActivationReceiptIdentity(after.CloseReceipt.ActivationID, after.CloseReceipt.ReservationID, after.CloseReceipt.InputSetDigest, after.CloseReceipt.OutputPathID, MutationCommandPlaceholder, seq)
				}
				if err != nil || want != after.CloseReceipt.ID {
					return fmt.Errorf("%w: sentinel close receipt identity does not recompute", ErrMutationInvalid)
				}
			}
		case MutationActivation:
			var after ActivationRecord
			_ = decodeExactPayload(mutation.After, &after)
			if after.CommandID != MutationCommandPlaceholder || after.Receipt.CommandID != MutationCommandPlaceholder {
				return fmt.Errorf("%w: command-owned activation/receipt lacks sentinel", ErrMutationInvalid)
			}
			seq, err := eventUint(after.Receipt.EventSeq)
			want := ""
			if err == nil {
				want, err = ActivationReceiptIdentity(after.Receipt.ActivationID, after.Receipt.ReservationID, after.Receipt.InputSetDigest, after.Receipt.OutputPathID, MutationCommandPlaceholder, seq)
			}
			if err != nil || want != after.Receipt.ID {
				return fmt.Errorf("%w: sentinel activation receipt identity does not recompute", ErrMutationInvalid)
			}
		case MutationCandidateClosure:
			var after CandidateClosure
			_ = decodeExactPayload(mutation.After, &after)
			if after.CommandID != MutationCommandPlaceholder {
				return fmt.Errorf("%w: command-owned closure lacks sentinel", ErrMutationInvalid)
			}
		case MutationCauseRecord:
			var after CauseRecord
			_ = decodeExactPayload(mutation.After, &after)
			if after.SourceCommandID == MutationCommandPlaceholder {
				seq, err := eventUint(after.EventSeq)
				want := ""
				if err == nil {
					want, err = CauseIdentity(after.SourcePathID, after.TerminalKind, after.DispositionReason, after.SourceActivationID, MutationCommandPlaceholder, after.AdminRecordID, seq)
				}
				if err != nil || want != after.ID || mutation.Key != after.ID {
					return fmt.Errorf("%w: sentinel cause identity does not recompute", ErrMutationInvalid)
				}
			}
		case MutationDetachment:
			var after DetachmentRecord
			_ = decodeExactPayload(mutation.After, &after)
			if after.CommandID != MutationCommandPlaceholder {
				return fmt.Errorf("%w: command-owned detachment lacks sentinel", ErrMutationInvalid)
			}
		case MutationPropagation:
			var before, after PropagationIntent
			if len(mutation.Before) > 0 {
				_ = decodeExactPayload(mutation.Before, &before)
			}
			_ = decodeExactPayload(mutation.After, &after)
			if len(mutation.Before) == 0 && after.CommandID != MutationCommandPlaceholder {
				return fmt.Errorf("%w: command-owned propagation intent lacks sentinel", ErrMutationInvalid)
			}
			if len(mutation.Before) > 0 && after.CommandID != before.CommandID {
				return fmt.Errorf("%w: propagation update changes immutable command authority", ErrMutationInvalid)
			}
		}
	}
	return nil
}

func (b MutationBatch) validateTemplateDigest(pre *RoutingState) error {
	template := Clone(*pre)
	for _, mutation := range b.Mutations {
		if err := applyRecordMutation(&template, mutation); err != nil {
			return err
		}
	}
	digest, err := RoutingDigest(&template)
	if err != nil {
		return err
	}
	if digest != b.AfterDigest {
		return fmt.Errorf("%w: command-independent post-state digest mismatch", ErrMutationInvalid)
	}
	return nil
}

func requireMutationSide(st *RoutingState, mutations []RecordMutation, before bool) error {
	for _, mutation := range mutations {
		want := mutation.After
		if before {
			want = mutation.Before
		}
		got, err := routingRecordBytes(st, mutation.Kind, mutation.Key)
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("%w: mutation %s/%s is partial or has different bytes", ErrMutationInconsistent, mutation.Kind, mutation.Key)
		}
	}
	return nil
}

func routingRecordBytes(st *RoutingState, kind MutationRecordKind, key string) ([]byte, error) {
	var value any
	var ok bool
	switch kind {
	case MutationPath:
		value, ok = st.Paths[key]
	case MutationScope:
		value, ok = st.Scopes[key]
	case MutationReservation:
		value, ok = st.Reservations[key]
	case MutationActivation:
		value, ok = st.Activations[key]
	case MutationCandidateClosure:
		value, ok = st.CandidateClosures[key]
	case MutationCauseRecord:
		value, ok = st.CauseRecords[key]
	case MutationCauseSet:
		value, ok = st.CauseSets[key]
	case MutationDetachmentSet:
		value, ok = st.DetachmentSets[key]
	case MutationDetachment:
		value, ok = st.Detachments[key]
	case MutationPropagation:
		value, ok = st.Propagation[key]
	default:
		return nil, fmt.Errorf("invalid mutation kind %q", kind)
	}
	if !ok {
		return nil, nil
	}
	return json.Marshal(value)
}

func applyRecordMutation(st *RoutingState, mutation RecordMutation) error {
	if len(mutation.After) == 0 {
		deleteRoutingRecord(st, mutation.Kind, mutation.Key)
		return nil
	}
	switch mutation.Kind {
	case MutationPath:
		var value PathRecord
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return err
		}
		st.Paths[mutation.Key] = clonePath(value)
	case MutationScope:
		var value ScopeRecord
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return err
		}
		value.ExpectedBranchEdgeIDs = cloneSlice(value.ExpectedBranchEdgeIDs)
		st.Scopes[mutation.Key] = value
	case MutationReservation:
		var value ActivationReservation
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return err
		}
		st.Reservations[mutation.Key] = cloneReservation(value)
	case MutationActivation:
		var value ActivationRecord
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return err
		}
		value.InputPathIDs = cloneSlice(value.InputPathIDs)
		st.Activations[mutation.Key] = value
	case MutationCandidateClosure:
		var value CandidateClosure
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return err
		}
		st.CandidateClosures[mutation.Key] = value
	case MutationCauseRecord:
		var value CauseRecord
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return err
		}
		st.CauseRecords[mutation.Key] = value
	case MutationCauseSet:
		var value CauseSetRecord
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return err
		}
		value.CauseIDs = cloneSlice(value.CauseIDs)
		st.CauseSets[mutation.Key] = value
	case MutationDetachmentSet:
		var value DetachmentSetRecord
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return err
		}
		st.DetachmentSets[mutation.Key] = value
	case MutationDetachment:
		var value DetachmentRecord
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return err
		}
		st.Detachments[mutation.Key] = value
	case MutationPropagation:
		var value PropagationIntent
		if err := decodeExactPayload(mutation.After, &value); err != nil {
			return err
		}
		value.Frontier = cloneSlice(value.Frontier)
		st.Propagation[mutation.Key] = value
	default:
		return fmt.Errorf("invalid mutation kind %q", mutation.Kind)
	}
	return nil
}

func deleteRoutingRecord(st *RoutingState, kind MutationRecordKind, key string) {
	switch kind {
	case MutationPath:
		delete(st.Paths, key)
	case MutationScope:
		delete(st.Scopes, key)
	case MutationReservation:
		delete(st.Reservations, key)
	case MutationActivation:
		delete(st.Activations, key)
	case MutationCandidateClosure:
		delete(st.CandidateClosures, key)
	case MutationCauseRecord:
		delete(st.CauseRecords, key)
	case MutationCauseSet:
		delete(st.CauseSets, key)
	case MutationDetachmentSet:
		delete(st.DetachmentSets, key)
	case MutationDetachment:
		delete(st.Detachments, key)
	case MutationPropagation:
		delete(st.Propagation, key)
	}
}
