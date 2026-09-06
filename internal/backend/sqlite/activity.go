package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const activitySchema = `
CREATE TABLE IF NOT EXISTS historical_activity (
  id TEXT PRIMARY KEY, source_key TEXT NOT NULL, source_revision TEXT NOT NULL,
  kind TEXT NOT NULL, actor_kind TEXT NOT NULL, actor_agent_id TEXT NOT NULL DEFAULT '',
  actor_execution_id TEXT NOT NULL DEFAULT '', actor_automation_run TEXT NOT NULL DEFAULT '',
  agent_id TEXT NOT NULL DEFAULT '', conversation_id TEXT NOT NULL DEFAULT '', execution_id TEXT NOT NULL DEFAULT '',
  work_run_id TEXT NOT NULL DEFAULT '', outcome TEXT NOT NULL DEFAULT '', reason TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL, finished_at INTEGER, provenance TEXT NOT NULL,
  UNIQUE(source_key,source_revision)
);
CREATE INDEX IF NOT EXISTS historical_activity_started ON historical_activity(started_at DESC,id);
`

func (s *Store) ImportHistoricalActivity(ctx context.Context, write app.HistoricalActivityWrite) (model.ActivityRecord, bool, error) {
	if !write.Record.Historical || strings.TrimSpace(write.SourceKey) == "" || strings.TrimSpace(write.SourceRevision) == "" || strings.TrimSpace(write.Record.Provenance) == "" || write.Record.StartedAt.IsZero() {
		return model.ActivityRecord{}, false, app.ErrInvalid
	}
	var finished any
	if write.Record.FinishedAt != nil {
		finished = nanos(*write.Record.FinishedAt)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ActivityRecord{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.ExecContext(ctx, `INSERT INTO historical_activity(id,source_key,source_revision,kind,actor_kind,actor_agent_id,actor_execution_id,actor_automation_run,agent_id,conversation_id,execution_id,work_run_id,outcome,reason,started_at,finished_at,provenance) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		write.Record.ID, write.SourceKey, write.SourceRevision, write.Record.Kind, write.Record.Actor.Kind, write.Record.Actor.AgentID, write.Record.Actor.ExecutionID, write.Record.Actor.AutomationRun,
		write.Record.AgentID, write.Record.ConversationID, write.Record.ExecutionID, write.Record.WorkRunID, write.Record.Outcome, write.Record.Reason, nanos(write.Record.StartedAt), finished, write.Record.Provenance)
	if err == nil {
		if bumpErr := bumpTx(ctx, tx); bumpErr != nil {
			return model.ActivityRecord{}, false, bumpErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return model.ActivityRecord{}, false, commitErr
		}
		return write.Record, false, nil
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unique") {
		return model.ActivityRecord{}, false, classify(err)
	}
	if rollbackErr := tx.Rollback(); rollbackErr != nil {
		return model.ActivityRecord{}, false, rollbackErr
	}
	existing, readErr := s.historicalActivity(ctx, write.SourceKey, write.SourceRevision)
	if readErr != nil {
		return model.ActivityRecord{}, false, readErr
	}
	if !reflect.DeepEqual(existing, write.Record) {
		return model.ActivityRecord{}, false, app.ErrConflict
	}
	return existing, true, nil
}

const activityUnion = `
SELECT o.id AS id,'operation' AS kind,o.principal_kind AS actor_kind,o.principal_agent_id AS actor_agent_id,
 o.principal_execution_id AS actor_execution_id,o.principal_automation_run AS actor_automation_run,'' AS actor_json,
 COALESCE(NULLIF(e.agent_id,''),o.principal_agent_id,'') AS agent_id,COALESCE(e.conversation_id,'') AS conversation_id,
 o.execution_id AS execution_id,'' AS work_run_id,o.state AS outcome,o.result_code AS reason,o.created_at AS started_at,
 CASE WHEN o.state IN ('succeeded','refused','failed','uncertain') THEN o.updated_at END AS finished_at,0 AS historical,'operation' AS provenance
FROM operations o LEFT JOIN executions e ON e.id=o.execution_id
UNION ALL
SELECT w.id,'work_run','json','','','',w.requester_json,COALESCE(NULLIF(e.agent_id,''),json_extract(w.requester_json,'$.AgentID'),''),COALESCE(e.conversation_id,''),w.worker_execution_id,w.id,w.state,w.cancellation_reason,w.created_at,
 CASE WHEN w.state IN ('succeeded','failed','cancelled') THEN w.updated_at END,0,'work_run'
FROM work_runs w LEFT JOIN executions e ON e.id=w.worker_execution_id
UNION ALL
SELECT v.id,'work_evidence','json','','','',v.reporter_json,COALESCE(NULLIF(e.agent_id,''),json_extract(v.reporter_json,'$.AgentID'),''),COALESCE(e.conversation_id,''),COALESCE(e.id,''),v.work_run_id,
 CASE WHEN v.passed IS NULL THEN v.kind WHEN v.passed=1 THEN v.kind||':passed' ELSE v.kind||':failed' END,'',v.recorded_at,v.recorded_at,0,'work_evidence'
FROM work_evidence v JOIN work_runs w ON w.id=v.work_run_id LEFT JOIN executions e ON e.id=w.worker_execution_id
UNION ALL
SELECT 'decision:'||d.work_run_id,'decision','json','','','',d.decider_json,COALESCE(NULLIF(e.agent_id,''),json_extract(d.decider_json,'$.AgentID'),''),COALESCE(e.conversation_id,''),COALESCE(e.id,''),d.work_run_id,d.decision,d.reason,d.decided_at,d.decided_at,0,'work_decision'
FROM work_decisions d JOIN work_runs w ON w.id=d.work_run_id LEFT JOIN executions e ON e.id=w.worker_execution_id
UNION ALL
SELECT 'decision:'||d.decision_id||':'||d.request_id,'decision','json','','','',d.actor_json,
 COALESCE(NULLIF(e.agent_id,''),json_extract(d.actor_json,'$.AgentID'),''),COALESCE(e.conversation_id,''),COALESCE(e.id,''),w.id,
 d.answer,d.reason,d.submitted_at,d.submitted_at,0,'decision_submission'
FROM decision_submissions d JOIN decision_windows window ON window.id=d.decision_id
 JOIN work_runs w ON w.id=window.work_run_id LEFT JOIN executions e ON e.id=w.worker_execution_id
UNION ALL
SELECT h.id,h.kind,h.actor_kind,h.actor_agent_id,h.actor_execution_id,h.actor_automation_run,'',h.agent_id,h.conversation_id,h.execution_id,h.work_run_id,h.outcome,h.reason,h.started_at,h.finished_at,1,h.provenance
FROM historical_activity h`

func (s *Store) QueryActivity(ctx context.Context, filter app.ActivityFilter, authority model.AuthorityRequest, at time.Time) (app.ActivityResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.ActivityResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = validateActivityTargetExists(ctx, tx, filter.Target); err != nil {
		return app.ActivityResult{}, err
	}
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil || !decision.Allowed {
		if err == nil {
			err = app.ErrUnauthorized
		}
		return app.ActivityResult{}, err
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	query := `SELECT * FROM (` + activityUnion + `) activity WHERE 1=1`
	args := []any{}
	switch {
	case filter.Target.AgentID != "":
		query += ` AND agent_id=?`
		args = append(args, filter.Target.AgentID)
	case filter.Target.ConversationID != "":
		query += ` AND conversation_id=?`
		args = append(args, filter.Target.ConversationID)
	case filter.Target.ExecutionID != "":
		query += ` AND execution_id=?`
		args = append(args, filter.Target.ExecutionID)
	case filter.Target.WorkRunID != "":
		query += ` AND work_run_id=?`
		args = append(args, filter.Target.WorkRunID)
	}
	if len(filter.Kinds) > 0 {
		query += ` AND kind IN (` + placeholders(len(filter.Kinds)) + `)`
		for _, kind := range filter.Kinds {
			args = append(args, kind)
		}
	}
	if !filter.After.IsZero() {
		query += ` AND started_at>=?`
		args = append(args, nanos(filter.After))
	}
	if !filter.Before.IsZero() {
		query += ` AND started_at<?`
		args = append(args, nanos(filter.Before))
	}
	if filter.Cursor != "" {
		cursorAt, cursorID, decodeErr := decodeCursor(filter.Cursor)
		if decodeErr != nil {
			return app.ActivityResult{}, app.ErrInvalid
		}
		query += ` AND (started_at<? OR (started_at=? AND id>?))`
		args = append(args, cursorAt, cursorAt, cursorID)
	}
	query += ` ORDER BY started_at DESC,id ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return app.ActivityResult{}, err
	}
	var result app.ActivityResult
	for rows.Next() {
		var record model.ActivityRecord
		var actorKind string
		var actorJSON, actorAgent, actorExecution, actorAutomation string
		var started int64
		var finished sql.NullInt64
		if err = rows.Scan(&record.ID, &record.Kind, &actorKind, &actorAgent, &actorExecution, &actorAutomation, &actorJSON, &record.AgentID, &record.ConversationID, &record.ExecutionID, &record.WorkRunID, &record.Outcome, &record.Reason, &started, &finished, &record.Historical, &record.Provenance); err != nil {
			rows.Close()
			return result, err
		}
		if actorKind == "json" {
			var principal model.Principal
			_ = json.Unmarshal([]byte(actorJSON), &principal)
			record.Actor = model.ActivityActor{Kind: principal.Kind, AgentID: principal.AgentID, ExecutionID: principal.ExecutionID, AutomationRun: principal.AutomationRun}
		} else {
			record.Actor = model.ActivityActor{Kind: model.PrincipalKind(actorKind), AgentID: model.AgentID(actorAgent), ExecutionID: model.ExecutionID(actorExecution), AutomationRun: actorAutomation}
		}
		record.StartedAt = fromNanos(started)
		if finished.Valid {
			value := fromNanos(finished.Int64)
			record.FinishedAt = &value
		}
		result.Records = append(result.Records, record)
	}
	if err = rows.Close(); err != nil {
		return result, err
	}
	if len(result.Records) > limit {
		last := result.Records[limit-1]
		result.NextCursor = encodeCursor(nanos(last.StartedAt), last.ID)
		result.Records = result.Records[:limit]
	}
	return result, tx.Commit()
}

func validateActivityTargetExists(ctx context.Context, q queryer, target app.ActivityTarget) error {
	var found int
	var err error
	switch {
	case target.AgentID != "":
		err = q.QueryRowContext(ctx, `SELECT 1 FROM agents WHERE id=?`, target.AgentID).Scan(&found)
	case target.ConversationID != "":
		err = q.QueryRowContext(ctx, `SELECT 1 FROM conversations WHERE id=?`, target.ConversationID).Scan(&found)
	case target.ExecutionID != "":
		err = q.QueryRowContext(ctx, `SELECT 1 FROM executions WHERE id=?`, target.ExecutionID).Scan(&found)
	case target.WorkRunID != "":
		err = q.QueryRowContext(ctx, `SELECT 1 FROM work_runs WHERE id=?`, target.WorkRunID).Scan(&found)
	default:
		return app.ErrInvalid
	}
	return classify(err)
}

func (s *Store) historicalActivity(ctx context.Context, key, revision string) (model.ActivityRecord, error) {
	var record model.ActivityRecord
	var started int64
	var finished sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT id,kind,actor_kind,actor_agent_id,actor_execution_id,actor_automation_run,agent_id,conversation_id,execution_id,work_run_id,outcome,reason,started_at,finished_at,provenance FROM historical_activity WHERE source_key=? AND source_revision=?`, key, revision).Scan(
		&record.ID, &record.Kind, &record.Actor.Kind, &record.Actor.AgentID, &record.Actor.ExecutionID, &record.Actor.AutomationRun, &record.AgentID, &record.ConversationID, &record.ExecutionID, &record.WorkRunID, &record.Outcome, &record.Reason, &started, &finished, &record.Provenance)
	if err != nil {
		return record, classify(err)
	}
	record.Historical, record.StartedAt = true, fromNanos(started)
	if finished.Valid {
		value := fromNanos(finished.Int64)
		record.FinishedAt = &value
	}
	return record, nil
}

func placeholders(count int) string {
	if count <= 0 {
		return ""
	}
	return strings.TrimRight(strings.Repeat("?,", count), ",")
}

var _ app.ActivityStore = (*Store)(nil)
