package execution

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
)

// OperationID identifies one durable lifecycle request. It is a correlation
// value only; possession of it (or an intended ExecutionID) never authorizes a
// child to claim an operation.
type OperationID string

func (id OperationID) String() string { return string(id) }

// ResumeState is the portable lifecycle of a daemon-managed resume request.
// Internal launch phases (claim, registered, release-granted) are persisted by
// the host adapter; these states deliberately describe only user-visible
// operation evidence.
type ResumeState string

const (
	ResumeRequested  ResumeState = "requested"
	ResumeAccepted   ResumeState = "accepted"
	ResumeStarted    ResumeState = "started"
	ResumeReady      ResumeState = "ready"
	ResumeRejected   ResumeState = "rejected"
	ResumeFailed     ResumeState = "failed"
	ResumeCancelling ResumeState = "cancelling"
	ResumeCancelled  ResumeState = "cancelled"
	ResumeUnknown    ResumeState = "unknown"
)

// ResumeOperation is the portable evidence needed to reconcile one request.
// The host/database adapter owns timestamps, revision CAS, and process/gate
// observations; this type contains no HTTP, SQLite, tmux, or row details.
type ResumeOperation struct {
	ID                OperationID
	State             ResumeState
	Attempt           AttemptRef
	Predecessor       AttemptRef
	Revision          int64
	DispatchDetail    string
	FailureDetail     string
	RecoveryReference string
	RequestedAt       time.Time
	AcceptedAt        time.Time
	StartedAt         time.Time
	ReadyAt           time.Time
}

var operationCounter atomic.Uint64

// NewOperationID returns an opaque non-secret correlation value. A fallback
// preserves launch progress when the host random source is unavailable; the
// value remains unusable as child authority without the private claim proof.
func NewOperationID() OperationID {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return OperationID("op_" + hex.EncodeToString(raw[:]))
	}
	return OperationID(fmt.Sprintf("op_fallback_%d", operationCounter.Add(1)))
}

// ValidateOperationID checks the persisted public representation.
func ValidateOperationID(id OperationID) error {
	raw := strings.TrimSpace(string(id))
	if len(raw) != len("op_")+32 || !strings.HasPrefix(raw, "op_") {
		// Fallback IDs are intentionally accepted for the same RNG failure mode
		// as NewID; they remain correlation-only and are never auth material.
		if len(raw) > len("op_fallback_") && strings.HasPrefix(raw, "op_fallback_") {
			return nil
		}
		return errors.New("operation id must be an opaque op_ value")
	}
	encoded := raw[len("op_"):]
	if _, err := hex.DecodeString(encoded); err != nil || encoded != strings.ToLower(encoded) {
		return errors.New("operation id must contain lowercase hexadecimal data")
	}
	return nil
}

func terminalResumeState(state ResumeState) bool {
	return state == ResumeReady || state == ResumeRejected || state == ResumeFailed || state == ResumeCancelled
}

// CanTransition reports whether a durable CAS may move an operation between
// portable states. Unknown is unresolved (not terminal): exact late evidence
// may establish started/ready, while replay is still blocked.
func CanTransition(from, to ResumeState) bool {
	if from == to {
		return true
	}
	if terminalResumeState(from) {
		return false
	}
	switch from {
	case ResumeRequested:
		return to == ResumeAccepted || to == ResumeRejected || to == ResumeCancelling
	case ResumeAccepted:
		return to == ResumeStarted || to == ResumeReady || to == ResumeFailed || to == ResumeCancelling || to == ResumeUnknown
	case ResumeStarted:
		return to == ResumeReady || to == ResumeFailed || to == ResumeCancelling || to == ResumeUnknown
	case ResumeCancelling:
		return to == ResumeCancelled || to == ResumeFailed || to == ResumeUnknown
	case ResumeUnknown:
		return to == ResumeStarted || to == ResumeReady || to == ResumeFailed || to == ResumeCancelling
	default:
		return false
	}
}

// ValidateTransition centralizes state and revision checks for adapters before
// they issue their storage CAS. A cancelled or failed operation cannot be
// revived by late process observations.
func ValidateTransition(op ResumeOperation, to ResumeState, expectedRevision int64) error {
	if err := ValidateOperationID(op.ID); err != nil {
		return err
	}
	if expectedRevision != op.Revision {
		return fmt.Errorf("stale resume operation revision %d (current %d)", expectedRevision, op.Revision)
	}
	if !CanTransition(op.State, to) {
		return fmt.Errorf("resume operation cannot transition %s -> %s", op.State, to)
	}
	if to == ResumeReady && strings.TrimSpace(op.Attempt.ExecutionID.String()) == "" {
		return errors.New("ready resume operation requires an intended execution id")
	}
	return nil
}
