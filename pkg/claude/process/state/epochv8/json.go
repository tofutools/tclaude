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
	encoded, err := json.Marshal(checkpoint.wire)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > MaxCheckpointBytes {
		return nil, &OverBudgetError{Limit: "checkpoint_bytes", Value: len(encoded), Maximum: MaxCheckpointBytes}
	}
	return encoded, nil
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
	encoded, err := json.Marshal(plan.core)
	if err != nil {
		return nil, err
	}
	encoded = append(encoded, '\n')
	if len(encoded) > MaxApplyPlanBytes {
		return nil, &OverBudgetError{Limit: "apply_plan_bytes", Value: len(encoded), Maximum: MaxApplyPlanBytes}
	}
	return encoded, nil
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
