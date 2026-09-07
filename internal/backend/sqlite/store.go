package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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
	if err := s.initializeMessageNotifications(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, configurationCatalogSchema); err != nil {
		return err
	}
	if err := s.initializeUsageActivity(ctx); err != nil {
		return fmt.Errorf("initialize usage and activity schema: %w", err)
	}
	if err := s.initializeAccessRequests(ctx); err != nil {
		return fmt.Errorf("initialize access request schema: %w", err)
	}
	for _, migration := range []struct{ table, column, definition string }{
		{"definition_revisions", "editor_layout_json", "BLOB"},
		{"agents", "configuration_profile_json", "BLOB"},
		{"executions", "configuration_profile_json", "BLOB"},
		{"groups", "owner_agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "lifecycle_state", "TEXT NOT NULL DEFAULT 'active'"},
		{"agents", "task_reference", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "parent_agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "clone_source_agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "retired_at", "INTEGER"},
		{"agents", "retired_by_kind", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "retired_by_agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "retired_by_execution_id", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "retirement_reason", "TEXT NOT NULL DEFAULT ''"},
		{"agents", "direct_notification_intent", "TEXT NOT NULL DEFAULT 'if_available'"},
		{"executions", "attempt_generation", "INTEGER NOT NULL DEFAULT 1"},
		{"executions", "context_readiness", "TEXT NOT NULL DEFAULT 'pending'"},
		{"executions", "context_provider_order", "TEXT NOT NULL DEFAULT ''"},
		{"executions", "workload_kind", "TEXT NOT NULL DEFAULT 'harness'"},
		{"executions", "shell_evidence_owner", "TEXT NOT NULL DEFAULT ''"},
		{"executions", "shell_evidence_version", "INTEGER NOT NULL DEFAULT 0"},
		{"executions", "shell_evidence_payload", "BLOB"},
		{"history_catalog", "source_name", "TEXT NOT NULL DEFAULT ''"},
		{"work_evidence", "request_scope", "TEXT NOT NULL DEFAULT ''"},
		{"work_evidence", "request_id", "TEXT NOT NULL DEFAULT ''"},
		{"work_decisions", "request_scope", "TEXT NOT NULL DEFAULT ''"},
		{"work_decisions", "request_id", "TEXT NOT NULL DEFAULT ''"},
		{"work_runs", "cancellation_requested", "INTEGER NOT NULL DEFAULT 0"},
		{"work_runs", "cancellation_reason", "TEXT NOT NULL DEFAULT ''"},
		{"work_runs", "cancel_request_scope", "TEXT NOT NULL DEFAULT ''"},
		{"work_runs", "cancel_request_id", "TEXT NOT NULL DEFAULT ''"},
		{"work_runs", "graph_json", "BLOB"},
		{"work_runs", "definition_closure_json", "BLOB"},
		{"work_runs", "parameters_json", "BLOB"},
		{"work_runs", "scope_json", "BLOB"},
		{"work_runs", "authorized_programs_json", "BLOB"},
		{"work_runs", "control_state", "TEXT NOT NULL DEFAULT ''"},
		{"work_runs", "outcome", "TEXT NOT NULL DEFAULT ''"},
		{"work_runs", "deadline", "INTEGER"},
		{"automation_occurrences", "request_fingerprint", "TEXT NOT NULL DEFAULT ''"},
		{"operations", "principal_execution_id", "TEXT NOT NULL DEFAULT ''"},
		{"operations", "request_scope", "TEXT NOT NULL DEFAULT 'operator'"},
		{"operations", "principal_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"operations", "principal_automation_run", "TEXT NOT NULL DEFAULT ''"},
		{"operations", "automation_delegation_json", "BLOB"},
		{"operations", "authority_subject_kind", "TEXT NOT NULL DEFAULT ''"},
		{"operations", "authority_subject_id", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "sender_execution_id", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "sender_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"messages", "sender_automation_run", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "sender_authority_subject_kind", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "sender_authority_subject_id", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "sender_conversation_id", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "subject", "TEXT NOT NULL DEFAULT 'Message'"},
		{"messages", "parent_message_id", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "thread_id", "TEXT NOT NULL DEFAULT ''"},
		{"messages", "request_digest", "TEXT NOT NULL DEFAULT ''"},
		{"message_recipients", "address_kind", "TEXT NOT NULL DEFAULT 'agent'"},
		{"message_recipients", "audience_kind", "TEXT NOT NULL DEFAULT 'to'"},
		{"message_recipients", "notification_intent", "TEXT NOT NULL DEFAULT 'none'"},
		{"message_recipients", "notification_outcome", "TEXT NOT NULL DEFAULT 'not_requested'"},
		{"message_recipients", "notification_detail", "TEXT NOT NULL DEFAULT ''"},
		{"message_recipients", "notified_at", "INTEGER"},
		{"automation_occurrences", "parent_occurrence_id", "TEXT NOT NULL DEFAULT ''"},
		{"automation_occurrences", "causal_depth", "INTEGER NOT NULL DEFAULT 0"},
		{"decision_submissions", "expected_run_revision", "INTEGER NOT NULL DEFAULT 0"},
		{"effect_permits", "eligibility_audience_json", "BLOB"},
		{"effect_permits", "eligibility_agent_id", "TEXT NOT NULL DEFAULT ''"},
		{"team_deployments", "role_pins_json", "BLOB NOT NULL DEFAULT '[]'"},
		{"team_deployments", "target_kind", "TEXT NOT NULL DEFAULT 'new_group'"},
		{"team_deployments", "workspaces_json", "BLOB NOT NULL DEFAULT '{}'"},
		{"team_deployments", "owned_workspace_ids_json", "BLOB NOT NULL DEFAULT '[]'"},
		{"team_deployments", "owned_automation_rule_ids_json", "BLOB NOT NULL DEFAULT '[]'"},
		{"team_deployments", "briefing_operation_ids_json", "BLOB NOT NULL DEFAULT '{}'"},
		{"team_deployments", "request_scope", "TEXT NOT NULL DEFAULT ''"},
		{"team_deployments", "request_id", "TEXT NOT NULL DEFAULT ''"},
		{"team_deployments", "request_digest", "TEXT NOT NULL DEFAULT ''"},
		{"team_deployments", "requester_json", "BLOB"},
		{"automation_rules", "deployment_id", "TEXT NOT NULL DEFAULT ''"},
	} {
		if err := s.ensureColumn(ctx, migration.table, migration.column, migration.definition); err != nil {
			return err
		}
	}
	if err := s.migrateMessageRecipients(ctx); err != nil {
		return err
	}
	if err := s.migrateOperationRequestScope(ctx); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS operations_scoped_request ON operations(request_scope,request_id)`); err != nil {
		return fmt.Errorf("index scoped operation requests: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS work_evidence_scoped_request ON work_evidence(request_scope,request_id) WHERE request_id<>''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS work_decisions_scoped_request ON work_decisions(request_scope,request_id) WHERE request_id<>''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS team_deployments_scoped_request ON team_deployments(request_scope,request_id) WHERE request_id<>''`); err != nil {
		return fmt.Errorf("index scoped team deployment requests: %w", err)
	}
	// Access is always suspended across a backend process boundary. Recovery is
	// the only workflow that can reactivate the exact proven runtime.
	if _, err := s.db.ExecContext(ctx, `UPDATE execution_accesses SET state=? WHERE state=?`, model.ExecutionAccessSuspended, model.ExecutionAccessActive); err != nil {
		return fmt.Errorf("suspend execution access at startup: %w", err)
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, definition string) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		found = found || name == column
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE `+table+` ADD COLUMN `+column+` `+definition); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

func (s *Store) migrateOperationRequestScope(ctx context.Context) error {
	var ddl string
	if err := s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='operations'`).Scan(&ddl); err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(ddl), "REQUEST_ID TEXT NOT NULL UNIQUE") {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		return err
	}
	defer func() { _, _ = s.db.ExecContext(context.Background(), `PRAGMA foreign_keys = ON`) }()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `UPDATE operations SET request_scope=CASE principal_kind WHEN 'execution' THEN 'execution:'||principal_execution_id WHEN 'automation' THEN 'automation:'||principal_automation_run WHEN 'agent' THEN 'agent:'||principal_agent_id ELSE 'operator' END`); err != nil {
		return fmt.Errorf("scope existing operations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `CREATE TABLE operations_replacement (
 id TEXT PRIMARY KEY, request_id TEXT NOT NULL, request_scope TEXT NOT NULL, kind TEXT NOT NULL,
 principal_kind TEXT NOT NULL, principal_agent_id TEXT NOT NULL DEFAULT '',
 principal_execution_id TEXT NOT NULL DEFAULT '', principal_generation INTEGER NOT NULL DEFAULT 0,
 principal_automation_run TEXT NOT NULL DEFAULT '', automation_delegation_json BLOB,
 authority_subject_kind TEXT NOT NULL DEFAULT '',
 authority_subject_id TEXT NOT NULL DEFAULT '', execution_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
 result_code TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL,
 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
)`); err != nil {
		return fmt.Errorf("create scoped operations replacement: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO operations_replacement(id,request_id,request_scope,kind,principal_kind,principal_agent_id,principal_execution_id,principal_generation,principal_automation_run,automation_delegation_json,authority_subject_kind,authority_subject_id,execution_id,state,result_code,detail,revision,created_at,updated_at)
	 SELECT id,request_id,request_scope,kind,principal_kind,principal_agent_id,principal_execution_id,principal_generation,principal_automation_run,automation_delegation_json,authority_subject_kind,authority_subject_id,execution_id,state,result_code,detail,revision,created_at,updated_at FROM operations`); err != nil {
		return fmt.Errorf("copy scoped operations: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE operations`); err != nil {
		return fmt.Errorf("replace operations table: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE operations_replacement RENAME TO operations`); err != nil {
		return fmt.Errorf("rename scoped operations table: %w", err)
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE IF NOT EXISTS backend_meta (
  singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
  revision INTEGER NOT NULL
);
INSERT OR IGNORE INTO backend_meta(singleton, revision) VALUES (1, 0);

CREATE TABLE IF NOT EXISTS agents (
  id TEXT PRIMARY KEY, name TEXT NOT NULL,
	 task_reference TEXT NOT NULL DEFAULT '', parent_agent_id TEXT NOT NULL DEFAULT '',
	 clone_source_agent_id TEXT NOT NULL DEFAULT '', lifecycle_state TEXT NOT NULL DEFAULT 'active',
	 retired_at INTEGER, retired_by_kind TEXT NOT NULL DEFAULT '', retired_by_agent_id TEXT NOT NULL DEFAULT '',
	 retired_by_execution_id TEXT NOT NULL DEFAULT '', retirement_reason TEXT NOT NULL DEFAULT '',
	 direct_notification_intent TEXT NOT NULL DEFAULT 'if_available',
  harness TEXT NOT NULL, model TEXT NOT NULL, working_directory TEXT NOT NULL,
  approval TEXT NOT NULL, sandbox TEXT NOT NULL,
  primary_execution_id TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS groups (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, owner_agent_id TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL,
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
  workload_kind TEXT NOT NULL DEFAULT 'harness',
  harness TEXT NOT NULL, model TEXT NOT NULL, working_directory TEXT NOT NULL,
  approval TEXT NOT NULL, sandbox TEXT NOT NULL, state TEXT NOT NULL,
  attempt_generation INTEGER NOT NULL DEFAULT 1,
  context_readiness TEXT NOT NULL DEFAULT 'pending',
  context_provider_order TEXT NOT NULL DEFAULT '',
  evidence_provider TEXT NOT NULL DEFAULT '', evidence_version INTEGER NOT NULL DEFAULT 0,
  evidence_payload BLOB,
  shell_evidence_owner TEXT NOT NULL DEFAULT '', shell_evidence_version INTEGER NOT NULL DEFAULT 0,
  shell_evidence_payload BLOB,
  native_namespace TEXT NOT NULL DEFAULT '', native_reference TEXT NOT NULL DEFAULT '', native_observed_at INTEGER,
  revision INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS executions_conversation ON executions(conversation_id, created_at);
CREATE TABLE IF NOT EXISTS operations (
  id TEXT PRIMARY KEY, request_id TEXT NOT NULL, request_scope TEXT NOT NULL, kind TEXT NOT NULL,
  principal_kind TEXT NOT NULL, principal_agent_id TEXT NOT NULL DEFAULT '',
  principal_execution_id TEXT NOT NULL DEFAULT '', principal_generation INTEGER NOT NULL DEFAULT 0,
  principal_automation_run TEXT NOT NULL DEFAULT '', automation_delegation_json BLOB,
  authority_subject_kind TEXT NOT NULL DEFAULT '',
  authority_subject_id TEXT NOT NULL DEFAULT '',
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
  sender_execution_id TEXT NOT NULL DEFAULT '', sender_generation INTEGER NOT NULL DEFAULT 0,
  sender_automation_run TEXT NOT NULL DEFAULT '',
  sender_authority_subject_kind TEXT NOT NULL DEFAULT '',
  sender_authority_subject_id TEXT NOT NULL DEFAULT '',
	 sender_conversation_id TEXT NOT NULL DEFAULT '', subject TEXT NOT NULL,
	 parent_message_id TEXT NOT NULL DEFAULT '', thread_id TEXT NOT NULL,
	 request_digest TEXT NOT NULL, body TEXT NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS message_recipients (
  id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
	 address_kind TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '', audience_kind TEXT NOT NULL,
	 read_at INTEGER, notification_intent TEXT NOT NULL, notification_outcome TEXT NOT NULL,
	 notification_detail TEXT NOT NULL DEFAULT '', notified_at INTEGER,
	 UNIQUE(message_id, address_kind, agent_id)
);
CREATE TABLE IF NOT EXISTS attachments (
  id TEXT PRIMARY KEY, owner_kind TEXT NOT NULL, owner_agent_id TEXT NOT NULL DEFAULT '',
	 owner_execution_id TEXT NOT NULL DEFAULT '', owner_generation INTEGER NOT NULL DEFAULT 0,
	 owner_automation_run TEXT NOT NULL DEFAULT '',
  filename TEXT NOT NULL, media_type TEXT NOT NULL, size INTEGER NOT NULL, sha256 TEXT NOT NULL,
  content BLOB NOT NULL, created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS attachment_claims (
  id TEXT PRIMARY KEY, attachment_id TEXT NOT NULL UNIQUE REFERENCES attachments(id) ON DELETE CASCADE,
  expires_at INTEGER NOT NULL, consumed_message_id TEXT REFERENCES messages(id), created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS message_attachments (
  message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
  attachment_id TEXT NOT NULL UNIQUE REFERENCES attachments(id), position INTEGER NOT NULL,
  PRIMARY KEY(message_id, attachment_id)
);

CREATE TABLE IF NOT EXISTS execution_accesses (
  execution_id TEXT PRIMARY KEY REFERENCES executions(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL DEFAULT '', generation INTEGER NOT NULL,
  credential_digest BLOB NOT NULL UNIQUE, delivery_id TEXT NOT NULL DEFAULT '',
  file_identity TEXT NOT NULL DEFAULT '', state TEXT NOT NULL,
  issued_at INTEGER NOT NULL, expires_at INTEGER NOT NULL, revoked_at INTEGER,
  revision INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS authority_grants (
  id TEXT PRIMARY KEY, subject_kind TEXT NOT NULL, subject_id TEXT NOT NULL,
  action TEXT NOT NULL, resource_kind TEXT NOT NULL, resource_id TEXT NOT NULL DEFAULT '',
  bounds_json BLOB NOT NULL, expires_at INTEGER, revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS authority_grants_subject ON authority_grants(subject_kind, subject_id);
CREATE TABLE IF NOT EXISTS roles (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, actions_json BLOB NOT NULL,
  revision INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS role_assignments (
  role_id TEXT NOT NULL REFERENCES roles(id), subject_kind TEXT NOT NULL, subject_id TEXT NOT NULL,
  resource_kind TEXT NOT NULL, resource_id TEXT NOT NULL DEFAULT '', bounds_json BLOB NOT NULL,
  revision INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  PRIMARY KEY(role_id, subject_kind, subject_id, resource_kind, resource_id)
);
CREATE TABLE IF NOT EXISTS operation_authority (
  operation_id TEXT PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
  action TEXT NOT NULL, resource_kind TEXT NOT NULL, resource_id TEXT NOT NULL DEFAULT '',
  requested_configuration_json BLOB, admitted_source_kind TEXT NOT NULL DEFAULT '',
  admitted_source_id TEXT NOT NULL DEFAULT '', admitted_revision INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS operation_additional_authority (
  operation_id TEXT NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
  position INTEGER NOT NULL, action TEXT NOT NULL,
  resource_kind TEXT NOT NULL, resource_id TEXT NOT NULL DEFAULT '',
  requested_configuration_json BLOB, admitted_source_kind TEXT NOT NULL DEFAULT '',
  admitted_source_id TEXT NOT NULL DEFAULT '', admitted_revision INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY(operation_id, position)
);
CREATE TABLE IF NOT EXISTS effect_permits (
  operation_id TEXT PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
	eligibility_audience_json BLOB, eligibility_agent_id TEXT NOT NULL DEFAULT '',
  consumed_at INTEGER
);
CREATE TABLE IF NOT EXISTS pending_context_transitions (
  operation_id TEXT PRIMARY KEY REFERENCES operations(id) ON DELETE CASCADE,
  execution_id TEXT NOT NULL REFERENCES executions(id), expected_conversation_id TEXT NOT NULL,
  expected_association_revision INTEGER NOT NULL, correlation TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS native_binding_history (
  execution_id TEXT NOT NULL REFERENCES executions(id), conversation_id TEXT NOT NULL,
  attempt_generation INTEGER NOT NULL, provider TEXT NOT NULL, disposition TEXT NOT NULL,
  prior_namespace TEXT NOT NULL DEFAULT '', prior_reference TEXT NOT NULL DEFAULT '',
  next_namespace TEXT NOT NULL DEFAULT '', next_reference TEXT NOT NULL DEFAULT '',
  primary_correlation TEXT NOT NULL, transition_correlation TEXT NOT NULL DEFAULT '',
  prior_provider_order TEXT NOT NULL DEFAULT '', provider_order TEXT NOT NULL,
  observed_at INTEGER NOT NULL, recorded_at INTEGER NOT NULL,
  PRIMARY KEY(execution_id, attempt_generation, provider, provider_order)
);
CREATE TABLE IF NOT EXISTS history_catalog (
  conversation_id TEXT PRIMARY KEY REFERENCES conversations(id), harness TEXT NOT NULL, source_name TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '', workspace_id TEXT NOT NULL DEFAULT '', workspace_hint TEXT NOT NULL DEFAULT '',
  archived INTEGER NOT NULL DEFAULT 0, availability TEXT NOT NULL,
  metadata_coverage TEXT NOT NULL, content_coverage TEXT NOT NULL,
  source_revision TEXT NOT NULL, refreshed_at INTEGER NOT NULL, modified_at INTEGER NOT NULL,
  native_namespace TEXT NOT NULL, native_reference TEXT NOT NULL, native_observed_at INTEGER NOT NULL,
  source_token TEXT NOT NULL, source_fingerprint TEXT NOT NULL, evidence_provider TEXT NOT NULL, evidence_version INTEGER NOT NULL,
  evidence_payload BLOB, search_text TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL,
  UNIQUE(harness,native_namespace,native_reference)
);
CREATE TABLE IF NOT EXISTS history_refreshes (
  harness TEXT NOT NULL, source_name TEXT NOT NULL, metadata_coverage TEXT NOT NULL,
  content_coverage TEXT NOT NULL, source_revision TEXT NOT NULL, refreshed_at INTEGER NOT NULL,
  PRIMARY KEY(harness,source_name)
);
CREATE TABLE IF NOT EXISTS history_metadata_requests (
  request_scope TEXT NOT NULL, request_id TEXT NOT NULL, conversation_id TEXT NOT NULL,
  title TEXT NOT NULL, archived INTEGER NOT NULL, PRIMARY KEY(request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS history_points (
  id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id) ON DELETE CASCADE,
  kind TEXT NOT NULL, provider_token TEXT NOT NULL, occurred_at INTEGER NOT NULL, revision INTEGER NOT NULL,
  UNIQUE(conversation_id,provider_token)
);
CREATE TABLE IF NOT EXISTS history_use_claims (
  id TEXT PRIMARY KEY, conversation_id TEXT NOT NULL REFERENCES conversations(id), point_id TEXT NOT NULL DEFAULT '',
  operation_id TEXT NOT NULL, work_run_id TEXT NOT NULL DEFAULT '', source_revision TEXT NOT NULL,
  source_fingerprint TEXT NOT NULL, state TEXT NOT NULL, revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, settled_at INTEGER
);
CREATE UNIQUE INDEX IF NOT EXISTS history_active_use ON history_use_claims(conversation_id)
  WHERE state IN ('held','uncertain');
CREATE TABLE IF NOT EXISTS workspaces (
  id TEXT PRIMARY KEY, intent_json BLOB NOT NULL, state TEXT NOT NULL,
  observation_json BLOB NOT NULL, resource_owner TEXT NOT NULL DEFAULT '', resource_version INTEGER NOT NULL DEFAULT 0,
  resource_payload BLOB, revision INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS workspace_uses (
  id TEXT PRIMARY KEY, workspace_id TEXT NOT NULL REFERENCES workspaces(id), execution_id TEXT NOT NULL DEFAULT '',
  work_run_id TEXT NOT NULL DEFAULT '', released_at INTEGER, created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS workspace_active_uses ON workspace_uses(workspace_id,released_at);
CREATE TABLE IF NOT EXISTS work_runs (
  id TEXT PRIMARY KEY, request_scope TEXT NOT NULL, request_id TEXT NOT NULL, requester_json BLOB NOT NULL,
  authority_json BLOB NOT NULL, delegation_json BLOB, spec_json BLOB NOT NULL, state TEXT NOT NULL,
  worker_execution_id TEXT NOT NULL DEFAULT '',
  cancellation_requested INTEGER NOT NULL DEFAULT 0, cancellation_reason TEXT NOT NULL DEFAULT '',
  cancel_request_scope TEXT NOT NULL DEFAULT '', cancel_request_id TEXT NOT NULL DEFAULT '',
  revision INTEGER NOT NULL, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
  UNIQUE(request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS work_attempts (
  work_run_id TEXT NOT NULL REFERENCES work_runs(id) ON DELETE CASCADE, step TEXT NOT NULL,
  attempt INTEGER NOT NULL, operation_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '',
  started_at INTEGER NOT NULL, settled_at INTEGER, PRIMARY KEY(work_run_id,step,attempt)
);
CREATE TABLE IF NOT EXISTS work_evidence (
  id TEXT PRIMARY KEY, work_run_id TEXT NOT NULL REFERENCES work_runs(id) ON DELETE CASCADE,
  request_scope TEXT NOT NULL DEFAULT '', request_id TEXT NOT NULL DEFAULT '',
  step TEXT NOT NULL, attempt INTEGER NOT NULL, kind TEXT NOT NULL, reporter_json BLOB NOT NULL,
  artifact_revision TEXT NOT NULL, passed INTEGER, detail TEXT NOT NULL, recorded_at INTEGER NOT NULL,
  revision INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS work_decisions (
  work_run_id TEXT PRIMARY KEY REFERENCES work_runs(id) ON DELETE CASCADE, step TEXT NOT NULL,
  request_scope TEXT NOT NULL DEFAULT '', request_id TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL, decision TEXT NOT NULL, decider_json BLOB NOT NULL,
  reason TEXT NOT NULL, decided_at INTEGER NOT NULL, revision INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS definitions (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, kind TEXT NOT NULL, head_revision_id TEXT NOT NULL,
  tombstoned INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS definition_revisions (
  id TEXT PRIMARY KEY, definition_id TEXT NOT NULL REFERENCES definitions(id), number INTEGER NOT NULL,
	request_scope TEXT NOT NULL, request_id TEXT NOT NULL,
  content_hash TEXT NOT NULL, schema_version INTEGER NOT NULL, compiler_version TEXT NOT NULL,
  source TEXT NOT NULL, parameters_json BLOB NOT NULL, team_json BLOB, process_json BLOB,
  dependencies_json BLOB NOT NULL, author_json BLOB NOT NULL, created_at INTEGER NOT NULL,
  UNIQUE(definition_id,number), UNIQUE(definition_id,content_hash), UNIQUE(request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS program_profiles (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, head_revision_id TEXT NOT NULL,
  tombstoned INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS program_profile_revisions (
  id TEXT PRIMARY KEY, profile_id TEXT NOT NULL REFERENCES program_profiles(id), number INTEGER NOT NULL,
	request_scope TEXT NOT NULL, request_id TEXT NOT NULL,
  content_hash TEXT NOT NULL, executable TEXT NOT NULL, argument_prefix_json BLOB NOT NULL,
  environment_json BLOB NOT NULL, working_directory TEXT NOT NULL, sandbox TEXT NOT NULL,
  timeout_ns INTEGER NOT NULL, output_limit_bytes INTEGER NOT NULL, effect_authority_json BLOB NOT NULL,
  author_json BLOB NOT NULL, created_at INTEGER NOT NULL,
  UNIQUE(profile_id,number), UNIQUE(profile_id,content_hash), UNIQUE(request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS work_node_attempts (
  work_run_id TEXT NOT NULL REFERENCES work_runs(id) ON DELETE CASCADE, node_id TEXT NOT NULL,
  activation_id TEXT NOT NULL, attempt INTEGER NOT NULL, issuance_id TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL, performer_json BLOB, operation_id TEXT NOT NULL DEFAULT '',
  execution_id TEXT NOT NULL DEFAULT '', ready_at INTEGER NOT NULL, retry_at INTEGER, deadline INTEGER NOT NULL,
  retry_budget INTEGER NOT NULL, join_winner TEXT NOT NULL DEFAULT '', decision_id TEXT NOT NULL DEFAULT '',
  outcome TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL, settled_at INTEGER,
  PRIMARY KEY(work_run_id,node_id,activation_id,attempt)
);
CREATE INDEX IF NOT EXISTS work_node_attempts_ready ON work_node_attempts(state,ready_at,retry_at);
CREATE UNIQUE INDEX IF NOT EXISTS work_node_attempts_issuance ON work_node_attempts(issuance_id) WHERE issuance_id <> '';
CREATE TABLE IF NOT EXISTS work_node_evidence (
  id TEXT PRIMARY KEY, request_scope TEXT NOT NULL, request_id TEXT NOT NULL,
  work_run_id TEXT NOT NULL, node_id TEXT NOT NULL, activation_id TEXT NOT NULL,
  attempt INTEGER NOT NULL, issuance_id TEXT NOT NULL, reporter_json BLOB NOT NULL,
  kind TEXT NOT NULL, artifact_revision TEXT NOT NULL DEFAULT '', passed INTEGER,
  disposition TEXT NOT NULL DEFAULT '', detail TEXT NOT NULL DEFAULT '', recorded_at INTEGER NOT NULL,
  revision INTEGER NOT NULL, UNIQUE(request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS decision_windows (
  id TEXT PRIMARY KEY, kind TEXT NOT NULL, source_revision INTEGER NOT NULL,
  work_run_id TEXT NOT NULL DEFAULT '', node_id TEXT NOT NULL DEFAULT '', activation_id TEXT NOT NULL DEFAULT '',
  attempt INTEGER NOT NULL DEFAULT 0, issuance_id TEXT NOT NULL DEFAULT '', audience_json BLOB NOT NULL,
  question TEXT NOT NULL, permitted_answers_json BLOB NOT NULL, evidence_refs_json BLOB NOT NULL,
  expires_at INTEGER NOT NULL, state TEXT NOT NULL, revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS decision_submissions (
  decision_id TEXT PRIMARY KEY REFERENCES decision_windows(id), request_scope TEXT NOT NULL,
  request_id TEXT NOT NULL, expected_window_revision INTEGER NOT NULL, expected_run_revision INTEGER NOT NULL DEFAULT 0, answer TEXT NOT NULL,
  reason TEXT NOT NULL, evidence_refs_json BLOB NOT NULL, actor_json BLOB NOT NULL,
  submitted_at INTEGER NOT NULL, UNIQUE(request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS automation_rules (
  id TEXT PRIMARY KEY, name TEXT NOT NULL, head_revision_id TEXT NOT NULL, enabled INTEGER NOT NULL,
  tombstoned INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS automation_state_requests (
 request_scope TEXT NOT NULL, request_id TEXT NOT NULL, rule_id TEXT NOT NULL,
 expected_revision INTEGER NOT NULL, enabled INTEGER NOT NULL, result_json BLOB NOT NULL,
 PRIMARY KEY(request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS automation_rule_revisions (
  id TEXT PRIMARY KEY, rule_id TEXT NOT NULL REFERENCES automation_rules(id), number INTEGER NOT NULL,
	request_scope TEXT NOT NULL, request_id TEXT NOT NULL,
  content_hash TEXT NOT NULL, owner_json BLOB NOT NULL, delegation_json BLOB NOT NULL,
  condition_json BLOB NOT NULL, action_json BLOB NOT NULL, policy_json BLOB NOT NULL,
  dependencies_json BLOB NOT NULL, author_json BLOB NOT NULL, created_at INTEGER NOT NULL,
  UNIQUE(rule_id,number), UNIQUE(rule_id,content_hash), UNIQUE(request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS automation_occurrences (
  id TEXT PRIMARY KEY, rule_id TEXT NOT NULL REFERENCES automation_rules(id),
  rule_revision_id TEXT NOT NULL REFERENCES automation_rule_revisions(id), source_occurrence_key TEXT NOT NULL,
	request_scope TEXT NOT NULL, request_id TEXT NOT NULL, requester_json BLOB NOT NULL,
	parent_occurrence_id TEXT NOT NULL DEFAULT '', causal_depth INTEGER NOT NULL DEFAULT 0,
	request_fingerprint TEXT NOT NULL DEFAULT '',
  scheduled_at INTEGER, event_at INTEGER, eligible_at INTEGER NOT NULL, expires_at INTEGER NOT NULL,
  state TEXT NOT NULL, operation_id TEXT NOT NULL DEFAULT '', work_run_id TEXT NOT NULL DEFAULT '',
  deployment_id TEXT NOT NULL DEFAULT '', revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL,
	UNIQUE(rule_revision_id,source_occurrence_key), UNIQUE(request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS automation_product_facts (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT, source_id TEXT NOT NULL, event_id TEXT NOT NULL,
  kind TEXT NOT NULL, value TEXT NOT NULL DEFAULT '', resource_json BLOB NOT NULL,
  occurred_at INTEGER NOT NULL, observed_at INTEGER NOT NULL,
  parent_occurrence_id TEXT NOT NULL DEFAULT '', causal_depth INTEGER NOT NULL DEFAULT 0,
  UNIQUE(source_id,event_id,observed_at)
);
CREATE INDEX IF NOT EXISTS automation_product_facts_scan ON automation_product_facts(source_id,sequence);
CREATE TABLE IF NOT EXISTS automation_occurrence_recipients (
  occurrence_id TEXT NOT NULL REFERENCES automation_occurrences(id) ON DELETE CASCADE,
  agent_id TEXT NOT NULL, disposition TEXT NOT NULL, operation_id TEXT NOT NULL DEFAULT '',
  detail TEXT NOT NULL DEFAULT '', PRIMARY KEY(occurrence_id,agent_id)
);
CREATE TABLE IF NOT EXISTS automation_condition_state (
  rule_id TEXT PRIMARY KEY REFERENCES automation_rules(id), source_cursor TEXT NOT NULL DEFAULT '',
  dwell_episode_id TEXT NOT NULL DEFAULT '', dwell_since INTEGER, cooldown_until INTEGER,
  debounce_at INTEGER, debounce_payload BLOB, observed_at INTEGER NOT NULL, revision INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS team_deployments (
  id TEXT PRIMARY KEY, definition_json BLOB NOT NULL, dependency_closure_json BLOB NOT NULL,
  mission TEXT NOT NULL, parameters_json BLOB NOT NULL, group_id TEXT NOT NULL,
	members_json BLOB NOT NULL, role_pins_json BLOB NOT NULL DEFAULT '[]', automation_rule_ids_json BLOB NOT NULL, work_run_id TEXT NOT NULL,
	target_kind TEXT NOT NULL DEFAULT 'new_group', workspaces_json BLOB NOT NULL DEFAULT '{}',
	owned_workspace_ids_json BLOB NOT NULL DEFAULT '[]', owned_automation_rule_ids_json BLOB NOT NULL DEFAULT '[]',
	briefing_operation_ids_json BLOB NOT NULL DEFAULT '{}', request_scope TEXT NOT NULL DEFAULT '', request_id TEXT NOT NULL DEFAULT '', request_digest TEXT NOT NULL DEFAULT '', requester_json BLOB,
  advisory_phase INTEGER NOT NULL, state TEXT NOT NULL, revision INTEGER NOT NULL,
  created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS team_continuations (
  deployment_id TEXT NOT NULL REFERENCES team_deployments(id), kind TEXT NOT NULL,
  request_scope TEXT NOT NULL, request_id TEXT NOT NULL, request_json BLOB NOT NULL,
  PRIMARY KEY(deployment_id,kind,request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS team_rebriefs (
  deployment_id TEXT NOT NULL REFERENCES team_deployments(id), request_scope TEXT NOT NULL,
  request_id TEXT NOT NULL, request_digest TEXT NOT NULL, definition_json BLOB NOT NULL,
  recipient_operations_json BLOB NOT NULL DEFAULT '{}', state TEXT NOT NULL,
  created_at INTEGER NOT NULL, completed_at INTEGER,
  PRIMARY KEY(request_scope,request_id)
);
CREATE TABLE IF NOT EXISTS team_lifecycle_requests (
  request_scope TEXT NOT NULL, request_id TEXT NOT NULL, deployment_id TEXT NOT NULL,
  kind TEXT NOT NULL, request_digest TEXT NOT NULL, created_at INTEGER NOT NULL,
  PRIMARY KEY(request_scope,request_id)
);
INSERT OR IGNORE INTO roles(id,name,actions_json,revision,created_at,updated_at)
VALUES('group_owner','Owner','["status.read","inbox.read","inbox.mark_read","message.send","execution.launch","execution.interact","execution.attach","execution.stop","execution.context.change","agent.configuration.update","group.membership.manage"]',1,0,0);
`

func (s *Store) CreateAgent(ctx context.Context, agent model.Agent) error {
	if agent.Lifecycle == "" {
		agent.Lifecycle = model.AgentActive
	}
	if agent.Notifications.DirectMessage == "" {
		agent.Notifications.DirectMessage = model.NotificationIfAvailable
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO agents(configuration_profile_json,id,name,task_reference,parent_agent_id,clone_source_agent_id,lifecycle_state,direct_notification_intent,harness,model,working_directory,approval,sandbox,primary_execution_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		configurationProfileJSON(agent.ConfigurationProfile), agent.ID, agent.Name, agent.TaskReference, agent.ParentAgentID, agent.CloneSourceAgentID, agent.Lifecycle, agent.Notifications.DirectMessage, agent.Desired.Harness, agent.Desired.Model, agent.Desired.WorkingDirectory, agent.Desired.Approval, agent.Desired.Sandbox, agent.PrimaryExecutionID, agent.Revision, nanos(agent.CreatedAt), nanos(agent.UpdatedAt))
	if err != nil {
		return classify(err)
	}
	return s.bump(ctx)
}

func (s *Store) UpdateAgent(ctx context.Context, id model.AgentID, expected model.Revision, name, taskReference string, notifications model.AgentNotificationPreferences, desired model.DesiredConfiguration, profile *model.ConfigurationProfileRef, authority model.AuthorityRequest, at time.Time) (model.Agent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Agent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil {
		return model.Agent{}, err
	}
	if !decision.Allowed {
		return model.Agent{}, app.ErrUnauthorized
	}
	result, err := tx.ExecContext(ctx, `UPDATE agents SET configuration_profile_json=?,name=?,task_reference=?,direct_notification_intent=?,harness=?,model=?,working_directory=?,approval=?,sandbox=?,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND lifecycle_state=?`,
		configurationProfileJSON(profile), name, taskReference, notifications.DirectMessage, desired.Harness, desired.Model, desired.WorkingDirectory, desired.Approval, desired.Sandbox, nanos(at), id, expected, model.AgentActive)
	if err != nil {
		return model.Agent{}, classify(err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.Agent{}, app.ErrConflict
	}
	if err := bumpTx(ctx, tx); err != nil {
		return model.Agent{}, err
	}
	agent, err := scanAgent(tx.QueryRowContext(ctx, agentSelect+` WHERE id=?`, id))
	if err != nil {
		return model.Agent{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Agent{}, err
	}
	return agent, nil
}

func (s *Store) Agent(ctx context.Context, id model.AgentID) (model.Agent, error) {
	row := s.db.QueryRowContext(ctx, agentSelect+` WHERE id=?`, id)
	return scanAgent(row)
}

func (s *Store) CreateGroup(ctx context.Context, group model.Group, ownerBounds model.ConfigurationBounds) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO groups(id,name,owner_agent_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?)`, group.ID, group.Name, group.OwnerAgentID, group.Revision, nanos(group.CreatedAt), nanos(group.UpdatedAt)); err != nil {
		return classify(err)
	}
	for position, member := range group.Members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,agent_id,position) VALUES(?,?,?)`, group.ID, member, position); err != nil {
			return classify(err)
		}
	}
	if group.OwnerAgentID != "" {
		bounds, err := json.Marshal(ownerBounds)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO role_assignments(role_id,subject_kind,subject_id,resource_kind,resource_id,bounds_json,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,1,?,?)`, model.GroupOwnerRole, model.AuthorityAgent, group.OwnerAgentID, model.ResourceGroupPeers, group.ID, bounds, nanos(group.CreatedAt), nanos(group.UpdatedAt)); err != nil {
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
	err := s.db.QueryRowContext(ctx, `SELECT id,name,owner_agent_id,revision,created_at,updated_at FROM groups WHERE id=?`, id).Scan(&group.ID, &group.Name, &group.OwnerAgentID, &group.Revision, &created, &updated)
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
	if in.Authority.Action != "" {
		decision, err := authorizeTx(ctx, tx, in.Authority, in.Operation.CreatedAt)
		if err != nil {
			return app.AdmissionResult{}, err
		}
		if !decision.Allowed {
			return app.AdmissionResult{}, app.ErrUnauthorized
		}
	}
	if repeated, ok, err := admissionByRequest(ctx, tx, in.Operation, in.AgentID, false); err != nil {
		return app.AdmissionResult{}, err
	} else if ok {
		_ = tx.Commit()
		return repeated, nil
	}
	if in.AgentID != "" {
		var revision model.Revision
		var primary model.ExecutionID
		var lifecycle model.AgentLifecycleState
		if err := tx.QueryRowContext(ctx, `SELECT revision,primary_execution_id,lifecycle_state FROM agents WHERE id=?`, in.AgentID).Scan(&revision, &primary, &lifecycle); err != nil {
			return app.AdmissionResult{}, classify(err)
		}
		if revision != in.Expected || lifecycle != model.AgentActive {
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
			_, _ = tx.ExecContext(ctx, `UPDATE execution_accesses SET state=?,revoked_at=?,revision=revision+1 WHERE execution_id=? AND state NOT IN (?,?)`, model.ExecutionAccessRevoked, nanos(in.Execution.CreatedAt), primary, model.ExecutionAccessRevoked, model.ExecutionAccessExpired)
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
	if in.Access.ExecutionID != "" {
		if err := insertExecutionAccess(ctx, tx, in.Access); err != nil {
			return app.AdmissionResult{}, err
		}
	}
	if in.Authority.Action != "" {
		decision, err := authorizeTx(ctx, tx, in.Authority, in.Operation.CreatedAt)
		if err != nil || !decision.Allowed {
			if err == nil {
				err = app.ErrUnauthorized
			}
			return app.AdmissionResult{}, err
		}
		if err := insertOperationAuthority(ctx, tx, in.Operation.ID, in.Authority, decision); err != nil {
			return app.AdmissionResult{}, err
		}
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
	if in.Eligibility != nil {
		eligible, eligibilityErr := audienceIncludesAgent(ctx, tx, *in.Eligibility, execution.AgentID)
		if eligibilityErr != nil {
			return app.AdmissionResult{}, eligibilityErr
		}
		if !eligible {
			return app.AdmissionResult{}, app.ErrConflict
		}
	}
	if err := insertOperation(ctx, tx, in.Operation); err != nil {
		return app.AdmissionResult{}, err
	}
	if in.Authority.Action != "" {
		decision, err := authorizeTx(ctx, tx, in.Authority, in.Operation.CreatedAt)
		if err != nil || !decision.Allowed {
			if err == nil {
				err = app.ErrUnauthorized
			}
			return app.AdmissionResult{}, err
		}
		if err := insertOperationAuthority(ctx, tx, in.Operation.ID, in.Authority, decision); err != nil {
			return app.AdmissionResult{}, err
		}
		var audienceJSON any
		var eligibilityAgent model.AgentID
		if in.Eligibility != nil {
			encoded, encodeErr := json.Marshal(in.Eligibility)
			if encodeErr != nil {
				return app.AdmissionResult{}, encodeErr
			}
			audienceJSON, eligibilityAgent = encoded, execution.AgentID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO effect_permits(operation_id,eligibility_audience_json,eligibility_agent_id) VALUES(?,?,?)`, in.Operation.ID, audienceJSON, eligibilityAgent); err != nil {
			return app.AdmissionResult{}, err
		}
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if request, ok, err := operationAuthority(ctx, tx, operationID); err != nil {
		return err
	} else if ok {
		decision, err := authorizeTx(ctx, tx, request, at)
		if err != nil {
			return err
		}
		if !decision.Allowed {
			return app.ErrUnauthorized
		}
		additional, err := additionalOperationAuthorities(ctx, tx, operationID, request.Principal)
		if err != nil {
			return err
		}
		for _, requirement := range additional {
			decision, err = authorizeTx(ctx, tx, requirement, at)
			if err != nil {
				return err
			}
			if !decision.Allowed {
				return app.ErrUnauthorized
			}
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE release_permits SET consumed_at=? WHERE execution_id=? AND operation_id=? AND consumed_at IS NULL AND EXISTS(SELECT 1 FROM executions WHERE id=? AND state=?) AND EXISTS(SELECT 1 FROM operations WHERE id=? AND state=?)`, nanos(at), executionID, operationID, executionID, model.ExecutionPrepared, operationID, model.OperationAdmitted)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return app.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_accesses SET state=?,revision=revision+1 WHERE execution_id=? AND state=? AND delivery_id<>'' AND expires_at>?`, model.ExecutionAccessActive, executionID, model.ExecutionAccessInactive, nanos(at)); err != nil {
		return err
	}
	if err := bumpTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ConsumeExecutionEffect(ctx context.Context, operationID model.OperationID, at time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	request, ok, err := operationAuthority(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if !ok {
		return app.ErrConflict
	}
	decision, err := authorizeTx(ctx, tx, request, at)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return app.ErrUnauthorized
	}
	var audienceJSON []byte
	var eligibilityAgent model.AgentID
	if err := tx.QueryRowContext(ctx, `SELECT eligibility_audience_json,eligibility_agent_id FROM effect_permits WHERE operation_id=?`, operationID).Scan(&audienceJSON, &eligibilityAgent); err != nil {
		return classify(err)
	}
	if len(audienceJSON) != 0 {
		var audience model.MessageAudience
		if err := json.Unmarshal(audienceJSON, &audience); err != nil {
			return err
		}
		eligible, err := audienceIncludesAgent(ctx, tx, audience, eligibilityAgent)
		if err != nil {
			return err
		}
		if !eligible {
			return app.ErrConflict
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE effect_permits SET consumed_at=? WHERE operation_id=? AND consumed_at IS NULL`, nanos(at), operationID)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return app.ErrConflict
	}
	if err := bumpTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
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

func (s *Store) OperationResult(ctx context.Context, operationID model.OperationID) (app.AdmissionResult, error) {
	operation, err := operationTx(ctx, s.db, operationID)
	if err != nil {
		return app.AdmissionResult{}, err
	}
	var execution model.Execution
	if operation.ExecutionID != "" {
		execution, err = s.Execution(ctx, operation.ExecutionID)
	}
	return app.AdmissionResult{Operation: operation, Execution: execution}, err
}

func (s *Store) Execution(ctx context.Context, id model.ExecutionID) (model.Execution, error) {
	return scanExecution(s.db.QueryRowContext(ctx, executionSelect+` WHERE id=?`, id))
}

func (s *Store) RecoverableExecutions(ctx context.Context) ([]model.Execution, error) {
	rows, err := s.db.QueryContext(ctx, executionSelect+` WHERE state IN (?,?,?,?) OR (workload_kind=? AND state IN (?,?) AND EXISTS(SELECT 1 FROM workspace_uses u WHERE u.execution_id=executions.id AND u.released_at IS NULL)) ORDER BY created_at`, model.ExecutionPrepared, model.ExecutionReleased, model.ExecutionRunning, model.ExecutionUnknown, model.ExecutionWorkloadProgram, model.ExecutionExited, model.ExecutionFailed)
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
	if state == model.ExecutionExited || state == model.ExecutionFailed {
		if _, err := s.db.ExecContext(ctx, `UPDATE execution_accesses SET state=?,revoked_at=?,revision=revision+1 WHERE execution_id=? AND state NOT IN (?,?)`, model.ExecutionAccessRevoked, nanos(at), id, model.ExecutionAccessRevoked, model.ExecutionAccessExpired); err != nil {
			return model.Execution{}, err
		}
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

func (s *Store) CreateMessage(ctx context.Context, in app.MessageAdmission) (app.MessageAdmissionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.MessageAdmissionResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	// Exact retries are immutable reads. They intentionally precede live
	// authority, parent, membership, preference, and claim re-evaluation.
	if existing, ok, err := messageByRequestDigest(ctx, tx, in.Message.Sender, in.RequestID, in.RequestDigest); err != nil {
		return app.MessageAdmissionResult{}, err
	} else if ok {
		_ = tx.Commit()
		return existing, nil
	}
	if err = requirePendingAutomationAction(ctx, tx, in.Message.Sender, model.AutomationSendMessage, "", in.Message.CreatedAt); err != nil {
		return app.MessageAdmissionResult{}, err
	}
	for _, request := range in.Authority {
		decision, err := authorizeTx(ctx, tx, request, in.Message.CreatedAt)
		if err != nil {
			return app.MessageAdmissionResult{}, err
		}
		if !decision.Allowed {
			return app.MessageAdmissionResult{}, app.ErrUnauthorized
		}
	}
	// The concrete audience must still be eligible at the same transaction
	// boundary as fresh admission. An exact retry above remains an immutable read.
	for _, recipient := range in.Message.Recipients {
		if recipient.AddressKind == model.MessageAddressAgent {
			var state model.AgentLifecycleState
			if err := tx.QueryRowContext(ctx, `SELECT lifecycle_state FROM agents WHERE id=?`, recipient.AgentID).Scan(&state); err != nil {
				return app.MessageAdmissionResult{}, classify(err)
			}
			if state != model.AgentActive {
				return app.MessageAdmissionResult{}, app.ErrConflict
			}
			if len(in.Eligibility) != 0 {
				eligible := false
				for _, audience := range in.Eligibility {
					included, includeErr := audienceIncludesAgent(ctx, tx, audience, recipient.AgentID)
					if includeErr != nil {
						return app.MessageAdmissionResult{}, includeErr
					}
					if included {
						eligible = true
						break
					}
				}
				if !eligible {
					return app.MessageAdmissionResult{}, app.ErrConflict
				}
			}
		}
	}
	if in.Message.ParentMessageID != "" {
		parent, err := messageTx(ctx, tx, in.Message.ParentMessageID)
		if err != nil {
			return app.MessageAdmissionResult{}, err
		}
		if !messageAccessible(in.Message.Sender, parent) {
			return app.MessageAdmissionResult{}, app.ErrUnauthorized
		}
		in.Message.ThreadID = parent.ThreadID
	} else {
		in.Message.ThreadID = in.Message.ID
	}
	resultCode := in.ResultCode
	if resultCode == "" {
		resultCode = "committed"
	}
	op := model.Operation{ID: in.OperationID, RequestID: in.RequestID, Kind: model.OperationSendMessage, Principal: in.Message.Sender, State: model.OperationSucceeded, ResultCode: resultCode, Revision: 1, CreatedAt: in.Message.CreatedAt, UpdatedAt: in.Message.CreatedAt}
	if err := insertOperation(ctx, tx, op); err != nil {
		return app.MessageAdmissionResult{}, err
	}
	authorityKind, authorityID := subjectParts(in.Message.Sender.Authority)
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,operation_id,sender_kind,sender_agent_id,sender_execution_id,sender_generation,sender_automation_run,sender_authority_subject_kind,sender_authority_subject_id,sender_conversation_id,subject,parent_message_id,thread_id,request_digest,body,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, in.Message.ID, in.OperationID, in.Message.Sender.Kind, in.Message.Sender.AgentID, in.Message.Sender.ExecutionID, in.Message.Sender.Generation, in.Message.Sender.AutomationRun, authorityKind, authorityID, in.Message.SenderConversationID, in.Message.Subject, in.Message.ParentMessageID, in.Message.ThreadID, in.RequestDigest, in.Message.Body, nanos(in.Message.CreatedAt)); err != nil {
		return app.MessageAdmissionResult{}, classify(err)
	}
	for _, recipient := range in.Message.Recipients {
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_recipients(id,message_id,address_kind,agent_id,audience_kind,read_at,notification_intent,notification_outcome,notification_detail,notified_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, recipient.ID, in.Message.ID, recipient.AddressKind, recipient.AgentID, recipient.Audience, nil, recipient.NotificationIntent, recipient.NotificationOutcome, recipient.NotificationDetail, nil); err != nil {
			return app.MessageAdmissionResult{}, classify(err)
		}
	}
	if err := insertMessageAttachments(ctx, tx, in); err != nil {
		return app.MessageAdmissionResult{}, err
	}
	if err := insertTerminalOperationFactsTx(ctx, tx, app.OperationCompletion{OperationID: op.ID, OperationState: op.State, At: op.UpdatedAt}); err != nil {
		return app.MessageAdmissionResult{}, err
	}
	if err := bumpTx(ctx, tx); err != nil {
		return app.MessageAdmissionResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return app.MessageAdmissionResult{}, err
	}
	return app.MessageAdmissionResult{Operation: op, Message: in.Message}, nil
}

func (s *Store) MarkMessageRead(ctx context.Context, messageID model.MessageID, addressKind model.MessageAddressKind, agentID model.AgentID, authority model.AuthorityRequest, at time.Time) (model.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, err
	}
	defer func() { _ = tx.Rollback() }()
	decision, err := authorizeTx(ctx, tx, authority, at)
	if err != nil {
		return model.Message{}, err
	}
	if !decision.Allowed {
		return model.Message{}, app.ErrUnauthorized
	}
	result, err := tx.ExecContext(ctx, `UPDATE message_recipients SET read_at=COALESCE(read_at,?) WHERE message_id=? AND address_kind=? AND agent_id=?`, nanos(at), messageID, addressKind, agentID)
	if err != nil {
		return model.Message{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.Message{}, app.ErrNotFound
	}
	if err := bumpTx(ctx, tx); err != nil {
		return model.Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Message{}, err
	}
	return s.message(ctx, messageID)
}

func (s *Store) MessagesForAgent(ctx context.Context, agentID model.AgentID, unreadOnly bool) ([]model.Message, error) {
	query := `SELECT message_id FROM message_recipients WHERE address_kind='agent' AND agent_id=?`
	if unreadOnly {
		query += ` AND read_at IS NULL`
	}
	query += ` ORDER BY rowid`
	rows, err := s.db.QueryContext(ctx, query, agentID)
	if err != nil {
		return nil, err
	}
	var ids []model.MessageID
	for rows.Next() {
		var id model.MessageID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	out := make([]model.Message, 0, len(ids))
	for _, id := range ids {
		message, err := s.message(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, message)
	}
	return out, nil
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
	rows, err := s.db.QueryContext(ctx, agentSelect+` ORDER BY id`)
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
	history, err := s.SearchHistory(ctx, app.HistorySearchFilter{})
	if err != nil {
		return snapshot, err
	}
	snapshot.History = history.Entries
	for _, entry := range snapshot.History {
		points, pointErr := s.HistoryPoints(ctx, entry.ConversationID)
		if pointErr != nil {
			return snapshot, pointErr
		}
		snapshot.HistoryPoints = append(snapshot.HistoryPoints, points...)
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id FROM workspaces ORDER BY created_at`)
	if err != nil {
		return snapshot, err
	}
	var workspaceIDs []model.WorkspaceID
	for rows.Next() {
		var id model.WorkspaceID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return snapshot, err
		}
		workspaceIDs = append(workspaceIDs, id)
	}
	rows.Close()
	for _, id := range workspaceIDs {
		workspace, workspaceErr := s.Workspace(ctx, id)
		if workspaceErr != nil {
			return snapshot, workspaceErr
		}
		snapshot.Workspaces = append(snapshot.Workspaces, app.WorkspaceView{ID: workspace.ID, Intent: workspace.Intent, State: workspace.State, Observation: workspace.Observation, Revision: workspace.Revision})
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id,workspace_id,execution_id,work_run_id,released_at,created_at FROM workspace_uses ORDER BY created_at`)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		var use model.WorkspaceUse
		var released sql.NullInt64
		var created int64
		if err = rows.Scan(&use.ID, &use.WorkspaceID, &use.ExecutionID, &use.WorkRunID, &released, &created); err != nil {
			rows.Close()
			return snapshot, err
		}
		use.CreatedAt = fromNanos(created)
		if released.Valid {
			value := fromNanos(released.Int64)
			use.ReleasedAt = &value
		}
		snapshot.WorkspaceUses = append(snapshot.WorkspaceUses, use)
	}
	rows.Close()
	rows, err = s.db.QueryContext(ctx, `SELECT id FROM work_runs ORDER BY created_at`)
	if err != nil {
		return snapshot, err
	}
	var workIDs []model.WorkRunID
	for rows.Next() {
		var id model.WorkRunID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return snapshot, err
		}
		workIDs = append(workIDs, id)
	}
	rows.Close()
	for _, id := range workIDs {
		record, workErr := s.WorkRun(ctx, id)
		if workErr != nil {
			return snapshot, workErr
		}
		snapshot.WorkRuns = append(snapshot.WorkRuns, record.Run)
		snapshot.WorkEvidence = append(snapshot.WorkEvidence, record.Evidence...)
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
	result, err := tx.ExecContext(ctx, `UPDATE operations SET state=?,result_code=?,detail=?,revision=revision+1,updated_at=? WHERE id=? AND state IN (?,?)`, in.OperationState, in.ResultCode, in.Detail, nanos(in.At), in.OperationID, model.OperationAdmitted, model.OperationRunning)
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
	if err == nil && in.UpdateExecutionState && (in.ExecutionState == model.ExecutionExited || in.ExecutionState == model.ExecutionFailed) {
		_, err = tx.ExecContext(ctx, `UPDATE execution_accesses SET state=?,revoked_at=?,revision=revision+1 WHERE execution_id=? AND state NOT IN (?,?)`, model.ExecutionAccessRevoked, nanos(in.At), in.ExecutionID, model.ExecutionAccessRevoked, model.ExecutionAccessExpired)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE workspace_uses SET released_at=? WHERE execution_id=? AND released_at IS NULL`, nanos(in.At), in.ExecutionID)
		}
	} else if err == nil && in.UpdateExecutionState && in.ExecutionState == model.ExecutionUnknown {
		_, err = tx.ExecContext(ctx, `UPDATE execution_accesses SET state=?,revision=revision+1 WHERE execution_id=? AND state=?`, model.ExecutionAccessSuspended, in.ExecutionID, model.ExecutionAccessActive)
	}
	if err != nil {
		return err
	}
	return insertTerminalOperationFactsTx(ctx, tx, in)
}

const executionSelect = `SELECT configuration_profile_json,id,workload_kind,agent_id,conversation_id,harness,model,working_directory,approval,sandbox,state,attempt_generation,context_readiness,context_provider_order,evidence_provider,evidence_version,evidence_payload,native_namespace,native_reference,native_observed_at,revision,created_at,updated_at FROM executions`
const operationSelect = `SELECT id,request_id,kind,principal_kind,principal_agent_id,principal_execution_id,principal_generation,principal_automation_run,automation_delegation_json,authority_subject_kind,authority_subject_id,execution_id,state,result_code,detail,revision,created_at,updated_at FROM operations`
const agentSelect = `SELECT configuration_profile_json,id,name,task_reference,parent_agent_id,clone_source_agent_id,lifecycle_state,retired_at,retired_by_kind,retired_by_agent_id,retired_by_execution_id,retirement_reason,direct_notification_intent,harness,model,working_directory,approval,sandbox,primary_execution_id,revision,created_at,updated_at FROM agents`

type scanner interface{ Scan(...any) error }

func scanAgent(row scanner) (model.Agent, error) {
	var a model.Agent
	var profile []byte
	var retired sql.NullInt64
	var created, updated int64
	err := row.Scan(&profile, &a.ID, &a.Name, &a.TaskReference, &a.ParentAgentID, &a.CloneSourceAgentID, &a.Lifecycle, &retired, &a.RetiredBy.Kind, &a.RetiredBy.AgentID, &a.RetiredBy.ExecutionID, &a.RetirementReason, &a.Notifications.DirectMessage, &a.Desired.Harness, &a.Desired.Model, &a.Desired.WorkingDirectory, &a.Desired.Approval, &a.Desired.Sandbox, &a.PrimaryExecutionID, &a.Revision, &created, &updated)
	if err != nil {
		return a, classify(err)
	}
	if len(profile) != 0 {
		if err := json.Unmarshal(profile, &a.ConfigurationProfile); err != nil {
			return a, err
		}
	}
	a.CreatedAt, a.UpdatedAt = fromNanos(created), fromNanos(updated)
	if retired.Valid {
		value := fromNanos(retired.Int64)
		a.RetiredAt = &value
	}
	return a, nil
}
func scanExecution(row scanner) (model.Execution, error) {
	var e model.Execution
	var profile []byte
	var observed sql.NullInt64
	var namespace, reference string
	var created, updated int64
	err := row.Scan(&profile, &e.ID, &e.Workload, &e.AgentID, &e.ConversationID, &e.Spec.Harness, &e.Spec.Model, &e.Spec.WorkingDirectory, &e.Spec.Approval, &e.Spec.Sandbox, &e.State, &e.Attempt, &e.ContextReadiness, &e.ContextOrder, &e.Evidence.Provider, &e.Evidence.Version, &e.Evidence.Payload, &namespace, &reference, &observed, &e.Revision, &created, &updated)
	if err != nil {
		return e, classify(err)
	}
	if len(profile) != 0 {
		if err := json.Unmarshal(profile, &e.Spec.ConfigurationProfile); err != nil {
			return e, err
		}
	}
	e.Spec.ExecutionID, e.Spec.Workload, e.Spec.AgentID, e.Spec.ConversationID, e.Spec.Attempt = e.ID, e.Workload, e.AgentID, e.ConversationID, e.Attempt
	e.CreatedAt, e.UpdatedAt = fromNanos(created), fromNanos(updated)
	if reference != "" {
		e.NativeConversation = &model.NativeConversationEvidence{Namespace: namespace, Reference: reference, ObservedAt: fromNanos(observed.Int64)}
	}
	return e, nil
}
func scanOperation(row scanner) (model.Operation, error) {
	var o model.Operation
	var created, updated int64
	var authorityKind, authorityID string
	var delegation []byte
	err := row.Scan(&o.ID, &o.RequestID, &o.Kind, &o.Principal.Kind, &o.Principal.AgentID, &o.Principal.ExecutionID, &o.Principal.Generation, &o.Principal.AutomationRun, &delegation, &authorityKind, &authorityID, &o.ExecutionID, &o.State, &o.ResultCode, &o.Detail, &o.Revision, &created, &updated)
	if err != nil {
		return o, classify(err)
	}
	o.CreatedAt, o.UpdatedAt = fromNanos(created), fromNanos(updated)
	if authorityKind != "" {
		o.Principal.Authority = makeSubject(authorityKind, authorityID)
	}
	if len(delegation) != 0 {
		o.Principal.Delegation = new(model.AutomationDelegation)
		if err := json.Unmarshal(delegation, o.Principal.Delegation); err != nil {
			return o, err
		}
	}
	return o, nil
}

func insertExecution(ctx context.Context, tx *sql.Tx, e model.Execution) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO executions(configuration_profile_json,id,workload_kind,agent_id,conversation_id,harness,model,working_directory,approval,sandbox,state,attempt_generation,context_readiness,context_provider_order,evidence_provider,evidence_version,evidence_payload,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, configurationProfileJSON(e.Spec.ConfigurationProfile), e.ID, e.Workload, e.AgentID, e.ConversationID, e.Spec.Harness, e.Spec.Model, e.Spec.WorkingDirectory, e.Spec.Approval, e.Spec.Sandbox, e.State, e.Attempt, e.ContextReadiness, e.ContextOrder, e.Evidence.Provider, e.Evidence.Version, e.Evidence.Payload, e.Revision, nanos(e.CreatedAt), nanos(e.UpdatedAt))
	return classify(err)
}
func insertOperation(ctx context.Context, tx *sql.Tx, o model.Operation) error {
	authorityKind, authorityID := subjectParts(o.Principal.Authority)
	var delegation any
	if o.Principal.Delegation != nil {
		encoded, err := json.Marshal(o.Principal.Delegation)
		if err != nil {
			return err
		}
		delegation = encoded
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO operations(id,request_id,request_scope,kind,principal_kind,principal_agent_id,principal_execution_id,principal_generation,principal_automation_run,automation_delegation_json,authority_subject_kind,authority_subject_id,execution_id,state,result_code,detail,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, o.ID, o.RequestID, requestScope(o.Principal), o.Kind, o.Principal.Kind, o.Principal.AgentID, o.Principal.ExecutionID, o.Principal.Generation, o.Principal.AutomationRun, delegation, authorityKind, authorityID, o.ExecutionID, o.State, o.ResultCode, o.Detail, o.Revision, nanos(o.CreatedAt), nanos(o.UpdatedAt))
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
	operation, err := operationByRequestTx(ctx, tx, desired.Principal, desired.RequestID)
	if errors.Is(err, app.ErrNotFound) {
		return app.AdmissionResult{}, false, nil
	}
	if err != nil {
		return app.AdmissionResult{}, false, err
	}
	if operation.Kind != desired.Kind || !sameRequester(operation.Principal, desired.Principal) || (strictExecution && operation.ExecutionID != desired.ExecutionID) {
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
func operationByRequestTx(ctx context.Context, tx *sql.Tx, principal model.Principal, id model.RequestID) (model.Operation, error) {
	return scanOperation(tx.QueryRowContext(ctx, operationSelect+` WHERE request_scope=? AND request_id=?`, requestScope(principal), id))
}
func operationTx(ctx context.Context, q queryer, id model.OperationID) (model.Operation, error) {
	return scanOperation(q.QueryRowContext(ctx, operationSelect+` WHERE id=?`, id))
}
func executionTx(ctx context.Context, tx *sql.Tx, id model.ExecutionID) (model.Execution, error) {
	return scanExecution(tx.QueryRowContext(ctx, executionSelect+` WHERE id=?`, id))
}

func (s *Store) message(ctx context.Context, id model.MessageID) (model.Message, error) {
	return messageTx(ctx, s.db, id)
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
