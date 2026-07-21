package pathv1

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"
)

// AuditedSettlementInput is explicit non-engine authority to rescue one exact
// failed performer generation. Retry revives the inner path; the outer epoch
// projection assigns that revived work a fresh authority identity.
type AuditedSettlementInput struct {
	NodeID         string
	BlockedAttempt uint64
	Decision       string
	Actor          string
	Reason         string
	EvidenceRef    string
	Timestamp      time.Time
}

// LatestFailedExclusiveAttempt binds a node-only operator request to the sole
// currently failed activation generation. Multiple failed activations are
// ambiguous and fail closed.
func LatestFailedExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput, nodeID string) (uint64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if input == nil || input.checkpoint == nil || strings.TrimSpace(nodeID) == "" {
		return 0, fmt.Errorf("%w: sealed input and node are required", ErrMutationInvalid)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return 0, err
	}
	var matched uint64
	var matchedActivation ActivationID
	for _, command := range aggregate.Commands {
		if command.Identity.Kind != CommandPerformAttempt || (command.State != CommandObserved && command.State != CommandReconciled) {
			continue
		}
		activation, ok := aggregate.Routing.Activations[command.Identity.SourceActivationID]
		if !ok || aggregate.Routing.Reservations[activation.ReservationID].NodeID != strings.TrimSpace(nodeID) || aggregate.Routing.Paths[activation.OutputPathID].State != PathFailed {
			continue
		}
		effectID, effectErr := AttemptIdentity(aggregate.RunID, activation.ID, command.Identity.Attempt)
		if effectErr != nil || aggregate.SideEffects[effectID].State != "failed" {
			continue
		}
		if matchedActivation != "" && matchedActivation != activation.ID {
			return 0, fmt.Errorf("%w: node has multiple failed activation generations", ErrMutationInvalid)
		}
		if command.Identity.Attempt > matched {
			matched, matchedActivation = command.Identity.Attempt, activation.ID
		}
	}
	if matched == 0 {
		return 0, fmt.Errorf("%w: node has no failed performer generation", ErrMutationInvalid)
	}
	return matched, nil
}

// SettleExclusiveAttempt records a generation-bound retry/skip/cancel choice.
// It is the only live path-v1 constructor for block-resolution provenance.
func SettleExclusiveAttempt(ctx context.Context, input *VerifiedExclusiveInput, settlement AuditedSettlementInput) (*ExecutionTransition, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if input == nil || input.checkpoint == nil || settlement.Timestamp.IsZero() {
		return nil, fmt.Errorf("%w: sealed input and settlement timestamp are required", ErrMutationInvalid)
	}
	resolution := BlockResolution{
		NodeID: strings.TrimSpace(settlement.NodeID), BlockedAttempt: settlement.BlockedAttempt,
		Decision: strings.TrimSpace(settlement.Decision), Actor: strings.TrimSpace(settlement.Actor),
		Reason: strings.TrimSpace(settlement.Reason), EvidenceRef: strings.TrimSpace(settlement.EvidenceRef),
		Timestamp: CanonicalTimestamp(settlement.Timestamp),
	}
	digest, err := ValidateBlockResolution(resolution)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	aggregate, err := CurrentAggregateCheckpoint(input.checkpoint)
	if err != nil {
		return nil, err
	}
	var activation ActivationRecord
	var source PathRecord
	var perform CommandRecord
	for _, candidate := range aggregate.Commands {
		if candidate.Identity.Kind != CommandPerformAttempt || candidate.Identity.Attempt != resolution.BlockedAttempt ||
			(candidate.State != CommandObserved && candidate.State != CommandReconciled) {
			continue
		}
		candidateActivation, ok := aggregate.Routing.Activations[candidate.Identity.SourceActivationID]
		if !ok {
			continue
		}
		reservation, ok := aggregate.Routing.Reservations[candidateActivation.ReservationID]
		if !ok || reservation.NodeID != resolution.NodeID {
			continue
		}
		candidateSource, ok := aggregate.Routing.Paths[candidateActivation.OutputPathID]
		if !ok || candidateSource.Kind != PathActivationOutput || candidateSource.State != PathFailed {
			continue
		}
		if perform.ID != "" {
			return nil, fmt.Errorf("%w: exact failed performer generation is ambiguous", ErrMutationInvalid)
		}
		activation, source, perform = candidateActivation, candidateSource, candidate
	}
	if perform.ID == "" {
		return nil, fmt.Errorf("%w: exact failed performer generation is absent or ambiguous", ErrMutationInvalid)
	}
	for id, existing := range aggregate.AdminResolutions {
		if existing.NodeID == resolution.NodeID && existing.BlockedAttempt == resolution.BlockedAttempt {
			existingDigest, _ := ValidateBlockResolution(existing)
			if existingDigest == digest {
				return nil, fmt.Errorf("%w: settlement %q is already recorded", ErrMutationInvalid, id)
			}
			return nil, fmt.Errorf("%w: performer generation already has a different settlement", ErrMutationInvalid)
		}
	}
	if CurrentLastLogSeq(input.checkpoint) >= math.MaxInt64 {
		return nil, &OverBudgetError{Limit: "log_entries", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
	}
	eventSeq := int64(CurrentLastLogSeq(input.checkpoint) + 1)
	record := PathV1AdminRecord{
		RunID: aggregate.RunID, EventSeq: eventSeq, AdminType: blockResolutionAdminType,
		Actor: resolution.Actor, ReasonCode: "resolved_" + resolution.Decision,
		EvidenceRef: resolution.EvidenceRef, Timestamp: resolution.Timestamp, ResolutionDigest: digest,
	}
	record.ID, err = AdminRecordIdentity(record)
	if err != nil {
		return nil, err
	}
	aggregate.AdminRecords[record.ID] = record
	aggregate.AdminResolutions[record.ID] = resolution
	blockID, err := BlockIdentity(aggregate.RunID, activation.ID, resolution.BlockedAttempt)
	if err != nil {
		return nil, err
	}
	aggregate.SideEffects[blockID] = SideEffectIdentity{
		Kind: SideEffectBlock, ID: blockID, RunID: aggregate.RunID, ActivationID: activation.ID,
		BlockedAttempt: resolution.BlockedAttempt, State: "resolved_" + resolution.Decision,
	}
	if resolution.Decision == "retry" {
		terminalCauseID := source.TerminalCauseID
		source.State = PathLive
		source.UpdatedSeq = eventSeq
		source.Disposition = nil
		source.TerminalCauseID = ""
		aggregate.Routing.Paths[source.ID] = source
		delete(aggregate.Routing.CauseRecords, terminalCauseID)
		for causeSetID, causeSet := range aggregate.Routing.CauseSets {
			if len(causeSet.CauseIDs) == 1 && causeSet.CauseIDs[0] == terminalCauseID {
				delete(aggregate.Routing.CauseSets, causeSetID)
			}
		}
		// A terminal run owns one completion self-command bound to the old log
		// anchors. Retrying invalidates that basis; the outer runtime receipt
		// retains its terminal authority while the inner aggregate must plan a
		// fresh completion after the rescued attempt settles.
		for commandID, command := range aggregate.Commands {
			if command.Identity.Kind == CommandCompleteRun {
				delete(aggregate.Commands, commandID)
			}
		}
	}
	status := CurrentRunStatus(input.checkpoint)
	if resolution.Decision == "retry" {
		status = "running"
	}
	next, err := advanceCheckpointV7(input.checkpoint, aggregate, status)
	if err != nil {
		return nil, err
	}
	witnessTimestamp, err := time.Parse(time.RFC3339Nano, resolution.Timestamp)
	if err != nil {
		return nil, fmt.Errorf("%w: canonical settlement timestamp is invalid", ErrMutationInvalid)
	}
	transition, err := newWitnessedExecutionTransition(input.checkpoint, next, TransitionAuditedSettlement, ExecutionWitnessV1{Settlement: &AuditedSettlementInput{
		NodeID: resolution.NodeID, BlockedAttempt: resolution.BlockedAttempt, Decision: resolution.Decision,
		Actor: resolution.Actor, Reason: resolution.Reason, EvidenceRef: resolution.EvidenceRef, Timestamp: witnessTimestamp.UTC(),
	}})
	if err != nil {
		return nil, err
	}
	transition.resolution = &resolution
	return transition, nil
}
