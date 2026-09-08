package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const groupDisbandSchema = `CREATE TABLE IF NOT EXISTS group_disband_requests(scope TEXT NOT NULL,request_id TEXT NOT NULL,group_id TEXT NOT NULL,expected_revision INTEGER NOT NULL,result BLOB NOT NULL,PRIMARY KEY(scope,request_id));`

// Tombstones reserve the exact identity while history and immutable pins remain readable.
func requireNotDisbandedGroup(ctx context.Context, tx *sql.Tx, id model.GroupID) error {
	if id == "" {
		return nil
	}
	var removed bool
	err := tx.QueryRowContext(ctx, `SELECT tombstoned FROM groups WHERE id=?`, id).Scan(&removed)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if removed {
		return fmt.Errorf("%w: group %s has been disbanded", app.ErrConflict, id)
	}
	return nil
}

func (s *Store) DisbandGroup(ctx context.Context, in app.DisbandGroupRequest, at time.Time) (app.DisbandGroupResult, error) {
	var out app.DisbandGroupResult
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	// Reading one's original receipt is separate from admitting another effect.
	// The mutation removes scoped roles, but must not revoke its own receipt.
	if in.Context.Principal.Kind != model.PrincipalOperator {
		if _, err = authoritySubject(ctx, tx, in.Context.Principal, at); err != nil {
			return out, err
		}
		if in.Context.Principal.Kind == model.PrincipalAutomation && (in.Context.Principal.Delegation == nil || !at.Before(in.Context.Principal.Delegation.ExpiresAt)) {
			return out, app.ErrUnauthorized
		}
	}
	var id model.GroupID
	var rev model.Revision
	var stored []byte
	err = tx.QueryRowContext(ctx, `SELECT group_id,expected_revision,result FROM group_disband_requests WHERE scope=? AND request_id=?`, requestScope(in.Context.Principal), in.Context.RequestID).Scan(&id, &rev, &stored)
	if err == nil {
		if id != in.ID || rev != in.ExpectedRevision {
			return out, app.ErrConflict
		}
		err = json.Unmarshal(stored, &out)
		return out, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: in.Context.Principal, Action: model.ActionDisbandGroup, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: in.ID}}, at)
	if err != nil {
		return out, err
	}
	if !decision.Allowed {
		return out, app.ErrUnauthorized
	}
	out.Group, err = readGroup(ctx, tx, in.ID)
	if err != nil {
		return out, err
	}
	if out.Group.Revision != in.ExpectedRevision {
		return out, app.ErrConflict
	}
	// Check the same durable admission boundary used by new group work. Member
	// executions are independent and are deliberately not stopped or retired.
	for _, check := range []struct{ query, hint string }{
		{`SELECT id FROM team_deployments WHERE group_id=? AND state!='stopped' LIMIT 1`, "Stand down team deployment"},
		{`SELECT id FROM work_runs WHERE json_extract(scope_json,'$.GroupID')=? AND (state NOT IN ('succeeded','failed','cancelled') OR control_state='draining') LIMIT 1`, "Settle group work run"},
		{`SELECT id FROM executions WHERE json_extract(shell_group_json,'$.GroupID')=? AND state NOT IN ('exited','failed') LIMIT 1`, "Stop and observe group shell"},
	} {
		var active string
		err = tx.QueryRowContext(ctx, check.query, in.ID).Scan(&active)
		if err == nil {
			return out, &app.GroupDisbandBlockedError{Instruction: check.hint, ID: active}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
	}
	rows, err := tx.QueryContext(ctx, `SELECT r.id,v.action_json FROM automation_rules r JOIN automation_rule_revisions v ON v.id=r.head_revision_id WHERE r.tombstoned=0 ORDER BY r.id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var rule model.AutomationRuleID
		var raw []byte
		if err = rows.Scan(&rule, &raw); err != nil {
			_ = rows.Close()
			return out, err
		}
		var action model.AutomationAction
		if err = json.Unmarshal(raw, &action); err != nil {
			_ = rows.Close()
			return out, err
		}
		if action.Message != nil && action.Message.GroupID == in.ID || action.Work != nil && action.Work.Scope.GroupID == in.ID || action.Team != nil && action.Team.Target.Kind == model.TeamTargetExistingGroup && action.Team.Target.GroupID == in.ID {
			out.ArchivedRules = append(out.ArchivedRules, rule)
		}
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return out, err
	}
	for _, rule := range out.ArchivedRules {
		if _, err = tx.ExecContext(ctx, `UPDATE automation_rules SET enabled=0,tombstoned=1,revision=revision+1,updated_at=? WHERE id=?`, nanos(at), rule); err != nil {
			return out, err
		}
	}
	rows, err = tx.QueryContext(ctx, `SELECT group_id FROM group_parents WHERE parent_id=? ORDER BY group_id`, in.ID)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var child model.GroupID
		if err = rows.Scan(&child); err != nil {
			_ = rows.Close()
			return out, err
		}
		out.DetachedChildren = append(out.DetachedChildren, child)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return out, err
	}
	for _, child := range out.DetachedChildren {
		if _, err = tx.ExecContext(ctx, `UPDATE groups SET revision=revision+1,updated_at=? WHERE id=?`, nanos(at), child); err != nil {
			return out, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM group_parents WHERE group_id=? OR parent_id=?`, in.ID, in.ID); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM role_assignments WHERE resource_id=? AND resource_kind IN (?,?)`, in.ID, model.ResourceGroup, model.ResourceGroupPeers); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM group_members WHERE group_id=?`, in.ID); err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE groups SET tombstoned=1,owner_agent_id='',revision=revision+1,updated_at=? WHERE id=?`, nanos(at), in.ID); err != nil {
		return out, err
	}
	out.DisbandedAt = at
	stored, err = json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO group_disband_requests(scope,request_id,group_id,expected_revision,result) VALUES(?,?,?,?,?)`, requestScope(in.Context.Principal), in.Context.RequestID, in.ID, in.ExpectedRevision, stored); err != nil {
		return out, err
	}
	if err = clearSandboxDefaultAssignments(ctx, tx, "", in.ID); err != nil {
		return out, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
