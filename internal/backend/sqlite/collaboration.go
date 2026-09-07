package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Store) MessageByRequest(ctx context.Context, sender model.Principal, requestID model.RequestID, digest string) (app.MessageAdmissionResult, bool, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return app.MessageAdmissionResult{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, ok, err := messageByRequestDigest(ctx, tx, sender, requestID, digest)
	if err != nil || !ok {
		return result, ok, err
	}
	if err := tx.Commit(); err != nil {
		return app.MessageAdmissionResult{}, false, err
	}
	return result, true, nil
}

func messageByRequestDigest(ctx context.Context, tx *sql.Tx, sender model.Principal, requestID model.RequestID, digest string) (app.MessageAdmissionResult, bool, error) {
	operation, err := operationByRequestTx(ctx, tx, sender, requestID)
	if errors.Is(err, app.ErrNotFound) {
		return app.MessageAdmissionResult{}, false, nil
	}
	if err != nil {
		return app.MessageAdmissionResult{}, false, err
	}
	if operation.Kind != model.OperationSendMessage || !sameRequester(operation.Principal, sender) {
		return app.MessageAdmissionResult{}, false, app.ErrConflict
	}
	var messageID model.MessageID
	var storedDigest string
	if err := tx.QueryRowContext(ctx, `SELECT id,request_digest FROM messages WHERE operation_id=?`, operation.ID).Scan(&messageID, &storedDigest); err != nil {
		return app.MessageAdmissionResult{}, false, classify(err)
	}
	if storedDigest == "" || storedDigest != digest {
		return app.MessageAdmissionResult{}, false, app.ErrConflict
	}
	message, err := messageTx(ctx, tx, messageID)
	if err != nil {
		return app.MessageAdmissionResult{}, false, err
	}
	return app.MessageAdmissionResult{Operation: operation, Message: message, Repeated: true}, true, nil
}

func messageTx(ctx context.Context, q queryer, id model.MessageID) (model.Message, error) {
	var message model.Message
	var created int64
	var authorityKind, authorityID string
	err := q.QueryRowContext(ctx, `SELECT id,sender_kind,sender_agent_id,sender_execution_id,sender_generation,sender_automation_run,sender_authority_subject_kind,sender_authority_subject_id,sender_conversation_id,subject,parent_message_id,thread_id,body,created_at FROM messages WHERE id=?`, id).Scan(&message.ID, &message.Sender.Kind, &message.Sender.AgentID, &message.Sender.ExecutionID, &message.Sender.Generation, &message.Sender.AutomationRun, &authorityKind, &authorityID, &message.SenderConversationID, &message.Subject, &message.ParentMessageID, &message.ThreadID, &message.Body, &created)
	if err != nil {
		return message, classify(err)
	}
	message.CreatedAt = fromNanos(created)
	message.Sender.Authority = makeSubject(authorityKind, authorityID)
	if message.ThreadID == "" {
		message.ThreadID = message.ID
	}
	rows, err := q.QueryContext(ctx, `SELECT id,address_kind,agent_id,audience_kind,read_at,notification_intent,notification_outcome,notification_detail,notified_at FROM message_recipients WHERE message_id=? ORDER BY rowid`, id)
	if err != nil {
		return message, err
	}
	for rows.Next() {
		var recipient model.MessageRecipient
		var read, notified sql.NullInt64
		if err := rows.Scan(&recipient.ID, &recipient.AddressKind, &recipient.AgentID, &recipient.Audience, &read, &recipient.NotificationIntent, &recipient.NotificationOutcome, &recipient.NotificationDetail, &notified); err != nil {
			rows.Close()
			return message, err
		}
		if read.Valid {
			value := fromNanos(read.Int64)
			recipient.ReadAt = &value
		}
		if notified.Valid {
			value := fromNanos(notified.Int64)
			recipient.NotifiedAt = &value
		}
		message.Recipients = append(message.Recipients, recipient)
	}
	if err := rows.Close(); err != nil {
		return message, err
	}
	rows, err = q.QueryContext(ctx, `SELECT a.id,a.filename,a.media_type,a.size,a.sha256,a.created_at FROM message_attachments ma JOIN attachments a ON a.id=ma.attachment_id WHERE ma.message_id=? ORDER BY ma.position`, id)
	if err != nil {
		return message, err
	}
	for rows.Next() {
		var attachment model.Attachment
		var attachmentCreated int64
		if err := rows.Scan(&attachment.ID, &attachment.Filename, &attachment.MediaType, &attachment.Size, &attachment.SHA256, &attachmentCreated); err != nil {
			rows.Close()
			return message, err
		}
		attachment.CreatedAt = fromNanos(attachmentCreated)
		message.Attachments = append(message.Attachments, attachment)
	}
	return message, rows.Close()
}

func (s *Store) CreateAttachmentClaim(ctx context.Context, claim model.AttachmentClaim, content []byte) error {
	if int64(len(content)) != claim.Attachment.Size {
		return app.ErrConflict
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != claim.Attachment.SHA256 {
		return app.ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if claim.Owner.Kind != model.PrincipalOperator {
		if _, err := authoritySubject(ctx, tx, claim.Owner, claim.CreatedAt); err != nil {
			return app.ErrUnauthorized
		}
	}
	if claim.Owner.Kind == model.PrincipalAutomation {
		// Preparing private bytes does not select the final audience. Require
		// at least one currently authorized delegated message destination;
		// message admission later checks every concrete destination atomically.
		if err := requireDelegatedAttachmentAction(ctx, tx, claim.Owner, model.ActionSendMessage, claim.Owner.Delegation.Resources, claim.CreatedAt); err != nil {
			return err
		}
	}
	if err := insertAttachment(ctx, tx, claim.Attachment, claim.Owner, content); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO attachment_claims(id,attachment_id,expires_at,created_at) VALUES(?,?,?,?)`, claim.ID, claim.Attachment.ID, nanos(claim.ExpiresAt), nanos(claim.CreatedAt)); err != nil {
		return classify(err)
	}
	if err := bumpTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

// PurgeExpiredAttachmentClaims owns cleanup of content that was never attached
// to a committed message. Committed attachments are intentionally unaffected.
func (s *Store) PurgeExpiredAttachmentClaims(ctx context.Context, before time.Time) (int64, error) {
	result, err := s.db.ExecContext(ctx, `DELETE FROM attachments WHERE id IN (SELECT attachment_id FROM attachment_claims WHERE consumed_message_id IS NULL AND expires_at<=?)`, nanos(before))
	if err != nil {
		return 0, err
	}
	count, err := result.RowsAffected()
	if err != nil || count == 0 {
		return count, err
	}
	return count, s.bump(ctx)
}

func insertMessageAttachments(ctx context.Context, tx *sql.Tx, in app.MessageAdmission) error {
	for position, item := range in.Attachments {
		if item.ClaimID != "" {
			var attachment model.Attachment
			var owner model.Principal
			var expires int64
			var consumed sql.NullString
			err := tx.QueryRowContext(ctx, `SELECT a.id,a.owner_kind,a.owner_agent_id,a.owner_execution_id,a.owner_generation,a.owner_automation_run,a.filename,a.media_type,a.size,a.sha256,a.created_at,c.expires_at,c.consumed_message_id FROM attachment_claims c JOIN attachments a ON a.id=c.attachment_id WHERE c.id=?`, item.ClaimID).Scan(&attachment.ID, &owner.Kind, &owner.AgentID, &owner.ExecutionID, &owner.Generation, &owner.AutomationRun, &attachment.Filename, &attachment.MediaType, &attachment.Size, &attachment.SHA256, new(int64), &expires, &consumed)
			if err != nil {
				return classify(err)
			}
			if consumed.Valid || expires <= nanos(in.Message.CreatedAt) || requestScope(owner) != requestScope(in.Message.Sender) || attachment.ID != item.Attachment.ID || attachment.Filename != item.Attachment.Filename || attachment.MediaType != item.Attachment.MediaType || attachment.Size != item.Attachment.Size || attachment.SHA256 != item.Attachment.SHA256 {
				return app.ErrConflict
			}
			if _, err := tx.ExecContext(ctx, `UPDATE attachment_claims SET consumed_message_id=? WHERE id=? AND consumed_message_id IS NULL`, in.Message.ID, item.ClaimID); err != nil {
				return err
			}
		} else {
			if err := insertAttachment(ctx, tx, item.Attachment, in.Message.Sender, item.Content); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO message_attachments(message_id,attachment_id,position) VALUES(?,?,?)`, in.Message.ID, item.Attachment.ID, position); err != nil {
			return classify(err)
		}
	}
	return nil
}

func insertAttachment(ctx context.Context, tx *sql.Tx, attachment model.Attachment, owner model.Principal, content []byte) error {
	if int64(len(content)) != attachment.Size {
		return app.ErrConflict
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != attachment.SHA256 {
		return app.ErrConflict
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO attachments(id,owner_kind,owner_agent_id,owner_execution_id,owner_generation,owner_automation_run,filename,media_type,size,sha256,content,created_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, attachment.ID, owner.Kind, owner.AgentID, owner.ExecutionID, owner.Generation, owner.AutomationRun, attachment.Filename, attachment.MediaType, attachment.Size, attachment.SHA256, content, nanos(attachment.CreatedAt))
	return classify(err)
}

func (s *Store) AttachmentContent(ctx context.Context, id model.AttachmentID, principal model.Principal, at time.Time) (model.Attachment, []byte, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.Attachment{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var attachment model.Attachment
	var owner model.Principal
	var created int64
	var content []byte
	err = tx.QueryRowContext(ctx, `SELECT id,owner_kind,owner_agent_id,owner_execution_id,owner_generation,owner_automation_run,filename,media_type,size,sha256,content,created_at FROM attachments WHERE id=?`, id).Scan(&attachment.ID, &owner.Kind, &owner.AgentID, &owner.ExecutionID, &owner.Generation, &owner.AutomationRun, &attachment.Filename, &attachment.MediaType, &attachment.Size, &attachment.SHA256, &content, &created)
	if err != nil {
		return model.Attachment{}, nil, classify(err)
	}
	attachment.CreatedAt = fromNanos(created)
	allowed := principal.Kind == model.PrincipalOperator
	if !allowed {
		if _, err := authoritySubject(ctx, tx, principal, at); err != nil {
			return model.Attachment{}, nil, app.ErrUnauthorized
		}
		allowed = requestScope(owner) == requestScope(principal) || owner.AgentID != "" && owner.AgentID == principal.AgentID
		if !allowed {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message_attachments ma JOIN message_recipients mr ON mr.message_id=ma.message_id WHERE ma.attachment_id=? AND mr.address_kind=? AND mr.agent_id=?`, id, model.MessageAddressAgent, principal.AgentID).Scan(&count); err != nil {
				return model.Attachment{}, nil, err
			}
			allowed = count != 0
		}
	}
	if allowed && principal.Kind == model.PrincipalAutomation {
		resources := []model.ResourceSelector{}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT mr.address_kind,mr.agent_id FROM message_attachments ma JOIN message_recipients mr ON mr.message_id=ma.message_id WHERE ma.attachment_id=?`, id)
		if err != nil {
			return model.Attachment{}, nil, err
		}
		for rows.Next() {
			var kind model.MessageAddressKind
			var agentID model.AgentID
			if err := rows.Scan(&kind, &agentID); err != nil {
				rows.Close()
				return model.Attachment{}, nil, err
			}
			if kind == model.MessageAddressOperator {
				resources = append(resources, model.ResourceSelector{Kind: model.ResourceOperator})
			} else {
				resources = append(resources, model.ResourceSelector{Kind: model.ResourceAgent, AgentID: agentID})
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return model.Attachment{}, nil, err
		}
		if err := rows.Close(); err != nil {
			return model.Attachment{}, nil, err
		}
		// An uncommitted private claim has no message audience yet. Its owner
		// still needs a live explicit read delegation; ownership alone is insufficient.
		if len(resources) == 0 {
			resources = principal.Delegation.Resources
		}
		if err := requireDelegatedAttachmentAction(ctx, tx, principal, model.ActionReadAttachment, resources, at); err != nil {
			return model.Attachment{}, nil, err
		}
	}
	if !allowed {
		return model.Attachment{}, nil, app.ErrUnauthorized
	}
	if err := tx.Commit(); err != nil {
		return model.Attachment{}, nil, err
	}
	return attachment, content, nil
}

func messageAccessible(principal model.Principal, message model.Message) bool {
	if principal.Kind == model.PrincipalOperator || sameRequester(principal, message.Sender) {
		return true
	}
	if principal.AgentID != "" && message.Sender.AgentID == principal.AgentID {
		return true
	}
	for _, recipient := range message.Recipients {
		if recipient.AddressKind == model.MessageAddressAgent && recipient.AgentID == principal.AgentID {
			return true
		}
	}
	return false
}

func (s *Store) RecordMessageNotification(ctx context.Context, recipientID model.RecipientID, outcome model.NotificationOutcome, detail string, at time.Time) (model.Message, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Message{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var messageID model.MessageID
	result, err := tx.ExecContext(ctx, `UPDATE message_recipients SET notification_outcome=?,notification_detail=?,notified_at=? WHERE id=? AND notification_intent=? AND notification_outcome=?`, outcome, detail, nanos(at), recipientID, model.NotificationIfAvailable, model.NotificationPending)
	if err != nil {
		return model.Message{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.Message{}, app.ErrConflict
	}
	if err := tx.QueryRowContext(ctx, `SELECT message_id FROM message_recipients WHERE id=?`, recipientID).Scan(&messageID); err != nil {
		return model.Message{}, classify(err)
	}
	if err := bumpTx(ctx, tx); err != nil {
		return model.Message{}, err
	}
	message, err := messageTx(ctx, tx, messageID)
	if err != nil {
		return model.Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Message{}, err
	}
	return message, nil
}

func (s *Store) migrateMessageRecipients(ctx context.Context) error {
	var ddl string
	if err := s.db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type='table' AND name='message_recipients'`).Scan(&ddl); err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(ddl), "references agents") {
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
	if _, err := tx.ExecContext(ctx, `CREATE TABLE message_recipients_replacement (
 id TEXT PRIMARY KEY, message_id TEXT NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
 address_kind TEXT NOT NULL, agent_id TEXT NOT NULL DEFAULT '', audience_kind TEXT NOT NULL,
 read_at INTEGER, notification_intent TEXT NOT NULL, notification_outcome TEXT NOT NULL,
 notification_detail TEXT NOT NULL DEFAULT '', notified_at INTEGER,
 UNIQUE(message_id,address_kind,agent_id)
)`); err != nil {
		return fmt.Errorf("create message recipient replacement: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO message_recipients_replacement(id,message_id,address_kind,agent_id,audience_kind,read_at,notification_intent,notification_outcome,notification_detail,notified_at)
 SELECT id,message_id,address_kind,agent_id,audience_kind,read_at,
 CASE WHEN notified<>0 THEN 'if_available' ELSE notification_intent END,
 CASE WHEN notified<>0 THEN 'delivered' ELSE notification_outcome END,
	 notification_detail,NULL
 FROM message_recipients`); err != nil {
		return fmt.Errorf("copy message recipients: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE message_recipients`); err != nil {
		return fmt.Errorf("replace message recipients: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `ALTER TABLE message_recipients_replacement RENAME TO message_recipients`); err != nil {
		return fmt.Errorf("rename message recipients: %w", err)
	}
	return tx.Commit()
}

func (s *Store) RetireAgent(ctx context.Context, id model.AgentID, expected model.Revision, principal model.Principal, reason string, at time.Time) (model.Agent, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Agent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var state model.AgentLifecycleState
	var revision model.Revision
	var primary model.ExecutionID
	if err := tx.QueryRowContext(ctx, `SELECT lifecycle_state,revision,primary_execution_id FROM agents WHERE id=?`, id).Scan(&state, &revision, &primary); err != nil {
		return model.Agent{}, classify(err)
	}
	if state != model.AgentActive || revision != expected {
		return model.Agent{}, app.ErrConflict
	}
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: principal, Action: model.ActionRetireAgent, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: id}}, at)
	if err != nil {
		return model.Agent{}, err
	}
	if !decision.Allowed {
		return model.Agent{}, app.ErrUnauthorized
	}
	if primary != "" {
		var executionState model.ExecutionState
		if err := tx.QueryRowContext(ctx, `SELECT state FROM executions WHERE id=?`, primary).Scan(&executionState); err != nil {
			return model.Agent{}, classify(err)
		}
		if executionState != model.ExecutionExited && executionState != model.ExecutionFailed {
			return model.Agent{}, app.ErrConflict
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE agents SET lifecycle_state=?,retired_at=?,retired_by_kind=?,retired_by_agent_id=?,retired_by_execution_id=?,retirement_reason=?,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND lifecycle_state=?`, model.AgentRetired, nanos(at), principal.Kind, principal.AgentID, principal.ExecutionID, reason, nanos(at), id, expected, model.AgentActive)
	if err != nil {
		return model.Agent{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.Agent{}, app.ErrConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE execution_accesses SET state=?,revoked_at=COALESCE(revoked_at,?),revision=revision+1 WHERE agent_id=? AND state NOT IN (?,?)`, model.ExecutionAccessRevoked, nanos(at), id, model.ExecutionAccessRevoked, model.ExecutionAccessExpired); err != nil {
		return model.Agent{}, err
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

func (s *Store) ReactivateAgent(ctx context.Context, id model.AgentID, expected model.Revision, principal model.Principal, at time.Time) (model.Agent, error) {
	if principal.Kind != model.PrincipalOperator {
		return model.Agent{}, app.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Agent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE agents SET lifecycle_state=?,retired_at=NULL,retired_by_kind='',retired_by_agent_id='',retired_by_execution_id='',retirement_reason='',revision=revision+1,updated_at=? WHERE id=? AND revision=? AND lifecycle_state=?`, model.AgentActive, nanos(at), id, expected, model.AgentRetired)
	if err != nil {
		return model.Agent{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.Agent{}, app.ErrConflict
	}
	if err := requireReactivationCapacity(ctx, tx, id); err != nil {
		return model.Agent{}, err
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

// ResolveMessageAudience expands authored agent, group, and role targets into
// the exact active agents that a message effect may pin. It deliberately does
// not treat an empty or unresolved role as a wildcard.
func (s *Store) ResolveMessageAudience(ctx context.Context, audience model.MessageAudience) ([]model.AgentID, error) {
	return resolveMessageAudience(ctx, s.db, audience)
}

func resolveMessageAudience(ctx context.Context, q queryer, audience model.MessageAudience) ([]model.AgentID, error) {
	selected := make(map[model.AgentID]struct{}, len(audience.AgentIDs))
	for _, id := range audience.AgentIDs {
		selected[id] = struct{}{}
	}
	if audience.GroupID != "" {
		var exists int
		if err := q.QueryRowContext(ctx, `SELECT 1 FROM groups WHERE id=?`, audience.GroupID).Scan(&exists); err != nil {
			return nil, classify(err)
		}
		query := `SELECT agent_id FROM group_members WHERE group_id=?`
		args := []any{audience.GroupID}
		if audience.RoleID != "" {
			query = `SELECT gm.agent_id FROM group_members gm
				WHERE gm.group_id=? AND EXISTS (
					SELECT 1 FROM role_assignments ra
					WHERE ra.role_id=? AND ra.subject_kind=? AND ra.subject_id=gm.agent_id
					AND ra.resource_kind IN (?,?) AND ra.resource_id=?
				)`
			args = []any{audience.GroupID, audience.RoleID, model.AuthorityAgent, model.ResourceGroup, model.ResourceGroupPeers, audience.GroupID}
		}
		rows, err := q.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id model.AgentID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			selected[id] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	if audience.RoleID != "" && audience.GroupID == "" {
		rows, err := q.QueryContext(ctx, `SELECT DISTINCT subject_id FROM role_assignments WHERE role_id=? AND subject_kind=?`, audience.RoleID, model.AuthorityAgent)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id model.AgentID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			selected[id] = struct{}{}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}

	resolved := make([]model.AgentID, 0, len(selected))
	for id := range selected {
		var state string
		if err := q.QueryRowContext(ctx, `SELECT lifecycle_state FROM agents WHERE id=?`, id).Scan(&state); err != nil {
			if classified := classify(err); errors.Is(classified, app.ErrNotFound) {
				continue
			} else {
				return nil, classified
			}
		}
		if state == "active" {
			resolved = append(resolved, id)
		}
	}
	sort.Slice(resolved, func(i, j int) bool { return resolved[i] < resolved[j] })
	return resolved, nil
}

func audienceIncludesAgent(ctx context.Context, q queryer, audience model.MessageAudience, id model.AgentID) (bool, error) {
	resolved, err := resolveMessageAudience(ctx, q, audience)
	if err != nil {
		return false, err
	}
	for _, candidate := range resolved {
		if candidate == id {
			return true, nil
		}
	}
	return false, nil
}

func requireDelegatedAttachmentAction(ctx context.Context, q queryer, principal model.Principal, action model.Action, resources []model.ResourceSelector, at time.Time) error {
	for _, resource := range resources {
		decision, err := authorizeTx(ctx, q, model.AuthorityRequest{Principal: principal, Action: action, Resource: resource}, at)
		if err != nil {
			return err
		}
		if decision.Allowed {
			return nil
		}
	}
	return app.ErrUnauthorized
}
