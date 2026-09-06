package sqlite

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Store) initializeMessageNotifications(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS message_notifications (
 recipient_id TEXT PRIMARY KEY REFERENCES message_recipients(id),
 execution_id TEXT NOT NULL REFERENCES executions(id),
 consumed_at INTEGER NOT NULL, completed_at INTEGER
 )`)
	return err
}

func (s *Store) PendingMessageNotifications(ctx context.Context, limit int) ([]app.NotificationCandidate, error) {
	if limit < 1 || limit > 64 {
		return nil, app.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,COALESCE(a.primary_execution_id,'') FROM message_recipients r LEFT JOIN agents a ON a.id=r.agent_id WHERE r.notification_intent=? AND r.notification_outcome=? ORDER BY r.rowid LIMIT ?`, model.NotificationIfAvailable, model.NotificationPending, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []app.NotificationCandidate
	for rows.Next() {
		var candidate app.NotificationCandidate
		if err := rows.Scan(&candidate.RecipientID, &candidate.ExecutionID); err != nil {
			return nil, err
		}
		result = append(result, candidate)
	}
	return result, rows.Err()
}

// The durable recipient receipt is the one-shot permission to send its notice.
// Rechecking the sender and exact recipient here prevents a persisted message
// from becoming permanent native-effect authority after revocation or replacement.
func (s *Store) ConsumeMessageNotification(ctx context.Context, candidate app.NotificationCandidate, at time.Time) error {
	if candidate.ExecutionID == "" {
		return app.ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var agentID model.AgentID
	var operationID model.OperationID
	var outcome model.NotificationOutcome
	var intent model.NotificationIntent
	err = tx.QueryRowContext(ctx, `SELECT r.agent_id,m.operation_id,r.notification_outcome,r.notification_intent FROM message_recipients r JOIN messages m ON m.id=r.message_id WHERE r.id=? AND r.address_kind=?`, candidate.RecipientID, model.MessageAddressAgent).Scan(&agentID, &operationID, &outcome, &intent)
	if err != nil {
		return classify(err)
	}
	if outcome != model.NotificationPending || intent != model.NotificationIfAvailable {
		return app.ErrConflict
	}
	agent, err := scanAgent(tx.QueryRowContext(ctx, agentSelect+` WHERE id=?`, agentID))
	if err != nil {
		return err
	}
	if agent.Lifecycle != model.AgentActive || agent.PrimaryExecutionID != candidate.ExecutionID || agent.Notifications.DirectMessage != model.NotificationIfAvailable {
		return app.ErrConflict
	}
	execution, err := executionTx(ctx, tx, candidate.ExecutionID)
	if err != nil {
		return err
	}
	if execution.AgentID != agentID || execution.State != model.ExecutionRunning {
		return app.ErrConflict
	}
	operation, err := operationTx(ctx, tx, operationID)
	if err != nil {
		return err
	}
	if operation.Kind != model.OperationSendMessage || operation.State != model.OperationSucceeded {
		return app.ErrConflict
	}
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: operation.Principal, Action: model.ActionSendMessage, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: agentID}}, at)
	if err != nil {
		return err
	}
	if !decision.Allowed {
		return app.ErrUnauthorized
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO message_notifications(recipient_id,execution_id,consumed_at) VALUES(?,?,?)`, candidate.RecipientID, candidate.ExecutionID, nanos(at)); err != nil {
		return classify(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE message_recipients SET notification_outcome=?,notification_detail=? WHERE id=?`, model.NotificationUnknown, "native notification outcome is uncertain", candidate.RecipientID); err != nil {
		return err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CompleteMessageNotification(ctx context.Context, candidate app.NotificationCandidate, outcome model.NotificationOutcome, detail string, at time.Time) error {
	switch outcome {
	case model.NotificationDelivered, model.NotificationUnavailable, model.NotificationUnknown:
	default:
		return app.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `UPDATE message_notifications SET completed_at=? WHERE recipient_id=? AND execution_id=? AND completed_at IS NULL`, nanos(at), candidate.RecipientID, candidate.ExecutionID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return app.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE message_recipients SET notification_outcome=?,notification_detail=?,notified_at=? WHERE id=? AND notification_outcome=?`, outcome, detail, nanos(at), candidate.RecipientID, model.NotificationUnknown); err != nil {
		return err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
