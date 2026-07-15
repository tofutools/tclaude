package pathv1

import (
	"bytes"
	"cmp"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

const hashPrefix = "tclaude.process/"

var ErrCanonicalLength = errors.New("path-v1 canonical value exceeds u32 length")

// Encoder is the single normative path-v1 field encoder. Identity helpers in
// this package only compose these three field forms; no identity has a second
// byte representation.
type Encoder struct {
	buf bytes.Buffer
	err error
}

func (e *Encoder) String(value string) {
	if e.err != nil {
		return
	}
	if uint64(len(value)) > uint64(^uint32(0)) {
		e.err = ErrCanonicalLength
		return
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(value)))
	e.buf.Write(size[:])
	e.buf.WriteString(value)
}

func (e *Encoder) Uint(value uint64) {
	if e.err != nil {
		return
	}
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	e.buf.Write(encoded[:])
}

func (e *Encoder) List(count int, element func(int)) {
	if e.err != nil {
		return
	}
	if count < 0 || uint64(count) > uint64(^uint32(0)) {
		e.err = ErrCanonicalLength
		return
	}
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], uint32(count))
	e.buf.Write(encoded[:])
	for i := 0; i < count && e.err == nil; i++ {
		element(i)
	}
}

func (e *Encoder) Bytes() ([]byte, error) {
	if e.err != nil {
		return nil, e.err
	}
	return append([]byte(nil), e.buf.Bytes()...), nil
}

func canonicalHash(tag string, fields func(*Encoder)) (string, error) {
	if bytes.IndexByte([]byte(tag), 0) >= 0 {
		return "", fmt.Errorf("path-v1 hash tag contains NUL")
	}
	var e Encoder
	e.buf.WriteString(hashPrefix)
	e.buf.WriteString(tag)
	e.buf.WriteByte(0)
	fields(&e)
	encoded, err := e.Bytes()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func sortedUniqueStrings(values []string) []string {
	out := append([]string(nil), values...)
	slices.Sort(out)
	return slices.Compact(out)
}

func writeStringSet(e *Encoder, values []string) {
	values = sortedUniqueStrings(values)
	e.List(len(values), func(i int) { e.String(values[i]) })
}

func writeStringList(e *Encoder, values []string) {
	e.List(len(values), func(i int) { e.String(values[i]) })
}

func tupleBytes(fields func(*Encoder)) ([]byte, error) {
	var e Encoder
	fields(&e)
	return e.Bytes()
}

func writeTupleList(e *Encoder, tuples [][]byte) {
	e.List(len(tuples), func(i int) { e.buf.Write(tuples[i]) })
}

func EdgeIdentity(templateRef, fromNodeID, outcome, toNodeID string) (string, error) {
	return canonicalHash("edge/v1", func(e *Encoder) { e.String(templateRef); e.String(fromNodeID); e.String(outcome); e.String(toNodeID) })
}

func ScopeIdentity(runID, parentScopeID, parentBranchEdgeID, forkActivationID, forkOutputPathID string, generation uint64) (string, error) {
	return canonicalHash("scope/v1", func(e *Encoder) {
		e.String(runID)
		e.String(parentScopeID)
		e.String(parentBranchEdgeID)
		e.String(forkActivationID)
		e.String(forkOutputPathID)
		e.Uint(generation)
	})
}

func ReservationIdentity(runID, nodeID, scopeID, branchEdgeID string, generation uint64) (string, error) {
	return canonicalHash("reservation/v1", func(e *Encoder) {
		e.String(runID)
		e.String(nodeID)
		e.String(scopeID)
		e.String(branchEdgeID)
		e.Uint(generation)
	})
}

func CandidateIdentity(reservationID string, kind CandidateKind, memberID string) (string, error) {
	return canonicalHash("candidate/v1", func(e *Encoder) { e.String(reservationID); e.String(string(kind)); e.String(memberID) })
}

func PossibleSlotIdentity(reservationID, candidateID, sourceNodeID, sourceEdgeID, sourceScopeID, sourceBranchEdgeID string, generation uint64) (string, error) {
	return canonicalHash("route-slot/v1", func(e *Encoder) {
		e.String(reservationID)
		e.String(candidateID)
		e.String(sourceNodeID)
		e.String(sourceEdgeID)
		e.String(sourceScopeID)
		e.String(sourceBranchEdgeID)
		e.Uint(generation)
	})
}

func InputSetIdentity(pathIDs []PathID) (string, error) {
	return canonicalHash("activation-input-set/v1", func(e *Encoder) { writeStringSet(e, pathIDs) })
}

func ActivationIdentity(runID, reservationID string, generation uint64, inputSetDigest string) (string, error) {
	return canonicalHash("activation/v1", func(e *Encoder) {
		e.String(runID)
		e.String(reservationID)
		e.Uint(generation)
		e.String(inputSetDigest)
	})
}

func ActivationOutputIdentity(activationID string, generation uint64) (string, error) {
	return canonicalHash("activation-output/v1", func(e *Encoder) { e.String(activationID); e.Uint(generation) })
}

func EdgePathIdentity(sourceActivationID, sourcePathID, edgeID, targetReservationID, candidateID string) (string, error) {
	return canonicalHash("edge-token/v1", func(e *Encoder) {
		e.String(sourceActivationID)
		e.String(sourcePathID)
		e.String(edgeID)
		e.String(targetReservationID)
		e.String(candidateID)
	})
}

func ImpossibleEdgePathIdentity(causeDigest, edgeID, targetReservationID string) (string, error) {
	return canonicalHash("impossible-edge-token/v1", func(e *Encoder) { e.String(causeDigest); e.String(edgeID); e.String(targetReservationID) })
}

func ArrivalIdentity(edgePathID, targetReservationID, candidateID string) (string, error) {
	return canonicalHash("arrival/v1", func(e *Encoder) { e.String(edgePathID); e.String(targetReservationID); e.String(candidateID) })
}

func CandidateClosureKeyIdentity(reservationID, candidateID string) (string, error) {
	return canonicalHash("candidate-closure-key/v1", func(e *Encoder) { e.String(reservationID); e.String(candidateID) })
}

func CauseIdentity(sourcePathID string, terminalKind TerminalKind, dispositionReason, sourceActivationID, sourceCommandID, adminRecordID string, eventSeq uint64) (string, error) {
	return canonicalHash("candidate-cause/v1", func(e *Encoder) {
		e.String(sourcePathID)
		e.String(string(terminalKind))
		e.String(dispositionReason)
		e.String(sourceActivationID)
		e.String(sourceCommandID)
		e.String(adminRecordID)
		e.Uint(eventSeq)
	})
}

func CauseSetIdentity(causeIDs []CauseID) (string, error) {
	return canonicalHash("cause-set/v1", func(e *Encoder) { writeStringSet(e, causeIDs) })
}

func CandidateClosureIdentity(reservationID, candidateID string, terminalKind TerminalKind, causeDigest string) (string, error) {
	return canonicalHash("candidate-closure/v1", func(e *Encoder) {
		e.String(reservationID)
		e.String(candidateID)
		e.String(string(terminalKind))
		e.String(causeDigest)
	})
}

func CandidateLineageIdentity(parentLineageID, reservationID, candidateID string) (string, error) {
	return canonicalHash("candidate-lineage/v1", func(e *Encoder) { e.String(parentLineageID); e.String(reservationID); e.String(candidateID) })
}

func DetachmentKeyIdentity(reservationID, candidateID string) (string, error) {
	return canonicalHash("detachment-key/v1", func(e *Encoder) { e.String(reservationID); e.String(candidateID) })
}

func DetachmentIdentity(reservationID, candidateID, winnerPathID string, activatedSeq uint64) (string, error) {
	return canonicalHash("detachment/v1", func(e *Encoder) {
		e.String(reservationID)
		e.String(candidateID)
		e.String(winnerPathID)
		e.Uint(activatedSeq)
	})
}

func DetachmentSetIdentity(parentSetID, detachmentID string) (string, error) {
	return canonicalHash("detachment-set/v1", func(e *Encoder) { e.String(parentSetID); e.String(detachmentID) })
}

func AttemptIdentity(runID, activationID string, attempt uint64) (string, error) {
	return canonicalHash("attempt/v1", func(e *Encoder) { e.String(runID); e.String(activationID); e.Uint(attempt) })
}

func WaitIdentity(runID, activationID string, attempt uint64, waitKind string) (string, error) {
	return canonicalHash("wait/v1", func(e *Encoder) { e.String(runID); e.String(activationID); e.Uint(attempt); e.String(waitKind) })
}

func TimerIdentity(runID, activationID string, attempt uint64, sourceCommandID string) (string, error) {
	return canonicalHash("timer/v1", func(e *Encoder) { e.String(runID); e.String(activationID); e.Uint(attempt); e.String(sourceCommandID) })
}

func ContactIdentity(runID, activationID string, attempt uint64, assignee string) (string, error) {
	return canonicalHash("contact/v1", func(e *Encoder) { e.String(runID); e.String(activationID); e.Uint(attempt); e.String(assignee) })
}

func ObligationIdentity(runID, activationID string, attempt uint64, waitKind, assignee string) (string, error) {
	return canonicalHash("obligation/v1", func(e *Encoder) {
		e.String(runID)
		e.String(activationID)
		e.Uint(attempt)
		e.String(waitKind)
		e.String(assignee)
	})
}

func BlockIdentity(runID, activationID string, blockedAttempt uint64) (string, error) {
	return canonicalHash("block/v1", func(e *Encoder) { e.String(runID); e.String(activationID); e.Uint(blockedAttempt) })
}

func DispositionReceiptIdentity(pathID string, fromState, toState PathState, reasonCode, commandID, adminRecordID string, eventSeq uint64) (string, error) {
	return canonicalHash("disposition/v1", func(e *Encoder) {
		e.String(pathID)
		e.String(string(fromState))
		e.String(string(toState))
		e.String(reasonCode)
		e.String(commandID)
		e.String(adminRecordID)
		e.Uint(eventSeq)
	})
}

func ActivationReceiptIdentity(activationID, reservationID, inputSetDigest, outputPathID, commandID string, eventSeq uint64) (string, error) {
	return canonicalHash("activation-receipt/v1", func(e *Encoder) {
		e.String(activationID)
		e.String(reservationID)
		e.String(inputSetDigest)
		e.String(outputPathID)
		e.String(commandID)
		e.Uint(eventSeq)
	})
}

func PropagationPlanIdentity(rootReservationID, rootCandidateID, rootCauseDigest string, shard uint64, frontier []CandidateClosureKey) (string, error) {
	return canonicalHash("propagation-plan/v1", func(e *Encoder) {
		e.String(rootReservationID)
		e.String(rootCandidateID)
		e.String(rootCauseDigest)
		e.Uint(shard)
		writeStringList(e, frontier)
	})
}

func PropagationIntentIdentity(rootCauseDigest string, shard uint64, propagationPlanDigest string) (string, error) {
	return canonicalHash("propagation-intent/v1", func(e *Encoder) { e.String(rootCauseDigest); e.Uint(shard); e.String(propagationPlanDigest) })
}

func CandidateFoldIdentity(entries []CandidateFoldEntry) (string, error) {
	entries = append([]CandidateFoldEntry(nil), entries...)
	slices.SortFunc(entries, func(a, b CandidateFoldEntry) int {
		if n := cmp.Compare(a.CandidateID, b.CandidateID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.FoldKind, b.FoldKind); n != 0 {
			return n
		}
		return cmp.Compare(a.PathOrClosureID, b.PathOrClosureID)
	})
	entries = slices.Compact(entries)
	tuples := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		tuple, err := tupleBytes(func(e *Encoder) {
			e.String(entry.CandidateID)
			e.String(entry.FoldKind)
			e.String(entry.PathOrClosureID)
		})
		if err != nil {
			return "", err
		}
		tuples = append(tuples, tuple)
	}
	return canonicalHash("candidate-fold/v1", func(e *Encoder) { writeTupleList(e, tuples) })
}

func ActiveCommandIdentity(commandIDs []string) (string, error) {
	return canonicalHash("active-command-set/v1", func(e *Encoder) { writeStringSet(e, commandIDs) })
}

func BlockResolutionIdentity(resolution BlockResolution) (string, error) {
	return canonicalHash("block-resolution/v1", func(e *Encoder) {
		e.String(resolution.NodeID)
		e.Uint(resolution.BlockedAttempt)
		e.String(resolution.Decision)
		e.String(resolution.Actor)
		e.String(resolution.Reason)
		e.String(resolution.EvidenceRef)
		e.String(resolution.Timestamp)
	})
}

func AdminRecordIdentity(record PathV1AdminRecord) (string, error) {
	if record.EventSeq < 0 {
		return "", fmt.Errorf("negative admin event sequence")
	}
	return canonicalHash("admin-record/v1", func(e *Encoder) {
		e.String(record.RunID)
		e.Uint(uint64(record.EventSeq))
		e.String(record.AdminType)
		e.String(record.Actor)
		e.String(record.ReasonCode)
		e.String(record.EvidenceRef)
		e.String(record.ResolutionDigest)
	})
}

func LegacyAdminRecordIdentity(record PathV1AdminRecord) (string, error) {
	return canonicalHash("legacy-admin-record/v1", func(e *Encoder) {
		e.String(record.RunID)
		e.Uint(record.OriginalArrayIndex)
		e.String(record.AdminType)
		e.String(record.Actor)
		e.String(record.ReasonCode)
		e.String(record.EvidenceRef)
		e.String(record.Timestamp)
		e.String(record.ResolutionDigest)
	})
}

func CheckpointIdentity(basisRunStatus string, basisLastLogSeq uint64, basisLogChecksum string, checkpointProjectionJSON []byte) (string, error) {
	return canonicalHash("checkpoint/v1", func(e *Encoder) {
		e.String(basisRunStatus)
		e.Uint(basisLastLogSeq)
		e.String(basisLogChecksum)
		e.String(string(checkpointProjectionJSON))
	})
}

func PathFoldIdentity(entries []PathFoldEntry) (string, error) {
	entries = append([]PathFoldEntry(nil), entries...)
	slices.SortFunc(entries, func(a, b PathFoldEntry) int {
		if n := cmp.Compare(a.PathID, b.PathID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.State, b.State); n != 0 {
			return n
		}
		return cmp.Compare(a.UpdatedSeq, b.UpdatedSeq)
	})
	entries = slices.Compact(entries)
	tuples := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		tuple, err := tupleBytes(func(e *Encoder) { e.String(entry.PathID); e.String(string(entry.State)); e.Uint(entry.UpdatedSeq) })
		if err != nil {
			return "", err
		}
		tuples = append(tuples, tuple)
	}
	return canonicalHash("aggregate-paths/v1", func(e *Encoder) { writeTupleList(e, tuples) })
}

func ReservationFoldIdentity(entries []ReservationFoldEntry) (string, error) {
	entries = append([]ReservationFoldEntry(nil), entries...)
	slices.SortFunc(entries, func(a, b ReservationFoldEntry) int {
		if n := cmp.Compare(a.ReservationID, b.ReservationID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.State, b.State); n != 0 {
			return n
		}
		return cmp.Compare(a.EventSeq, b.EventSeq)
	})
	entries = slices.Compact(entries)
	tuples := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		tuple, err := tupleBytes(func(e *Encoder) { e.String(entry.ReservationID); e.String(string(entry.State)); e.Uint(entry.EventSeq) })
		if err != nil {
			return "", err
		}
		tuples = append(tuples, tuple)
	}
	return canonicalHash("aggregate-reservations/v1", func(e *Encoder) { writeTupleList(e, tuples) })
}

func PropagationFoldIdentity(entries []PropagationFoldEntry) (string, error) {
	entries = append([]PropagationFoldEntry(nil), entries...)
	slices.SortFunc(entries, func(a, b PropagationFoldEntry) int {
		if n := cmp.Compare(a.IntentID, b.IntentID); n != 0 {
			return n
		}
		if n := cmp.Compare(a.State, b.State); n != 0 {
			return n
		}
		return cmp.Compare(a.Cursor, b.Cursor)
	})
	entries = slices.Compact(entries)
	tuples := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		tuple, err := tupleBytes(func(e *Encoder) { e.String(entry.IntentID); e.String(string(entry.State)); e.Uint(entry.Cursor) })
		if err != nil {
			return "", err
		}
		tuples = append(tuples, tuple)
	}
	return canonicalHash("aggregate-propagation/v1", func(e *Encoder) { writeTupleList(e, tuples) })
}

func SideEffectFoldIdentity(entries []SideEffectFoldEntry) (string, error) {
	tuples := make([][]byte, 0, len(entries))
	for _, entry := range entries {
		tuple, err := tupleBytes(func(e *Encoder) { e.String(string(entry.Kind)); e.String(entry.ID); e.String(entry.State) })
		if err != nil {
			return "", err
		}
		tuples = append(tuples, tuple)
	}
	// The contract sorts the complete canonical tuple bytes, not the raw Go
	// fields. Length-prefixed string encoding can produce a different order.
	slices.SortFunc(tuples, bytes.Compare)
	tuples = slices.CompactFunc(tuples, bytes.Equal)
	return canonicalHash("aggregate-side-effects/v1", func(e *Encoder) { writeTupleList(e, tuples) })
}

func AggregateIdentity(runID, templateRef, checkpointDigest, pathFoldDigest, reservationFoldDigest, propagationFoldDigest, sideEffectFoldDigest, terminalCauseDigest string) (string, error) {
	return canonicalHash("aggregate/v1", func(e *Encoder) {
		e.String(runID)
		e.String(templateRef)
		e.String(checkpointDigest)
		e.String(pathFoldDigest)
		e.String(reservationFoldDigest)
		e.String(propagationFoldDigest)
		e.String(sideEffectFoldDigest)
		e.String(terminalCauseDigest)
	})
}

func CommandIdentityDigest(identity CommandIdentity) (string, error) {
	return canonicalHash("command/v1", func(e *Encoder) {
		e.String(identity.RunID)
		e.String(string(identity.Kind))
		e.Uint(identity.PayloadSchema)
		e.String(identity.SourceActivationID)
		e.Uint(identity.SourceGeneration)
		e.String(identity.SourcePathID)
		e.String(identity.TargetReservationID)
		e.Uint(identity.TargetGeneration)
		e.Uint(identity.Attempt)
		e.String(identity.InputDigest)
		e.String(identity.CauseDigest)
		e.String(identity.PlanDigest)
		e.String(identity.ResultCode)
	})
}

func CommandIdempotencyKey(kind CommandKindV1, commandID string) string {
	return "path-v1/" + string(kind) + "/" + commandID
}
