package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (s *Store) AuthorityState(ctx context.Context) (app.AuthorityStateResult, error) {
	var out app.AuthorityStateResult
	rows, err := s.db.QueryContext(ctx, `SELECT id,subject_kind,subject_id,action,resource_kind,resource_id,bounds_json,expires_at,revision,created_at,updated_at FROM authority_grants ORDER BY id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		grant, err := scanGrant(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Grants = append(out.Grants, grant)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT id,name,actions_json,revision,created_at,updated_at FROM roles ORDER BY id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		role, err := scanRole(rows)
		if err != nil {
			rows.Close()
			return out, err
		}
		out.Roles = append(out.Roles, role)
	}
	if err := rows.Close(); err != nil {
		return out, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT role_id,subject_kind,subject_id,resource_kind,resource_id,bounds_json,revision,created_at,updated_at FROM role_assignments ORDER BY role_id,subject_kind,subject_id,resource_kind,resource_id`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		assignment, err := scanAssignment(rows)
		if err != nil {
			return out, err
		}
		out.Assignments = append(out.Assignments, assignment)
	}
	return out, rows.Err()
}

func (s *Store) PutGrant(ctx context.Context, grant model.AuthorityGrant, expected model.Revision) (model.AuthorityGrant, error) {
	bounds, err := json.Marshal(grant.Bounds)
	if err != nil {
		return model.AuthorityGrant{}, err
	}
	subjectKind, subjectID := subjectParts(grant.Subject)
	resourceKind, resourceID := resourceParts(grant.Resource)
	var expires any
	if grant.ExpiresAt != nil {
		expires = nanos(*grant.ExpiresAt)
	}
	if expected == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO authority_grants(id,subject_kind,subject_id,action,resource_kind,resource_id,bounds_json,expires_at,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,1,?,?)`, grant.ID, subjectKind, subjectID, grant.Action, resourceKind, resourceID, bounds, expires, nanos(grant.CreatedAt), nanos(grant.UpdatedAt))
	} else {
		var result sql.Result
		result, err = s.db.ExecContext(ctx, `UPDATE authority_grants SET subject_kind=?,subject_id=?,action=?,resource_kind=?,resource_id=?,bounds_json=?,expires_at=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, subjectKind, subjectID, grant.Action, resourceKind, resourceID, bounds, expires, nanos(grant.UpdatedAt), grant.ID, expected)
		if err == nil {
			if affected, _ := result.RowsAffected(); affected != 1 {
				return model.AuthorityGrant{}, app.ErrConflict
			}
		}
	}
	if err != nil {
		return model.AuthorityGrant{}, classify(err)
	}
	if err := s.bump(ctx); err != nil {
		return model.AuthorityGrant{}, err
	}
	return grantByID(ctx, s.db, grant.ID)
}

func (s *Store) DeleteGrant(ctx context.Context, id model.GrantID, expected model.Revision) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM authority_grants WHERE id=? AND revision=?`, id, expected)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return app.ErrConflict
	}
	return s.bump(ctx)
}

func (s *Store) PutRole(ctx context.Context, role model.Role, expected model.Revision) (model.Role, error) {
	actions, err := json.Marshal(role.Actions)
	if err != nil {
		return model.Role{}, err
	}
	if expected == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO roles(id,name,actions_json,revision,created_at,updated_at) VALUES(?,?,?,1,?,?)`, role.ID, role.Name, actions, nanos(role.CreatedAt), nanos(role.UpdatedAt))
	} else {
		var result sql.Result
		result, err = s.db.ExecContext(ctx, `UPDATE roles SET name=?,actions_json=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, role.Name, actions, nanos(role.UpdatedAt), role.ID, expected)
		if err == nil {
			if affected, _ := result.RowsAffected(); affected != 1 {
				return model.Role{}, app.ErrConflict
			}
		}
	}
	if err != nil {
		return model.Role{}, classify(err)
	}
	if err := s.bump(ctx); err != nil {
		return model.Role{}, err
	}
	return roleByID(ctx, s.db, role.ID)
}

func (s *Store) PutRoleAssignment(ctx context.Context, assignment model.RoleAssignment, expected model.Revision) (model.RoleAssignment, error) {
	bounds, err := json.Marshal(assignment.Bounds)
	if err != nil {
		return model.RoleAssignment{}, err
	}
	sk, sid := subjectParts(assignment.Subject)
	rk, rid := resourceParts(assignment.Resource)
	if expected == 0 {
		_, err = s.db.ExecContext(ctx, `INSERT INTO role_assignments(role_id,subject_kind,subject_id,resource_kind,resource_id,bounds_json,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,1,?,?)`, assignment.RoleID, sk, sid, rk, rid, bounds, nanos(assignment.CreatedAt), nanos(assignment.UpdatedAt))
	} else {
		var result sql.Result
		result, err = s.db.ExecContext(ctx, `UPDATE role_assignments SET bounds_json=?,revision=revision+1,updated_at=? WHERE role_id=? AND subject_kind=? AND subject_id=? AND resource_kind=? AND resource_id=? AND revision=?`, bounds, nanos(assignment.UpdatedAt), assignment.RoleID, sk, sid, rk, rid, expected)
		if err == nil {
			if affected, _ := result.RowsAffected(); affected != 1 {
				return model.RoleAssignment{}, app.ErrConflict
			}
		}
	}
	if err != nil {
		return model.RoleAssignment{}, classify(err)
	}
	if err := s.bump(ctx); err != nil {
		return model.RoleAssignment{}, err
	}
	return assignmentByKey(ctx, s.db, assignment)
}

func (s *Store) DeleteRoleAssignment(ctx context.Context, assignment model.RoleAssignment, expected model.Revision) error {
	sk, sid := subjectParts(assignment.Subject)
	rk, rid := resourceParts(assignment.Resource)
	result, err := s.db.ExecContext(ctx, `DELETE FROM role_assignments WHERE role_id=? AND subject_kind=? AND subject_id=? AND resource_kind=? AND resource_id=? AND revision=?`, assignment.RoleID, sk, sid, rk, rid, expected)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return app.ErrConflict
	}
	return s.bump(ctx)
}

func (s *Store) SetGroupOwner(ctx context.Context, groupID model.GroupID, owner model.AgentID, bounds model.ConfigurationBounds, expected model.Revision, at time.Time) (model.Group, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Group{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var prior model.AgentID
	if err := tx.QueryRowContext(ctx, `SELECT owner_agent_id FROM groups WHERE id=?`, groupID).Scan(&prior); err != nil {
		return model.Group{}, classify(err)
	}
	if owner != "" {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_members WHERE group_id=? AND agent_id=?`, groupID, owner).Scan(&present); err != nil {
			return model.Group{}, err
		}
		if present != 1 {
			return model.Group{}, app.ErrUnauthorized
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE groups SET owner_agent_id=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, owner, nanos(at), groupID, expected)
	if err != nil {
		return model.Group{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.Group{}, app.ErrConflict
	}
	if prior != "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM role_assignments WHERE role_id=? AND subject_kind=? AND subject_id=? AND resource_kind=? AND resource_id=?`, model.GroupOwnerRole, model.AuthorityAgent, prior, model.ResourceGroupPeers, groupID)
		if err != nil {
			return model.Group{}, err
		}
	}
	if owner != "" {
		encoded, err := json.Marshal(bounds)
		if err != nil {
			return model.Group{}, err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO role_assignments(role_id,subject_kind,subject_id,resource_kind,resource_id,bounds_json,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,1,?,?) ON CONFLICT(role_id,subject_kind,subject_id,resource_kind,resource_id) DO UPDATE SET bounds_json=excluded.bounds_json,revision=role_assignments.revision+1,updated_at=excluded.updated_at`, model.GroupOwnerRole, model.AuthorityAgent, owner, model.ResourceGroupPeers, groupID, encoded, nanos(at), nanos(at))
		if err != nil {
			return model.Group{}, err
		}
	}
	if err := bumpTx(ctx, tx); err != nil {
		return model.Group{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Group{}, err
	}
	return s.Group(ctx, groupID)
}

func (s *Store) Authorize(ctx context.Context, request model.AuthorityRequest, at time.Time) (model.AuthorityDecision, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.AuthorityDecision{}, err
	}
	defer func() { _ = tx.Rollback() }()
	decision, err := authorizeTx(ctx, tx, request, at)
	if err != nil {
		return decision, err
	}
	return decision, tx.Commit()
}

type queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func authorizeTx(ctx context.Context, q queryer, request model.AuthorityRequest, at time.Time) (model.AuthorityDecision, error) {
	decision := model.AuthorityDecision{Action: request.Action, Resource: request.Resource}
	if request.Principal.Kind == model.PrincipalOperator {
		decision.Allowed, decision.SourceKind, decision.SourceID = true, model.AuthorityDefault, "operator"
		return decision, nil
	}
	subject, err := authoritySubject(ctx, q, request.Principal, at)
	if err != nil {
		return decision, err
	}
	if request.Principal.Kind == model.PrincipalAutomation {
		if !automationDelegates(ctx, q, request, at) {
			return decision, nil
		}
		if subject.Kind == model.AuthorityOperator {
			decision.Allowed, decision.SourceKind, decision.SourceID = true, model.AuthorityDefault, "automation:"+request.Principal.AutomationRun
			return decision, nil
		}
	}
	if defaultAuthority(request.Principal, request.Action, request.Resource) && request.RequestedConfiguration == nil {
		decision.Allowed, decision.SourceKind, decision.SourceID = true, model.AuthorityDefault, "execution_self"
		return decision, nil
	}
	sk, sid := subjectParts(subject)
	rows, err := q.QueryContext(ctx, `SELECT id,subject_kind,subject_id,action,resource_kind,resource_id,bounds_json,expires_at,revision,created_at,updated_at FROM authority_grants WHERE subject_kind=? AND subject_id=? AND action=?`, sk, sid, request.Action)
	if err != nil {
		return decision, err
	}
	for rows.Next() {
		grant, err := scanGrant(rows)
		if err != nil {
			rows.Close()
			return decision, err
		}
		if (grant.ExpiresAt == nil || at.Before(*grant.ExpiresAt)) && resourceMatches(ctx, q, request.Principal, grant.Resource, request.Resource) && configurationMatches(grant.Bounds, request.RequestedConfiguration) {
			rows.Close()
			return model.AuthorityDecision{Allowed: true, Action: request.Action, Resource: request.Resource, SourceKind: model.AuthorityDirect, SourceID: string(grant.ID), Revision: grant.Revision, Bounds: grant.Bounds}, nil
		}
	}
	if err := rows.Close(); err != nil {
		return decision, err
	}
	rows, err = q.QueryContext(ctx, `SELECT a.role_id,a.subject_kind,a.subject_id,a.resource_kind,a.resource_id,a.bounds_json,a.revision,a.created_at,a.updated_at,r.actions_json,r.revision FROM role_assignments a JOIN roles r ON r.id=a.role_id WHERE a.subject_kind=? AND a.subject_id=?`, sk, sid)
	if err != nil {
		return decision, err
	}
	defer rows.Close()
	for rows.Next() {
		var assignment model.RoleAssignment
		var subjectKind, subjectID, resourceKind, resourceID string
		var bounds, actions []byte
		var created, updated int64
		var roleRevision model.Revision
		if err := rows.Scan(&assignment.RoleID, &subjectKind, &subjectID, &resourceKind, &resourceID, &bounds, &assignment.Revision, &created, &updated, &actions, &roleRevision); err != nil {
			return decision, err
		}
		assignment.Subject = makeSubject(subjectKind, subjectID)
		assignment.Resource = makeResource(resourceKind, resourceID)
		assignment.CreatedAt, assignment.UpdatedAt = fromNanos(created), fromNanos(updated)
		if err := json.Unmarshal(bounds, &assignment.Bounds); err != nil {
			return decision, err
		}
		var roleActions []model.Action
		if err := json.Unmarshal(actions, &roleActions); err != nil {
			return decision, err
		}
		if slices.Contains(roleActions, request.Action) && resourceMatches(ctx, q, request.Principal, assignment.Resource, request.Resource) && configurationMatches(assignment.Bounds, request.RequestedConfiguration) {
			return model.AuthorityDecision{Allowed: true, Action: request.Action, Resource: request.Resource, SourceKind: model.AuthorityRole, SourceID: string(assignment.RoleID), Revision: max(assignment.Revision, roleRevision), Bounds: assignment.Bounds}, nil
		}
	}
	return decision, rows.Err()
}

func authoritySubject(ctx context.Context, q queryer, principal model.Principal, at time.Time) (model.AuthoritySubject, error) {
	switch principal.Kind {
	case model.PrincipalExecution:
		access, err := executionAccessRow(q.QueryRowContext(ctx, accessSelect+` WHERE execution_id=?`, principal.ExecutionID))
		if err != nil {
			return model.AuthoritySubject{}, err
		}
		if access.State != model.ExecutionAccessActive || access.Generation != principal.Generation || !at.Before(access.ExpiresAt) || access.AgentID != principal.AgentID {
			return model.AuthoritySubject{}, app.ErrUnauthorized
		}
		var state model.ExecutionState
		if err := q.QueryRowContext(ctx, `SELECT state FROM executions WHERE id=?`, principal.ExecutionID).Scan(&state); err != nil {
			return model.AuthoritySubject{}, classify(err)
		}
		if state == model.ExecutionExited || state == model.ExecutionFailed || state == model.ExecutionUnknown {
			return model.AuthoritySubject{}, app.ErrUnauthorized
		}
		if access.AgentID != "" {
			var primary model.ExecutionID
			var lifecycle model.AgentLifecycleState
			if err := q.QueryRowContext(ctx, `SELECT primary_execution_id,lifecycle_state FROM agents WHERE id=?`, access.AgentID).Scan(&primary, &lifecycle); err != nil || primary != access.ExecutionID || lifecycle != model.AgentActive {
				return model.AuthoritySubject{}, app.ErrUnauthorized
			}
			return model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: access.AgentID}, nil
		}
		return model.AuthoritySubject{Kind: model.AuthorityExecution, ExecutionID: access.ExecutionID}, nil
	case model.PrincipalAutomation:
		if principal.AutomationRun == "" || principal.Authority.Kind == "" || principal.Delegation == nil {
			return model.AuthoritySubject{}, app.ErrUnauthorized
		}
		if principal.Authority.Kind == model.AuthorityExecution {
			access, err := executionAccessRow(q.QueryRowContext(ctx, accessSelect+` WHERE execution_id=?`, principal.Authority.ExecutionID))
			if err != nil || access.State != model.ExecutionAccessActive || !at.Before(access.ExpiresAt) || access.AgentID != "" {
				return model.AuthoritySubject{}, app.ErrUnauthorized
			}
			var state model.ExecutionState
			if err := q.QueryRowContext(ctx, `SELECT state FROM executions WHERE id=?`, principal.Authority.ExecutionID).Scan(&state); err != nil || state == model.ExecutionExited || state == model.ExecutionFailed || state == model.ExecutionUnknown {
				return model.AuthoritySubject{}, app.ErrUnauthorized
			}
		}
		if principal.Authority.Kind == model.AuthorityAgent {
			var lifecycle model.AgentLifecycleState
			if err := q.QueryRowContext(ctx, `SELECT lifecycle_state FROM agents WHERE id=?`, principal.Authority.AgentID).Scan(&lifecycle); err != nil || lifecycle != model.AgentActive {
				return model.AuthoritySubject{}, app.ErrUnauthorized
			}
		}
		return principal.Authority, nil
	case model.PrincipalAgent: // Transitional in-process callers; never bearer-authenticated.
		var lifecycle model.AgentLifecycleState
		if err := q.QueryRowContext(ctx, `SELECT lifecycle_state FROM agents WHERE id=?`, principal.AgentID).Scan(&lifecycle); err != nil || lifecycle != model.AgentActive {
			return model.AuthoritySubject{}, app.ErrUnauthorized
		}
		return model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: principal.AgentID}, nil
	default:
		return model.AuthoritySubject{}, app.ErrUnauthorized
	}
}

func automationDelegates(ctx context.Context, q queryer, request model.AuthorityRequest, at time.Time) bool {
	delegation := request.Principal.Delegation
	if delegation == nil || delegation.ExpiresAt.IsZero() || !at.Before(delegation.ExpiresAt) || !slices.Contains(delegation.Actions, request.Action) || !configurationMatches(delegation.Bounds, request.RequestedConfiguration) {
		return false
	}
	for _, resource := range delegation.Resources {
		if resourceMatches(ctx, q, request.Principal, resource, request.Resource) {
			return true
		}
	}
	return false
}

func defaultAuthority(principal model.Principal, action model.Action, resource model.ResourceSelector) bool {
	if principal.Kind != model.PrincipalExecution && principal.Kind != model.PrincipalAgent {
		return false
	}
	if action != model.ActionReadIdentity && action != model.ActionReadStatus && action != model.ActionReadInbox && action != model.ActionMarkInboxRead && action != model.ActionReadUsage && action != model.ActionRefreshUsage && action != model.ActionReadActivity {
		return false
	}
	switch resource.Kind {
	case model.ResourceSelf:
		return true
	case model.ResourceAgent:
		return principal.AgentID != "" && resource.AgentID == principal.AgentID
	case model.ResourceExecution:
		return principal.ExecutionID != "" && resource.ExecutionID == principal.ExecutionID
	default:
		return false
	}
}

func resourceMatches(ctx context.Context, q queryer, principal model.Principal, granted, requested model.ResourceSelector) bool {
	if granted.Kind == model.ResourceSelf {
		switch principal.Authority.Kind {
		case model.AuthorityAgent:
			return requested.Kind == model.ResourceAgent && requested.AgentID == principal.Authority.AgentID || requested.Kind == model.ResourceExecution && executionBelongsTo(ctx, q, requested.ExecutionID, principal.Authority.AgentID)
		case model.AuthorityExecution:
			return requested.Kind == model.ResourceExecution && requested.ExecutionID == principal.Authority.ExecutionID
		default:
			return defaultAuthority(principal, model.ActionReadIdentity, requested)
		}
	}
	if granted.Kind == requested.Kind {
		_, grantedID := resourceParts(granted)
		_, requestedID := resourceParts(requested)
		return grantedID == requestedID
	}
	if granted.Kind != model.ResourceGroupPeers {
		return false
	}
	if requested.Kind == model.ResourceGroup {
		return requested.GroupID == granted.GroupID
	}
	var target model.AgentID
	switch requested.Kind {
	case model.ResourceAgent:
		target = requested.AgentID
	case model.ResourceExecution:
		if err := q.QueryRowContext(ctx, `SELECT agent_id FROM executions WHERE id=?`, requested.ExecutionID).Scan(&target); err != nil {
			return false
		}
	default:
		return false
	}
	var member int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_members WHERE group_id=? AND agent_id=?`, granted.GroupID, target).Scan(&member); err != nil {
		return false
	}
	return member == 1
}

func executionBelongsTo(ctx context.Context, q queryer, executionID model.ExecutionID, agentID model.AgentID) bool {
	var actual model.AgentID
	return q.QueryRowContext(ctx, `SELECT agent_id FROM executions WHERE id=?`, executionID).Scan(&actual) == nil && actual == agentID
}

func configurationMatches(bounds model.ConfigurationBounds, requested *model.DesiredConfiguration) bool {
	if requested == nil {
		return true
	}
	if len(bounds.Harnesses) == 0 || len(bounds.Models) == 0 || len(bounds.WorkingDirectoryRoots) == 0 || len(bounds.ApprovalModes) == 0 || len(bounds.SandboxModes) == 0 {
		return false
	}
	if !slices.Contains(bounds.Harnesses, requested.Harness) || !slices.Contains(bounds.Models, requested.Model) || !slices.Contains(bounds.ApprovalModes, requested.Approval) || !slices.Contains(bounds.SandboxModes, requested.Sandbox) {
		return false
	}
	requestedPath, err := filepath.Abs(requested.WorkingDirectory)
	if err != nil {
		return false
	}
	for _, root := range bounds.WorkingDirectoryRoots {
		rootPath, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		relative, err := filepath.Rel(rootPath, requestedPath)
		if err == nil && relative != ".." && !filepath.IsAbs(relative) && (relative == "." || len(relative) < 3 || relative[:3] != "../") {
			return true
		}
	}
	return false
}

const accessSelect = `SELECT execution_id,agent_id,generation,credential_digest,delivery_id,file_identity,state,issued_at,expires_at,revoked_at,revision FROM execution_accesses`

func (s *Store) ExecutionAccess(ctx context.Context, id model.ExecutionID) (model.ExecutionAccess, error) {
	return executionAccessRow(s.db.QueryRowContext(ctx, accessSelect+` WHERE execution_id=?`, id))
}

func (s *Store) ExecutionAccessesDue(ctx context.Context, after, before time.Time) ([]model.ExecutionAccess, error) {
	rows, err := s.db.QueryContext(ctx, accessSelect+` WHERE state=? AND expires_at>? AND expires_at<=? ORDER BY expires_at,execution_id`, model.ExecutionAccessActive, nanos(after), nanos(before))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accesses []model.ExecutionAccess
	for rows.Next() {
		access, err := executionAccessRow(rows)
		if err != nil {
			return nil, err
		}
		accesses = append(accesses, access)
	}
	return accesses, rows.Err()
}

func (s *Store) AuthenticateExecutionAccess(ctx context.Context, digest []byte, at time.Time) (model.ExecutionAccess, error) {
	access, err := executionAccessRow(s.db.QueryRowContext(ctx, accessSelect+` WHERE credential_digest=?`, digest))
	if err != nil {
		return access, err
	}
	if access.State != model.ExecutionAccessActive || !at.Before(access.ExpiresAt) {
		return model.ExecutionAccess{}, app.ErrUnauthorized
	}
	return access, nil
}

func (s *Store) RecordAccessDelivery(ctx context.Context, id model.ExecutionID, generation model.AccessGeneration, receipt ports.ActionCredentialReceipt, at time.Time) (model.ExecutionAccess, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE execution_accesses SET delivery_id=?,file_identity=?,revision=revision+1 WHERE execution_id=? AND generation=? AND state=?`, receipt.DeliveryID, receipt.FileIdentity, id, generation, model.ExecutionAccessInactive)
	if err != nil {
		return model.ExecutionAccess{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.ExecutionAccess{}, app.ErrConflict
	}
	if err := s.bump(ctx); err != nil {
		return model.ExecutionAccess{}, err
	}
	return s.ExecutionAccess(ctx, id)
}

func (s *Store) RotateExecutionAccess(ctx context.Context, id model.ExecutionID, expected model.AccessGeneration, expectedRevision model.Revision, digest []byte, receipt ports.ActionCredentialReceipt, issuedAt, expiresAt, checkedAt time.Time) (model.ExecutionAccess, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE execution_accesses SET generation=?,credential_digest=?,delivery_id=?,file_identity=?,issued_at=?,expires_at=?,revoked_at=NULL,revision=revision+1 WHERE execution_id=? AND generation=? AND revision=? AND state IN (?,?) AND expires_at>? AND ?>?`, receipt.Generation, digest, receipt.DeliveryID, receipt.FileIdentity, nanos(issuedAt), nanos(expiresAt), id, expected, expectedRevision, model.ExecutionAccessActive, model.ExecutionAccessSuspended, nanos(checkedAt), nanos(expiresAt), nanos(checkedAt))
	if err != nil {
		return model.ExecutionAccess{}, classify(err)
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.ExecutionAccess{}, app.ErrConflict
	}
	if err := s.bump(ctx); err != nil {
		return model.ExecutionAccess{}, err
	}
	return s.ExecutionAccess(ctx, id)
}

func (s *Store) ReactivateExecutionAccess(ctx context.Context, id model.ExecutionID, generation model.AccessGeneration, proof ports.ActionCredentialRecoveryProof, at time.Time) (model.ExecutionAccess, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE execution_accesses SET state=?,file_identity=?,revision=revision+1 WHERE execution_id=? AND generation=? AND delivery_id=? AND state=? AND expires_at>?`, model.ExecutionAccessActive, proof.FileIdentity, id, generation, proof.DeliveryID, model.ExecutionAccessSuspended, nanos(at))
	if err != nil {
		return model.ExecutionAccess{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.ExecutionAccess{}, app.ErrConflict
	}
	if err := s.bump(ctx); err != nil {
		return model.ExecutionAccess{}, err
	}
	return s.ExecutionAccess(ctx, id)
}

func (s *Store) RevokeExecutionAccess(ctx context.Context, id model.ExecutionID, expected model.Revision, at time.Time) (model.ExecutionAccess, error) {
	query := `UPDATE execution_accesses SET state=?,revoked_at=?,revision=revision+1 WHERE execution_id=? AND state NOT IN (?,?)`
	args := []any{model.ExecutionAccessRevoked, nanos(at), id, model.ExecutionAccessRevoked, model.ExecutionAccessExpired}
	if expected != 0 {
		query += ` AND revision=?`
		args = append(args, expected)
	}
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return model.ExecutionAccess{}, err
	}
	if affected, _ := result.RowsAffected(); affected != 1 {
		return model.ExecutionAccess{}, app.ErrConflict
	}
	if err := s.bump(ctx); err != nil {
		return model.ExecutionAccess{}, err
	}
	return s.ExecutionAccess(ctx, id)
}

func executionAccessRow(row scanner) (model.ExecutionAccess, error) {
	var access model.ExecutionAccess
	var issued, expires int64
	var revoked sql.NullInt64
	if err := row.Scan(&access.ExecutionID, &access.AgentID, &access.Generation, &access.CredentialDigest, &access.DeliveryID, &access.FileIdentity, &access.State, &issued, &expires, &revoked, &access.Revision); err != nil {
		return access, classify(err)
	}
	access.IssuedAt, access.ExpiresAt = fromNanos(issued), fromNanos(expires)
	if revoked.Valid {
		value := fromNanos(revoked.Int64)
		access.RevokedAt = &value
	}
	return access, nil
}

func insertExecutionAccess(ctx context.Context, tx *sql.Tx, access model.ExecutionAccess) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO execution_accesses(execution_id,agent_id,generation,credential_digest,delivery_id,file_identity,state,issued_at,expires_at,revoked_at,revision) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, access.ExecutionID, access.AgentID, access.Generation, access.CredentialDigest, access.DeliveryID, access.FileIdentity, access.State, nanos(access.IssuedAt), nanos(access.ExpiresAt), nil, access.Revision)
	return classify(err)
}

func insertOperationAuthority(ctx context.Context, tx *sql.Tx, operationID model.OperationID, request model.AuthorityRequest, decision model.AuthorityDecision) error {
	rk, rid := resourceParts(request.Resource)
	var configuration any
	if request.RequestedConfiguration != nil {
		encoded, err := json.Marshal(request.RequestedConfiguration)
		if err != nil {
			return err
		}
		configuration = encoded
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO operation_authority(operation_id,action,resource_kind,resource_id,requested_configuration_json,admitted_source_kind,admitted_source_id,admitted_revision) VALUES(?,?,?,?,?,?,?,?)`, operationID, request.Action, rk, rid, configuration, decision.SourceKind, decision.SourceID, decision.Revision)
	return err
}

func operationAuthority(ctx context.Context, tx *sql.Tx, operationID model.OperationID) (model.AuthorityRequest, bool, error) {
	var request model.AuthorityRequest
	var subjectKind, subjectID, rk, rid string
	var configuration, delegation []byte
	err := tx.QueryRowContext(ctx, `SELECT o.principal_kind,o.principal_agent_id,o.principal_execution_id,o.principal_generation,o.principal_automation_run,o.automation_delegation_json,o.authority_subject_kind,o.authority_subject_id,a.action,a.resource_kind,a.resource_id,a.requested_configuration_json FROM operations o JOIN operation_authority a ON a.operation_id=o.id WHERE o.id=?`, operationID).Scan(&request.Principal.Kind, &request.Principal.AgentID, &request.Principal.ExecutionID, &request.Principal.Generation, &request.Principal.AutomationRun, &delegation, &subjectKind, &subjectID, &request.Action, &rk, &rid, &configuration)
	if errors.Is(err, sql.ErrNoRows) {
		return request, false, nil
	}
	if err != nil {
		return request, false, err
	}
	request.Principal.Authority = makeSubject(subjectKind, subjectID)
	if len(delegation) != 0 {
		request.Principal.Delegation = new(model.AutomationDelegation)
		if err := json.Unmarshal(delegation, request.Principal.Delegation); err != nil {
			return request, false, err
		}
	}
	request.Resource = makeResource(rk, rid)
	if len(configuration) != 0 {
		request.RequestedConfiguration = new(model.DesiredConfiguration)
		if err := json.Unmarshal(configuration, request.RequestedConfiguration); err != nil {
			return request, false, err
		}
	}
	return request, true, nil
}

func grantByID(ctx context.Context, q queryer, id model.GrantID) (model.AuthorityGrant, error) {
	return scanGrant(q.QueryRowContext(ctx, `SELECT id,subject_kind,subject_id,action,resource_kind,resource_id,bounds_json,expires_at,revision,created_at,updated_at FROM authority_grants WHERE id=?`, id))
}

func scanGrant(row scanner) (model.AuthorityGrant, error) {
	var grant model.AuthorityGrant
	var sk, sid, rk, rid string
	var bounds []byte
	var expires sql.NullInt64
	var created, updated int64
	if err := row.Scan(&grant.ID, &sk, &sid, &grant.Action, &rk, &rid, &bounds, &expires, &grant.Revision, &created, &updated); err != nil {
		return grant, classify(err)
	}
	grant.Subject, grant.Resource = makeSubject(sk, sid), makeResource(rk, rid)
	grant.CreatedAt, grant.UpdatedAt = fromNanos(created), fromNanos(updated)
	if expires.Valid {
		value := fromNanos(expires.Int64)
		grant.ExpiresAt = &value
	}
	if err := json.Unmarshal(bounds, &grant.Bounds); err != nil {
		return grant, err
	}
	return grant, nil
}

func roleByID(ctx context.Context, q queryer, id model.RoleID) (model.Role, error) {
	return scanRole(q.QueryRowContext(ctx, `SELECT id,name,actions_json,revision,created_at,updated_at FROM roles WHERE id=?`, id))
}

func scanRole(row scanner) (model.Role, error) {
	var role model.Role
	var actions []byte
	var created, updated int64
	if err := row.Scan(&role.ID, &role.Name, &actions, &role.Revision, &created, &updated); err != nil {
		return role, classify(err)
	}
	role.CreatedAt, role.UpdatedAt = fromNanos(created), fromNanos(updated)
	if err := json.Unmarshal(actions, &role.Actions); err != nil {
		return role, err
	}
	return role, nil
}

func assignmentByKey(ctx context.Context, q queryer, assignment model.RoleAssignment) (model.RoleAssignment, error) {
	sk, sid := subjectParts(assignment.Subject)
	rk, rid := resourceParts(assignment.Resource)
	return scanAssignment(q.QueryRowContext(ctx, `SELECT role_id,subject_kind,subject_id,resource_kind,resource_id,bounds_json,revision,created_at,updated_at FROM role_assignments WHERE role_id=? AND subject_kind=? AND subject_id=? AND resource_kind=? AND resource_id=?`, assignment.RoleID, sk, sid, rk, rid))
}

func scanAssignment(row scanner) (model.RoleAssignment, error) {
	var assignment model.RoleAssignment
	var sk, sid, rk, rid string
	var bounds []byte
	var created, updated int64
	if err := row.Scan(&assignment.RoleID, &sk, &sid, &rk, &rid, &bounds, &assignment.Revision, &created, &updated); err != nil {
		return assignment, classify(err)
	}
	assignment.Subject, assignment.Resource = makeSubject(sk, sid), makeResource(rk, rid)
	assignment.CreatedAt, assignment.UpdatedAt = fromNanos(created), fromNanos(updated)
	if err := json.Unmarshal(bounds, &assignment.Bounds); err != nil {
		return assignment, err
	}
	return assignment, nil
}

func subjectParts(subject model.AuthoritySubject) (string, string) {
	if subject.Kind == model.AuthorityAgent {
		return string(subject.Kind), string(subject.AgentID)
	}
	if subject.Kind == model.AuthorityOperator {
		return string(subject.Kind), ""
	}
	return string(subject.Kind), string(subject.ExecutionID)
}

func makeSubject(kind, id string) model.AuthoritySubject {
	switch model.AuthoritySubjectKind(kind) {
	case model.AuthorityOperator:
		return model.AuthoritySubject{Kind: model.AuthorityOperator}
	case model.AuthorityAgent:
		return model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: model.AgentID(id)}
	default:
		return model.AuthoritySubject{Kind: model.AuthorityExecution, ExecutionID: model.ExecutionID(id)}
	}
}

func resourceParts(resource model.ResourceSelector) (string, string) {
	switch resource.Kind {
	case model.ResourceAgent:
		return string(resource.Kind), string(resource.AgentID)
	case model.ResourceExecution:
		return string(resource.Kind), string(resource.ExecutionID)
	case model.ResourceGroup, model.ResourceGroupPeers:
		return string(resource.Kind), string(resource.GroupID)
	case model.ResourceConversation:
		return string(resource.Kind), string(resource.ConversationID)
	case model.ResourceWorkspace:
		return string(resource.Kind), string(resource.WorkspaceID)
	case model.ResourceWorkRun:
		return string(resource.Kind), string(resource.WorkRunID)
	default:
		return string(resource.Kind), ""
	}
}

func makeResource(kind, id string) model.ResourceSelector {
	resource := model.ResourceSelector{Kind: model.ResourceSelectorKind(kind)}
	switch resource.Kind {
	case model.ResourceAgent:
		resource.AgentID = model.AgentID(id)
	case model.ResourceExecution:
		resource.ExecutionID = model.ExecutionID(id)
	case model.ResourceGroup, model.ResourceGroupPeers:
		resource.GroupID = model.GroupID(id)
	case model.ResourceConversation:
		resource.ConversationID = model.ConversationID(id)
	case model.ResourceWorkspace:
		resource.WorkspaceID = model.WorkspaceID(id)
	case model.ResourceWorkRun:
		resource.WorkRunID = model.WorkRunID(id)
	}
	return resource
}

func requestScope(principal model.Principal) string {
	switch principal.Kind {
	case model.PrincipalExecution:
		return "execution:" + string(principal.ExecutionID)
	case model.PrincipalAutomation:
		return "automation:" + principal.AutomationRun
	case model.PrincipalAgent:
		return "agent:" + string(principal.AgentID)
	default:
		return "operator"
	}
}

func sameRequester(left, right model.Principal) bool {
	return left.Kind == right.Kind && left.AgentID == right.AgentID && left.ExecutionID == right.ExecutionID && left.AutomationRun == right.AutomationRun && left.Authority == right.Authority
}
