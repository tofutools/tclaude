package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

type Store struct{ db *sql.DB }

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("replacement backend database path is required")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open replacement backend database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) initialize(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize replacement backend schema: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS backend_meta (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  revision INTEGER NOT NULL
);
INSERT OR IGNORE INTO backend_meta(singleton, revision) VALUES (1, 0);

CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY, name TEXT NOT NULL,
  harness TEXT NOT NULL, model TEXT NOT NULL, working_directory TEXT NOT NULL,
  approval TEXT NOT NULL, sandbox TEXT NOT NULL,
  primary_execution_id TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS groups (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS group_members (
  group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL REFERENCES agents(id),
  position INTEGER NOT NULL,
  PRIMARY KEY(group_id, agent_id)
);
CREATE TABLE IF NOT EXISTS conversations (
  id TEXT PRIMARY KEY, revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS agent_conversations (
  agent_id TEXT NOT NULL REFERENCES agents(id),
  conversation_id TEXT NOT NULL REFERENCES conversations(id),
  current INTEGER NOT NULL, revision INTEGER NOT NULL,
  associated_at INTEGER NOT NULL, replaced_at INTEGER,
  PRIMARY KEY(agent_id, conversation_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS agent_current_conversation
  ON agent_conversations(agent_id) WHERE current = 1;

CREATE TABLE IF NOT EXISTS executions (
  id TEXT PRIMARY KEY, agent_id TEXT NOT NULL DEFAULT '', conversation_id TEXT NOT NULL,
  harness TEXT NOT NULL, model TEXT NOT NULL, working_directory TEXT NOT NULL,
  approval TEXT NOT NULL, sandbox TEXT NOT NULL, state TEXT NOT NULL,
  evidence_provider TEXT NOT NULL DEFAULT '', evidence_version INTEGER NOT NULL DEFAULT 0,
  evidence_payload BLOB,
  native_namespace TEXT NOT NULL DEFAULT '', native_reference TEXT NOT NULL DEFAULT '', native_observed_at INTEGER,
  revision INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS executions_conversation ON executions(conversation_id, created_at);
CREATE TABLE IF NOT EXISTS operations (
  id TEXT PRIMARY KEY, request_id TEXT NOT NULL UNIQUE, kind TEXT NOT NULL,
  principal_kind TEXT NOT NULL, principal_agent_id TEXT NOT NULL DEFAULT '',
  execution_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
  result_code TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS release_permits (
  execution_id TEXT PRIMARY KEY REFERENCES executions(id),
  operation_id TEXT NOT NULL UNIQUE REFERENCES operations(id),
  consumed_at INTEGER
);
CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY, operation_id TEXT NOT NULL UNIQUE REFERENCES operations(id),
  sender_kind TEXT NOT NULL, sender_agent_id TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS message_recipients (
  id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL REFERENCES agents(id), read_at INTEGER, notified INTEGER NOT NULL DEFAULT 0,
  UNIQUE(message_id, agent_id)
);
`

func (s *Store) CreateAgent(ctx context.Context, agent model.Agent) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents(id,name,harness,model,working_directory,approval,sandbox,primary_execution_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		agent.ID, agent.Name, agent.Desired.Harness, agent.Desired.Model, agent.Desired.WorkingDirectory, agent.Desired.Approval, agent.Desired.Sandbox, agent.PrimaryExecutionID, agent.Revision, nanos(agent.CreatedAt), nanos(agent.UpdatedAt))
	if err != nil {
		return classify(err)
	}
	return s.bump(ctx)
}

func (s *Store) UpdateAgent(ctx context.Context, id model.AgentID, expected model.Revision, name string, desired model.DesiredConfiguration, at time.Time) (model.Agent, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE agents SET name=?,harness=?,model=?,working_directory=?,approval=?,sandbox=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`,
		name, desired.Harness, desired.Model, desired.WorkingDirectory, desired.Approval, desired.Sandbox, nanos(at), id, expected)
	if err != nil {
		return model.Agent{}, classify(err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.Agent{}, app.ErrConflict
	}
	if err := s.bump(ctx); err != nil {
		return model.Agent{}, err
	}
	return s.Agent(ctx, id)
}

func (s *Store) Agent(ctx context.Context, id model.AgentID) (model.Agent, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,harness,model,working_directory,approval,sandbox,primary_execution_id,revision,created_at,updated_at FROM agents WHERE id=?`, id)
	return scanAgent(row)
}

func (s *Store) CreateGroup(ctx context.Context, group model.Group) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO groups(id,name,revision,created_at,updated_at) VALUES(?,?,?,?,?)`, group.ID, group.Name, group.Revision, nanos(group.CreatedAt), nanos(group.UpdatedAt)); err != nil {
		return classify(err)
	}
	for position, member := range group.Members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,agent_id,position) VALUES(?,?,?)`, group.ID, member, position); err != nil {
			return classify(err)
		}
	}
	if err := bumpTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Group(ctx context.Context, id model.GroupID) (model.Group, error) {
	var group model.Group
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,name,revision,created_at,updated_at FROM groups WHERE id=?`, id).Scan(&group.ID, &group.Name, &group.Revision, &created, &updated)
	if err != nil {
		return model.Group{}, classify(err)
	}
	group.CreatedAt, group.UpdatedAt = fromNanos(created), fromNanos(updated)
	rows, err := s.db.QueryContext(ctx, `SELECT agent_id FROM group_members WHERE group_id=? ORDER BY position`, id)
	if err != nil {
		return model.Group{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var member model.AgentID
		if err := rows.Scan(&member); err != nil {
			return model.Group{}, err
		}
		group.Members = append(group.Members, member)
	}
	return group, rows.Err()
}

func (s *Store) AdmitLaunch(ctx context.Context, in app.LaunchAdmission) (app.AdmissionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if repeated, ok, err := admissionByRequest(ctx, tx, in.Operation, in.AgentID, false); err != nil {
		return app.AdmissionResult{}, err
	} else if ok {
		_ = tx.Commit()
		return repeated, nil
	}
	if in.AgentID != "" {
		var revision model.Revision
		var primary model.ExecutionID
		if err := tx.QueryRowContext(ctx, `SELECT revision,primary_execution_id FROM agents WHERE id=?`, in.AgentID).Scan(&revision, &primary); err != nil {
			return app.AdmissionResult{}, classify(err)
		}
		if revision != in.Expected {
			return app.AdmissionResult{}, app.ErrConflict
		}
		if primary != "" {
			var state model.ExecutionState
			if err := tx.QueryRowContext(ctx, `SELECT state FROM executions WHERE id=?`, primary).Scan(&state); err != nil {
				return app.AdmissionResult{}, classify(err)
			}
			if state != model.ExecutionExited && state != model.ExecutionFailed {
				return app.AdmissionResult{}, app.ErrConflict
			}
		}
		if in.ExpectedConversationRevision != 0 {
			var associationRevision model.Revision
			err := tx.QueryRowContext(ctx, `SELECT revision FROM agent_conversations WHERE agent_id=? AND conversation_id=? AND current=1`, in.AgentID, in.Execution.ConversationID).Scan(&associationRevision)
			if err != nil {
				return app.AdmissionResult{}, classify(err)
			}
			if associationRevision != in.ExpectedConversationRevision {
				return app.AdmissionResult{}, app.ErrConflict
			}
		}
	}
	if err := insertConversationAndAssociation(ctx, tx, in.Execution.AgentID, in.Execution.ConversationID, in.Execution.CreatedAt); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := insertExecution(ctx, tx, in.Execution); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := insertOperation(ctx, tx, in.Operation); err != nil {
		return app.AdmissionResult{}, err
	}
	if in.AgentID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET primary_execution_id=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, in.Execution.ID, nanos(in.Execution.CreatedAt), in.AgentID, in.Expected); err != nil {
			return app.AdmissionResult{}, err
		}
	}
	if err := bumpTx(ctx, tx); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return app.AdmissionResult{}, err
	}
	return app.AdmissionResult{Operation: in.Operation, Execution: in.Execution}, nil
}

func (s *Store) AdmitExecutionOperation(ctx context.Context, in app.ExecutionOperationAdmission) (app.AdmissionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if repeated, ok, err := admissionByRequest(ctx, tx, in.Operation, "", true); err != nil {
		return app.AdmissionResult{}, err
	} else if ok {
		_ = tx.Commit()
		return repeated, nil
	}
	execution, err := executionTx(ctx, tx, in.Operation.ExecutionID)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	if err := insertOperation(ctx, tx, in.Operation); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := bumpTx(ctx, tx); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return app.AdmissionResult{}, err
	}
	return app.AdmissionResult{Operation: in.Operation, Execution: execution}, nil
}

func (s *Store) RecordPrepared(ctx context.Context, executionID model.ExecutionID, operationID model.OperationID, evidence model.ProviderEvidence, at time.Time) (model.Execution, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Execution{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE executions SET state=?,evidence_provider=?,evidence_version=?,evidence_payload=?,revision=revision+1,updated_at=? WHERE id=? AND state=?`, model.ExecutionPrepared, evidence.Provider, evidence.Version, evidence.Payload, nanos(at), executionID, model.ExecutionReserved)
	if err != nil {
		return model.Execution{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.Execution{}, app.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO release_permits(execution_id,operation_id) VALUES(?,?)`, executionID, operationID); err != nil {
		return model.Execution{}, classify(err)
	}
	if err := bumpTx(ctx, tx); err != nil {
		return model.Execution{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Execution{}, err
	}
	return s.Execution(ctx, executionID)
}

func (s *Store) ConsumeRelease(ctx context.Context, executionID model.ExecutionID, operationID model.OperationID, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `UPDATE release_permits SET consumed_at=? WHERE execution_id=? AND operation_id=? AND consumed_at IS NULL AND EXISTS(SELECT 1 FROM executions WHERE id=? AND state=?) AND EXISTS(SELECT 1 FROM operations WHERE id=? AND state=?)`, nanos(at), executionID, operationID, executionID, model.ExecutionPrepared, operationID, model.OperationAdmitted)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return app.ErrConflict
	}
	return s.bump(ctx)
}

func (s *Store) CompleteOperation(ctx context.Context, in app.OperationCompletion) (app.AdmissionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := completeOperationTx(ctx, tx, in); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := bumpTx(ctx, tx); err != nil {
		return app.AdmissionResult{}, err
	}
	operation, err := operationTx(ctx, tx, in.OperationID)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	execution, err := executionTx(ctx, tx, in.ExecutionID)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return app.AdmissionResult{}, err
	}
	return app.AdmissionResult{Operation: operation, Execution: execution}, nil
}

func (s *Store) CompleteContextOperation(ctx context.Context, completion app.OperationCompletion, association app.ContextAssociation) (app.AdmissionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := completeOperationTx(ctx, tx, completion); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := associateConversationTx(ctx, tx, association); err != nil {
		return app.AdmissionResult{}, err
	}
	if err := bumpTx(ctx, tx); err != nil {
		return app.AdmissionResult{}, err
	}
	operation, err := operationTx(ctx, tx, completion.OperationID)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	execution, err := executionTx(ctx, tx, completion.ExecutionID)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return app.AdmissionResult{}, err
	}
	return app.AdmissionResult{Operation: operation, Execution: execution}, nil
}

func (s *Store) Execution(ctx context.Context, id model.ExecutionID) (model.Execution, error) {
	return scanExecution(s.db.QueryRowContext(ctx, executionSelect+` WHERE id=?`, id))
}

func (s *Store) RecoverableExecutions(ctx context.Context) ([]model.Execution, error) {
	rows, err := s.db.QueryContext(ctx, executionSelect+` WHERE state IN (?,?,?,?) ORDER BY created_at`, model.ExecutionPrepared, model.ExecutionReleased, model.ExecutionRunning, model.ExecutionUnknown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Execution
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, execution)
	}
	return out, rows.Err()
}

func (s *Store) RecordRecovery(ctx context.Context, id model.ExecutionID, state model.ExecutionState, native *model.NativeConversationEvidence, evidence model.ProviderEvidence, at time.Time) (model.Execution, error) {
	var namespace, reference string
	var observed any
	if native != nil {
		namespace, reference, observed = native.Namespace, native.Reference, nanos(native.ObservedAt)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE executions SET state=CASE WHEN state IN (?,?) THEN state ELSE ? END,evidence_provider=CASE WHEN state IN (?,?) OR ?='' THEN evidence_provider ELSE ? END,evidence_version=CASE WHEN state IN (?,?) OR ?='' THEN evidence_version ELSE ? END,evidence_payload=CASE WHEN state IN (?,?) OR ?='' THEN evidence_payload ELSE ? END,native_namespace=CASE WHEN state IN (?,?) OR ?='' THEN native_namespace ELSE ? END,native_reference=CASE WHEN state IN (?,?) OR ?='' THEN native_reference ELSE ? END,native_observed_at=CASE WHEN state IN (?,?) OR ?='' THEN native_observed_at ELSE ? END,revision=revision+1,updated_at=? WHERE id=?`, model.ExecutionExited, model.ExecutionFailed, state, model.ExecutionExited, model.ExecutionFailed, evidence.Provider, evidence.Provider, model.ExecutionExited, model.ExecutionFailed, evidence.Provider, evidence.Version, model.ExecutionExited, model.ExecutionFailed, evidence.Provider, evidence.Payload, model.ExecutionExited, model.ExecutionFailed, reference, namespace, model.ExecutionExited, model.ExecutionFailed, reference, reference, model.ExecutionExited, model.ExecutionFailed, reference, observed, nanos(at), id)
	if err != nil {
		return model.Execution{}, err
	}
	if err = s.bump(ctx); err != nil {
		return model.Execution{}, err
	}
	return s.Execution(ctx, id)
}

func (s *Store) Continuation(ctx context.Context, agentID model.AgentID, conversationID model.ConversationID, expected model.Revision) (app.ContinuationRecord, error) {
	var record app.ContinuationRecord
	if agentID != "" {
		var associated, replaced sql.NullInt64
		err := s.db.QueryRowContext(ctx, `SELECT agent_id,conversation_id,current,revision,associated_at,replaced_at FROM agent_conversations WHERE agent_id=? AND conversation_id=?`, agentID, conversationID).Scan(&record.Conversation.AgentID, &record.Conversation.ConversationID, &record.Conversation.Current, &record.Conversation.Revision, &associated, &replaced)
		if err != nil {
			return record, classify(err)
		}
		if record.Conversation.Revision != expected {
			return record, app.ErrConflict
		}
		record.Conversation.AssociatedAt = fromNanos(associated.Int64)
		if replaced.Valid {
			v := fromNanos(replaced.Int64)
			record.Conversation.ReplacedAt = &v
		}
	} else {
		var revision model.Revision
		if err := s.db.QueryRowContext(ctx, `SELECT revision FROM conversations WHERE id=?`, conversationID).Scan(&revision); err != nil {
			return record, classify(err)
		}
		if revision != expected {
			return record, app.ErrConflict
		}
	}
	var observed sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT native_namespace,native_reference,native_observed_at,evidence_provider,evidence_version,evidence_payload FROM executions WHERE conversation_id=? AND (?='' OR agent_id=?) AND native_reference<>'' ORDER BY updated_at DESC LIMIT 1`, conversationID, agentID, agentID).Scan(&record.Native.Namespace, &record.Native.Reference, &observed, &record.Evidence.Provider, &record.Evidence.Version, &record.Evidence.Payload)
	if err != nil {
		return record, classify(err)
	}
	record.Native.ObservedAt = fromNanos(observed.Int64)
	if agentID == "" {
		record.Conversation.ConversationID = conversationID
		record.Conversation.Revision = expected
	}
	return record, nil
}

func (s *Store) CurrentConversation(ctx context.Context, agentID model.AgentID) (model.ConversationAssociation, error) {
	var association model.ConversationAssociation
	var associated int64
	err := s.db.QueryRowContext(ctx, `SELECT agent_id,conversation_id,current,revision,associated_at FROM agent_conversations WHERE agent_id=? AND current=1`, agentID).Scan(&association.AgentID, &association.ConversationID, &association.Current, &association.Revision, &associated)
	if err != nil {
		return association, classify(err)
	}
	association.AssociatedAt = fromNanos(associated)
	return association, nil
}

func (s *Store) CreateMessage(ctx context.Context, message model.Message, requestID model.RequestID, operationID model.OperationID) (app.MessageAdmissionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.MessageAdmissionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if existing, ok, err := messageByRequest(ctx, tx, requestID, message); err != nil {
		return app.MessageAdmissionResult{}, err
	} else if ok {
		_ = tx.Commit()
		return existing, nil
	}
	op := model.Operation{ID: operationID, RequestID: requestID, Kind: model.OperationSendMessage, Principal: message.Sender, State: model.OperationSucceeded, ResultCode: "committed", Revision: 1, CreatedAt: message.CreatedAt, UpdatedAt: message.CreatedAt}
	if err := insertOperation(ctx, tx, op); err != nil {
		return app.MessageAdmissionResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,operation_id,sender_kind,sender_agent_id,body,created_at) VALUES(?,?,?,?,?,?)`, message.ID, operationID, message.Sender.Kind, message.Sender.AgentID, message.Body, nanos(message.CreatedAt)); err != nil {
		return app.MessageAdmissionResult{}, classify(err)
	}
	for _, recipient := range message.Recipients {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_recipients(id,message_id,agent_id,read_at,notified) VALUES(?,?,?,?,?)`, recipient.ID, message.ID, recipient.AgentID, nil, recipient.Notified); err != nil {
			return app.MessageAdmissionResult{}, classify(err)
		}
	}
	if err := bumpTx(ctx, tx); err != nil {
		return app.MessageAdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return app.MessageAdmissionResult{}, err
	}
	return app.MessageAdmissionResult{Operation: op, Message: message}, nil
}

func (s *Store) MarkMessageRead(ctx context.Context, messageID model.MessageID, agentID model.AgentID, at time.Time) (model.Message, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE message_recipients SET read_at=COALESCE(read_at,?) WHERE message_id=? AND agent_id=?`, nanos(at), messageID, agentID)
	if err != nil {
		return model.Message{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.Message{}, app.ErrNotFound
	}
	if err := s.bump(ctx); err != nil {
		return model.Message{}, err
	}
	return s.message(ctx, messageID)
}

func (s *Store) Snapshot(ctx context.Context) (app.Snapshot, error) {
	var snapshot app.Snapshot
	if err := s.db.QueryRowContext(ctx, `SELECT revision FROM backend_meta WHERE singleton=1`).Scan(&snapshot.Revision); err != nil {
		return snapshot, err
	}
	conversationRows, err := s.db.QueryContext(ctx, `SELECT id,revision,created_at,updated_at FROM conversations ORDER BY id`)
	if err != nil {
		return snapshot, err
	}
	for conversationRows.Next() {
		var conversation model.Conversation
		var created, updated int64
		if err := conversationRows.Scan(&conversation.ID, &conversation.Revision, &created, &updated); err != nil {
			conversationRows.Close()
			return snapshot, err
		}
		conversation.CreatedAt, conversation.UpdatedAt = fromNanos(created), fromNanos(updated)
		snapshot.Conversations = append(snapshot.Conversations, conversation)
	}
	if err := conversationRows.Err(); err != nil {
		conversationRows.Close()
		return snapshot, err
	}
	conversationRows.Close()
	associationRows, err := s.db.QueryContext(ctx, `SELECT agent_id,conversation_id,current,revision,associated_at,replaced_at FROM agent_conversations ORDER BY agent_id,associated_at`)
	if err != nil {
		return snapshot, err
	}
	for associationRows.Next() {
		var association model.ConversationAssociation
		var associated int64
		var replaced sql.NullInt64
		if err := associationRows.Scan(&association.AgentID, &association.ConversationID, &association.Current, &association.Revision, &associated, &replaced); err != nil {
			associationRows.Close()
			return snapshot, err
		}
		association.AssociatedAt = fromNanos(associated)
		if replaced.Valid {
			value := fromNanos(replaced.Int64)
			association.ReplacedAt = &value
		}
		snapshot.Associations = append(snapshot.Associations, association)
	}
	if err := associationRows.Err(); err != nil {
		associationRows.Close()
		return snapshot, err
	}
	associationRows.Close()
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,harness,model,working_directory,approval,sandbox,primary_execution_id,revision,created_at,updated_at FROM agents ORDER BY id`)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		agent, err := scanAgent(rows)
		if err != nil {
			rows.Close()
			return snapshot, err
		}
		snapshot.Agents = append(snapshot.Agents, agent)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT id FROM groups ORDER BY id`)
	if err != nil {
		return snapshot, err
	}
	var groupIDs []model.GroupID
	for rows.Next() {
		var id model.GroupID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return snapshot, err
		}
		groupIDs = append(groupIDs, id)
	}
	rows.Close()
	for _, id := range groupIDs {
		group, err := s.Group(ctx, id)
		if err != nil {
			return snapshot, err
		}
		snapshot.Groups = append(snapshot.Groups, group)
	}
	rows, err = s.db.QueryContext(ctx, executionSelect+` ORDER BY created_at`)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		execution, err := scanExecution(rows)
		if err != nil {
			rows.Close()
			return snapshot, err
		}
		snapshot.Executions = append(snapshot.Executions, execution)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, operationSelect+` ORDER BY created_at`)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		operation, err := scanOperation(rows)
		if err != nil {
			rows.Close()
			return snapshot, err
		}
		snapshot.Operations = append(snapshot.Operations, operation)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT id FROM messages ORDER BY created_at`)
	if err != nil {
		return snapshot, err
	}
	var messageIDs []model.MessageID
	for rows.Next() {
		var id model.MessageID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return snapshot, err
		}
		messageIDs = append(messageIDs, id)
	}
	rows.Close()
	for _, id := range messageIDs {
		message, err := s.message(ctx, id)
		if err != nil {
			return snapshot, err
		}
		snapshot.Messages = append(snapshot.Messages, message)
	}
	return snapshot, nil
}

func (s *Store) AssociateConversation(ctx context.Context, in app.ContextAssociation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := associateConversationTx(ctx, tx, in); err != nil {
		return err
	}
	if err := bumpTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func associateConversationTx(ctx context.Context, tx *sql.Tx, in app.ContextAssociation) error {
	var currentID model.ConversationID
	var currentRevision model.Revision
	err := tx.QueryRowContext(ctx, `SELECT conversation_id,revision FROM agent_conversations WHERE agent_id=? AND current=1`, in.AgentID).Scan(&currentID, &currentRevision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if in.ExpectedRevision != currentRevision {
		return app.ErrConflict
	}
	if currentID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE agent_conversations SET current=0,replaced_at=? WHERE agent_id=? AND conversation_id=?`, nanos(in.At), in.AgentID, currentID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversations(id,revision,created_at,updated_at) VALUES(?,1,?,?)`, in.ConversationID, nanos(in.At), nanos(in.At)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_conversations(agent_id,conversation_id,current,revision,associated_at) VALUES(?,?,1,?,?) ON CONFLICT(agent_id,conversation_id) DO UPDATE SET current=1,revision=excluded.revision,associated_at=excluded.associated_at,replaced_at=NULL`, in.AgentID, in.ConversationID, currentRevision+1, nanos(in.At)); err != nil {
		return err
	}
	if in.ExecutionID != "" {
		var namespace, reference string
		var observed any
		if in.Native != nil {
			namespace, reference, observed = in.Native.Namespace, in.Native.Reference, nanos(in.Native.ObservedAt)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE executions SET conversation_id=?,native_namespace=?,native_reference=?,native_observed_at=?,revision=revision+1,updated_at=? WHERE id=? AND agent_id=?`, in.ConversationID, namespace, reference, observed, nanos(in.At), in.ExecutionID, in.AgentID); err != nil {
			return err
		}
	}
	return nil
}

func completeOperationTx(ctx context.Context, tx *sql.Tx, in app.OperationCompletion) error {
	result, err := tx.ExecContext(ctx, `UPDATE operations SET state=?,result_code=?,detail=?,revision=revision+1,updated_at=? WHERE id=? AND state=?`, in.OperationState, in.ResultCode, in.Detail, nanos(in.At), in.OperationID, model.OperationAdmitted)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return app.ErrConflict
	}
	var namespace, reference string
	var observed any
	if in.Native != nil {
		namespace, reference, observed = in.Native.Namespace, in.Native.Reference, nanos(in.Native.ObservedAt)
	}
	_, err = tx.ExecContext(ctx, `UPDATE executions SET state=CASE WHEN ? THEN ? ELSE state END,evidence_provider=CASE WHEN ?='' THEN evidence_provider ELSE ? END,evidence_version=CASE WHEN ?='' THEN evidence_version ELSE ? END,evidence_payload=CASE WHEN ?='' THEN evidence_payload ELSE ? END,native_namespace=CASE WHEN ?='' THEN native_namespace ELSE ? END,native_reference=CASE WHEN ?='' THEN native_reference ELSE ? END,native_observed_at=CASE WHEN ?='' THEN native_observed_at ELSE ? END,revision=revision+1,updated_at=? WHERE id=?`, in.UpdateExecutionState, in.ExecutionState, in.Evidence.Provider, in.Evidence.Provider, in.Evidence.Provider, in.Evidence.Version, in.Evidence.Provider, in.Evidence.Payload, reference, namespace, reference, reference, reference, observed, nanos(in.At), in.ExecutionID)
	return err
}

const executionSelect = `SELECT id,agent_id,conversation_id,harness,model,working_directory,approval,sandbox,state,evidence_provider,evidence_version,evidence_payload,native_namespace,native_reference,native_observed_at,revision,created_at,updated_at FROM executions`
const operationSelect = `SELECT id,request_id,kind,principal_kind,principal_agent_id,execution_id,state,result_code,detail,revision,created_at,updated_at FROM operations`

type scanner interface{ Scan(...any) error }

func scanAgent(row scanner) (model.Agent, error) {
	var a model.Agent
	var created, updated int64
	err := row.Scan(&a.ID, &a.Name, &a.Desired.Harness, &a.Desired.Model, &a.Desired.WorkingDirectory, &a.Desired.Approval, &a.Desired.Sandbox, &a.PrimaryExecutionID, &a.Revision, &created, &updated)
	if err != nil {
		return a, classify(err)
	}
	a.CreatedAt, a.UpdatedAt = fromNanos(created), fromNanos(updated)
	return a, nil
}
func scanExecution(row scanner) (model.Execution, error) {
	var e model.Execution
	var observed sql.NullInt64
	var namespace, reference string
	var created, updated int64
	err := row.Scan(&e.ID, &e.AgentID, &e.ConversationID, &e.Spec.Harness, &e.Spec.Model, &e.Spec.WorkingDirectory, &e.Spec.Approval, &e.Spec.Sandbox, &e.State, &e.Evidence.Provider, &e.Evidence.Version, &e.Evidence.Payload, &namespace, &reference, &observed, &e.Revision, &created, &updated)
	if err != nil {
		return e, classify(err)
	}
	e.Spec.ExecutionID, e.Spec.AgentID, e.Spec.ConversationID = e.ID, e.AgentID, e.ConversationID
	e.CreatedAt, e.UpdatedAt = fromNanos(created), fromNanos(updated)
	if reference != "" {
		e.NativeConversation = &model.NativeConversationEvidence{Namespace: namespace, Reference: reference, ObservedAt: fromNanos(observed.Int64)}
	}
	return e, nil
}
func scanOperation(row scanner) (model.Operation, error) {
	var o model.Operation
	var created, updated int64
	err := row.Scan(&o.ID, &o.RequestID, &o.Kind, &o.Principal.Kind, &o.Principal.AgentID, &o.ExecutionID, &o.State, &o.ResultCode, &o.Detail, &o.Revision, &created, &updated)
	if err != nil {
		return o, classify(err)
	}
	o.CreatedAt, o.UpdatedAt = fromNanos(created), fromNanos(updated)
	return o, nil
}

func insertExecution(ctx context.Context, tx *sql.Tx, e model.Execution) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO executions(id,agent_id,conversation_id,harness,model,working_directory,approval,sandbox,state,evidence_provider,evidence_version,evidence_payload,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, e.ID, e.AgentID, e.ConversationID, e.Spec.Harness, e.Spec.Model, e.Spec.WorkingDirectory, e.Spec.Approval, e.Spec.Sandbox, e.State, e.Evidence.Provider, e.Evidence.Version, e.Evidence.Payload, e.Revision, nanos(e.CreatedAt), nanos(e.UpdatedAt))
	return classify(err)
}
func insertOperation(ctx context.Context, tx *sql.Tx, o model.Operation) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO operations(id,request_id,kind,principal_kind,principal_agent_id,execution_id,state,result_code,detail,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, o.ID, o.RequestID, o.Kind, o.Principal.Kind, o.Principal.AgentID, o.ExecutionID, o.State, o.ResultCode, o.Detail, o.Revision, nanos(o.CreatedAt), nanos(o.UpdatedAt))
	return classify(err)
}
func insertConversationAndAssociation(ctx context.Context, tx *sql.Tx, agentID model.AgentID, conversationID model.ConversationID, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO conversations(id,revision,created_at,updated_at) VALUES(?,1,?,?)`, conversationID, nanos(at), nanos(at)); err != nil {
		return err
	}
	if agentID == "" {
		return nil
	}
	var current model.ConversationID
	var revision model.Revision
	err := tx.QueryRowContext(ctx, `SELECT conversation_id,revision FROM agent_conversations WHERE agent_id=? AND current=1`, agentID).Scan(&current, &revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if current == conversationID {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE agent_conversations SET current=0,replaced_at=? WHERE agent_id=? AND current=1`, nanos(at), agentID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_conversations(agent_id,conversation_id,current,revision,associated_at) VALUES(?,?,1,?,?) ON CONFLICT(agent_id,conversation_id) DO UPDATE SET current=1,revision=excluded.revision,associated_at=excluded.associated_at,replaced_at=NULL`, agentID, conversationID, revision+1, nanos(at))
	return err
}
func admissionByRequest(ctx context.Context, tx *sql.Tx, desired model.Operation, targetAgent model.AgentID, strictExecution bool) (app.AdmissionResult, bool, error) {
	operation, err := operationByRequestTx(ctx, tx, desired.RequestID)
	if errors.Is(err, app.ErrNotFound) {
		return app.AdmissionResult{}, false, nil
	}
	if err != nil {
		return app.AdmissionResult{}, false, err
	}
	if operation.Kind != desired.Kind || operation.Principal != desired.Principal || (strictExecution && operation.ExecutionID != desired.ExecutionID) {
		return app.AdmissionResult{}, false, app.ErrConflict
	}
	var execution model.Execution
	if operation.ExecutionID != "" {
		execution, err = executionTx(ctx, tx, operation.ExecutionID)
		if err != nil {
			return app.AdmissionResult{}, false, err
		}
	}
	if !strictExecution && execution.AgentID != targetAgent {
		return app.AdmissionResult{}, false, app.ErrConflict
	}
	return app.AdmissionResult{Operation: operation, Execution: execution, Repeated: true}, true, nil
}
func operationByRequestTx(ctx context.Context, tx *sql.Tx, id model.RequestID) (model.Operation, error) {
	return scanOperation(tx.QueryRowContext(ctx, operationSelect+` WHERE request_id=?`, id))
}
func operationTx(ctx context.Context, tx *sql.Tx, id model.OperationID) (model.Operation, error) {
	return scanOperation(tx.QueryRowContext(ctx, operationSelect+` WHERE id=?`, id))
}
func executionTx(ctx context.Context, tx *sql.Tx, id model.ExecutionID) (model.Execution, error) {
	return scanExecution(tx.QueryRowContext(ctx, executionSelect+` WHERE id=?`, id))
}

func (s *Store) message(ctx context.Context, id model.MessageID) (model.Message, error) {
	var m model.Message
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT id,sender_kind,sender_agent_id,body,created_at FROM messages WHERE id=?`, id).Scan(&m.ID, &m.Sender.Kind, &m.Sender.AgentID, &m.Body, &created)
	if err != nil {
		return m, classify(err)
	}
	m.CreatedAt = fromNanos(created)
	rows, err := s.db.QueryContext(ctx, `SELECT id,agent_id,read_at,notified FROM message_recipients WHERE message_id=? ORDER BY rowid`, id)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var recipient model.MessageRecipient
		var read sql.NullInt64
		if err := rows.Scan(&recipient.ID, &recipient.AgentID, &read, &recipient.Notified); err != nil {
			return m, err
		}
		if read.Valid {
			v := fromNanos(read.Int64)
			recipient.ReadAt = &v
		}
		m.Recipients = append(m.Recipients, recipient)
	}
	return m, rows.Err()
}
func messageByRequest(ctx context.Context, tx *sql.Tx, requestID model.RequestID, desired model.Message) (app.MessageAdmissionResult, bool, error) {
	operation, err := operationByRequestTx(ctx, tx, requestID)
	if errors.Is(err, app.ErrNotFound) {
		return app.MessageAdmissionResult{}, false, nil
	}
	if err != nil {
		return app.MessageAdmissionResult{}, false, err
	}
	if operation.Kind != model.OperationSendMessage || operation.Principal != desired.Sender {
		return app.MessageAdmissionResult{}, false, app.ErrConflict
	}
	var messageID model.MessageID
	if err := tx.QueryRowContext(ctx, `SELECT id FROM messages WHERE operation_id=?`, operation.ID).Scan(&messageID); err != nil {
		return app.MessageAdmissionResult{}, false, classify(err)
	}
	var m model.Message
	var created int64
	if err := tx.QueryRowContext(ctx, `SELECT id,sender_kind,sender_agent_id,body,created_at FROM messages WHERE id=?`, messageID).Scan(&m.ID, &m.Sender.Kind, &m.Sender.AgentID, &m.Body, &created); err != nil {
		return app.MessageAdmissionResult{}, false, classify(err)
	}
	m.CreatedAt = fromNanos(created)
	if m.Body != desired.Body {
		return app.MessageAdmissionResult{}, false, app.ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,agent_id,read_at,notified FROM message_recipients WHERE message_id=? ORDER BY rowid`, messageID)
	if err != nil {
		return app.MessageAdmissionResult{}, false, err
	}
	defer rows.Close()
	for rows.Next() {
		var r model.MessageRecipient
		var read sql.NullInt64
		if err := rows.Scan(&r.ID, &r.AgentID, &read, &r.Notified); err != nil {
			return app.MessageAdmissionResult{}, false, err
		}
		if read.Valid {
			v := fromNanos(read.Int64)
			r.ReadAt = &v
		}
		m.Recipients = append(m.Recipients, r)
	}
	if err := rows.Err(); err != nil {
		return app.MessageAdmissionResult{}, false, err
	}
	if len(m.Recipients) != len(desired.Recipients) {
		return app.MessageAdmissionResult{}, false, app.ErrConflict
	}
	for index := range m.Recipients {
		if m.Recipients[index].AgentID != desired.Recipients[index].AgentID {
			return app.MessageAdmissionResult{}, false, app.ErrConflict
		}
	}
	return app.MessageAdmissionResult{Operation: operation, Message: m, Repeated: true}, true, nil
}

func (s *Store) bump(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backend_meta SET revision=revision+1 WHERE singleton=1`)
	return err
}
func bumpTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE backend_meta SET revision=revision+1 WHERE singleton=1`)
	return err
}
func nanos(value time.Time) int64 { return value.UTC().UnixNano() }
func fromNanos(value int64) time.Time {
	if value == 0 {
		return time.Time{}
	}
	return time.Unix(0, value).UTC()
}
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return app.ErrNotFound
	}
	return err
}
