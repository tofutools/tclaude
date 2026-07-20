package pathv1

import (
	"fmt"
	"strings"
	"time"
)

// MaxContactFieldBytes bounds every free-form contact string (assignee,
// escalation target, pause reason). Cadence keeps the tighter legacy viewer
// bound so a durable schedule can never smuggle an unbounded payload.
const (
	MaxContactFieldBytes   = 256
	MaxContactCadenceBytes = 32
)

// ContactKind is the closed performer-kind vocabulary a schema-7 contact may
// service. Program performers are rejected by the schema-7 release and
// therefore have no contact kind.
type ContactKind string

const (
	ContactKindAgent ContactKind = "agent"
	ContactKindHuman ContactKind = "human"
)

func (k ContactKind) Valid() bool {
	return k == ContactKindAgent || k == ContactKindHuman
}

// contactKindForAssignee mirrors the legacy owner-prefix table: agents are
// "agent:", humans are "human:" or "role:". Program/system/engine owners have
// no schema-7 contact identity.
func contactKindForAssignee(assignee string) (ContactKind, bool) {
	switch {
	case strings.HasPrefix(assignee, "agent:"):
		return ContactKindAgent, true
	case strings.HasPrefix(assignee, "human:"), strings.HasPrefix(assignee, "role:"):
		return ContactKindHuman, true
	default:
		return "", false
	}
}

// ContactProvenance is the closed origin vocabulary for a durable schema-7
// contact schedule.
type ContactProvenance string

const (
	// ContactProvenanceDispatch marks a schedule created by the same executor
	// generation that dispatched the deferred attempt.
	ContactProvenanceDispatch ContactProvenance = "dispatch"
	// ContactProvenanceLateInitialization marks a schedule back-filled for an
	// attempt that was already in flight before contact parity existed (or
	// whose dispatch crashed before the schedule append).
	ContactProvenanceLateInitialization ContactProvenance = "late_initialization"
	// ContactProvenanceLegacyProjection is reserved for the progressed-history
	// parity migrator projecting a legacy v6 ContactState.
	ContactProvenanceLegacyProjection ContactProvenance = "legacy_projection"
)

func (p ContactProvenance) Valid() bool {
	switch p {
	case ContactProvenanceDispatch, ContactProvenanceLateInitialization, ContactProvenanceLegacyProjection:
		return true
	}
	return false
}

// Contact marker states. The SideEffectContact marker is the sole lifecycle
// authority; ContactRecordV7 never duplicates it.
const (
	ContactStateScheduled = "scheduled"
	ContactStateDue       = "due"
	ContactStatePaused    = "paused"
	ContactStateCompleted = "completed"
	ContactStateCanceled  = "canceled"
)

// ContactRecordV7 is the durable schema-7 reminder/escalation schedule for one
// deferred performer attempt. Lifecycle state lives exclusively on the paired
// SideEffectContact marker; this record carries the schedule bound at creation
// plus monotonic progress. It is a lossless superset of the legacy v6
// ContactState so the progressed-history migrator has an exact target.
// Timestamps are canonical RFC3339Nano UTC strings; empty means unset.
type ContactRecordV7 struct {
	ID              string            `json:"id"`
	RunID           string            `json:"runId"`
	ActivationID    ActivationID      `json:"activationId"`
	Attempt         uint64            `json:"attempt"`
	SourceCommandID string            `json:"sourceCommandId"`
	Assignee        string            `json:"assignee"`
	Kind            ContactKind       `json:"kind"`
	Provenance      ContactProvenance `json:"provenance"`

	Cadence          string `json:"cadence"`
	Budget           uint64 `json:"budget"`
	EscalationTarget string `json:"escalationTarget"`

	Used            uint64 `json:"used,omitempty"`
	ScheduledAt     string `json:"scheduledAt"`
	LastContactedAt string `json:"lastContactedAt,omitempty"`
	NextContactAt   string `json:"nextContactAt,omitempty"`
	LastRecoveredAt string `json:"lastRecoveredAt,omitempty"`
	EscalatedAt     string `json:"escalatedAt,omitempty"`
	PauseReason     string `json:"pauseReason,omitempty"`
	// LegacyPauseReason preserves a terminal schema-6 contact's historical
	// pause explanation without weakening the live invariant that PauseReason
	// is present iff the contact marker is actively paused.
	LegacyPauseReason string `json:"legacyPauseReason,omitempty"`
	HumanInteractedAt string `json:"humanInteractedAt,omitempty"`

	EventSeq int64 `json:"eventSeq"`
}

// ParseContactCadence is the single cadence gate: bounded, parseable, and
// strictly positive, or the schedule is invalid.
func ParseContactCadence(cadence string) (time.Duration, error) {
	if cadence == "" || len(cadence) > MaxContactCadenceBytes {
		return 0, fmt.Errorf("contact cadence is empty or over %d bytes", MaxContactCadenceBytes)
	}
	parsed, err := time.ParseDuration(cadence)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("invalid contact cadence %q", cadence)
	}
	return parsed, nil
}

// ValidateContactRecord checks every marker-independent invariant of one
// record. Marker/command coherence is aggregate-level and lives in
// ValidateAggregate.
func ValidateContactRecord(record ContactRecordV7) error {
	if record.RunID == "" || record.ActivationID == "" || record.Attempt == 0 {
		return fmt.Errorf("contact record lacks run/activation/attempt identity")
	}
	if record.Assignee == "" || len(record.Assignee) > MaxContactFieldBytes {
		return fmt.Errorf("contact assignee is empty or over %d bytes", MaxContactFieldBytes)
	}
	wantID, err := ContactIdentity(record.RunID, record.ActivationID, record.Attempt, record.Assignee)
	if err != nil {
		return err
	}
	if record.ID != wantID {
		return fmt.Errorf("contact identity mismatch")
	}
	if !record.Kind.Valid() {
		return fmt.Errorf("invalid contact kind %q", record.Kind)
	}
	if kind, ok := contactKindForAssignee(record.Assignee); !ok || kind != record.Kind {
		return fmt.Errorf("contact assignee %q does not cohere with kind %q", record.Assignee, record.Kind)
	}
	if !record.Provenance.Valid() {
		return fmt.Errorf("invalid contact provenance %q", record.Provenance)
	}
	if record.LegacyPauseReason != "" && record.Provenance != ContactProvenanceLegacyProjection {
		return fmt.Errorf("legacy pause reason requires legacy projection provenance")
	}
	if record.LegacyPauseReason != "" && record.PauseReason != "" {
		return fmt.Errorf("live and historical contact pause reasons are mutually exclusive")
	}
	if record.SourceCommandID == "" {
		return fmt.Errorf("contact record lacks a source command")
	}
	if _, err := ParseContactCadence(record.Cadence); err != nil {
		return err
	}
	if record.Budget == 0 {
		return fmt.Errorf("contact budget must be positive")
	}
	if record.Used > record.Budget {
		return fmt.Errorf("contact used %d exceeds budget %d", record.Used, record.Budget)
	}
	if record.EscalationTarget == "" || len(record.EscalationTarget) > MaxContactFieldBytes {
		return fmt.Errorf("contact escalation target is empty or over %d bytes", MaxContactFieldBytes)
	}
	if len(record.PauseReason) > MaxContactFieldBytes || len(record.LegacyPauseReason) > MaxContactFieldBytes {
		return fmt.Errorf("contact pause reason is over %d bytes", MaxContactFieldBytes)
	}
	if record.ScheduledAt == "" {
		return fmt.Errorf("contact record lacks its scheduling instant")
	}
	for _, value := range []string{
		record.ScheduledAt, record.LastContactedAt, record.NextContactAt,
		record.LastRecoveredAt, record.EscalatedAt, record.HumanInteractedAt,
	} {
		if _, err := ParseCanonicalTimestamp(value); err != nil {
			return err
		}
	}
	if record.EscalatedAt != "" {
		if record.Used != record.Budget {
			return fmt.Errorf("escalated contact must have exhausted its budget")
		}
		if record.NextContactAt != "" {
			return fmt.Errorf("escalated contact cannot have a next contact time")
		}
	}
	if record.EventSeq <= 0 {
		return fmt.Errorf("contact record requires a positive event sequence")
	}
	return nil
}
