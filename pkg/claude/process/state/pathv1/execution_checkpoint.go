package pathv1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
)

// CanonicalInitializationAnchor returns detached canonical bytes and the
// digest of the immutable initialization event. Mutable execution revisions
// must retain both exactly; nil and empty values are equivalent only when the
// canonical JSON encoding makes them equivalent.
func CanonicalInitializationAnchor(checkpoint *CheckpointV7) ([]byte, string, error) {
	if checkpoint == nil {
		return nil, "", fmt.Errorf("%w: checkpoint is required", ErrInitializationInvalid)
	}
	canonical, err := canonicalJSON(checkpoint.Initialize)
	if err != nil {
		return nil, "", err
	}
	digest, err := initializeEventDigest(checkpoint.Initialize)
	if err != nil {
		return nil, "", err
	}
	return bytes.Clone(canonical), digest, nil
}

// ExecutionCheckpoint is the mutable schema-7 execution head. Revision zero
// is represented by an absent section and reads from Initialize.Aggregate.
// Every installed section is a complete replacement, never a delta whose
// meaning depends on evidence or a preceding file.
type ExecutionCheckpoint struct {
	Revision       uint64              `json:"revision"`
	PreviousDigest string              `json:"previousDigest"`
	Status         string              `json:"status"`
	LogAdvanced    bool                `json:"logAdvanced,omitempty"`
	LastLogSeq     uint64              `json:"lastLogSeq"`
	LogChecksum    string              `json:"logChecksum"`
	Aggregate      AggregateCheckpoint `json:"aggregate"`
}

// CheckpointRevision returns the ABA-safe mutable revision. Legacy installed
// schema-7 checkpoints are revision zero.
func CheckpointRevision(checkpoint *CheckpointV7) uint64 {
	if checkpoint == nil || checkpoint.Execution == nil {
		return 0
	}
	return checkpoint.Execution.Revision
}

// CurrentCheckpointBinding identifies the exact complete checkpoint used by
// planners and CAS appenders. Generation advances with every mutable revision.
func CurrentCheckpointBinding(checkpoint *CheckpointV7) CheckpointBinding {
	if checkpoint == nil {
		return CheckpointBinding{}
	}
	generation := uint64(0)
	if checkpoint.Initialize.EventSeq > 0 {
		generation = uint64(checkpoint.Initialize.EventSeq)
	}
	return CheckpointBinding{Generation: generation + CheckpointRevision(checkpoint), Digest: checkpoint.Digest}
}

func CurrentRunStatus(checkpoint *CheckpointV7) string {
	if checkpoint != nil && checkpoint.Execution != nil {
		return checkpoint.Execution.Status
	}
	return "running"
}

func CurrentLastLogSeq(checkpoint *CheckpointV7) uint64 {
	if checkpoint != nil && checkpoint.Execution != nil {
		return checkpoint.Execution.LastLogSeq
	}
	if checkpoint != nil && checkpoint.Initialize.EventSeq > 0 {
		return uint64(checkpoint.Initialize.EventSeq)
	}
	return 0
}

func CurrentLogChecksum(checkpoint *CheckpointV7) string {
	if checkpoint != nil && checkpoint.Execution != nil {
		return checkpoint.Execution.LogChecksum
	}
	if checkpoint == nil {
		return ""
	}
	return checkpoint.Digest
}

func currentCompletionCheckpointDigest(checkpoint *CheckpointV7) string {
	if checkpoint != nil && checkpoint.Execution != nil && !checkpoint.Execution.LogAdvanced {
		return checkpoint.Execution.PreviousDigest
	}
	if checkpoint == nil {
		return ""
	}
	return checkpoint.Digest
}

// CurrentAggregateCheckpoint returns a detached complete execution aggregate.
// It never aliases the decoded checkpoint retained by a coherent read.
func CurrentAggregateCheckpoint(checkpoint *CheckpointV7) (AggregateCheckpoint, error) {
	if checkpoint == nil {
		return AggregateCheckpoint{}, fmt.Errorf("%w: checkpoint is required", ErrInitializationInvalid)
	}
	value := checkpoint.Initialize.Aggregate
	if checkpoint.Execution != nil {
		value = checkpoint.Execution.Aggregate
	}
	return cloneAggregateCheckpoint(value)
}

// advanceCheckpointV7 is deliberately unexported: a structurally valid
// aggregate is not persistence authority. Only exact planner/reducer
// constructors may wrap its result in a sealed ExecutionTransition.
func advanceCheckpointV7(checkpoint *CheckpointV7, aggregate AggregateCheckpoint, status string) (*CheckpointV7, error) {
	current := CurrentLastLogSeq(checkpoint)
	if current >= math.MaxInt64 {
		return nil, &OverBudgetError{Limit: "log_entries", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
	}
	target := current + 1
	aggregateLast, err := aggregateLogicalLastSeq(aggregate)
	if err != nil {
		return nil, err
	}
	if aggregateLast > target {
		target = aggregateLast
	}
	return advanceCheckpointV7To(checkpoint, aggregate, status, target)
}

func advanceCheckpointV7PreservingLog(checkpoint *CheckpointV7, aggregate AggregateCheckpoint, status string) (*CheckpointV7, error) {
	return advanceCheckpointV7To(checkpoint, aggregate, status, CurrentLastLogSeq(checkpoint))
}

func advanceCheckpointV7To(checkpoint *CheckpointV7, aggregate AggregateCheckpoint, status string, lastLogSeq uint64) (*CheckpointV7, error) {
	if err := ValidateCheckpointV7(checkpoint); err != nil {
		return nil, err
	}
	if !runtimeStatusValid(status) {
		return nil, fmt.Errorf("%w: invalid execution status %q", ErrMutationInvalid, status)
	}
	aggregate, err := cloneAggregateCheckpoint(aggregate)
	if err != nil {
		return nil, err
	}
	view := aggregate.View()
	if report := ValidateAggregate(view); !report.Valid() {
		return nil, fmt.Errorf("%w: current aggregate diagnostics=%v (%d suppressed)", ErrMutationInconsistent, report.Diagnostics, report.Suppressed)
	}
	initialize := checkpoint.Initialize
	if aggregate.RunID != initialize.UpgradeNeeded.RunID || aggregate.TemplateRef != initialize.TemplateHash ||
		aggregate.TemplateSourceHash != initialize.UpgradeNeeded.TemplateSourceHash {
		return nil, fmt.Errorf("%w: current aggregate differs from immutable initialization authority", ErrMutationInconsistent)
	}
	current := CurrentCheckpointBinding(checkpoint)
	if CheckpointRevision(checkpoint) == math.MaxUint64 || current.Generation >= math.MaxInt64 {
		return nil, &OverBudgetError{Limit: "execution_revision", Value: math.MaxInt64, Maximum: math.MaxInt64 - 1}
	}
	currentLogSeq := CurrentLastLogSeq(checkpoint)
	if lastLogSeq < currentLogSeq || lastLogSeq > math.MaxInt64 {
		return nil, fmt.Errorf("%w: invalid logical log sequence %d after %d", ErrMutationInvalid, lastLogSeq, currentLogSeq)
	}
	delta := lastLogSeq - currentLogSeq
	if delta > uint64(MaxRoutingLogEntries) {
		return nil, &OverBudgetError{Limit: "log_entries", Value: int(delta), Maximum: MaxRoutingLogEntries}
	}
	aggregateLast, err := aggregateLogicalLastSeq(aggregate)
	if err != nil {
		return nil, err
	}
	if aggregateLast > lastLogSeq {
		return nil, fmt.Errorf("%w: aggregate event sequence %d exceeds logical checkpoint sequence %d", ErrMutationInvalid, aggregateLast, lastLogSeq)
	}
	logAdvanced := delta > 0
	execution := &ExecutionCheckpoint{
		Revision:       CheckpointRevision(checkpoint) + 1,
		PreviousDigest: current.Digest,
		Status:         status,
		LogAdvanced:    logAdvanced,
		LastLogSeq:     lastLogSeq,
		Aggregate:      aggregate,
	}
	if logAdvanced {
		checksum, err := executionLogChecksum(execution)
		if err != nil {
			return nil, err
		}
		execution.LogChecksum = checksum
	} else {
		execution.LogChecksum = CurrentLogChecksum(checkpoint)
	}
	encoded, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, err
	}
	var next CheckpointV7
	if err := json.Unmarshal(encoded, &next); err != nil {
		return nil, err
	}
	next.Execution = execution
	genesisDigest, err := initializeEventDigest(next.Initialize)
	if err != nil {
		return nil, err
	}
	next.Digest, err = executionCheckpointDigest(genesisDigest, execution)
	if err != nil {
		return nil, err
	}
	if err := ValidateCheckpointV7(&next); err != nil {
		return nil, err
	}
	if _, err := EncodeCheckpointV7(&next); err != nil {
		return nil, err
	}
	return &next, nil
}

// CompletionCheckpointJSON creates the narrow checkpoint-only completion
// projection consumed by CompletionBasis. The completion self command is
// excluded from the nested aggregate and retained at top level so the existing
// self-removal rule produces identical bytes before claim and after recovery.
func CompletionCheckpointJSON(checkpoint *CheckpointV7, selfCommandID string) ([]byte, error) {
	if err := ValidateCheckpointV7(checkpoint); err != nil {
		return nil, err
	}
	aggregate, err := CurrentAggregateCheckpoint(checkpoint)
	if err != nil {
		return nil, err
	}
	outstanding := make(map[string]CommandRecord)
	for id, command := range aggregate.Commands {
		if command.State.Active() || command.Identity.Kind == CommandCompleteRun {
			outstanding[id] = cloneCommandRecord(command)
		}
	}
	if selfCommandID != "" {
		command, ok := aggregate.Commands[selfCommandID]
		if !ok || command.Identity.Kind != CommandCompleteRun {
			return nil, fmt.Errorf("%w: completion self command missing", ErrMutationInconsistent)
		}
		outstanding[selfCommandID] = cloneCommandRecord(command)
		delete(aggregate.Commands, selfCommandID)
	}
	value := struct {
		StateSchemaVersion int                      `json:"stateSchemaVersion"`
		Status             string                   `json:"status"`
		LastLogSeq         uint64                   `json:"lastLogSeq"`
		LogChecksum        string                   `json:"logChecksum"`
		CheckpointDigest   string                   `json:"checkpointDigest"`
		Aggregate          AggregateCheckpoint      `json:"aggregate"`
		Outstanding        map[string]CommandRecord `json:"outstandingCommands"`
	}{
		StateSchemaVersion: CheckpointStateSchemaVersion,
		Status:             CurrentRunStatus(checkpoint),
		LastLogSeq:         CurrentLastLogSeq(checkpoint),
		LogChecksum:        CurrentLogChecksum(checkpoint),
		CheckpointDigest:   currentCompletionCheckpointDigest(checkpoint),
		Aggregate:          aggregate,
		Outstanding:        outstanding,
	}
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxCheckpointBytes {
		return nil, &OverBudgetError{Limit: "checkpoint_bytes", Value: len(data), Maximum: MaxCheckpointBytes}
	}
	return data, nil
}

func validateExecutionCheckpoint(checkpoint *CheckpointV7, genesisDigest string) error {
	execution := checkpoint.Execution
	if execution == nil {
		return fmt.Errorf("%w: execution checkpoint is absent", ErrInitializationInvalid)
	}
	if execution.Revision == 0 || execution.PreviousDigest == "" || !canonicalDigest(execution.PreviousDigest) ||
		!runtimeStatusValid(execution.Status) || execution.LastLogSeq < uint64(checkpoint.Initialize.EventSeq) || execution.LastLogSeq > math.MaxInt64 {
		return fmt.Errorf("%w: execution revision metadata is invalid", ErrInitializationInvalid)
	}
	if execution.Revision == 1 && execution.PreviousDigest != genesisDigest {
		return fmt.Errorf("%w: first execution revision does not extend genesis", ErrInitializationInvalid)
	}
	if execution.Aggregate.RunID != checkpoint.Initialize.UpgradeNeeded.RunID || execution.Aggregate.TemplateRef != checkpoint.Initialize.TemplateHash ||
		execution.Aggregate.TemplateSourceHash != checkpoint.Initialize.UpgradeNeeded.TemplateSourceHash {
		return fmt.Errorf("%w: execution aggregate anchor mismatch", ErrInitializationInvalid)
	}
	if report := ValidateAggregate(execution.Aggregate.View()); !report.Valid() {
		return fmt.Errorf("%w: execution aggregate diagnostics=%v (%d suppressed)", ErrInitializationInvalid, report.Diagnostics, report.Suppressed)
	}
	aggregateLast, err := aggregateLogicalLastSeq(execution.Aggregate)
	if err != nil || aggregateLast > execution.LastLogSeq {
		return fmt.Errorf("%w: execution aggregate exceeds logical log sequence", ErrInitializationInvalid)
	}
	if execution.LogAdvanced {
		wantChecksum, err := executionLogChecksum(execution)
		if err != nil || execution.LogChecksum != wantChecksum {
			return fmt.Errorf("%w: execution log checksum mismatch", ErrInitializationInvalid)
		}
	} else if !canonicalDigest(execution.LogChecksum) {
		return fmt.Errorf("%w: preserved execution log checksum is invalid", ErrInitializationInvalid)
	}
	wantDigest, err := executionCheckpointDigest(genesisDigest, execution)
	if err != nil || checkpoint.Digest != wantDigest {
		return fmt.Errorf("%w: execution checkpoint digest mismatch", ErrInitializationInvalid)
	}
	return nil
}

func executionLogChecksum(execution *ExecutionCheckpoint) (string, error) {
	copy := *execution
	copy.LogChecksum = ""
	data, err := canonicalJSON(struct {
		PreviousDigest string              `json:"previousDigest"`
		Revision       uint64              `json:"revision"`
		Status         string              `json:"status"`
		LogAdvanced    bool                `json:"logAdvanced,omitempty"`
		LastLogSeq     uint64              `json:"lastLogSeq"`
		Aggregate      AggregateCheckpoint `json:"aggregate"`
	}{copy.PreviousDigest, copy.Revision, copy.Status, copy.LogAdvanced, copy.LastLogSeq, copy.Aggregate})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func executionCheckpointDigest(genesisDigest string, execution *ExecutionCheckpoint) (string, error) {
	data, err := canonicalJSON(struct {
		GenesisDigest string               `json:"genesisDigest"`
		Execution     *ExecutionCheckpoint `json:"execution"`
	}{genesisDigest, execution})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func runtimeStatusValid(status string) bool {
	switch status {
	case "pending", "running", "blocked", "completed", "failed", "canceled":
		return true
	default:
		return false
	}
}
