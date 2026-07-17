package pathv1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

func PlanCompleteRun(view CompletionReplayView) (CommandRecord, error) {
	if err := validateCompletionView(view); err != nil {
		return CommandRecord{}, err
	}
	if !completionRunStatusAllowsExecution(view.RunStatus) {
		return CommandRecord{}, fmt.Errorf("%w: cannot plan completion while run status is %q", ErrMutationInconsistent, view.RunStatus)
	}
	for _, command := range view.Aggregate.Commands {
		if command.Identity.Kind == CommandCompleteRun {
			return CommandRecord{}, fmt.Errorf("%w: completion command already exists", ErrMutationInconsistent)
		}
	}
	basis, err := computeCompletionBasis(view, CompletionBasis{
		BasisRunStatus:   view.RunStatus,
		BasisLastLogSeq:  view.LastLogSeq,
		BasisLogChecksum: view.LogChecksum,
	}, "")
	if err != nil {
		return CommandRecord{}, err
	}
	if view.Checkpoint != completionBasisCheckpoint(basis) {
		return CommandRecord{}, fmt.Errorf("%w: completion checkpoint is not the deterministic basis projection", ErrMutationInvalid)
	}
	identity := CommandIdentity{
		RunID:         view.Aggregate.RunID,
		Kind:          CommandCompleteRun,
		PayloadSchema: 1,
		InputDigest:   basis.AggregateDigest,
		PlanDigest:    basis.ActiveCommandDigest,
		ResultCode:    basis.Result,
	}
	commandID, err := CommandIdentityDigest(identity)
	if err != nil {
		return CommandRecord{}, err
	}
	basis.SelfCommandID = commandID
	payload, err := json.Marshal(CompleteRunCommandPayload{
		TemplateRef:        view.Aggregate.TemplateRef,
		TemplateSourceHash: view.Aggregate.TemplateSourceHash,
		Checkpoint:         completionBasisCheckpoint(basis),
		Basis:              basis,
	})
	if err != nil {
		return CommandRecord{}, err
	}
	if len(payload) > MaxCommandPayloadBytes {
		return CommandRecord{}, &OverBudgetError{Limit: "payload_bytes", Value: len(payload), Maximum: MaxCommandPayloadBytes}
	}
	sum := sha256.Sum256(payload)
	command := CommandRecord{
		ID:             commandID,
		IdempotencyKey: CommandIdempotencyKey(CommandCompleteRun, commandID),
		Identity:       identity,
		Payload:        payload,
		PayloadHash:    hex.EncodeToString(sum[:]),
		State:          CommandIssued,
	}
	if err := validateCompleteRunCommandPrimitive(command); err != nil {
		return CommandRecord{}, err
	}
	return command, nil
}

// RecoverCompleteRun validates every durable completion state and returns the
// sole safe next phase. It never mutates a checkpoint or active state.
func RecoverCompleteRun(view CompletionReplayView) (CompletionRecovery, error) {
	if err := validateCompletionView(view); err != nil {
		return CompletionRecovery{}, err
	}
	completeCommands := make([]CommandRecord, 0, 1)
	for _, command := range view.Aggregate.Commands {
		if command.Identity.Kind == CommandCompleteRun {
			completeCommands = append(completeCommands, command)
		}
	}
	if len(completeCommands) == 0 {
		if terminalRunStatus(view.RunStatus) {
			return CompletionRecovery{}, fmt.Errorf("%w: terminal run lacks its completion command", ErrMutationInconsistent)
		}
		command, err := PlanCompleteRun(view)
		if err != nil {
			return CompletionRecovery{}, err
		}
		return CompletionRecovery{Phase: CompletionReadyToClaim, Command: command, Result: command.Identity.ResultCode}, nil
	}
	if len(completeCommands) != 1 {
		return CompletionRecovery{}, fmt.Errorf("%w: found %d completion commands, want one", ErrMutationInconsistent, len(completeCommands))
	}
	command := completeCommands[0]
	payload, err := ValidateCompleteRunCommand(view, command)
	if err != nil {
		return CompletionRecovery{}, err
	}
	switch command.State {
	case CommandIssued, CommandReconciling:
		if terminalRunStatus(view.RunStatus) {
			return CompletionRecovery{}, fmt.Errorf("%w: terminal run retains active completion command", ErrMutationInconsistent)
		}
		return CompletionRecovery{Phase: CompletionReadyToObserve, Command: command, Result: payload.Basis.Result}, nil
	case CommandObserved:
		if view.RunStatus != payload.Basis.Result {
			return CompletionRecovery{}, fmt.Errorf("%w: observed completion result %q differs from run status %q", ErrMutationInconsistent, payload.Basis.Result, view.RunStatus)
		}
		return CompletionRecovery{Phase: CompletionRecovered, Command: command, Result: payload.Basis.Result}, nil
	default:
		return CompletionRecovery{}, fmt.Errorf("%w: completion command has terminally unusable state %q", ErrMutationInconsistent, command.State)
	}
}

func ValidateCompleteRunCommand(view CompletionReplayView, command CommandRecord) (CompleteRunCommandPayload, error) {
	var payload CompleteRunCommandPayload
	if err := validateCompleteRunCommandPrimitive(command); err != nil {
		return payload, err
	}
	if command.Identity.RunID != view.Aggregate.RunID {
		return payload, fmt.Errorf("%w: completion command run differs from aggregate", ErrMutationInvalid)
	}
	stored, ok := view.Aggregate.Commands[command.ID]
	if !ok || !canonicalEqual(stored, command) {
		return payload, fmt.Errorf("%w: completion command is not byte-exact in aggregate", ErrMutationInvalid)
	}
	if err := decodeExactPayload(command.Payload, &payload); err != nil {
		return payload, fmt.Errorf("%w: complete_run typed payload: %v", ErrMutationInvalid, err)
	}
	if payload.TemplateRef == "" || payload.TemplateSourceHash == "" || payload.TemplateRef != view.Aggregate.TemplateRef || payload.TemplateSourceHash != view.Aggregate.TemplateSourceHash {
		return payload, fmt.Errorf("%w: completion template binding mismatch", ErrMutationInvalid)
	}
	basis := payload.Basis
	if basis.SelfCommandID != command.ID || basis.BasisRunStatus == "" || basis.BasisLogChecksum == "" {
		return payload, fmt.Errorf("%w: completion basis lacks exact self/anchor binding", ErrMutationInvalid)
	}
	if !completionRunStatusAllowsExecution(basis.BasisRunStatus) {
		return payload, fmt.Errorf("%w: completion basis status %q is not executable", ErrMutationInconsistent, basis.BasisRunStatus)
	}
	if basis.BasisLastLogSeq != view.LastLogSeq || basis.BasisLogChecksum != view.LogChecksum {
		return payload, fmt.Errorf("%w: completion basis log anchors differ from current durable run", ErrMutationInconsistent)
	}
	if command.State.Active() && basis.BasisRunStatus != view.RunStatus {
		return payload, fmt.Errorf("%w: active completion basis status %q differs from current run status %q", ErrMutationInconsistent, basis.BasisRunStatus, view.RunStatus)
	}
	if command.State == CommandObserved && (!terminalRunStatus(view.RunStatus) || view.RunStatus != basis.Result) {
		return payload, fmt.Errorf("%w: observed completion result %q differs from current terminal run status %q", ErrMutationInconsistent, basis.Result, view.RunStatus)
	}
	recomputed, err := computeCompletionBasis(view, basis, command.ID)
	if err != nil {
		return payload, err
	}
	if recomputed != basis {
		return payload, fmt.Errorf("%w: completion basis digest/result drift", ErrMutationInconsistent)
	}
	if payload.Checkpoint != completionBasisCheckpoint(recomputed) || view.Checkpoint != payload.Checkpoint {
		return payload, fmt.Errorf("%w: completion checkpoint is not the deterministic basis projection", ErrMutationInvalid)
	}
	if command.Identity.InputDigest != basis.AggregateDigest || command.Identity.PlanDigest != basis.ActiveCommandDigest || command.Identity.ResultCode != basis.Result {
		return payload, fmt.Errorf("%w: completion command identity differs from basis", ErrMutationInvalid)
	}
	empty, _ := ActiveCommandIdentity(nil)
	if basis.ActiveCommandDigest != empty {
		return payload, fmt.Errorf("%w: completion basis contains another active command", ErrMutationInconsistent)
	}
	active := make([]string, 0)
	for id, candidate := range view.Aggregate.Commands {
		if candidate.State.Active() {
			active = append(active, id)
		}
	}
	slices.Sort(active)
	switch command.State {
	case CommandIssued, CommandReconciling:
		if len(active) != 1 || active[0] != command.ID {
			return payload, fmt.Errorf("%w: completion self is not the sole active command", ErrMutationInconsistent)
		}
	case CommandObserved:
		if len(active) != 0 {
			return payload, fmt.Errorf("%w: observed completion retains active commands", ErrMutationInconsistent)
		}
	default:
		return payload, fmt.Errorf("%w: invalid completion command state %q", ErrMutationInconsistent, command.State)
	}
	return payload, nil
}

func completionBasisCheckpoint(basis CompletionBasis) CheckpointBinding {
	return CheckpointBinding{Generation: basis.BasisLastLogSeq, Digest: basis.CheckpointDigest}
}

func validateCompleteRunCommandPrimitive(command CommandRecord) error {
	if command.Identity.Kind != CommandCompleteRun {
		return fmt.Errorf("%w: command is not complete_run_v1", ErrMutationInvalid)
	}
	if len(command.Payload) > MaxCommandPayloadBytes {
		return &OverBudgetError{Limit: "payload_bytes", Value: len(command.Payload), Maximum: MaxCommandPayloadBytes}
	}
	id, err := CommandIdentityDigest(command.Identity)
	if err != nil {
		return err
	}
	if command.ID != id || command.IdempotencyKey != CommandIdempotencyKey(CommandCompleteRun, id) {
		return fmt.Errorf("%w: completion command identity/idempotency mismatch", ErrMutationInvalid)
	}
	if err := ValidateCommandIdentity(command.Identity); err != nil {
		return fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	if !command.State.Valid() {
		return fmt.Errorf("%w: invalid completion command state", ErrMutationInvalid)
	}
	sum := sha256.Sum256(command.Payload)
	if command.PayloadHash != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%w: completion payload hash mismatch", ErrMutationInvalid)
	}
	return nil
}

func validateCompletionView(view CompletionReplayView) error {
	if view.Aggregate.Routing == nil || view.Aggregate.Commands == nil || view.Aggregate.SideEffects == nil {
		return fmt.Errorf("%w: incomplete completion aggregate", ErrMutationInvalid)
	}
	if view.Aggregate.RunID == "" || view.Aggregate.TemplateRef == "" || view.Aggregate.TemplateSourceHash == "" || view.RunStatus == "" || view.LogChecksum == "" {
		return fmt.Errorf("%w: incomplete completion run/template/anchor binding", ErrMutationInvalid)
	}
	if err := view.Checkpoint.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrMutationInvalid, err)
	}
	if len(view.CheckpointJSON) == 0 || len(view.CheckpointJSON) > MaxCheckpointBytes {
		return fmt.Errorf("%w: invalid completion checkpoint size", ErrMutationInvalid)
	}
	if _, err := parseJCS(view.CheckpointJSON); err != nil {
		return fmt.Errorf("%w: invalid completion checkpoint JSON: %v", ErrMutationInvalid, err)
	}
	var anchors struct {
		Status      string `json:"status"`
		LastLogSeq  uint64 `json:"lastLogSeq"`
		LogChecksum string `json:"logChecksum"`
	}
	if err := json.Unmarshal(view.CheckpointJSON, &anchors); err != nil {
		return fmt.Errorf("%w: invalid completion checkpoint anchors: %v", ErrMutationInvalid, err)
	}
	if anchors.Status != view.RunStatus || anchors.LastLogSeq != view.LastLogSeq || anchors.LogChecksum != view.LogChecksum {
		return fmt.Errorf("%w: checkpoint anchors differ from current durable run", ErrMutationInconsistent)
	}
	if err := reconcileCheckpointCommands(view); err != nil {
		return err
	}
	return nil
}

func reconcileCheckpointCommands(view CompletionReplayView) error {
	var envelope struct {
		OutstandingCommands map[string]json.RawMessage `json:"outstandingCommands"`
	}
	if err := json.Unmarshal(view.CheckpointJSON, &envelope); err != nil || envelope.OutstandingCommands == nil {
		return fmt.Errorf("%w: checkpoint outstanding commands are missing or invalid", ErrMutationInconsistent)
	}
	checkpointCommands := make(map[string]CommandRecord, len(envelope.OutstandingCommands))
	for id, raw := range envelope.OutstandingCommands {
		var command CommandRecord
		if err := decodeExactPayload(raw, &command); err != nil || command.ID != id {
			return fmt.Errorf("%w: checkpoint command %q is not an exact command record", ErrMutationInconsistent, id)
		}
		if command.Identity.RunID != view.Aggregate.RunID {
			return fmt.Errorf("%w: checkpoint command %q belongs to run %q, want %q", ErrMutationInconsistent, id, command.Identity.RunID, view.Aggregate.RunID)
		}
		var validationErr error
		if command.Identity.Kind == CommandCompleteRun {
			validationErr = validateCompleteRunCommandPrimitive(command)
		} else {
			validationErr = ValidateCommand(command)
		}
		if validationErr != nil {
			return fmt.Errorf("%w: checkpoint command %q is invalid: %v", ErrMutationInconsistent, id, validationErr)
		}
		checkpointCommands[id] = command
		stored, ok := view.Aggregate.Commands[id]
		if !ok || !canonicalEqual(stored, command) {
			return fmt.Errorf("%w: checkpoint-retained command %q is absent or different in aggregate", ErrMutationInconsistent, id)
		}
	}
	for id, command := range view.Aggregate.Commands {
		if !command.State.Active() {
			continue
		}
		checkpoint, ok := checkpointCommands[id]
		if !ok || !canonicalEqual(checkpoint, command) {
			return fmt.Errorf("%w: active aggregate command %q is absent or different in checkpoint", ErrMutationInconsistent, id)
		}
	}
	return nil
}

func computeCompletionBasis(view CompletionReplayView, anchors CompletionBasis, selfCommandID string) (CompletionBasis, error) {
	filtered := view.Aggregate
	filtered.Commands = cloneMap(view.Aggregate.Commands)
	if selfCommandID != "" {
		command, ok := filtered.Commands[selfCommandID]
		if !ok || command.Identity.Kind != CommandCompleteRun {
			return CompletionBasis{}, fmt.Errorf("%w: completion self command missing", ErrMutationInconsistent)
		}
		delete(filtered.Commands, selfCommandID)
	}
	completion, err := AssessAggregateCompletion(filtered)
	if err != nil {
		return CompletionBasis{}, err
	}
	projection, err := CanonicalCheckpointProjection(view.CheckpointJSON, selfCommandID)
	if err != nil {
		return CompletionBasis{}, err
	}
	checkpointDigest, err := CheckpointIdentity(anchors.BasisRunStatus, anchors.BasisLastLogSeq, anchors.BasisLogChecksum, projection)
	if err != nil {
		return CompletionBasis{}, err
	}
	paths := make([]PathFoldEntry, 0, len(filtered.Routing.Paths))
	terminalCauses := make([]CauseID, 0)
	for _, path := range filtered.Routing.Paths {
		seq, err := eventUint(path.UpdatedSeq)
		if err != nil {
			return CompletionBasis{}, err
		}
		paths = append(paths, PathFoldEntry{PathID: path.ID, State: path.State, UpdatedSeq: seq})
		if path.State.TerminalNonSuccess() {
			terminalCauses = append(terminalCauses, path.TerminalCauseID)
		}
	}
	pathDigest, err := PathFoldIdentity(paths)
	if err != nil {
		return CompletionBasis{}, err
	}
	reservations := make([]ReservationFoldEntry, 0, len(filtered.Routing.Reservations))
	for _, reservation := range filtered.Routing.Reservations {
		seq, err := eventUint(reservation.EventSeq)
		if err != nil {
			return CompletionBasis{}, err
		}
		reservations = append(reservations, ReservationFoldEntry{ReservationID: reservation.ID, State: reservation.State, EventSeq: seq})
	}
	reservationDigest, err := ReservationFoldIdentity(reservations)
	if err != nil {
		return CompletionBasis{}, err
	}
	propagation := make([]PropagationFoldEntry, 0, len(filtered.Routing.Propagation))
	for _, intent := range filtered.Routing.Propagation {
		propagation = append(propagation, PropagationFoldEntry{IntentID: intent.ID, State: intent.State, Cursor: uint64(intent.Cursor)})
	}
	propagationDigest, err := PropagationFoldIdentity(propagation)
	if err != nil {
		return CompletionBasis{}, err
	}
	effects := make([]SideEffectFoldEntry, 0, len(filtered.Commands)+len(filtered.SideEffects))
	activeCommands := make([]string, 0)
	for _, command := range filtered.Commands {
		effects = append(effects, SideEffectFoldEntry{Kind: SideEffectCommand, ID: command.ID, State: string(command.State)})
		if command.State.Active() {
			activeCommands = append(activeCommands, command.ID)
		}
	}
	for _, effect := range filtered.SideEffects {
		effects = append(effects, SideEffectFoldEntry{Kind: effect.Kind, ID: effect.ID, State: effect.State})
	}
	sideEffectDigest, err := SideEffectFoldIdentity(effects)
	if err != nil {
		return CompletionBasis{}, err
	}
	terminalCauseDigest, err := CauseSetIdentity(terminalCauses)
	if err != nil {
		return CompletionBasis{}, err
	}
	activeCommandDigest, err := ActiveCommandIdentity(activeCommands)
	if err != nil {
		return CompletionBasis{}, err
	}
	aggregateDigest, err := AggregateIdentity(filtered.RunID, filtered.TemplateRef, checkpointDigest, pathDigest, reservationDigest, propagationDigest, sideEffectDigest, terminalCauseDigest)
	if err != nil {
		return CompletionBasis{}, err
	}
	return CompletionBasis{
		SelfCommandID:         selfCommandID,
		BasisRunStatus:        anchors.BasisRunStatus,
		BasisLastLogSeq:       anchors.BasisLastLogSeq,
		BasisLogChecksum:      anchors.BasisLogChecksum,
		CheckpointDigest:      checkpointDigest,
		PathFoldDigest:        pathDigest,
		ReservationFoldDigest: reservationDigest,
		PropagationFoldDigest: propagationDigest,
		SideEffectFoldDigest:  sideEffectDigest,
		TerminalCauseDigest:   terminalCauseDigest,
		ActiveCommandDigest:   activeCommandDigest,
		AggregateDigest:       aggregateDigest,
		Result:                completion.Result,
	}, nil
}

func terminalRunStatus(status string) bool {
	return status == "completed" || status == "failed" || status == "canceled"
}

func completionRunStatusAllowsExecution(status string) bool {
	switch status {
	case "pending", "running", "blocked":
		return true
	default:
		return false
	}
}
