package processexec

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/process/state/epochv8"
	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

type EpochV8SettlementPreflight struct {
	Token       string
	Target      pathv1.FailedExclusiveAttempt
	Transition  *pathv1.ExecutionTransition
	Prospective epochv8.RuntimeTransitionResult
}

// PreflightEpochV8AuditedSettlement is the shared pure classifier/handler
// seam. An empty expectedToken acquires the opaque target token; a non-empty
// token must match before the exact audited constructor is invoked.
func PreflightEpochV8AuditedSettlement(ctx context.Context, view store.EpochV8ExecutionView, expectedToken string, settlement pathv1.AuditedSettlementInput) (EpochV8SettlementPreflight, error) {
	if view.Checkpoint == nil || view.Runtime == nil {
		return EpochV8SettlementPreflight{}, fmt.Errorf("schema-8 runtime is unavailable")
	}
	source := view.EpochSources[view.Runtime.EpochID]
	input, err := pathv1.VerifyExecutionInput(ctx, view.Runtime.Checkpoint, source)
	if err != nil {
		return EpochV8SettlementPreflight{}, err
	}
	target, err := pathv1.UniqueFailedExclusiveAttempt(ctx, input)
	if err != nil {
		return EpochV8SettlementPreflight{}, err
	}
	inner, err := pathv1.DecodeCheckpointV7(view.Runtime.Checkpoint)
	if err != nil {
		return EpochV8SettlementPreflight{}, err
	}
	token, err := settlementToken(view, pathv1.CurrentCheckpointBinding(inner), target)
	if err != nil {
		return EpochV8SettlementPreflight{}, err
	}
	if expectedToken != "" && (len(expectedToken) != len(token) || subtle.ConstantTimeCompare([]byte(expectedToken), []byte(token)) != 1) {
		return EpochV8SettlementPreflight{}, fmt.Errorf("settlement token is stale or invalid")
	}
	settlement.NodeID = target.NodeID
	settlement.BlockedAttempt = target.BlockedAttempt
	transition, err := pathv1.SettleExclusiveAttempt(ctx, input, settlement)
	if err != nil {
		return EpochV8SettlementPreflight{}, err
	}
	prospective, err := epochv8.PreflightAuditedSettlement(ctx, view.Checkpoint, view.RuntimeJSON, source, transition)
	if err != nil {
		return EpochV8SettlementPreflight{}, err
	}
	return EpochV8SettlementPreflight{Token: token, Target: target, Transition: transition, Prospective: prospective}, nil
}

func settlementToken(view store.EpochV8ExecutionView, inner pathv1.CheckpointBinding, target pathv1.FailedExclusiveAttempt) (string, error) {
	payload := struct {
		Domain  string                   `json:"domain"`
		RunID   string                   `json:"runId"`
		Outer   epochv8.Binding          `json:"outer"`
		Runtime epochv8.RuntimeBinding   `json:"runtime"`
		Inner   pathv1.CheckpointBinding `json:"inner"`
		Node    string                   `json:"node"`
		Attempt uint64                   `json:"attempt"`
	}{"process-preview-settlement/v1", view.Run.ID, view.Checkpoint.Binding(), view.Checkpoint.View().RuntimeBinding, inner, target.NodeID, target.BlockedAttempt}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
