package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const terminalFileSchema = `CREATE TABLE IF NOT EXISTS terminal_files(operation_id TEXT PRIMARY KEY REFERENCES operations(id),execution_id TEXT NOT NULL REFERENCES executions(id),attempt INTEGER NOT NULL,filename TEXT NOT NULL,size INTEGER NOT NULL,sha256 TEXT NOT NULL,result_json BLOB NOT NULL);`

func (s *Store) AdmitTerminalFile(ctx context.Context, in app.TerminalFileAdmission) (app.TerminalFileResult, error) {
	var result app.TerminalFileResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()
	execution, err := executionTx(ctx, tx, in.File.ExecutionID)
	if err != nil {
		return result, err
	}
	decision, err := authorizeTx(ctx, tx, in.Authority, in.Operation.CreatedAt)
	if err != nil {
		return result, err
	}
	if !decision.Allowed {
		return result, app.ErrUnauthorized
	}
	if repeated, ok, retryErr := admissionByRequest(ctx, tx, in.Operation, "", true); retryErr != nil {
		return result, retryErr
	} else if ok {
		var data []byte
		if err = tx.QueryRowContext(ctx, `SELECT result_json FROM terminal_files WHERE operation_id=?`, repeated.Operation.ID).Scan(&data); err != nil {
			return result, classify(err)
		}
		if err = json.Unmarshal(data, &result.File); err != nil {
			return result, err
		}
		if result.File.Filename != in.File.Filename || result.File.Size != in.File.Size || result.File.SHA256 != in.File.SHA256 {
			return app.TerminalFileResult{}, app.ErrConflict
		}
		result.Operation = repeated.Operation
		result.Repeated = true
		return result, nil
	}
	if err = terminalFileExecutionEligible(ctx, tx, execution); err != nil {
		return result, err
	}
	var count, total int64
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(size),0) FROM terminal_files WHERE execution_id=?`, execution.ID).Scan(&count, &total); err != nil {
		return result, err
	}
	if count >= 100 || total+in.File.Size > 128<<20 {
		return result, app.ErrConflict
	}
	if err = insertOperation(ctx, tx, in.Operation); err != nil {
		return result, err
	}
	if err = insertOperationAuthority(ctx, tx, in.Operation.ID, in.Authority, decision); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO effect_permits(operation_id) VALUES(?)`, in.Operation.ID); err != nil {
		return result, err
	}
	data, err := json.Marshal(in.File)
	if err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO terminal_files(operation_id,execution_id,attempt,filename,size,sha256,result_json) VALUES(?,?,?,?,?,?,?)`, in.Operation.ID, execution.ID, execution.Attempt, in.File.Filename, in.File.Size, in.File.SHA256, data); err != nil {
		return result, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return result, err
	}
	result.Operation = in.Operation
	result.File = in.File
	return result, tx.Commit()
}
func terminalFileExecutionEligible(ctx context.Context, q queryer, e model.Execution) error {
	if (e.State != model.ExecutionRunning && e.State != model.ExecutionReleased) || (e.Workload != "" && e.Workload != model.ExecutionWorkloadHarness && e.Workload != model.ExecutionWorkloadShell) {
		return app.ErrConflict
	}
	if e.AgentID != "" {
		var lifecycle model.AgentLifecycleState
		var primary model.ExecutionID
		if err := q.QueryRowContext(ctx, `SELECT lifecycle_state,primary_execution_id FROM agents WHERE id=?`, e.AgentID).Scan(&lifecycle, &primary); err != nil {
			return classify(err)
		}
		if lifecycle != model.AgentActive || primary != e.ID {
			return app.ErrConflict
		}
	}
	return nil
}
func (s *Store) ConsumeTerminalFile(ctx context.Context, id model.OperationID, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	op, err := operationTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if op.Kind != model.OperationStageTerminalFile || op.State != model.OperationAdmitted {
		return app.ErrConflict
	}
	e, err := executionTx(ctx, tx, op.ExecutionID)
	if err != nil {
		return err
	}
	if err = terminalFileExecutionEligible(ctx, tx, e); err != nil {
		return err
	}
	var attempt model.AttemptGeneration
	if err = tx.QueryRowContext(ctx, `SELECT attempt FROM terminal_files WHERE operation_id=? AND execution_id=?`, id, e.ID).Scan(&attempt); err != nil {
		return classify(err)
	}
	if attempt != e.Attempt {
		return app.ErrConflict
	}
	authority, ok, err := operationAuthority(ctx, tx, id)
	if err != nil {
		return err
	}
	if !ok || authority.Action != model.ActionStageTerminalFile {
		return app.ErrConflict
	}
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return app.ErrUnauthorized
	}
	r, err := tx.ExecContext(ctx, `UPDATE effect_permits SET consumed_at=? WHERE operation_id=? AND consumed_at IS NULL`, nanos(at), id)
	if err != nil {
		return err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return app.ErrConflict
	}
	if err = bumpTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) CompleteTerminalFile(ctx context.Context, in app.OperationCompletion, file model.TerminalFile) (app.TerminalFileResult, error) {
	var out app.TerminalFileResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	op, err := operationTx(ctx, tx, in.OperationID)
	if err != nil {
		return out, err
	}
	if op.Kind != model.OperationStageTerminalFile || op.State != model.OperationAdmitted || file.OperationID != op.ID || file.ExecutionID != op.ExecutionID {
		return out, app.ErrConflict
	}
	if in.OperationState == model.OperationSucceeded {
		var consumed sql.NullInt64
		if err = tx.QueryRowContext(ctx, `SELECT consumed_at FROM effect_permits WHERE operation_id=?`, op.ID).Scan(&consumed); err != nil {
			return out, err
		}
		if !consumed.Valid {
			return out, app.ErrConflict
		}
	}
	data, err := json.Marshal(file)
	if err != nil {
		return out, err
	}
	r, err := tx.ExecContext(ctx, `UPDATE terminal_files SET result_json=? WHERE operation_id=? AND filename=? AND size=? AND sha256=?`, data, op.ID, file.Filename, file.Size, file.SHA256)
	if err != nil {
		return out, err
	}
	n, err := r.RowsAffected()
	if err != nil {
		return out, err
	}
	if n != 1 {
		return out, app.ErrConflict
	}
	if err = completeOperationTx(ctx, tx, in); err != nil {
		return out, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return out, err
	}
	out.Operation, err = operationTx(ctx, tx, op.ID)
	if err != nil {
		return out, err
	}
	out.File = file
	return out, tx.Commit()
}
