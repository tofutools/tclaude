package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func requireGraphInteractionExecution(ctx context.Context, tx *sql.Tx, id model.ExecutionID) (model.Execution, error) {
	execution, err := executionTx(ctx, tx, id)
	if err != nil {
		return execution, err
	}
	if execution.AgentID == "" || (execution.Workload != "" && execution.Workload != model.ExecutionWorkloadHarness) || execution.State != model.ExecutionRunning {
		return execution, app.ErrConflict
	}
	var primary model.ExecutionID
	var lifecycle model.AgentLifecycleState
	if err = tx.QueryRowContext(ctx, `SELECT primary_execution_id,lifecycle_state FROM agents WHERE id=?`, execution.AgentID).Scan(&primary, &lifecycle); err != nil {
		return execution, classify(err)
	}
	if primary != id || lifecycle != model.AgentActive {
		return execution, app.ErrConflict
	}
	return execution, nil
}

func insertGraphInteraction(ctx context.Context, tx *sql.Tx, in app.GraphInteractionAdmission, operation model.Operation) error {
	if operation.Kind != model.OperationInteract || operation.ExecutionID != in.ExecutionID || in.Attempt.IssuanceID == "" {
		return app.ErrInvalid
	}
	execution, err := requireGraphInteractionExecution(ctx, tx, in.ExecutionID)
	if err != nil {
		return err
	}
	var revision model.Revision
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM agents WHERE id=?`, in.AgentID).Scan(&revision); err != nil {
		return classify(err)
	}
	if execution.AgentID != in.AgentID || execution.Revision != in.ExecutionRevision || revision != in.AgentRevision {
		return app.ErrConflict
	}
	var active int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM work_node_attempts WHERE execution_id=? AND state IN (?,?,?)`, in.ExecutionID, model.NodeAttemptAdmitted, model.NodeAttemptRunning, model.NodeAttemptUncertain).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return app.ErrConflict
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO work_agent_interactions(operation_id,work_run_id,node_id,activation_id,attempt,issuance_id,execution_id) VALUES(?,?,?,?,?,?,?)`, operation.ID, in.Attempt.RunID, in.Attempt.NodeID, in.Attempt.ActivationID, in.Attempt.Attempt, in.Attempt.IssuanceID, in.ExecutionID)
	return classify(err)
}

// ClaimGraphInteraction consumes once immediately before native input. A new
// service instance encountering another owner's consumed input records uncertainty
// rather than sending it again. Concurrent sweeps in the same instance wait.
func (s *Store) ClaimGraphInteraction(ctx context.Context, id model.OperationID, owner string, at time.Time) (bool, error) {
	if owner == "" {
		return false, app.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var priorOwner string
	var consumed sql.NullInt64
	var ref model.WorkAttemptRef
	var executionID model.ExecutionID
	err = tx.QueryRowContext(ctx, `SELECT owner,consumed_at,work_run_id,node_id,activation_id,attempt,issuance_id,execution_id FROM work_agent_interactions WHERE operation_id=?`, id).Scan(&priorOwner, &consumed, &ref.RunID, &ref.NodeID, &ref.ActivationID, &ref.Attempt, &ref.IssuanceID, &executionID)
	if err != nil {
		return false, classify(err)
	}
	operation, err := operationTx(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if operation.State != model.OperationAdmitted {
		return false, nil
	}
	if consumed.Valid {
		if priorOwner == owner {
			return false, nil
		}
		err = completeOperationTx(ctx, tx, app.OperationCompletion{OperationID: id, OperationState: model.OperationUncertain, ResultCode: "work_input_unknown", Detail: "previous work input was consumed without a durable outcome; it will not be replayed", ExecutionID: executionID, At: at})
		if err != nil {
			return false, err
		}
		if err = bumpTx(ctx, tx); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	_, err = requireGraphInteractionExecution(ctx, tx, executionID)
	if err != nil {
		return false, err
	}
	var cancelled bool
	var deadline int64
	var state model.WorkNodeAttemptState
	var runState model.WorkRunState
	err = tx.QueryRowContext(ctx, `SELECT r.cancellation_requested,r.deadline,a.state,r.state FROM work_runs r JOIN work_node_attempts a ON a.work_run_id=r.id WHERE r.id=? AND a.node_id=? AND a.activation_id=? AND a.attempt=? AND a.issuance_id=? AND a.operation_id=? AND a.execution_id=?`, ref.RunID, ref.NodeID, ref.ActivationID, ref.Attempt, ref.IssuanceID, id, executionID).Scan(&cancelled, &deadline, &state, &runState)
	if err != nil {
		return false, classify(err)
	}
	if (runState != model.WorkRunRunning && runState != model.WorkRunWaiting) || cancelled || !at.Before(fromNanos(deadline)) || state != model.NodeAttemptAdmitted {
		return false, app.ErrConflict
	}
	request, ok, err := operationAuthority(ctx, tx, id)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, app.ErrConflict
	}
	decision, err := authorizeTx(ctx, tx, request, at)
	if err != nil {
		return false, err
	}
	if !decision.Allowed {
		return false, app.ErrUnauthorized
	}
	extra, err := additionalOperationAuthorities(ctx, tx, id, operation.Principal)
	if err != nil {
		return false, err
	}
	for _, request := range extra {
		decision, err := authorizeTx(ctx, tx, request, at)
		if err != nil {
			return false, err
		}
		if !decision.Allowed {
			return false, app.ErrUnauthorized
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE effect_permits SET consumed_at=? WHERE operation_id=? AND consumed_at IS NULL`, nanos(at), id)
	if err != nil {
		return false, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return false, app.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE work_agent_interactions SET owner=?,consumed_at=? WHERE operation_id=? AND consumed_at IS NULL`, owner, nanos(at), id); err != nil {
		return false, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
