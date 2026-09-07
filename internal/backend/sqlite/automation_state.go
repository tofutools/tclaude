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

// SetAutomationEnabled changes activation without creating another authored
// revision or resetting its source cursor. A scoped receipt survives response loss.
func (s *Store) SetAutomationEnabled(ctx context.Context, req app.SetAutomationEnabledRequest, now time.Time) (model.AutomationRule, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AutomationRule{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var priorID model.AutomationRuleID
	var priorRevision model.Revision
	var priorEnabled bool
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT rule_id,expected_revision,enabled,result_json FROM automation_state_requests WHERE request_scope=? AND request_id=?`, requestScope(req.Context.Principal), req.Context.RequestID).Scan(&priorID, &priorRevision, &priorEnabled, &data)
	if err == nil {
		if priorID != req.ID || priorRevision != req.ExpectedRevision || priorEnabled != req.Enabled {
			return model.AutomationRule{}, app.ErrConflict
		}
		var prior model.AutomationRule
		err = json.Unmarshal(data, &prior)
		return prior, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.AutomationRule{}, err
	}
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionManageAutomation, Resource: model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: req.ID}}, now)
	if err != nil {
		return model.AutomationRule{}, err
	}
	if !decision.Allowed {
		return model.AutomationRule{}, app.ErrUnauthorized
	}
	var rule model.AutomationRule
	var created, updated int64
	err = tx.QueryRowContext(ctx, `SELECT id,name,head_revision_id,enabled,deployment_id,tombstoned,revision,created_at,updated_at FROM automation_rules WHERE id=?`, req.ID).Scan(&rule.ID, &rule.Name, &rule.HeadRevisionID, &rule.Enabled, &rule.DeploymentID, &rule.Tombstoned, &rule.Revision, &created, &updated)
	if err != nil {
		return rule, classify(err)
	}
	if rule.Revision != req.ExpectedRevision || rule.Tombstoned {
		return rule, app.ErrConflict
	}
	// Deployment-owned rhythms follow their deployment lifecycle, including stop.
	if rule.DeploymentID != "" {
		return rule, app.ErrUnauthorized
	}
	rule.CreatedAt = fromNanos(created)
	rule.UpdatedAt = fromNanos(updated)
	if rule.Enabled != req.Enabled {
		rule.Enabled = req.Enabled
		rule.Revision++
		rule.UpdatedAt = now
		if _, err = tx.ExecContext(ctx, `UPDATE automation_rules SET enabled=?,revision=?,updated_at=? WHERE id=?`, rule.Enabled, rule.Revision, nanos(now), rule.ID); err != nil {
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
	if _, err = tx.ExecContext(ctx, `INSERT INTO automation_state_requests(request_scope,request_id,rule_id,expected_revision,enabled,result_json) VALUES(?,?,?,?,?,?)`, requestScope(req.Context.Principal), req.Context.RequestID, req.ID, req.ExpectedRevision, req.Enabled, data); err != nil {
		return rule, err
	}
	return rule, tx.Commit()
}
