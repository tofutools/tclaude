package epochv8

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func EncodeCheckpointV8(checkpoint *CheckpointV8) ([]byte, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return nil, err
	}
	return marshalCheckpointWire(checkpoint.wire)
}

func DecodeCheckpointV8(data []byte) (*CheckpointV8, error) {
	if len(data) == 0 || len(data) > MaxCheckpointBytes {
		return nil, &OverBudgetError{Limit: "checkpoint_bytes", Value: len(data), Maximum: MaxCheckpointBytes}
	}
	var wire checkpointWire
	if err := decodeStrictJSON(data, &wire); err != nil {
		return nil, fmt.Errorf("%w: decode checkpoint: %v", ErrInvalid, err)
	}
	checkpoint := &CheckpointV8{wire: wire}
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return nil, err
	}
	canonical, err := EncodeCheckpointV8(checkpoint)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, data) {
		return nil, ErrNonCanonical
	}
	return checkpoint, nil
}

func EncodeApplyPlan(plan *ApplyPlan) ([]byte, error) {
	if plan == nil {
		return nil, fmt.Errorf("%w: apply plan is nil", ErrInvalid)
	}
	if err := validateApplyCoreStatic(plan.core.RunID, plan.core); err != nil {
		return nil, err
	}
	return marshalApplyCore(plan.core)
}

func DecodeApplyPlan(data []byte) (*ApplyPlan, error) {
	if len(data) == 0 || len(data) > MaxApplyPlanBytes {
		return nil, &OverBudgetError{Limit: "apply_plan_bytes", Value: len(data), Maximum: MaxApplyPlanBytes}
	}
	var core applyCore
	if err := decodeStrictJSON(data, &core); err != nil {
		return nil, fmt.Errorf("%w: decode apply plan: %v", ErrInvalid, err)
	}
	if err := validateApplyCoreStatic(core.RunID, core); err != nil {
		return nil, err
	}
	plan := &ApplyPlan{core: core}
	canonical, err := EncodeApplyPlan(plan)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, data) {
		return nil, ErrNonCanonical
	}
	return plan, nil
}

// EncodeAppliedEpochDiff returns the canonical persisted diff and reason
// digest for an epoch that was introduced by a verified apply record. Epoch
// zero and epochs without a unique apply record are not publication records.
func EncodeAppliedEpochDiff(checkpoint *CheckpointV8, epochID EpochID) ([]byte, string, error) {
	if err := VerifyCheckpointV8(checkpoint); err != nil {
		return nil, "", err
	}
	if epochID == "" || epochID == checkpoint.wire.Anchor.OriginalEpoch.ID {
		return nil, "", fmt.Errorf("%w: epoch is not an applied epoch", ErrInvalid)
	}

	var record *ApplyRecord
	for i := range checkpoint.wire.History {
		event := &checkpoint.wire.History[i]
		if event.Kind != HistoryApply || event.Apply == nil || event.Apply.CandidateEpoch.ID != epochID {
			continue
		}
		if record != nil {
			return nil, "", fmt.Errorf("%w: applied epoch record is ambiguous", ErrInvalid)
		}
		record = event.Apply
	}
	if record == nil {
		return nil, "", fmt.Errorf("%w: applied epoch record is absent", ErrInvalid)
	}

	encoded, err := json.Marshal(record.Diff)
	if err != nil {
		return nil, "", err
	}
	encoded = append(encoded, '\n')
	if err := checkWireBudget("apply_diff_bytes", len(encoded), MaxApplyPlanBytes); err != nil {
		return nil, "", err
	}
	return encoded, record.ReasonDigest, nil
}

func decodeStrictJSON(data []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func marshalCheckpointWire(wire checkpointWire) ([]byte, error) {
	encoded, err := json.Marshal(wire)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if err := checkWireBudget("checkpoint_bytes", len(encoded), MaxCheckpointBytes); err != nil {
		return nil, err
	}
	return encoded, nil
}

func marshalApplyCore(core applyCore) ([]byte, error) {
	encoded, err := json.Marshal(core)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if err := checkWireBudget("apply_plan_bytes", len(encoded), MaxApplyPlanBytes); err != nil {
		return nil, err
	}
	return encoded, nil
}

func ensureCheckpointWireBudget(wire checkpointWire) error {
	_, err := marshalCheckpointWire(wire)
	return err
}

func ensureApplyCoreWireBudget(core applyCore) error {
	_, err := marshalApplyCore(core)
	return err
}

func checkWireBudget(limit string, value, maximum int) error {
	if value > maximum {
		return &OverBudgetError{Limit: limit, Value: value, Maximum: maximum}
	}
	return nil
}
