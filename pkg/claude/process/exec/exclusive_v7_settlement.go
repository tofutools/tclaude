package processexec

import (
	"context"
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/process/state/pathv1"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// ResolveAuditedSettlement binds and appends one external schema-8 rescue.
// The coherent read lock is released before the external append reacquires the
// run flock; the sealed transition's pre-binding supplies the CAS boundary.
func (e *ExclusiveV7Executor) ResolveAuditedSettlement(ctx context.Context, runID, nodeID, decision, actor, reason, evidenceRef string) (*pathv1.CheckpointV7, pathv1.BlockResolution, error) {
	if e == nil || e.Store == nil || !e.epochExternal {
		return nil, pathv1.BlockResolution{}, fmt.Errorf("external schema-8 executor is required")
	}
	var transition *pathv1.ExecutionTransition
	err := e.withExecutionView(ctx, strings.TrimSpace(runID), func(view store.PathV1ExecutionView) error {
		attempt, bindErr := pathv1.LatestFailedExclusiveAttempt(ctx, view.Input, nodeID)
		if bindErr != nil {
			return bindErr
		}
		transition, bindErr = pathv1.SettleExclusiveAttempt(ctx, view.Input, pathv1.AuditedSettlementInput{
			NodeID: nodeID, BlockedAttempt: attempt, Decision: decision, Actor: actor,
			Reason: reason, EvidenceRef: evidenceRef, Timestamp: e.now(),
		})
		return bindErr
	})
	if err != nil {
		return nil, pathv1.BlockResolution{}, err
	}
	resolution, ok := transition.AuditedResolution()
	if !ok {
		return nil, pathv1.BlockResolution{}, fmt.Errorf("sealed settlement metadata is absent")
	}
	appended, err := e.appendTransition(ctx, strings.TrimSpace(runID), transition)
	if err != nil {
		return nil, pathv1.BlockResolution{}, err
	}
	return appended.Checkpoint, resolution, nil
}
