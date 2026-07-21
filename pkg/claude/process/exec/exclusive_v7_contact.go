package processexec

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// scheduleExclusiveContact makes the just-recovered dispatch result durable as
// the attempt's contact schedule. Defaults apply when the performer declares
// no explicit contact, restoring exact legacy parity for nil-contact
// performers. An already-present schedule makes the call a no-op so dispatch
// recovery stays idempotent.
func (e *ExclusiveV7Executor) scheduleExclusiveContact(ctx context.Context, runID string, plan *pathv1.ExclusiveAttemptPlan, dispatched DispatchResult, provenance pathv1.ContactProvenance) error {
	performer := plan.Performer()
	if performer == nil {
		return nil
	}
	cadence, budget, escalation, err := ContactScheduleFor(*performer)
	if err != nil {
		return fmt.Errorf("schedule path-v1 contact for command %q: %w", plan.Command().ID, err)
	}
	assignee := strings.TrimSpace(dispatched.Assignee)
	if assignee == "" || budget <= 0 {
		// No durable identity yet (A3): never synthesize an assignee. The
		// in-flight reconcile path retries as late initialization next tick.
		return nil
	}
	schedule := pathv1.ContactScheduleV7{
		SourceCommandID: plan.Command().ID, Assignee: assignee, Cadence: cadence.String(),
		Budget: uint64(budget), EscalationTarget: escalation, Provenance: provenance,
	}
	err = e.applyContactTransition(ctx, runID, func(input *pathv1.VerifiedExclusiveInput) (*pathv1.ExecutionTransition, error) {
		return pathv1.ScheduleExclusiveContact(ctx, input, schedule, e.now())
	})
	if errors.Is(err, pathv1.ErrMutationInconsistent) {
		// The schedule already exists (replayed dispatch recovery) or the
		// command settled concurrently; both are terminal for this call.
		return nil
	}
	return err
}

// serviceExclusiveContact is the schema-7 counterpart of the legacy host's
// serviceContact: shared DecideContact policy, sealed pathv1 transitions, and
// the same send-then-append duplicate window now bounded to the durable due
// state. Adapters without contact support no-op.
func (e *ExclusiveV7Executor) serviceExclusiveContact(ctx context.Context, runID string, plan *pathv1.ExclusiveAttemptPlan, request Request, adapter Adapter) error {
	contactAdapter, ok := adapter.(ContactAdapter)
	if !ok {
		return nil
	}
	contact, err := e.contactForCommand(ctx, runID, plan.Command().ID)
	if err != nil {
		return err
	}
	if contact != nil && !activeContactState(contact.State()) {
		return nil
	}
	if contact == nil {
		deferred, ok := adapter.(DeferredAdapter)
		if !ok {
			return nil
		}
		// Late initialization: recover the exact dispatch result through the
		// adapter's idempotent-by-command-ID Dispatch contract. Errors skip
		// this tick and retry; identity is never guessed (A3).
		dispatched, dispatchErr := deferred.Dispatch(ctx, request)
		if dispatchErr != nil {
			return nil
		}
		return e.scheduleExclusiveContact(ctx, runID, plan, dispatched, pathv1.ContactProvenanceLateInitialization)
	}

	record := contact.Record()
	snapshot, err := v7ContactSnapshot(record, contact.State())
	if err != nil {
		return err
	}
	activity, err := contactAdapter.Activity(ctx, request, snapshot.ActivitySince())
	if err != nil {
		return err
	}
	now := e.now()
	decision := DecideContact(snapshot, activity, now)
	if decision.Reset {
		if err := e.applyContactTransition(ctx, runID, func(input *pathv1.VerifiedExclusiveInput) (*pathv1.ExecutionTransition, error) {
			return pathv1.RecoverExclusiveContactReset(ctx, input, record.ID, decision.ResetAt, now)
		}); err != nil {
			return err
		}
	}
	if decision.ClearLatch {
		if err := e.applyContactTransition(ctx, runID, func(input *pathv1.VerifiedExclusiveInput) (*pathv1.ExecutionTransition, error) {
			return pathv1.ClearExclusiveContactHumanLatch(ctx, input, record.ID, ContactPauseReasonHumanPreemption)
		}); err != nil {
			return err
		}
	}
	if !decision.LatchAt.IsZero() {
		if err := e.applyContactTransition(ctx, runID, func(input *pathv1.VerifiedExclusiveInput) (*pathv1.ExecutionTransition, error) {
			return pathv1.LatchExclusiveContactHumanInteraction(ctx, input, record.ID, decision.LatchAt)
		}); err != nil {
			return err
		}
	}
	if decision.Pause {
		if err := e.applyContactTransition(ctx, runID, func(input *pathv1.VerifiedExclusiveInput) (*pathv1.ExecutionTransition, error) {
			return pathv1.PauseExclusiveContact(ctx, input, record.ID, ContactPauseReasonHumanPreemption)
		}); err != nil {
			return err
		}
	}

	// The flag transitions above may have changed lifecycle state; re-derive
	// before any external send so no diagnosed or paused contact is contacted.
	contact, err = e.contactForCommand(ctx, runID, plan.Command().ID)
	if err != nil || contact == nil {
		return err
	}
	record = contact.Record()
	switch contact.State() {
	case pathv1.ContactStateDue:
		// Either freshly marked below on a previous crashed tick or held over
		// from a send whose seal was lost: resend once, then seal.
	case pathv1.ContactStateScheduled:
		if decision.Send == ContactSendNone {
			return nil
		}
		if err := e.applyContactTransition(ctx, runID, func(input *pathv1.VerifiedExclusiveInput) (*pathv1.ExecutionTransition, error) {
			return pathv1.MarkExclusiveContactDue(ctx, input, record.ID, now)
		}); err != nil {
			return err
		}
	default:
		return nil
	}
	escalate := record.Used >= record.Budget
	if escalate && record.EscalatedAt != "" {
		return fmt.Errorf("path-v1 contact %q is due after escalation", record.ID)
	}
	if err := contactAdapter.Contact(ctx, request, escalate); err != nil {
		return err
	}
	return e.applyContactTransition(ctx, runID, func(input *pathv1.VerifiedExclusiveInput) (*pathv1.ExecutionTransition, error) {
		if escalate {
			return pathv1.EscalateExclusiveContact(ctx, input, record.ID, e.now())
		}
		return pathv1.NudgeExclusiveContact(ctx, input, record.ID, e.now())
	})
}

// applyContactTransition seals one contact transition with the standard
// coherent-view/CAS-append retry discipline.
func (e *ExclusiveV7Executor) applyContactTransition(ctx context.Context, runID string, build func(input *pathv1.VerifiedExclusiveInput) (*pathv1.ExecutionTransition, error)) error {
	for attempt := 0; attempt < maxObservationCASAttempts; attempt++ {
		var transition *pathv1.ExecutionTransition
		err := e.withExecutionView(ctx, runID, func(view store.PathV1ExecutionView) error {
			var buildErr error
			transition, buildErr = build(view.Input)
			return buildErr
		})
		if err != nil {
			return err
		}
		_, err = e.appendTransition(ctx, runID, transition)
		if store.IsConflict(err) {
			continue
		}
		return err
	}
	return fmt.Errorf("path-v1 contact transition remained contended")
}

func (e *ExclusiveV7Executor) contactForCommand(ctx context.Context, runID, sourceCommandID string) (*pathv1.ExclusiveContactPlan, error) {
	var found *pathv1.ExclusiveContactPlan
	err := e.withExecutionView(ctx, runID, func(view store.PathV1ExecutionView) error {
		contacts, err := pathv1.RecoverExclusiveContacts(ctx, view.Input)
		if err != nil {
			return err
		}
		for _, contact := range contacts {
			if contact.Record().SourceCommandID == sourceCommandID {
				found = contact
				return nil
			}
		}
		return nil
	})
	return found, err
}

func activeContactState(state string) bool {
	switch state {
	case pathv1.ContactStateScheduled, pathv1.ContactStateDue, pathv1.ContactStatePaused:
		return true
	}
	return false
}

// v7ContactSnapshot projects the durable schema-7 pair into the shared
// decision-core shape. Validation already guaranteed canonical values; any
// surviving corruption fails here, before any external send.
func v7ContactSnapshot(record pathv1.ContactRecordV7, markerState string) (ContactSnapshot, error) {
	cadence, err := pathv1.ParseContactCadence(record.Cadence)
	if err != nil {
		return ContactSnapshot{}, err
	}
	parse := func(value string) (time.Time, error) {
		return pathv1.ParseCanonicalTimestamp(value)
	}
	nextContactAt, err := parse(record.NextContactAt)
	if err != nil {
		return ContactSnapshot{}, err
	}
	lastContactedAt, err := parse(record.LastContactedAt)
	if err != nil {
		return ContactSnapshot{}, err
	}
	lastRecoveredAt, err := parse(record.LastRecoveredAt)
	if err != nil {
		return ContactSnapshot{}, err
	}
	escalatedAt, err := parse(record.EscalatedAt)
	if err != nil {
		return ContactSnapshot{}, err
	}
	humanInteractedAt, err := parse(record.HumanInteractedAt)
	if err != nil {
		return ContactSnapshot{}, err
	}
	if record.Used > uint64(int(^uint(0)>>1)) || record.Budget > uint64(int(^uint(0)>>1)) {
		return ContactSnapshot{}, fmt.Errorf("path-v1 contact %q has out-of-range budget", record.ID)
	}
	return ContactSnapshot{
		Cadence: cadence, Budget: int(record.Budget), Used: int(record.Used),
		Paused: markerState == pathv1.ContactStatePaused, PauseReason: record.PauseReason,
		NextContactAt: nextContactAt, LastContactedAt: lastContactedAt,
		LastRecoveredAt: lastRecoveredAt, EscalatedAt: escalatedAt,
		HumanInteractedAt: humanInteractedAt,
	}, nil
}
