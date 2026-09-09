package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const importSchema = `
CREATE TABLE IF NOT EXISTS import_receipts (
  id TEXT PRIMARY KEY, source_schema_version INTEGER NOT NULL,
  source_database_sha256 TEXT NOT NULL, manifest_sha256 TEXT NOT NULL,
  importer_format_version INTEGER NOT NULL, plan_format_version INTEGER NOT NULL,
  target_schema_version INTEGER NOT NULL, plan_sha256 TEXT NOT NULL,
  semantic_sha256 TEXT NOT NULL, metadata_only_attachments INTEGER NOT NULL,
  counts_json BLOB NOT NULL, completed_at INTEGER NOT NULL,
  UNIQUE(source_database_sha256,manifest_sha256,importer_format_version,plan_format_version,target_schema_version)
);
CREATE TABLE IF NOT EXISTS import_id_map (
  source_namespace TEXT NOT NULL, source_table TEXT NOT NULL, source_key TEXT NOT NULL,
  target_kind TEXT NOT NULL, target_id TEXT NOT NULL,
  PRIMARY KEY(source_namespace,source_table,source_key,target_kind)
);
CREATE TABLE IF NOT EXISTS imported_source_records (
  source_table TEXT NOT NULL, source_key TEXT NOT NULL, source_path TEXT NOT NULL,
  class TEXT NOT NULL, conversion TEXT NOT NULL, reason_code TEXT NOT NULL,
  payload BLOB NOT NULL, payload_sha256 TEXT NOT NULL,
  PRIMARY KEY(source_table,source_key)
);
CREATE TABLE IF NOT EXISTS imported_diagnostics (
  ordinal INTEGER PRIMARY KEY, severity TEXT NOT NULL, code TEXT NOT NULL,
  source_table TEXT NOT NULL, source_key TEXT NOT NULL, source_path TEXT NOT NULL,
  detail TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS imported_attachment_availability (
  attachment_id TEXT PRIMARY KEY REFERENCES attachments(id) ON DELETE CASCADE,
  source_table TEXT NOT NULL, source_key TEXT NOT NULL, availability TEXT NOT NULL,
  loss_reason TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS imported_message_envelopes (
  message_id TEXT PRIMARY KEY REFERENCES messages(id) ON DELETE CASCADE,
  source_table TEXT NOT NULL, source_key TEXT NOT NULL, record BLOB NOT NULL
);
`

// ApplyImport is the sole target write boundary for an offline conversion. It
// intentionally bypasses live admission APIs: imported history has no current
// authority, permit, access, claim, route, notification, or replay effect.
func (s *Store) ApplyImport(ctx context.Context, batch app.ImportBatch) error {
	if err := model.ValidateGroupHierarchy(batch.Groups); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, importSchema); err != nil {
		return fmt.Errorf("initialize import schema: %w", err)
	}
	var receipts int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM import_receipts`).Scan(&receipts); err != nil {
		return err
	}
	if receipts != 0 {
		return fmt.Errorf("replacement database already has an import receipt")
	}
	for _, table := range []string{"agents", "groups", "conversations", "messages", "workspaces", "configuration_profiles", "usage_observations", "historical_activity"} {
		var count int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			return fmt.Errorf("replacement database is not fresh: %s has rows", table)
		}
	}
	if err = insertImportEntities(ctx, tx, batch); err != nil {
		return err
	}
	counts, err := json.Marshal(batch.Receipt.Counts)
	if err != nil {
		return err
	}
	r := batch.Receipt
	if _, err = tx.ExecContext(ctx, `INSERT INTO import_receipts(id,source_schema_version,source_database_sha256,manifest_sha256,importer_format_version,plan_format_version,target_schema_version,plan_sha256,semantic_sha256,metadata_only_attachments,counts_json,completed_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.SourceSchemaVersion, r.SourceDatabaseSHA256, r.ManifestSHA256, r.ImporterFormatVersion, r.PlanFormatVersion, r.TargetSchemaVersion, r.PlanSHA256, r.SemanticSHA256, r.MetadataOnlyAttachments, counts, importNanos(r.CompletedAt)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE backend_meta SET revision=revision+1 WHERE singleton=1`); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit offline import: %w", err)
	}
	return nil
}

func insertImportEntities(ctx context.Context, tx *sql.Tx, batch app.ImportBatch) error {
	for _, mapping := range batch.IDMappings {
		if _, err := tx.ExecContext(ctx, `INSERT INTO import_id_map(source_namespace,source_table,source_key,target_kind,target_id) VALUES(?,?,?,?,?)`, mapping.SourceNamespace, mapping.SourceTable, mapping.SourceKey, mapping.TargetKind, mapping.TargetID); err != nil {
			return err
		}
	}
	for _, record := range batch.SourceRecords {
		if _, err := tx.ExecContext(ctx, `INSERT INTO imported_source_records(source_table,source_key,source_path,class,conversion,reason_code,payload,payload_sha256) VALUES(?,?,?,?,?,?,?,?)`, record.SourceTable, record.SourceKey, record.SourcePath, record.Class, record.Conversion, record.ReasonCode, record.Payload, record.PayloadSHA256); err != nil {
			return err
		}
	}
	for i, diagnostic := range batch.Diagnostics {
		if _, err := tx.ExecContext(ctx, `INSERT INTO imported_diagnostics(ordinal,severity,code,source_table,source_key,source_path,detail) VALUES(?,?,?,?,?,?,?)`, i, diagnostic.Severity, diagnostic.Code, diagnostic.SourceTable, diagnostic.SourceKey, diagnostic.SourcePath, diagnostic.Detail); err != nil {
			return err
		}
	}
	for _, agent := range batch.Agents {
		profile, _ := json.Marshal(agent.ConfigurationProfile)
		var retiredAt any
		if agent.RetiredAt != nil {
			retiredAt = importNanos(*agent.RetiredAt)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agents(labels_json,host_sandbox_json,environment_json,configuration_profile_json,id,name,task_reference,parent_agent_id,clone_source_agent_id,lifecycle_state,retired_at,retired_by_kind,retired_by_agent_id,retired_by_execution_id,retirement_reason,direct_notification_intent,harness,model,effort,tool_governance,working_directory,approval,sandbox,primary_execution_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			agentLabelsJSON(&agent.Labels), sandboxSelectionJSON(agent.Desired.HostSandbox), environmentJSON(agent.Desired.Environment), profile, agent.ID, agent.Name, agent.TaskReference, agent.ParentAgentID, agent.CloneSourceAgentID, agent.Lifecycle, retiredAt, agent.RetiredBy.Kind, agent.RetiredBy.AgentID, agent.RetiredBy.ExecutionID, agent.RetirementReason, agent.Notifications.DirectMessage, agent.Desired.Harness, agent.Desired.Model, agent.Desired.Effort, agent.Desired.ToolGovernance, agent.Desired.WorkingDirectory, agent.Desired.Approval, agent.Desired.Sandbox, "", agent.Revision, importNanos(agent.CreatedAt), importNanos(agent.UpdatedAt)); err != nil {
			return fmt.Errorf("import agent %s: %w", agent.ID, err)
		}
	}
	for _, group := range batch.Groups {
		if _, err := tx.ExecContext(ctx, `INSERT INTO groups(id,name,owner_agent_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?)`, group.ID, group.Name, "", group.Revision, importNanos(group.CreatedAt), importNanos(group.UpdatedAt)); err != nil {
			return fmt.Errorf("import group %s: %w", group.ID, err)
		}
		for position, agentID := range group.Members {
			if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,agent_id,position) VALUES(?,?,?)`, group.ID, agentID, position); err != nil {
				return err
			}
		}
	}
	for _, g := range batch.Groups {
		if !model.ValidGroupCapacity(g.MaxActiveMembers) {
			return app.ErrInvalid
		}
		if g.MaxActiveMembers > 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO group_capacity(group_id,max_active_members) VALUES(?,?)`, g.ID, g.MaxActiveMembers); err != nil {
				return err
			}
		}
		if g.Details != nil {
			if err := model.ValidateGroupDetails(*g.Details); err != nil {
				return app.ErrInvalid
			}
			data, err := json.Marshal(g.Details)
			if err != nil {
				return err
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO group_details(group_id,record) VALUES(?,?)`, g.ID, data); err != nil {
				return err
			}
		}
		if g.ParentGroupID != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO group_parents(group_id,parent_id) VALUES(?,?)`, g.ID, g.ParentGroupID); err != nil {
				return err
			}
		}
	}

	for _, conversation := range batch.Conversations {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conversations(id,revision,created_at,updated_at) VALUES(?,?,?,?)`, conversation.ID, conversation.Revision, importNanos(conversation.CreatedAt), importNanos(conversation.UpdatedAt)); err != nil {
			return fmt.Errorf("import conversation %s: %w", conversation.ID, err)
		}
	}
	for _, link := range batch.ConversationLinks {
		var replaced any
		if link.ReplacedAt != nil {
			replaced = importNanos(*link.ReplacedAt)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_conversations(agent_id,conversation_id,current,revision,associated_at,replaced_at) VALUES(?,?,?,?,?,?)`, link.AgentID, link.ConversationID, link.Current, link.Revision, importNanos(link.AssociatedAt), replaced); err != nil {
			return err
		}
	}
	for _, entry := range batch.History {
		if _, err := tx.ExecContext(ctx, `INSERT INTO history_catalog(conversation_id,harness,source_name,title,workspace_id,workspace_hint,archived,availability,metadata_coverage,content_coverage,source_revision,refreshed_at,modified_at,native_namespace,native_reference,native_observed_at,source_token,source_fingerprint,evidence_provider,evidence_version,evidence_payload,search_text,revision) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			entry.ConversationID, entry.Harness, "offline-v228", entry.Title, entry.WorkspaceID, entry.WorkspaceHint, entry.Archived, entry.Availability, entry.Coverage.Metadata, entry.Coverage.Content, entry.Coverage.SourceRevision, importNanos(entry.Coverage.RefreshedAt), importNanos(entry.ModifiedAt), "offline-v228-metadata", entry.ConversationID, importNanos(entry.ModifiedAt), "", entry.Coverage.SourceRevision, "", 0, nil, "", entry.Revision); err != nil {
			return err
		}
	}
	for _, message := range batch.Messages {
		opID, requestID := importedMessageOperationIDs(message.ID)
		if _, err := tx.ExecContext(ctx, `INSERT INTO operations(id,request_id,request_scope,kind,principal_kind,principal_agent_id,principal_execution_id,principal_generation,principal_automation_run,authority_subject_kind,authority_subject_id,execution_id,state,result_code,detail,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			opID, requestID, "migration:v228", model.OperationSendMessage, message.Sender.Kind, message.Sender.AgentID, "", 0, "", "", "", "", model.OperationSucceeded, "imported_admission", "historical accepted message; no notification replay", 1, importNanos(message.CreatedAt), importNanos(message.CreatedAt)); err != nil {
			return err
		}
		thread := message.ThreadID
		if thread == "" {
			thread = message.ID
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO messages(id,operation_id,sender_kind,sender_agent_id,sender_execution_id,sender_generation,sender_automation_run,sender_authority_subject_kind,sender_authority_subject_id,sender_conversation_id,subject,parent_message_id,thread_id,request_digest,body,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			message.ID, opID, message.Sender.Kind, message.Sender.AgentID, "", 0, "", "", "", message.SenderConversationID, message.Subject, message.ParentMessageID, thread, requestID, message.Body, importNanos(message.CreatedAt)); err != nil {
			return err
		}
		for _, recipient := range message.Recipients {
			var readAt, notifiedAt any
			if recipient.ReadAt != nil {
				readAt = importNanos(*recipient.ReadAt)
			}
			if recipient.NotifiedAt != nil {
				notifiedAt = importNanos(*recipient.NotifiedAt)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO message_recipients(id,message_id,address_kind,agent_id,audience_kind,read_at,notification_intent,notification_outcome,notification_detail,notified_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, recipient.ID, message.ID, recipient.AddressKind, recipient.AgentID, recipient.Audience, readAt, recipient.NotificationIntent, recipient.NotificationOutcome, recipient.NotificationDetail, notifiedAt); err != nil {
				return err
			}
		}
	}
	for _, envelope := range batch.MessageEnvelopes {
		data, err := json.Marshal(envelope)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO imported_message_envelopes(message_id,source_table,source_key,record) VALUES(?,?,?,?)`, envelope.MessageID, envelope.SourceTable, envelope.SourceKey, data); err != nil {
			return err
		}
	}
	for _, imported := range batch.ImportedAttachments {
		attachment := imported.Attachment
		if _, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,owner_kind,owner_agent_id,owner_execution_id,owner_generation,owner_automation_run,filename,media_type,size,sha256,content,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, attachment.ID, model.PrincipalOperator, "", "", 0, "", attachment.Filename, attachment.MediaType, attachment.Size, attachment.SHA256, imported.Content, importNanos(attachment.CreatedAt)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_attachments(message_id,attachment_id,position) VALUES(?,?,?)`, imported.MessageID, attachment.ID, imported.Position); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO imported_attachment_availability(attachment_id,source_table,source_key,availability,loss_reason) VALUES(?,?,?,?,?)`, attachment.ID, imported.SourceTable, imported.SourceKey, imported.Availability, imported.LossReason); err != nil {
			return err
		}
	}
	for _, result := range batch.ConfigurationProfiles {
		profileJSON, _ := json.Marshal(result.Profile)
		revisionJSON, _ := json.Marshal(result.Revision)
		if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_profiles(id,record) VALUES(?,?)`, result.Profile.ID, profileJSON); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_profile_revisions(profile_id,revision_id,record) VALUES(?,?,?)`, result.Profile.ID, result.Revision.Ref.RevisionID, revisionJSON); err != nil {
			return err
		}
	}
	if err := applyImportedSandboxProfiles(ctx, tx, batch.SandboxProfiles); err != nil {
		return err
	}
	if err := applyImportedSandboxDefaults(ctx, tx, batch.SandboxDefaults); err != nil {
		return err
	}
	if err := applyImportedGroupConfigurations(ctx, tx, batch.GroupConfigurations); err != nil {
		return err
	}
	if batch.ConfigurationDefaults != nil {
		data, err := json.Marshal(batch.ConfigurationDefaults)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO configuration_defaults(id,record) VALUES(1,?)`, data); err != nil {
			return err
		}
	}
	for _, record := range batch.Definitions {
		definition, revision := record.Definition, record.Head
		parameters, _ := json.Marshal(revision.Parameters)
		dependencies, _ := json.Marshal(revision.Dependencies)
		author, _ := json.Marshal(revision.Author)
		if _, err := tx.ExecContext(ctx, `INSERT INTO definitions(id,name,kind,head_revision_id,tombstoned,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, definition.ID, definition.Name, definition.Kind, definition.HeadRevisionID, definition.Tombstoned, definition.Revision, importNanos(definition.CreatedAt), importNanos(definition.UpdatedAt)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO definition_revisions(id,definition_id,number,request_scope,request_id,content_hash,schema_version,compiler_version,source,parameters_json,team_json,process_json,dependencies_json,author_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, revision.ID, revision.DefinitionID, revision.Number, "migration:v228", revision.RequestID, revision.ContentHash, revision.SchemaVersion, revision.CompilerVersion, revision.Source, parameters, nil, nil, dependencies, author, importNanos(revision.CreatedAt)); err != nil {
			return err
		}
	}
	for _, record := range batch.AutomationRules {
		rule, revision := record.Rule, record.Head
		owner, _ := json.Marshal(revision.Owner)
		delegation, _ := json.Marshal(revision.Delegation)
		condition, _ := json.Marshal(revision.Condition)
		action, _ := json.Marshal(revision.Action)
		policy, _ := json.Marshal(revision.Policy)
		dependencies, _ := json.Marshal(revision.Dependencies)
		author, _ := json.Marshal(revision.Author)
		if _, err := tx.ExecContext(ctx, `INSERT INTO automation_rules(id,name,head_revision_id,enabled,tombstoned,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?)`, rule.ID, rule.Name, rule.HeadRevisionID, false, rule.Tombstoned, rule.Revision, importNanos(rule.CreatedAt), importNanos(rule.UpdatedAt)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO automation_rule_revisions(id,rule_id,number,request_scope,request_id,content_hash,owner_json,delegation_json,condition_json,action_json,policy_json,dependencies_json,author_json,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, revision.ID, revision.RuleID, revision.Number, "migration:v228", revision.RequestID, revision.ContentHash, owner, delegation, condition, action, policy, dependencies, author, importNanos(revision.CreatedAt)); err != nil {
			return err
		}
	}
	for _, workspace := range batch.Workspaces {
		intent, _ := json.Marshal(workspace.Intent)
		observation, _ := json.Marshal(workspace.Observation)
		if _, err := tx.ExecContext(ctx, `INSERT INTO workspaces(id,intent_json,state,observation_json,resource_owner,resource_version,resource_payload,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, workspace.ID, intent, workspace.State, observation, "", 0, nil, workspace.Revision, importNanos(workspace.CreatedAt), importNanos(workspace.UpdatedAt)); err != nil {
			return err
		}
	}
	for _, write := range batch.Usage {
		observation := write.Observation
		counters, _ := json.Marshal(observation.Counters)
		var amount, currency, costKind any
		if observation.Cost != nil {
			amount, currency, costKind = observation.Cost.Amount, observation.Cost.Currency, observation.Cost.Kind
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO usage_observations(id,agent_id,conversation_id,execution_id,attribution_precision,harness,source_key,source,source_revision,observed_at,collected_at,counters_json,cost_amount,cost_currency,cost_kind,counter_coverage,cost_coverage,coverage_reason,cumulative,historical,provenance) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, observation.ID, observation.Attribution.AgentID, observation.Attribution.ConversationID, observation.Attribution.ExecutionID, observation.Attribution.Precision, observation.Harness, write.SourceKey, observation.Source, observation.SourceRevision, importNanos(observation.ObservedAt), importNanos(observation.CollectedAt), counters, amount, currency, costKind, observation.Coverage.Counters, observation.Coverage.Cost, observation.Coverage.Reason, write.Cumulative, true, observation.Provenance); err != nil {
			return err
		}
	}
	for _, write := range batch.Activity {
		record := write.Record
		var finished any
		if record.FinishedAt != nil {
			finished = importNanos(*record.FinishedAt)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO historical_activity(id,source_key,source_revision,kind,actor_kind,actor_agent_id,actor_execution_id,actor_automation_run,agent_id,conversation_id,execution_id,work_run_id,outcome,reason,started_at,finished_at,provenance) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, record.ID, write.SourceKey, write.SourceRevision, record.Kind, record.Actor.Kind, record.Actor.AgentID, record.Actor.ExecutionID, record.Actor.AutomationRun, record.AgentID, record.ConversationID, record.ExecutionID, record.WorkRunID, record.Outcome, record.Reason, importNanos(record.StartedAt), finished, record.Provenance); err != nil {
			return err
		}
	}
	return nil
}

func importedMessageOperationIDs(id model.MessageID) (string, string) {
	sum := sha256.Sum256([]byte("offline-v228-message\x00" + string(id)))
	token := hex.EncodeToString(sum[:12])
	return "op_import_" + token, "req_import_" + token
}

func importNanos(value time.Time) int64 {
	if value.IsZero() {
		return 0
	}
	return value.UTC().UnixNano()
}

func (s *Store) ImportReceipt(ctx context.Context) (model.ImportReceipt, error) {
	return queryImportReceipt(ctx, s.db)
}

type importQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryImportReceipt(ctx context.Context, queryer importQueryer) (model.ImportReceipt, error) {
	var out model.ImportReceipt
	var counts []byte
	var completedAt int64
	err := queryer.QueryRowContext(ctx, `SELECT id,source_schema_version,source_database_sha256,manifest_sha256,importer_format_version,plan_format_version,target_schema_version,plan_sha256,semantic_sha256,metadata_only_attachments,counts_json,completed_at FROM import_receipts LIMIT 1`).Scan(
		&out.ID, &out.SourceSchemaVersion, &out.SourceDatabaseSHA256, &out.ManifestSHA256, &out.ImporterFormatVersion, &out.PlanFormatVersion, &out.TargetSchemaVersion, &out.PlanSHA256, &out.SemanticSHA256, &out.MetadataOnlyAttachments, &counts, &completedAt)
	if err != nil {
		return out, classify(err)
	}
	out.CompletedAt = fromNanos(completedAt)
	err = json.Unmarshal(counts, &out.Counts)
	return out, err
}

func (s *Store) ImportReport(ctx context.Context) (model.ImportReport, error) {
	return ReadImportReport(ctx, s.db)
}

// ReadImportReport reads the redacted report through a consistent read-only
// transaction. Callers that only have a database path should open it with
// mode=ro and query_only before passing the connection here.
func ReadImportReport(ctx context.Context, db *sql.DB) (model.ImportReport, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.ImportReport{}, err
	}
	defer func() { _ = tx.Rollback() }()
	report := model.ImportReport{}
	report.Receipt, err = queryImportReceipt(ctx, tx)
	if err != nil {
		return model.ImportReport{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT source_namespace,source_table,source_key,target_kind,target_id FROM import_id_map ORDER BY source_namespace,source_table,source_key,target_kind`)
	if err != nil {
		return model.ImportReport{}, err
	}
	for rows.Next() {
		var mapping model.ImportIDMapping
		if err := rows.Scan(&mapping.SourceNamespace, &mapping.SourceTable, &mapping.SourceKey, &mapping.TargetKind, &mapping.TargetID); err != nil {
			_ = rows.Close()
			return model.ImportReport{}, err
		}
		report.IDMappings = append(report.IDMappings, mapping)
	}
	if err := rows.Close(); err != nil {
		return model.ImportReport{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT severity,code,source_table,source_key,source_path,detail FROM imported_diagnostics ORDER BY ordinal`)
	if err != nil {
		return model.ImportReport{}, err
	}
	for rows.Next() {
		var diagnostic model.ImportedDiagnostic
		if err := rows.Scan(&diagnostic.Severity, &diagnostic.Code, &diagnostic.SourceTable, &diagnostic.SourceKey, &diagnostic.SourcePath, &diagnostic.Detail); err != nil {
			_ = rows.Close()
			return model.ImportReport{}, err
		}
		report.Diagnostics = append(report.Diagnostics, diagnostic)
	}
	if err := rows.Close(); err != nil {
		return model.ImportReport{}, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT source_table,source_key,source_path,class,conversion,reason_code,payload_sha256 FROM imported_source_records ORDER BY source_table,source_key`)
	if err != nil {
		return model.ImportReport{}, err
	}
	for rows.Next() {
		var record model.ImportedRecordDisposition
		if err := rows.Scan(&record.SourceTable, &record.SourceKey, &record.SourcePath, &record.Class, &record.Conversion, &record.ReasonCode, &record.PayloadSHA256); err != nil {
			_ = rows.Close()
			return model.ImportReport{}, err
		}
		report.Records = append(report.Records, record)
	}
	if err := rows.Close(); err != nil {
		return model.ImportReport{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.ImportReport{}, err
	}
	return report, nil
}

func (s *Store) ImportedSourceRecords(ctx context.Context, table string) ([]model.ImportedSourceRecord, error) {
	query := `SELECT source_table,source_key,source_path,class,conversion,reason_code,payload,payload_sha256 FROM imported_source_records`
	args := []any{}
	if strings.TrimSpace(table) != "" {
		query += ` WHERE source_table=?`
		args = append(args, table)
	}
	query += ` ORDER BY source_table,source_key`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ImportedSourceRecord
	for rows.Next() {
		var record model.ImportedSourceRecord
		if err := rows.Scan(&record.SourceTable, &record.SourceKey, &record.SourcePath, &record.Class, &record.Conversion, &record.ReasonCode, &record.Payload, &record.PayloadSHA256); err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func (s *Store) ImportedMessageEnvelope(ctx context.Context, id model.MessageID) (model.ImportedMessageEnvelope, error) {
	var data []byte
	if err := s.db.QueryRowContext(ctx, `SELECT record FROM imported_message_envelopes WHERE message_id=?`, id).Scan(&data); err != nil {
		return model.ImportedMessageEnvelope{}, classify(err)
	}
	var envelope model.ImportedMessageEnvelope
	err := json.Unmarshal(data, &envelope)
	return envelope, err
}

func (s *Store) ImportedAttachment(ctx context.Context, id model.AttachmentID) (model.ImportedAttachment, error) {
	var out model.ImportedAttachment
	var created int64
	err := s.db.QueryRowContext(ctx, `SELECT a.id,v.source_table,v.source_key,ma.message_id,ma.position,v.availability,v.loss_reason,a.filename,a.media_type,a.size,a.sha256,a.created_at,a.content FROM attachments a JOIN imported_attachment_availability v ON v.attachment_id=a.id JOIN message_attachments ma ON ma.attachment_id=a.id WHERE a.id=?`, id).Scan(&out.AttachmentID, &out.SourceTable, &out.SourceKey, &out.MessageID, &out.Position, &out.Availability, &out.LossReason, &out.Attachment.Filename, &out.Attachment.MediaType, &out.Attachment.Size, &out.Attachment.SHA256, &created, &out.Content)
	if err != nil {
		return out, classify(err)
	}
	out.Attachment.ID = out.AttachmentID
	out.Attachment.CreatedAt = fromNanos(created)
	return out, nil
}

func IsMissingImportReceipt(err error) bool { return errors.Is(err, app.ErrNotFound) }

// VerifyImport exercises the same typed store reads used by the application;
// raw import row counts alone are not semantic conversion evidence.
func (s *Store) VerifyImport(ctx context.Context, batch app.ImportBatch) error {
	receipt, err := s.ImportReceipt(ctx)
	if err != nil {
		return err
	}
	if receipt.SemanticSHA256 != batch.Receipt.SemanticSHA256 || !reflect.DeepEqual(receipt.Counts, batch.Receipt.Counts) {
		return fmt.Errorf("import receipt does not match translated batch")
	}
	expectedReport := model.ImportReport{Receipt: receipt, IDMappings: batch.IDMappings, Diagnostics: batch.Diagnostics}
	for _, record := range batch.SourceRecords {
		expectedReport.Records = append(expectedReport.Records, model.ImportedRecordDisposition{
			SourceTable: record.SourceTable, SourceKey: record.SourceKey, SourcePath: record.SourcePath,
			Class: record.Class, Conversion: record.Conversion, ReasonCode: record.ReasonCode, PayloadSHA256: record.PayloadSHA256,
		})
	}
	report, err := s.ImportReport(ctx)
	if err != nil || !reflect.DeepEqual(report, expectedReport) {
		return fmt.Errorf("verify imported report: %w", err)
	}
	sourceRecords, err := s.ImportedSourceRecords(ctx, "")
	if err != nil || !reflect.DeepEqual(sourceRecords, batch.SourceRecords) {
		return fmt.Errorf("verify retained source records: %w", err)
	}
	if err := s.verifyImportCounts(ctx, batch); err != nil {
		return err
	}
	if err := s.verifyImportedOperations(ctx, batch.Messages); err != nil {
		return err
	}
	for _, expected := range batch.Agents {
		actual, err := s.Agent(ctx, expected.ID)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported agent %s: %w", expected.ID, err)
		}
	}
	for _, expected := range batch.Groups {
		actual, err := s.Group(ctx, expected.ID)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported group %s: actual=%#v expected=%#v: %w", expected.ID, actual, expected, err)
		}
	}
	for _, expected := range batch.Messages {
		actual, err := s.message(ctx, expected.ID)
		// Empty legacy sender authority is inert provenance. The shared scanner's
		// empty subject representation is not part of message identity.
		actual.Sender.Authority = model.AuthoritySubject{}
		expected.Sender.Authority = model.AuthoritySubject{}
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported message %s: actual=%#v expected=%#v: %w", expected.ID, actual, expected, err)
		}
	}
	for _, expected := range batch.MessageEnvelopes {
		actual, err := s.ImportedMessageEnvelope(ctx, expected.MessageID)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported message envelope %s: %w", expected.MessageID, err)
		}
	}
	for _, expected := range batch.ImportedAttachments {
		actual, err := s.ImportedAttachment(ctx, expected.AttachmentID)
		if len(actual.Content) == 0 && len(expected.Content) == 0 {
			actual.Content, expected.Content = nil, nil
		}
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported attachment %s: actual=%#v expected=%#v: %v", expected.AttachmentID, actual, expected, err)
		}
		var ownerMatches int
		err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM attachments WHERE id=? AND owner_kind=? AND owner_agent_id='' AND owner_execution_id='' AND owner_generation=0 AND owner_automation_run=''`, expected.AttachmentID, model.PrincipalOperator).Scan(&ownerMatches)
		if err != nil || ownerMatches != 1 {
			return fmt.Errorf("verify imported attachment %s inert owner fields: %w", expected.AttachmentID, err)
		}
	}
	for _, expected := range batch.ConfigurationProfiles {
		actual, err := s.ConfigurationProfile(ctx, expected.Profile.ID, expected.Revision.Ref.RevisionID)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported configuration profile %s: %w", expected.Profile.ID, err)
		}
	}
	if err := s.verifyImportedSandboxProfiles(ctx, batch.SandboxProfiles); err != nil {
		return err
	}
	if batch.SandboxDefaults != nil {
		actual, err := s.SandboxDefaults(ctx)
		if err != nil || !reflect.DeepEqual(actual, *batch.SandboxDefaults) {
			return fmt.Errorf("verify imported sandbox defaults: %v", err)
		}
	}
	if err := s.verifyImportedGroupConfigurations(ctx, batch.GroupConfigurations); err != nil {
		return err
	}
	if batch.ConfigurationDefaults != nil {
		actual, err := s.ConfigurationDefaults(ctx)
		if err != nil || !reflect.DeepEqual(actual, *batch.ConfigurationDefaults) {
			return fmt.Errorf("verify imported configuration defaults: %w", err)
		}
	}
	for _, expected := range batch.Definitions {
		actual, err := s.Definition(ctx, expected.Definition.ID)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported definition %s: %w", expected.Definition.ID, err)
		}
	}
	for _, expected := range batch.AutomationRules {
		actual, err := s.AutomationRule(ctx, expected.Rule.ID)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported automation rule %s: %w", expected.Rule.ID, err)
		}
	}
	for _, expected := range batch.Workspaces {
		actual, err := s.Workspace(ctx, expected.ID)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported workspace %s: %w", expected.ID, err)
		}
	}
	for _, expected := range batch.Usage {
		actual, cumulative, err := s.usageBySourceRevision(ctx, expected.SourceKey, expected.Observation.SourceRevision)
		if err != nil || cumulative != expected.Cumulative || !reflect.DeepEqual(actual, expected.Observation) {
			return fmt.Errorf("verify imported usage %s: %w", expected.Observation.ID, err)
		}
	}
	for _, expected := range batch.Activity {
		actual, err := s.historicalActivity(ctx, expected.SourceKey, expected.SourceRevision)
		if err != nil || !reflect.DeepEqual(actual, expected.Record) {
			return fmt.Errorf("verify imported activity %s: %w", expected.Record.ID, err)
		}
	}
	for _, expected := range batch.Conversations {
		var actual model.Conversation
		var created, updated int64
		err := s.db.QueryRowContext(ctx, `SELECT id,revision,created_at,updated_at FROM conversations WHERE id=?`, expected.ID).Scan(&actual.ID, &actual.Revision, &created, &updated)
		actual.CreatedAt, actual.UpdatedAt = fromNanos(created), fromNanos(updated)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported conversation %s: %w", expected.ID, err)
		}
	}
	for _, expected := range batch.ConversationLinks {
		var actual model.ConversationAssociation
		var associated int64
		var replaced sql.NullInt64
		err := s.db.QueryRowContext(ctx, `SELECT agent_id,conversation_id,current,revision,associated_at,replaced_at FROM agent_conversations WHERE agent_id=? AND conversation_id=?`, expected.AgentID, expected.ConversationID).Scan(&actual.AgentID, &actual.ConversationID, &actual.Current, &actual.Revision, &associated, &replaced)
		actual.AssociatedAt = fromNanos(associated)
		if replaced.Valid {
			value := fromNanos(replaced.Int64)
			actual.ReplacedAt = &value
		}
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported conversation association %s/%s: %w", expected.AgentID, expected.ConversationID, err)
		}
	}
	for _, expected := range batch.History {
		actual, err := scanHistoryEntry(s.db.QueryRowContext(ctx, `SELECT conversation_id,harness,title,workspace_id,workspace_hint,archived,availability,metadata_coverage,content_coverage,source_revision,refreshed_at,modified_at,revision FROM history_catalog WHERE conversation_id=?`, expected.ConversationID))
		if err != nil || !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("verify imported history %s: %w", expected.ConversationID, err)
		}
		var provenanceMatches int
		err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM history_catalog WHERE conversation_id=? AND source_name='offline-v228' AND native_namespace='offline-v228-metadata' AND native_reference=? AND native_observed_at=? AND source_token='' AND source_fingerprint=? AND evidence_provider='' AND evidence_version=0 AND evidence_payload IS NULL AND search_text=''`,
			expected.ConversationID, expected.ConversationID, importNanos(expected.ModifiedAt), expected.Coverage.SourceRevision).Scan(&provenanceMatches)
		if err != nil || provenanceMatches != 1 {
			return fmt.Errorf("verify imported history %s provenance fields: %w", expected.ConversationID, err)
		}
	}
	var integrity string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		return fmt.Errorf("verify imported database integrity: %s: %w", integrity, err)
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA foreign_key_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		return fmt.Errorf("imported database has foreign-key violations")
	}
	return rows.Err()
}

// VerifyImportDatabase performs exact import verification without Store
// initialization. The supplied connection may be opened mode=ro/query_only.
func VerifyImportDatabase(ctx context.Context, db *sql.DB, batch app.ImportBatch) error {
	return (&Store{db: db}).VerifyImport(ctx, batch)
}

func (s *Store) verifyImportCounts(ctx context.Context, batch app.ImportBatch) error {
	memberCount, recipientCount, parentCount, detailsCount, capacityCount := 0, 0, 0, 0, 0
	for _, group := range batch.Groups {
		memberCount += len(group.Members)
		if group.MaxActiveMembers > 0 {
			capacityCount++
		}
		if group.Details != nil {
			detailsCount++
		}
		if group.ParentGroupID != "" {
			parentCount++
		}
	}
	for _, message := range batch.Messages {
		recipientCount += len(message.Recipients)
	}
	defaults := 0
	if batch.ConfigurationDefaults != nil {
		defaults = 1
	}
	sandboxDefaults := 0
	if batch.SandboxDefaults != nil {
		sandboxDefaults = 1
	}
	expected := map[string]int{
		"sandbox_defaults": sandboxDefaults,
		"agents":           len(batch.Agents), "groups": len(batch.Groups), "group_members": memberCount, "group_parents": parentCount, "group_details": detailsCount, "group_capacity": capacityCount,
		"conversations": len(batch.Conversations), "agent_conversations": len(batch.ConversationLinks), "history_catalog": len(batch.History),
		"messages": len(batch.Messages), "operations": len(batch.Messages), "message_recipients": recipientCount,
		"attachments": len(batch.ImportedAttachments), "message_attachments": len(batch.ImportedAttachments),
		"import_id_map": len(batch.IDMappings), "imported_source_records": len(batch.SourceRecords),
		"imported_diagnostics": len(batch.Diagnostics), "imported_message_envelopes": len(batch.MessageEnvelopes),
		"imported_attachment_availability": len(batch.ImportedAttachments),
		"sandbox_profiles":                 len(batch.SandboxProfiles), "sandbox_profile_revisions": len(batch.SandboxProfiles),
		"configuration_profiles": len(batch.ConfigurationProfiles), "configuration_profile_revisions": len(batch.ConfigurationProfiles),
		"group_configurations": len(batch.GroupConfigurations), "configuration_defaults": defaults, "definitions": len(batch.Definitions), "definition_revisions": len(batch.Definitions),
		"automation_rules": len(batch.AutomationRules), "automation_rule_revisions": len(batch.AutomationRules),
		"workspaces": len(batch.Workspaces), "usage_observations": len(batch.Usage), "historical_activity": len(batch.Activity),
	}
	for _, table := range []string{
		"shell_requests", "group_clone_requests", "group_member_requests", "group_parent_requests", "executions", "release_permits", "attachment_claims", "execution_accesses", "authority_grants", "role_assignments",
		"operation_authority", "operation_additional_authority", "effect_permits", "pending_context_transitions", "native_binding_history",
		"history_refreshes", "history_metadata_requests", "history_points", "history_use_claims", "workspace_uses",
		"work_runs", "work_attempts", "work_evidence", "work_decisions", "program_profiles", "program_profile_revisions",
		"sandbox_profile_requests", "sandbox_defaults_requests",
		"work_node_attempts", "work_agent_interactions", "work_node_evidence", "decision_windows", "decision_submissions", "automation_occurrences",
		"automation_occurrence_recipients", "automation_condition_state", "team_deployments", "team_continuations", "configuration_defaults_requests",
		"configuration_profile_requests", "configuration_profile_lifecycle_requests", "configuration_bundle_requests", "automation_state_requests", "automation_archive_requests", "definition_archive_requests", "group_disband_requests", "access_requests", "access_request_decisions", "message_notifications", "presentation_preferences", "terminal_files", "process_snippets", "process_snippet_requests",
		"automation_product_facts", "team_rebriefs", "team_lifecycle_requests",
	} {
		expected[table] = 0
	}
	for table, want := range expected {
		var got int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM `+table).Scan(&got); err != nil {
			return fmt.Errorf("count imported %s: %w", table, err)
		}
		if got != want {
			return fmt.Errorf("verify imported %s count: got %d want %d", table, got, want)
		}
	}
	var backendRevision int
	if err := s.db.QueryRowContext(ctx, `SELECT revision FROM backend_meta WHERE singleton=1`).Scan(&backendRevision); err != nil {
		return fmt.Errorf("read imported backend revision: %w", err)
	}
	if backendRevision != 1 {
		return fmt.Errorf("verify imported backend revision: got %d want 1", backendRevision)
	}
	return nil
}

func (s *Store) verifyImportedOperations(ctx context.Context, messages []model.Message) error {
	for _, message := range messages {
		operationID, requestID := importedMessageOperationIDs(message.ID)
		var matches int
		err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM operations o JOIN messages m ON m.operation_id=o.id WHERE
			o.id=? AND m.id=? AND m.request_digest=? AND o.request_id=? AND o.request_scope='migration:v228' AND o.kind=?
			AND o.principal_kind=? AND o.principal_agent_id=? AND o.principal_execution_id='' AND o.principal_generation=0
			AND o.principal_automation_run='' AND o.automation_delegation_json IS NULL AND o.authority_subject_kind=''
			AND o.authority_subject_id='' AND o.execution_id='' AND o.initial_message_digest='' AND o.state=? AND o.result_code='imported_admission'
			AND o.detail='historical accepted message; no notification replay' AND o.revision=1 AND o.created_at=? AND o.updated_at=?`,
			operationID, message.ID, requestID, requestID, model.OperationSendMessage, message.Sender.Kind, message.Sender.AgentID,
			model.OperationSucceeded, importNanos(message.CreatedAt), importNanos(message.CreatedAt)).Scan(&matches)
		if err != nil {
			return fmt.Errorf("verify imported operation %s: %w", operationID, err)
		}
		if matches != 1 {
			return fmt.Errorf("verify imported operation %s semantic fields", operationID)
		}
	}
	return nil
}

var _ app.ImportStore = (*Store)(nil)
