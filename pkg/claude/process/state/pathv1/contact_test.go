package pathv1

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

var contactTestTemplate = []byte(`apiVersion: tclaude.dev/v1alpha1
kind: ProcessTemplate
id: contact-parity
start: work
nodes:
  work:
    type: task
    performer: {kind: agent, prompt: work}
    next: {pass: done, fail: failed}
  done: {type: end}
  failed: {type: end, result: failed}
`)

func contactTestBase() time.Time {
	return time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
}

// claimedContactCheckpoint returns sealed bytes with one active deferred
// attempt claim plus that claim's command ID.
func claimedContactCheckpoint(t *testing.T) ([]byte, string) {
	t.Helper()
	source := contactTestTemplate
	initial := initializedExclusiveCheckpoint(t, source)
	input, err := VerifyExclusiveInput(t.Context(), initial, source)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := DecodeCheckpointV7(initial)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanExclusiveAttempt(t.Context(), input, aggregate.Authority.Genesis.OutputPathID, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := ClaimExclusiveAttempt(t.Context(), input, plan)
	if err != nil {
		t.Fatal(err)
	}
	_, claimedBytes, _, err := ValidateExecutionTransitionForAppend(t.Context(), initial, source, claim)
	if err != nil {
		t.Fatal(err)
	}
	return claimedBytes, plan.Command().ID
}

func applyContactTransition(t *testing.T, pre []byte, build func(input *VerifiedExclusiveInput) (*ExecutionTransition, error)) []byte {
	t.Helper()
	input, err := VerifyExclusiveInput(t.Context(), pre, contactTestTemplate)
	if err != nil {
		t.Fatal(err)
	}
	transition, err := build(input)
	if err != nil {
		t.Fatal(err)
	}
	_, post, _, err := ValidateExecutionTransitionForAppend(t.Context(), pre, contactTestTemplate, transition)
	if err != nil {
		t.Fatal(err)
	}
	return post
}

func contactPair(t *testing.T, sealed []byte) (ContactRecordV7, SideEffectIdentity) {
	t.Helper()
	checkpoint, err := DecodeCheckpointV7(sealed)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregate.Contacts) != 1 {
		t.Fatalf("contacts = %d, want 1", len(aggregate.Contacts))
	}
	for id, record := range aggregate.Contacts {
		marker, ok := aggregate.SideEffects[id]
		if !ok {
			t.Fatalf("contact %q has no marker", id)
		}
		return record, marker
	}
	panic("unreachable")
}

func testContactSchedule(commandID string) ContactScheduleV7 {
	return ContactScheduleV7{
		SourceCommandID: commandID, Assignee: "agent:agt_worker", Cadence: "5m",
		Budget: 2, EscalationTarget: "human:operator", Provenance: ContactProvenanceDispatch,
	}
}

func TestContactScheduleNudgeEscalateAndAtomicCompletion(t *testing.T) {
	claimed, commandID := claimedContactCheckpoint(t)
	base := contactTestBase()

	scheduled := applyContactTransition(t, claimed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return ScheduleExclusiveContact(t.Context(), input, testContactSchedule(commandID), base)
	})
	record, marker := contactPair(t, scheduled)
	if marker.State != ContactStateScheduled || record.Used != 0 || record.Kind != ContactKindAgent {
		t.Fatalf("scheduled contact = %+v marker=%q", record, marker.State)
	}
	if record.NextContactAt != CanonicalTimestamp(base.Add(5*time.Minute)) {
		t.Fatalf("next contact at = %q", record.NextContactAt)
	}

	// Early due-marking is refused; the durable due state appears only once
	// the recorded instant has passed.
	input, err := VerifyExclusiveInput(t.Context(), scheduled, contactTestTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MarkExclusiveContactDue(t.Context(), input, record.ID, base.Add(time.Minute)); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("early due error = %v", err)
	}

	sealed := scheduled
	now := base
	for nudge := uint64(1); nudge <= 2; nudge++ {
		now = now.Add(5 * time.Minute)
		sealed = applyContactTransition(t, sealed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
			return MarkExclusiveContactDue(t.Context(), input, record.ID, now)
		})
		if _, marker := contactPair(t, sealed); marker.State != ContactStateDue {
			t.Fatalf("marker after due = %q", marker.State)
		}
		sealed = applyContactTransition(t, sealed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
			return NudgeExclusiveContact(t.Context(), input, record.ID, now)
		})
		got, marker := contactPair(t, sealed)
		if marker.State != ContactStateScheduled || got.Used != nudge || got.LastContactedAt != CanonicalTimestamp(now) {
			t.Fatalf("after nudge %d: used=%d marker=%q last=%q", nudge, got.Used, marker.State, got.LastContactedAt)
		}
	}

	// Budget exhausted: next due tick escalates exactly once and clears the
	// nudge schedule while the contact stays active.
	now = now.Add(5 * time.Minute)
	sealed = applyContactTransition(t, sealed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return MarkExclusiveContactDue(t.Context(), input, record.ID, now)
	})
	escalateInput, err := VerifyExclusiveInput(t.Context(), sealed, contactTestTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NudgeExclusiveContact(t.Context(), escalateInput, record.ID, now); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("over-budget nudge error = %v", err)
	}
	sealed = applyContactTransition(t, sealed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return EscalateExclusiveContact(t.Context(), input, record.ID, now)
	})
	got, marker := contactPair(t, sealed)
	if marker.State != ContactStateScheduled || got.EscalatedAt != CanonicalTimestamp(now) || got.NextContactAt != "" {
		t.Fatalf("after escalate: %+v marker=%q", got, marker.State)
	}
	postEscalate, err := VerifyExclusiveInput(t.Context(), sealed, contactTestTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MarkExclusiveContactDue(t.Context(), postEscalate, record.ID, now.Add(time.Hour)); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("post-escalation due error = %v", err)
	}

	// Settlement completes the contact in the SAME sealed transition that
	// observes the attempt command.
	observeInput, err := VerifyExclusiveInput(t.Context(), sealed, contactTestTemplate)
	if err != nil {
		t.Fatal(err)
	}
	recovered, found, err := RecoverExclusiveAttempt(t.Context(), observeInput)
	if err != nil || !found {
		t.Fatalf("recover claim: found=%v err=%v", found, err)
	}
	observe, err := ObserveExclusiveAttempt(t.Context(), observeInput, recovered, ExclusiveObservation{Outcome: "pass", Actor: "human:operator"}, false)
	if err != nil {
		t.Fatal(err)
	}
	_, observedBytes, _, err := ValidateExecutionTransitionForAppend(t.Context(), sealed, contactTestTemplate, observe)
	if err != nil {
		t.Fatal(err)
	}
	finalRecord, finalMarker := contactPair(t, observedBytes)
	if finalMarker.State != ContactStateCompleted || finalRecord.NextContactAt != "" {
		t.Fatalf("post-settlement contact = %+v marker=%q", finalRecord, finalMarker.State)
	}
}

func TestContactPauseLatchAndRecoveryReset(t *testing.T) {
	claimed, commandID := claimedContactCheckpoint(t)
	base := contactTestBase()
	const preemptionReason = "human interaction with live agent session"

	sealed := applyContactTransition(t, claimed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return ScheduleExclusiveContact(t.Context(), input, testContactSchedule(commandID), base)
	})
	record, _ := contactPair(t, sealed)

	sealed = applyContactTransition(t, sealed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return LatchExclusiveContactHumanInteraction(t.Context(), input, record.ID, base.Add(time.Minute))
	})
	sealed = applyContactTransition(t, sealed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return PauseExclusiveContact(t.Context(), input, record.ID, preemptionReason)
	})
	got, marker := contactPair(t, sealed)
	if marker.State != ContactStatePaused || got.PauseReason != preemptionReason || got.HumanInteractedAt == "" {
		t.Fatalf("paused contact = %+v marker=%q", got, marker.State)
	}

	// A paused contact is never marked due.
	pausedInput, err := VerifyExclusiveInput(t.Context(), sealed, contactTestTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MarkExclusiveContactDue(t.Context(), pausedInput, record.ID, base.Add(time.Hour)); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("paused due error = %v", err)
	}

	// Delivery metadata clears the latch and releases the same-reason pause.
	sealed = applyContactTransition(t, sealed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return ClearExclusiveContactHumanLatch(t.Context(), input, record.ID, preemptionReason)
	})
	got, marker = contactPair(t, sealed)
	if marker.State != ContactStateScheduled || got.PauseReason != "" || got.HumanInteractedAt != "" {
		t.Fatalf("latch-cleared contact = %+v marker=%q", got, marker.State)
	}

	// Consume the budget, then recovery resets it and restarts the cadence.
	now := base
	for range 2 {
		now = now.Add(5 * time.Minute)
		sealed = applyContactTransition(t, sealed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
			return MarkExclusiveContactDue(t.Context(), input, record.ID, now)
		})
		sealed = applyContactTransition(t, sealed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
			return NudgeExclusiveContact(t.Context(), input, record.ID, now)
		})
	}
	recoveredAt := now.Add(time.Minute)
	sealed = applyContactTransition(t, sealed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return RecoverExclusiveContactReset(t.Context(), input, record.ID, recoveredAt, recoveredAt)
	})
	got, marker = contactPair(t, sealed)
	if marker.State != ContactStateScheduled || got.Used != 0 || got.EscalatedAt != "" ||
		got.LastRecoveredAt != CanonicalTimestamp(recoveredAt) ||
		got.NextContactAt != CanonicalTimestamp(recoveredAt.Add(5*time.Minute)) {
		t.Fatalf("recovered contact = %+v marker=%q", got, marker.State)
	}
}

func TestScheduleContactRejectsDuplicatesAndForeignCommands(t *testing.T) {
	claimed, commandID := claimedContactCheckpoint(t)
	base := contactTestBase()
	sealed := applyContactTransition(t, claimed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return ScheduleExclusiveContact(t.Context(), input, testContactSchedule(commandID), base)
	})
	input, err := VerifyExclusiveInput(t.Context(), sealed, contactTestTemplate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ScheduleExclusiveContact(t.Context(), input, testContactSchedule(commandID), base); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("duplicate schedule error = %v", err)
	}
	missing := testContactSchedule("cmd_does_not_exist")
	if _, err := ScheduleExclusiveContact(t.Context(), input, missing, base); !errors.Is(err, ErrMutationInconsistent) {
		t.Fatalf("foreign command error = %v", err)
	}
	badAssignee := testContactSchedule(commandID)
	badAssignee.Assignee = "program:tool"
	if _, err := ScheduleExclusiveContact(t.Context(), input, badAssignee, base); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("bad assignee error = %v", err)
	}
	badProvenance := testContactSchedule(commandID)
	badProvenance.Provenance = ContactProvenanceLegacyProjection
	if _, err := ScheduleExclusiveContact(t.Context(), input, badProvenance, base); !errors.Is(err, ErrMutationInvalid) {
		t.Fatalf("live legacy-projection provenance error = %v", err)
	}
}

func TestContactSettlementIncoherenceFailsClosed(t *testing.T) {
	claimed, commandID := claimedContactCheckpoint(t)
	base := contactTestBase()
	sealed := applyContactTransition(t, claimed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return ScheduleExclusiveContact(t.Context(), input, testContactSchedule(commandID), base)
	})
	checkpoint, err := DecodeCheckpointV7(sealed)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	// A settled source command with a still-active marker must be refused in
	// both pre-persist validation directions.
	command := aggregate.Commands[commandID]
	command.State = CommandObserved
	aggregate.Commands[commandID] = command
	report := ValidateAggregate(aggregate.View())
	if report.Valid() || !hasDiagnostic(report, "contact_settlement_incoherent") {
		t.Fatalf("settle-without-complete accepted: %+v", report.Diagnostics)
	}
}

func TestContactMarkerWithoutRecordFailsClosed(t *testing.T) {
	claimed, commandID := claimedContactCheckpoint(t)
	base := contactTestBase()
	sealed := applyContactTransition(t, claimed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return ScheduleExclusiveContact(t.Context(), input, testContactSchedule(commandID), base)
	})
	checkpoint, err := DecodeCheckpointV7(sealed)
	if err != nil {
		t.Fatal(err)
	}
	aggregate, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	var contactID string
	for id := range aggregate.Contacts {
		contactID = id
	}
	delete(aggregate.Contacts, contactID)
	report := ValidateAggregate(aggregate.View())
	if report.Valid() || !hasDiagnostic(report, "contact_record_missing") {
		t.Fatalf("marker-without-record accepted: %+v", report.Diagnostics)
	}
}

func TestContactlessCheckpointBytesAreStable(t *testing.T) {
	claimed, _ := claimedContactCheckpoint(t)
	if bytes.Contains(claimed, []byte(`"contacts"`)) {
		t.Fatal("contact-less checkpoint serialized a contacts field")
	}
	checkpoint, err := DecodeCheckpointV7(claimed)
	if err != nil {
		t.Fatal(err)
	}
	resealed, err := EncodeCheckpointV7(checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(resealed, claimed) {
		t.Fatal("decode → reseal changed contact-less checkpoint bytes")
	}
}

func TestContactTamperFailsClosed(t *testing.T) {
	claimed, commandID := claimedContactCheckpoint(t)
	sealed := applyContactTransition(t, claimed, func(input *VerifiedExclusiveInput) (*ExecutionTransition, error) {
		return ScheduleExclusiveContact(t.Context(), input, testContactSchedule(commandID), contactTestBase())
	})
	tampered := bytes.Replace(sealed, []byte(`"budget":2`), []byte(`"budget":3`), 1)
	if bytes.Equal(tampered, sealed) {
		t.Fatal("tamper target not found")
	}
	if _, err := DecodeCheckpointV7(tampered); err == nil {
		t.Fatal("tampered contact budget decoded successfully")
	}
}

func TestContactRecordValidationNegatives(t *testing.T) {
	base := contactTestBase()
	valid := func(t *testing.T) ContactRecordV7 {
		t.Helper()
		record := ContactRecordV7{
			RunID: "run_x", ActivationID: "act_x", Attempt: 1, SourceCommandID: "cmd",
			Assignee: "agent:agt_worker", Kind: ContactKindAgent, Provenance: ContactProvenanceDispatch,
			Cadence: "5m", Budget: 3, EscalationTarget: "human:operator",
			ScheduledAt: CanonicalTimestamp(base), NextContactAt: CanonicalTimestamp(base.Add(5 * time.Minute)),
			EventSeq: 4,
		}
		id, err := ContactIdentity(record.RunID, record.ActivationID, record.Attempt, record.Assignee)
		if err != nil {
			t.Fatal(err)
		}
		record.ID = id
		return record
	}
	if err := ValidateContactRecord(valid(t)); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}
	cases := []struct {
		name    string
		mutate  func(*ContactRecordV7)
		message string
	}{
		{"identity mismatch", func(r *ContactRecordV7) { r.Attempt = 2 }, "identity mismatch"},
		{"kind incoherent", func(r *ContactRecordV7) { r.Kind = ContactKindHuman }, "cohere"},
		{"bad cadence", func(r *ContactRecordV7) { r.Cadence = "-5m" }, "cadence"},
		{"zero budget", func(r *ContactRecordV7) { r.Budget = 0 }, "budget"},
		{"used over budget", func(r *ContactRecordV7) { r.Used = 4 }, "exceeds budget"},
		{"empty escalation", func(r *ContactRecordV7) { r.EscalationTarget = "" }, "escalation target"},
		{"bad provenance", func(r *ContactRecordV7) { r.Provenance = "guesswork" }, "provenance"},
		{"noncanonical timestamp", func(r *ContactRecordV7) { r.NextContactAt = "2026-07-19 12:00:00" }, "timestamp"},
		{"escalated with budget left", func(r *ContactRecordV7) {
			r.EscalatedAt = CanonicalTimestamp(base)
			r.NextContactAt = ""
		}, "exhausted"},
		{"escalated with next contact", func(r *ContactRecordV7) {
			r.Used = 3
			r.EscalatedAt = CanonicalTimestamp(base)
		}, "next contact"},
		{"zero event seq", func(r *ContactRecordV7) { r.EventSeq = 0 }, "event sequence"},
		{"missing scheduled at", func(r *ContactRecordV7) { r.ScheduledAt = "" }, "scheduling instant"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := valid(t)
			tc.mutate(&record)
			err := ValidateContactRecord(record)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error = %v, want containing %q", err, tc.message)
			}
		})
	}
}

func hasDiagnostic(report InvariantReport, code string) bool {
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
