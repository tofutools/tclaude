package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// SetAutomationArchived preserves authored revisions and admitted history.
// Restoring does not enable the rule; activation remains a separate command.
func (s *Store) SetAutomationArchived(ctx context.Context, req app.SetAutomationArchivedRequest, now time.Time) (model.AutomationRule, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AutomationRule{}, err
	}
	defer func() { _ = tx.Rollback() }()
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionManageAutomation, Resource: model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: req.ID}}, now)
	if err != nil {
		return model.AutomationRule{}, err
	}
	if !decision.Allowed {
		return model.AutomationRule{}, app.ErrUnauthorized
	}
	var priorID model.AutomationRuleID
	var priorRevision model.Revision
	var priorArchived bool
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT rule_id,expected_revision,archived,result_json FROM automation_archive_requests WHERE request_scope=? AND request_id=?`, requestScope(req.Context.Principal), req.Context.RequestID).Scan(&priorID, &priorRevision, &priorArchived, &data)
	if err == nil {
		if priorID != req.ID || priorRevision != req.ExpectedRevision || priorArchived != req.Archived {
			return model.AutomationRule{}, app.ErrConflict
		}
		var prior model.AutomationRule
		err = json.Unmarshal(data, &prior)
		return prior, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.AutomationRule{}, err
	}

	var rule model.AutomationRule
	var created, updated int64
	err = tx.QueryRowContext(ctx, `SELECT id,name,head_revision_id,enabled,deployment_id,tombstoned,revision,created_at,updated_at FROM automation_rules WHERE id=?`, req.ID).Scan(&rule.ID, &rule.Name, &rule.HeadRevisionID, &rule.Enabled, &rule.DeploymentID, &rule.Tombstoned, &rule.Revision, &created, &updated)
	if err != nil {
		return rule, classify(err)
	}
	if rule.Revision != req.ExpectedRevision {
		return rule, app.ErrConflict
	}
	// Deployment-owned rhythms follow their deployment lifecycle, including stop.
	if rule.DeploymentID != "" {
		return rule, app.ErrUnauthorized
	}
	rule.CreatedAt = fromNanos(created)
	rule.UpdatedAt = fromNanos(updated)
	if rule.Tombstoned != req.Archived {
		rule.Tombstoned = req.Archived
		rule.Enabled = false
		rule.Revision++
		rule.UpdatedAt = now
		if _, err = tx.ExecContext(ctx, `UPDATE automation_rules SET enabled=0,tombstoned=?,revision=?,updated_at=? WHERE id=?`, rule.Tombstoned, rule.Revision, nanos(now), rule.ID); err != nil {
			return rule, err
		}
		if err = bumpTx(ctx, tx); err != nil {
			return rule, err
		}
	}
	data, err = json.Marshal(rule)
	if err != nil {
		return rule, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO automation_archive_requests(request_scope,request_id,rule_id,expected_revision,archived,result_json) VALUES(?,?,?,?,?,?)`, requestScope(req.Context.Principal), req.Context.RequestID, req.ID, req.ExpectedRevision, req.Archived, data); err != nil {
		return rule, err
	}
	return rule, tx.Commit()
}
