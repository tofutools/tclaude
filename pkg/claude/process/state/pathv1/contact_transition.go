package pathv1

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ExclusiveContactPlan is the sealed read view of one durable contact:
// schedule record plus its marker state. Mutation happens only through the
// exact transition constructors below.
type ExclusiveContactPlan struct {
	record ContactRecordV7
	state  string
}

func (p *ExclusiveContactPlan) Record() ContactRecordV7 {
	if p == nil {
		return ContactRecordV7{}
	}
	return p.record
}

func (p *ExclusiveContactPlan) State() string {
	if p == nil {
		return ""
	}
	return p.state
}

func (p *ExclusiveContactPlan) ID() string {
	if p == nil {
		return ""
	}
	return p.record.ID
}

// ContactScheduleV7 is the exact creation authority for one durable contact.
// The caller (executor) resolves it from the performer schedule and the
// dispatch-recovered assignee before sealing.
type ContactScheduleV7 struct {
	SourceCommandID  string
	Assignee         string
	Cadence          string
	Budget           uint64
	EscalationTarget string
	Provenance       ContactProvenance
}

// RecoverExclusiveContacts returns every contact in stable ID order,
// including terminal ones; callers filter by State. Pre-contact checkpoints
// return an empty slice.
func RecoverExclusiveContacts(ctx context.Context, input *VerifiedExclusiveInput) ([]*ExclusiveContactPlan, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.checkpoint == nil {
		return nil, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	found := make([]*ExclusiveContactPlan, 0, len(aggregate.Contacts))
	for id, record := range aggregate.Contacts {
		marker, ok := aggregate.SideEffects[id]
		if !ok || marker.Kind != SideEffectContact {
			return nil, fmt.Errorf("%w: contact %q has no side-effect marker", ErrMutationInconsistent, id)
		}
		found = append(found, &ExclusiveContactPlan{record: record, state: marker.State})
	}
	slices.SortFunc(found, func(a, b *ExclusiveContactPlan) int {
		return strings.Compare(a.record.ID, b.record.ID)
	})
	return found, nil
}

// ScheduleExclusiveContact seals the creation of the one durable contact for
// an active deferred perform_attempt_v1 command. now is scheduling authority
// exactly like PlanExclusiveWait's; every derived instant is recorded
// canonically and never recomputed on replay.
func ScheduleExclusiveContact(ctx context.Context, input *VerifiedExclusiveInput, schedule ContactScheduleV7, now time.Time) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.checkpoint == nil {
		return nil, fmt.Errorf("%w: sealed input is required", ErrExclusiveInputInvalid)
	}
	cadence, err := ParseContactCadence(schedule.Cadence)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: contact scheduling instant is required", ErrMutationInvalid)
	}
	if !schedule.Provenance.Valid() || schedule.Provenance == ContactProvenanceLegacyProjection {
		// Legacy projection is reserved for the progressed-history migrator's
		// own initialization authority, not the live executor.
		return nil, fmt.Errorf("%w: invalid contact provenance %q", ErrMutationInvalid, schedule.Provenance)
	}
	kind, ok := contactKindForAssignee(schedule.Assignee)
	if !ok {
		return nil, fmt.Errorf("%w: contact assignee %q has no contact kind", ErrMutationInvalid, schedule.Assignee)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	command, ok := aggregate.Commands[schedule.SourceCommandID]
	if !ok || command.Identity.Kind != CommandPerformAttempt || !command.State.Active() {
		return nil, fmt.Errorf("%w: contact source command is not an active perform attempt", ErrMutationInconsistent)
	}
	for _, existing := range aggregate.Contacts {
		if existing.ActivationID == command.Identity.SourceActivationID && existing.Attempt == command.Identity.Attempt {
			return nil, fmt.Errorf("%w: activation attempt already has contact %q", ErrMutationInconsistent, existing.ID)
		}
	}
	seq := int64(CurrentLastLogSeq(input.checkpoint)) + 1
	record := ContactRecordV7{
		RunID:            aggregate.RunID,
		ActivationID:     command.Identity.SourceActivationID,
		Attempt:          command.Identity.Attempt,
		SourceCommandID:  command.ID,
		Assignee:         schedule.Assignee,
		Kind:             kind,
		Provenance:       schedule.Provenance,
		Cadence:          schedule.Cadence,
		Budget:           schedule.Budget,
		EscalationTarget: schedule.EscalationTarget,
		ScheduledAt:      CanonicalTimestamp(now.UTC()),
		NextContactAt:    CanonicalTimestamp(now.UTC().Add(cadence)),
		EventSeq:         seq,
	}
	record.ID, err = ContactIdentity(record.RunID, record.ActivationID, record.Attempt, record.Assignee)
	if err != nil {
		return nil, err
	}
	if err := ValidateContactRecord(record); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	marker := SideEffectIdentity{
		Kind: SideEffectContact, ID: record.ID, RunID: record.RunID,
		ActivationID: record.ActivationID, Attempt: record.Attempt,
		Assignee: record.Assignee, State: ContactStateScheduled,
	}
	if err := ValidateSideEffect(marker); err != nil {
		return nil, err
	}
	if _, exists := aggregate.SideEffects[marker.ID]; exists {
		return nil, fmt.Errorf("%w: contact marker already exists", ErrMutationInconsistent)
	}
	if aggregate.Contacts == nil {
		aggregate.Contacts = map[string]ContactRecordV7{}
	}
	aggregate.Contacts[record.ID] = record
	aggregate.SideEffects[marker.ID] = marker
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint))
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, TransitionScheduleContact)
}

// contactMutation seals one exact record/marker change. Every public contact
// transition below reduces to it; preconditions were checked by the caller
// against the same detached aggregate.
func contactMutation(input *VerifiedExclusiveInput, aggregate AggregateCheckpoint, record ContactRecordV7, markerState, label string) (*ExecutionTransition, error) {
	record.EventSeq = int64(CurrentLastLogSeq(input.checkpoint)) + 1
	if err := ValidateContactRecord(record); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	marker := aggregate.SideEffects[record.ID]
	marker.State = markerState
	if err := ValidateSideEffect(marker); err != nil {
		return nil, err
	}
	aggregate.Contacts[record.ID] = record
	aggregate.SideEffects[record.ID] = marker
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, CurrentRunStatus(input.checkpoint))
	if err != nil {
		return nil, err
	}
	return newExecutionTransition(input.checkpoint, next, label)
}

// currentContact re-derives the exact contact pair from the sealed input; the
// supplied plan must match it byte-for-byte to mutate.
func currentContact(input *VerifiedExclusiveInput, contactID string) (AggregateCheckpoint, ContactRecordV7, SideEffectIdentity, error) {
	var record ContactRecordV7
	var marker SideEffectIdentity
	if input == nil || input.checkpoint == nil || contactID == "" {
		return AggregateCheckpoint{}, record, marker, fmt.Errorf("%w: sealed input and contact id are required", ErrExclusiveInputInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return AggregateCheckpoint{}, record, marker, err
	}
	record, ok := aggregate.Contacts[contactID]
	if !ok {
		return AggregateCheckpoint{}, record, marker, fmt.Errorf("%w: contact %q is absent", ErrMutationInconsistent, contactID)
	}
	marker, ok = aggregate.SideEffects[contactID]
	if !ok || marker.Kind != SideEffectContact {
		return AggregateCheckpoint{}, record, marker, fmt.Errorf("%w: contact %q has no side-effect marker", ErrMutationInconsistent, contactID)
	}
	return aggregate, record, marker, nil
}

// MarkExclusiveContactDue makes the poll-observed due condition durable:
// scheduled becomes due exactly when now has reached the recorded next
// contact instant. The external send is a later, separate service step.
func MarkExclusiveContactDue(ctx context.Context, input *VerifiedExclusiveInput, contactID string, now time.Time) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, record, marker, err := currentContact(input, contactID)
	if err != nil {
		return nil, err
	}
	if marker.State != ContactStateScheduled {
		return nil, fmt.Errorf("%w: contact %q is not scheduled", ErrMutationInconsistent, contactID)
	}
	if record.NextContactAt == "" || record.EscalatedAt != "" {
		return nil, fmt.Errorf("%w: contact %q has no pending nudge schedule", ErrMutationInconsistent, contactID)
	}
	next, err := ParseCanonicalTimestamp(record.NextContactAt)
	if err != nil {
		return nil, err
	}
	if now.Before(next) {
		return nil, fmt.Errorf("%w: contact %q is not due until %s", ErrMutationInvalid, contactID, record.NextContactAt)
	}
	return contactMutation(input, aggregate, record, ContactStateDue, TransitionMarkContactDue)
}

// NudgeExclusiveContact seals a delivered non-escalation nudge. The caller
// performed the external send from the durable due state first; a crash
// between send and this append re-observes due and may resend once — the
// accepted legacy duplicate window, bounded to the due state.
func NudgeExclusiveContact(ctx context.Context, input *VerifiedExclusiveInput, contactID string, now time.Time) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, record, marker, err := currentContact(input, contactID)
	if err != nil {
		return nil, err
	}
	if marker.State != ContactStateDue {
		return nil, fmt.Errorf("%w: contact %q is not due", ErrMutationInconsistent, contactID)
	}
	if record.Used >= record.Budget {
		return nil, fmt.Errorf("%w: contact %q has exhausted its nudge budget", ErrMutationInconsistent, contactID)
	}
	cadence, err := ParseContactCadence(record.Cadence)
	if err != nil {
		return nil, err
	}
	record.Used++
	record.LastContactedAt = CanonicalTimestamp(now.UTC())
	record.NextContactAt = CanonicalTimestamp(now.UTC().Add(cadence))
	return contactMutation(input, aggregate, record, ContactStateScheduled, TransitionNudgeContact)
}

// EscalateExclusiveContact seals the exactly-once escalation after the budget
// is exhausted. The contact stays active (blocking completion) with no
// further due instant until its attempt settles.
func EscalateExclusiveContact(ctx context.Context, input *VerifiedExclusiveInput, contactID string, now time.Time) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, record, marker, err := currentContact(input, contactID)
	if err != nil {
		return nil, err
	}
	if marker.State != ContactStateDue {
		return nil, fmt.Errorf("%w: contact %q is not due", ErrMutationInconsistent, contactID)
	}
	if record.Used != record.Budget {
		return nil, fmt.Errorf("%w: contact %q still has nudge budget", ErrMutationInconsistent, contactID)
	}
	if record.EscalatedAt != "" {
		return nil, fmt.Errorf("%w: contact %q is already escalated", ErrMutationInconsistent, contactID)
	}
	record.EscalatedAt = CanonicalTimestamp(now.UTC())
	record.NextContactAt = ""
	return contactMutation(input, aggregate, record, ContactStateScheduled, TransitionEscalateContact)
}

// PauseExclusiveContact freezes automation with an explicit reason (human
// preemption). The schedule is retained; resuming re-derives the next instant.
func PauseExclusiveContact(ctx context.Context, input *VerifiedExclusiveInput, contactID, reason string) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, record, marker, err := currentContact(input, contactID)
	if err != nil {
		return nil, err
	}
	if marker.State != ContactStateScheduled && marker.State != ContactStateDue {
		return nil, fmt.Errorf("%w: contact %q is not pausable", ErrMutationInconsistent, contactID)
	}
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("%w: contact pause requires a reason", ErrMutationInvalid)
	}
	record.PauseReason = strings.TrimSpace(reason)
	return contactMutation(input, aggregate, record, ContactStatePaused, TransitionPauseContact)
}

// LatchExclusiveContactHumanInteraction records observed human activity on the
// live performer session without changing lifecycle state. The pause decision
// itself belongs to the shared decision core's grace rule.
func LatchExclusiveContactHumanInteraction(ctx context.Context, input *VerifiedExclusiveInput, contactID string, at time.Time) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, record, marker, err := currentContact(input, contactID)
	if err != nil {
		return nil, err
	}
	if !ActiveSideEffect(marker) {
		return nil, fmt.Errorf("%w: contact %q is not active", ErrMutationInconsistent, contactID)
	}
	record.HumanInteractedAt = CanonicalTimestamp(at.UTC())
	return contactMutation(input, aggregate, record, marker.State, TransitionLatchContactHuman)
}

// ClearExclusiveContactHumanLatch removes the human-interaction latch once
// delivery metadata proves the activity was tclaude's own automation. A pause
// held for that same latch is released.
func ClearExclusiveContactHumanLatch(ctx context.Context, input *VerifiedExclusiveInput, contactID, pauseReason string) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, record, marker, err := currentContact(input, contactID)
	if err != nil {
		return nil, err
	}
	if !ActiveSideEffect(marker) {
		return nil, fmt.Errorf("%w: contact %q is not active", ErrMutationInconsistent, contactID)
	}
	record.HumanInteractedAt = ""
	state := marker.State
	if marker.State == ContactStatePaused && record.PauseReason == pauseReason {
		record.PauseReason = ""
		state = ContactStateScheduled
	}
	return contactMutation(input, aggregate, record, state, TransitionClearContactHumanLatch)
}

// RecoverExclusiveContactReset applies performer-recovery semantics: budget
// and escalation reset, latches cleared, and the cadence restarted from now.
// It applies from any active state, releasing a pause.
func RecoverExclusiveContactReset(ctx context.Context, input *VerifiedExclusiveInput, contactID string, recoveredAt, now time.Time) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aggregate, record, marker, err := currentContact(input, contactID)
	if err != nil {
		return nil, err
	}
	if !ActiveSideEffect(marker) {
		return nil, fmt.Errorf("%w: contact %q is not active", ErrMutationInconsistent, contactID)
	}
	cadence, err := ParseContactCadence(record.Cadence)
	if err != nil {
		return nil, err
	}
	record.Used = 0
	record.EscalatedAt = ""
	record.LastRecoveredAt = CanonicalTimestamp(recoveredAt.UTC())
	record.HumanInteractedAt = ""
	record.PauseReason = ""
	record.NextContactAt = CanonicalTimestamp(now.UTC().Add(cadence))
	return contactMutation(input, aggregate, record, ContactStateScheduled, TransitionRecoverContact)
}

// completeContactsForSettledCommand composes contact settlement into a command
// settle/cancel mutation. It is intentionally unexported: settlement and
// contact completion are one sealed aggregate change, never two transitions.
// Commands without contacts (waits, completion, non-deferred attempts) are
// untouched.
func completeContactsForSettledCommand(aggregate *AggregateCheckpoint, commandID string, canceled bool, eventSeq int64) {
	for id, record := range aggregate.Contacts {
		if record.SourceCommandID != commandID {
			continue
		}
		marker, ok := aggregate.SideEffects[id]
		if !ok || marker.Kind != SideEffectContact || !ActiveSideEffect(marker) {
			continue
		}
		marker.State = ContactStateCompleted
		if canceled {
			marker.State = ContactStateCanceled
		}
		record.NextContactAt = ""
		record.PauseReason = ""
		record.EventSeq = eventSeq
		aggregate.Contacts[id] = record
		aggregate.SideEffects[id] = marker
	}
}
