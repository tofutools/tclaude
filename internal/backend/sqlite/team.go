package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Store) CreateTeamDeployment(ctx context.Context, deployment model.TeamDeployment, group model.Group, agents []model.Agent, assignments []model.RoleAssignment, principal model.Principal, requestID model.RequestID, requestDigest string, at time.Time) (model.TeamDeployment, bool, error) {
	if prior, err := s.TeamDeployment(ctx, deployment.ID); err == nil {
		var scope, storedRequestID, digest string
		if readErr := s.db.QueryRowContext(ctx, `SELECT request_scope,request_id,request_digest FROM team_deployments WHERE id=?`, deployment.ID).Scan(&scope, &storedRequestID, &digest); readErr != nil || scope != requestScope(principal) || storedRequestID != string(requestID) || digest != requestDigest {
			return model.TeamDeployment{}, false, app.ErrConflict
		}
		return prior, true, nil
	} else if !errors.Is(err, app.ErrNotFound) {
		return model.TeamDeployment{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.TeamDeployment{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err = requirePendingAutomationAction(ctx, tx, principal, model.AutomationDeployTeam, "", at); err != nil {
		return model.TeamDeployment{}, false, err
	}
	if deployment.TargetKind == model.TeamTargetExistingGroup {
		decision, authorizeErr := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: principal, Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: group.ID}}, at)
		if authorizeErr != nil {
			return model.TeamDeployment{}, false, authorizeErr
		}
		if !decision.Allowed {
			return model.TeamDeployment{}, false, app.ErrUnauthorized
		}
	}
	if (len(deployment.RolePins) != 0 || len(assignments) != 0) && principal.Kind != model.PrincipalOperator {
		return model.TeamDeployment{}, false, app.ErrUnauthorized
	}
	pins := make(map[model.RoleID]model.TeamRolePin, len(deployment.RolePins))
	for _, pin := range deployment.RolePins {
		if _, duplicate := pins[pin.RoleID]; duplicate {
			return model.TeamDeployment{}, false, app.ErrInvalid
		}
		role, roleErr := roleByID(ctx, tx, pin.RoleID)
		if roleErr != nil {
			return model.TeamDeployment{}, false, roleErr
		}
		if role.Revision != pin.Revision || !reflect.DeepEqual(role.Actions, pin.Actions) {
			return model.TeamDeployment{}, false, app.ErrConflict
		}
		pins[pin.RoleID] = pin
	}
	for _, agent := range agents {
		if _, err = tx.ExecContext(ctx, `INSERT INTO agents(id,name,harness,model,effort,working_directory,approval,sandbox,primary_execution_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, agent.ID, agent.Name, agent.Desired.Harness, agent.Desired.Model, agent.Desired.Effort, agent.Desired.WorkingDirectory, agent.Desired.Approval, agent.Desired.Sandbox, agent.PrimaryExecutionID, agent.Revision, nanos(agent.CreatedAt), nanos(agent.UpdatedAt)); err != nil {
			return model.TeamDeployment{}, false, classify(err)
		}
	}
	position := 0
	if deployment.TargetKind == model.TeamTargetExistingGroup {
		if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(position)+1,0) FROM group_members WHERE group_id=?`, group.ID).Scan(&position); err != nil {
			return model.TeamDeployment{}, false, classify(err)
		}
		if _, err = tx.ExecContext(ctx, `UPDATE groups SET revision=revision+1,updated_at=? WHERE id=?`, nanos(at), group.ID); err != nil {
			return model.TeamDeployment{}, false, classify(err)
		}
	} else {
		if _, err = tx.ExecContext(ctx, `INSERT INTO groups(id,name,owner_agent_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?)`, group.ID, group.Name, group.OwnerAgentID, group.Revision, nanos(group.CreatedAt), nanos(group.UpdatedAt)); err != nil {
			return model.TeamDeployment{}, false, classify(err)
		}
	}
	ownedMembers := make(map[model.AgentID]bool, len(deployment.Members))
	for _, member := range deployment.Members {
		ownedMembers[member] = true
	}
	for _, member := range group.Members {
		if !ownedMembers[member] {
			continue
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO group_members(group_id,agent_id,position) VALUES(?,?,?)`, group.ID, member, position); err != nil {
			return model.TeamDeployment{}, false, classify(err)
		}
		position++
	}
	if len(ownedMembers) > 0 {
		if err = requireGroupCapacity(ctx, tx, group.ID); err != nil {
			return model.TeamDeployment{}, false, err
		}
	}
	groupMembers := make(map[model.AgentID]bool, len(group.Members))
	for _, member := range group.Members {
		groupMembers[member] = true
	}
	for _, assignment := range assignments {
		if _, ok := pins[assignment.RoleID]; !ok || assignment.Subject.Kind != model.AuthorityAgent || !groupMembers[assignment.Subject.AgentID] || assignment.Resource.Kind != model.ResourceGroupPeers || assignment.Resource.GroupID != group.ID {
			return model.TeamDeployment{}, false, app.ErrInvalid
		}
		bounds, encodeErr := json.Marshal(assignment.Bounds)
		if encodeErr != nil {
			return model.TeamDeployment{}, false, encodeErr
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO role_assignments(role_id,subject_kind,subject_id,resource_kind,resource_id,bounds_json,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,1,?,?)`, assignment.RoleID, model.AuthorityAgent, assignment.Subject.AgentID, model.ResourceGroupPeers, group.ID, bounds, nanos(assignment.CreatedAt), nanos(assignment.UpdatedAt)); err != nil {
			return model.TeamDeployment{}, false, classify(err)
		}
	}
	definition, _ := json.Marshal(deployment.Definition)
	closure, _ := json.Marshal(deployment.DependencyClosure)
	parameters, _ := json.Marshal(deployment.Parameters)
	members, _ := json.Marshal(deployment.Members)
	rolePins, _ := json.Marshal(deployment.RolePins)
	rules, _ := json.Marshal(deployment.AutomationRuleIDs)
	workspaces, _ := json.Marshal(deployment.Workspaces)
	ownedWorkspaces, _ := json.Marshal(deployment.OwnedWorkspaceIDs)
	ownedRules, _ := json.Marshal(deployment.OwnedAutomationRuleIDs)
	briefingOps, _ := json.Marshal(deployment.BriefingOperationIDs)
	requester, _ := json.Marshal(principal)
	if _, err = tx.ExecContext(ctx, `INSERT INTO team_deployments(id,definition_json,dependency_closure_json,mission,parameters_json,group_id,members_json,role_pins_json,automation_rule_ids_json,work_run_id,target_kind,workspaces_json,owned_workspace_ids_json,owned_automation_rule_ids_json,briefing_operation_ids_json,request_scope,request_id,request_digest,requester_json,advisory_phase,state,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, deployment.ID, definition, closure, deployment.Mission, parameters, deployment.GroupID, members, rolePins, rules, deployment.WorkRunID, deployment.TargetKind, workspaces, ownedWorkspaces, ownedRules, briefingOps, requestScope(principal), requestID, requestDigest, requester, deployment.AdvisoryPhase, deployment.State, deployment.Revision, nanos(deployment.CreatedAt), nanos(deployment.UpdatedAt)); err != nil {
		return model.TeamDeployment{}, false, classify(err)
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.TeamDeployment{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return model.TeamDeployment{}, false, err
	}
	return deployment, false, nil
}

func (s *Store) TeamDeploymentRequester(ctx context.Context, id model.DeploymentID) (model.Principal, error) {
	var encoded []byte
	if err := s.db.QueryRowContext(ctx, `SELECT requester_json FROM team_deployments WHERE id=?`, id).Scan(&encoded); err != nil {
		return model.Principal{}, classify(err)
	}
	if len(encoded) == 0 {
		return model.OperatorPrincipal(), nil
	}
	var principal model.Principal
	if err := json.Unmarshal(encoded, &principal); err != nil {
		return model.Principal{}, err
	}
	return principal, nil
}

func (s *Store) TeamDeployment(ctx context.Context, id model.DeploymentID) (model.TeamDeployment, error) {
	var deployment model.TeamDeployment
	var definition, closure, parameters, members, rolePins, rules, workspaces, ownedWorkspaces, ownedRules, briefingOps []byte
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,definition_json,dependency_closure_json,mission,parameters_json,group_id,members_json,role_pins_json,automation_rule_ids_json,work_run_id,target_kind,workspaces_json,owned_workspace_ids_json,owned_automation_rule_ids_json,briefing_operation_ids_json,advisory_phase,state,revision,created_at,updated_at FROM team_deployments WHERE id=?`, id).Scan(&deployment.ID, &definition, &closure, &deployment.Mission, &parameters, &deployment.GroupID, &members, &rolePins, &rules, &deployment.WorkRunID, &deployment.TargetKind, &workspaces, &ownedWorkspaces, &ownedRules, &briefingOps, &deployment.AdvisoryPhase, &deployment.State, &deployment.Revision, &created, &updated)
	if err != nil {
		return deployment, classify(err)
	}
	if err = unmarshalMany([][]byte{definition, closure, parameters, members, rolePins, rules, workspaces, ownedWorkspaces, ownedRules, briefingOps}, []any{&deployment.Definition, &deployment.DependencyClosure, &deployment.Parameters, &deployment.Members, &deployment.RolePins, &deployment.AutomationRuleIDs, &deployment.Workspaces, &deployment.OwnedWorkspaceIDs, &deployment.OwnedAutomationRuleIDs, &deployment.BriefingOperationIDs}); err != nil {
		return deployment, err
	}
	deployment.CreatedAt, deployment.UpdatedAt = fromNanos(created), fromNanos(updated)
	rows, err := s.db.QueryContext(ctx, `SELECT definition_json,recipient_operations_json,state,created_at,completed_at FROM team_rebriefs WHERE deployment_id=? ORDER BY created_at,request_id`, id)
	if err != nil {
		return deployment, err
	}
	defer rows.Close()
	for rows.Next() {
		var item model.TeamRebrief
		var selected, operations []byte
		var completed sql.NullInt64
		var createdAt int64
		if err = rows.Scan(&selected, &operations, &item.State, &createdAt, &completed); err != nil {
			return deployment, err
		}
		if err = unmarshalMany([][]byte{selected, operations}, []any{&item.Definition, &item.RecipientOperations}); err != nil {
			return deployment, err
		}
		item.CreatedAt = fromNanos(createdAt)
		if completed.Valid {
			value := fromNanos(completed.Int64)
			item.CompletedAt = &value
		}
		deployment.Rebriefs = append(deployment.Rebriefs, item)
	}
	if err = rows.Err(); err != nil {
		return deployment, err
	}
	return deployment, nil
}

func (s *Store) TeamDeploymentByRequest(ctx context.Context, principal model.Principal, requestID model.RequestID, digest string) (model.TeamDeployment, bool, error) {
	var id model.DeploymentID
	var storedDigest string
	err := s.db.QueryRowContext(ctx, `SELECT id,request_digest FROM team_deployments WHERE request_scope=? AND request_id=?`, requestScope(principal), requestID).Scan(&id, &storedDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return model.TeamDeployment{}, false, nil
	}
	if err != nil {
		return model.TeamDeployment{}, false, err
	}
	if storedDigest != digest {
		return model.TeamDeployment{}, false, app.ErrConflict
	}
	deployment, err := s.TeamDeployment(ctx, id)
	return deployment, true, err
}

func (s *Store) ListTeamDeployments(ctx context.Context, groupID model.GroupID) ([]model.TeamDeployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM team_deployments WHERE (?='' OR group_id=?) ORDER BY created_at,id`, groupID, groupID)
	if err != nil {
		return nil, err
	}
	var ids []model.DeploymentID
	for rows.Next() {
		var id model.DeploymentID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	result := make([]model.TeamDeployment, 0, len(ids))
	for _, id := range ids {
		item, readErr := s.TeamDeployment(ctx, id)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, item)
	}
	return result, nil
}

func (s *Store) PendingTeamDeployments(ctx context.Context) ([]model.TeamDeployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM team_deployments WHERE state IN (?,?,?,?) ORDER BY created_at,id`, model.DeploymentDeploying, model.DeploymentPartial, model.DeploymentStandingDown, model.DeploymentReady)
	if err != nil {
		return nil, err
	}
	var ids []model.DeploymentID
	for rows.Next() {
		var id model.DeploymentID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	result := make([]model.TeamDeployment, 0, len(ids))
	for _, id := range ids {
		deployment, readErr := s.TeamDeployment(ctx, id)
		if readErr != nil {
			return nil, readErr
		}
		result = append(result, deployment)
	}
	return result, nil
}

func (s *Store) UpdateTeamDeployment(ctx context.Context, id model.DeploymentID, expected model.Revision, state model.DeploymentState, phase uint32, at time.Time) (model.TeamDeployment, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE team_deployments SET state=?,advisory_phase=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, state, phase, nanos(at), id, expected)
	if err != nil {
		return model.TeamDeployment{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return model.TeamDeployment{}, app.ErrConflict
	}
	if err = s.bump(ctx); err != nil {
		return model.TeamDeployment{}, err
	}
	return s.TeamDeployment(ctx, id)
}

func (s *Store) RecordTeamBriefingOperations(ctx context.Context, id model.DeploymentID, expected model.Revision, memberKey string, operations []model.OperationID, at time.Time) (model.TeamDeployment, error) {
	deployment, err := s.TeamDeployment(ctx, id)
	if err != nil {
		return model.TeamDeployment{}, err
	}
	if deployment.Revision != expected {
		return model.TeamDeployment{}, app.ErrConflict
	}
	if deployment.BriefingOperationIDs == nil {
		deployment.BriefingOperationIDs = make(map[string][]model.OperationID)
	}
	if reflect.DeepEqual(deployment.BriefingOperationIDs[memberKey], operations) {
		return deployment, nil
	}
	deployment.BriefingOperationIDs[memberKey] = append([]model.OperationID(nil), operations...)
	encoded, _ := json.Marshal(deployment.BriefingOperationIDs)
	result, err := s.db.ExecContext(ctx, `UPDATE team_deployments SET briefing_operation_ids_json=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, encoded, nanos(at), id, expected)
	if err != nil {
		return model.TeamDeployment{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return model.TeamDeployment{}, app.ErrConflict
	}
	if err = s.bump(ctx); err != nil {
		return model.TeamDeployment{}, err
	}
	return s.TeamDeployment(ctx, id)
}

func (s *Store) BeginTeamRebrief(ctx context.Context, id model.DeploymentID, expected model.Revision, definition model.DefinitionRef, principal model.Principal, requestID model.RequestID, digest string, at time.Time) (model.TeamDeployment, model.TeamRebrief, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var storedID model.DeploymentID
	var storedDigest string
	var selected, operations []byte
	var state model.TeamRebriefState
	var created int64
	var completed sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT deployment_id,request_digest,definition_json,recipient_operations_json,state,created_at,completed_at FROM team_rebriefs WHERE request_scope=? AND request_id=?`, requestScope(principal), requestID).Scan(&storedID, &storedDigest, &selected, &operations, &state, &created, &completed)
	if err == nil {
		if storedID != id || storedDigest != digest {
			return model.TeamDeployment{}, model.TeamRebrief{}, false, app.ErrConflict
		}
		var item model.TeamRebrief
		item.State, item.CreatedAt = state, fromNanos(created)
		if err = unmarshalMany([][]byte{selected, operations}, []any{&item.Definition, &item.RecipientOperations}); err != nil {
			return model.TeamDeployment{}, model.TeamRebrief{}, false, err
		}
		if completed.Valid {
			value := fromNanos(completed.Int64)
			item.CompletedAt = &value
		}
		if err = tx.Commit(); err != nil {
			return model.TeamDeployment{}, model.TeamRebrief{}, false, err
		}
		deployment, readErr := s.TeamDeployment(ctx, id)
		return deployment, item, true, readErr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, err
	}
	var revision model.Revision
	var deploymentState model.DeploymentState
	var groupID model.GroupID
	if err = tx.QueryRowContext(ctx, `SELECT revision,state,group_id FROM team_deployments WHERE id=?`, id).Scan(&revision, &deploymentState, &groupID); err != nil {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, classify(err)
	}
	if revision != expected || deploymentState == model.DeploymentStopped || deploymentState == model.DeploymentStandingDown {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, app.ErrConflict
	}
	decision, authorizeErr := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: principal, Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: groupID}}, at)
	if authorizeErr != nil {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, authorizeErr
	}
	if !decision.Allowed {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, app.ErrUnauthorized
	}
	request := app.RebriefDeploymentRequest{Context: app.RequestContext{Principal: principal, RequestID: requestID}, DeploymentID: id, ExpectedRevision: expected, Definition: definition}
	if err = saveTeamContinuation(ctx, tx, id, "rebrief", principal, requestID, request); err != nil {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, err
	}
	selected, _ = json.Marshal(definition)
	if _, err = tx.ExecContext(ctx, `INSERT INTO team_rebriefs(deployment_id,request_scope,request_id,request_digest,definition_json,recipient_operations_json,state,created_at) VALUES(?,?,?,?,?,'{}',?,?)`, id, requestScope(principal), requestID, digest, selected, model.TeamRebriefDelivering, nanos(at)); err != nil {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, classify(err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE team_deployments SET revision=revision+1,updated_at=? WHERE id=? AND revision=?`, nanos(at), id, expected); err != nil {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return model.TeamDeployment{}, model.TeamRebrief{}, false, err
	}
	deployment, err := s.TeamDeployment(ctx, id)
	return deployment, model.TeamRebrief{Definition: definition, RecipientOperations: map[string][]model.OperationID{}, State: model.TeamRebriefDelivering, CreatedAt: at}, false, err
}

func (s *Store) CompleteTeamRebrief(ctx context.Context, id model.DeploymentID, principal model.Principal, requestID model.RequestID, operations map[string][]model.OperationID, at time.Time) (model.TeamDeployment, error) {
	encoded, _ := json.Marshal(operations)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.TeamDeployment{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var storedID model.DeploymentID
	var state model.TeamRebriefState
	var stored []byte
	if err = tx.QueryRowContext(ctx, `SELECT deployment_id,state,recipient_operations_json FROM team_rebriefs WHERE request_scope=? AND request_id=?`, requestScope(principal), requestID).Scan(&storedID, &state, &stored); err != nil {
		return model.TeamDeployment{}, classify(err)
	}
	if storedID != id {
		return model.TeamDeployment{}, app.ErrConflict
	}
	if state == model.TeamRebriefCompleted {
		if !reflect.DeepEqual(stored, encoded) {
			return model.TeamDeployment{}, app.ErrConflict
		}
		_ = tx.Commit()
		return s.TeamDeployment(ctx, id)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE team_rebriefs SET recipient_operations_json=?,state=?,completed_at=? WHERE request_scope=? AND request_id=? AND state=?`, encoded, model.TeamRebriefCompleted, nanos(at), requestScope(principal), requestID, model.TeamRebriefDelivering); err != nil {
		return model.TeamDeployment{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE team_deployments SET revision=revision+1,updated_at=? WHERE id=?`, nanos(at), id); err != nil {
		return model.TeamDeployment{}, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.TeamDeployment{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.TeamDeployment{}, err
	}
	return s.TeamDeployment(ctx, id)
}

func (s *Store) AdvanceTeamAdvisoryPhase(ctx context.Context, id model.DeploymentID, expected model.Revision, principal model.Principal, requestID model.RequestID, digest string, at time.Time) (model.TeamDeployment, error) {
	return s.mutateTeamLifecycle(ctx, id, expected, principal, requestID, "advance_phase", digest, at, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE team_deployments SET advisory_phase=advisory_phase+1,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND state<>?`, nanos(at), id, expected, model.DeploymentStopped)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return app.ErrConflict
		}
		return nil
	})
}

func (s *Store) BeginTeamStandDown(ctx context.Context, id model.DeploymentID, expected model.Revision, principal model.Principal, requestID model.RequestID, digest, reason string, at time.Time) (model.TeamDeployment, error) {
	return s.mutateTeamLifecycle(ctx, id, expected, principal, requestID, "stand_down", digest, at, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `UPDATE team_deployments SET state=?,revision=revision+1,updated_at=? WHERE id=? AND revision=? AND state<>?`, model.DeploymentStandingDown, nanos(at), id, expected, model.DeploymentStopped)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return app.ErrConflict
		}
		// A new admitted command explicitly replaces the continuation actor; an
		// exact retry above never changes the actor or original reason.
		if _, err = tx.ExecContext(ctx, `DELETE FROM team_continuations WHERE deployment_id=? AND kind='stand_down'`, id); err != nil {
			return err
		}
		request := app.StandDownDeploymentRequest{Context: app.RequestContext{Principal: principal, RequestID: requestID}, DeploymentID: id, ExpectedRevision: expected, Reason: reason}
		return saveTeamContinuation(ctx, tx, id, "stand_down", principal, requestID, request)
	})
}

func (s *Store) mutateTeamLifecycle(ctx context.Context, id model.DeploymentID, expected model.Revision, principal model.Principal, requestID model.RequestID, kind, digest string, at time.Time, mutate func(*sql.Tx) error) (model.TeamDeployment, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.TeamDeployment{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var storedID model.DeploymentID
	var storedKind, storedDigest string
	err = tx.QueryRowContext(ctx, `SELECT deployment_id,kind,request_digest FROM team_lifecycle_requests WHERE request_scope=? AND request_id=?`, requestScope(principal), requestID).Scan(&storedID, &storedKind, &storedDigest)
	if err == nil {
		if storedID != id || storedKind != kind || storedDigest != digest {
			return model.TeamDeployment{}, app.ErrConflict
		}
		_ = tx.Commit()
		return s.TeamDeployment(ctx, id)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.TeamDeployment{}, err
	}
	var groupID model.GroupID
	if err = tx.QueryRowContext(ctx, `SELECT group_id FROM team_deployments WHERE id=?`, id).Scan(&groupID); err != nil {
		return model.TeamDeployment{}, classify(err)
	}
	decision, authorizeErr := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: principal, Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: groupID}}, at)
	if authorizeErr != nil {
		return model.TeamDeployment{}, authorizeErr
	}
	if !decision.Allowed {
		return model.TeamDeployment{}, app.ErrUnauthorized
	}
	if err = mutate(tx); err != nil {
		return model.TeamDeployment{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO team_lifecycle_requests(request_scope,request_id,deployment_id,kind,request_digest,created_at) VALUES(?,?,?,?,?,?)`, requestScope(principal), requestID, id, kind, digest, nanos(at)); err != nil {
		return model.TeamDeployment{}, classify(err)
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.TeamDeployment{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.TeamDeployment{}, err
	}
	return s.TeamDeployment(ctx, id)
}

func (s *Store) RecordExecutionReadiness(ctx context.Context, id model.ExecutionID, attempt model.AttemptGeneration, readiness model.ContextReadiness, at time.Time) (model.Execution, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE executions SET context_readiness=?,revision=revision+1,updated_at=? WHERE id=? AND attempt_generation=? AND state NOT IN (?,?)`, readiness, nanos(at), id, attempt, model.ExecutionExited, model.ExecutionFailed)
	if err != nil {
		return model.Execution{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return model.Execution{}, app.ErrConflict
	}
	if err = s.bump(ctx); err != nil {
		return model.Execution{}, err
	}
	return s.Execution(ctx, id)
}

// Continuation requests are private application state and never deployment DTOs.
func saveTeamContinuation(ctx context.Context, tx *sql.Tx, id model.DeploymentID, kind string, principal model.Principal, requestID model.RequestID, request any) error {
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO team_continuations(deployment_id,kind,request_scope,request_id,request_json) VALUES(?,?,?,?,?)`, id, kind, requestScope(principal), requestID, encoded)
	return err
}

func (s *Store) TeamStandDownRequest(ctx context.Context, id model.DeploymentID) (app.StandDownDeploymentRequest, error) {
	var encoded []byte
	err := s.db.QueryRowContext(ctx, `SELECT request_json FROM team_continuations WHERE deployment_id=? AND kind='stand_down'`, id).Scan(&encoded)
	if err != nil {
		return app.StandDownDeploymentRequest{}, classify(err)
	}
	var request app.StandDownDeploymentRequest
	err = json.Unmarshal(encoded, &request)
	return request, err
}

func (s *Store) PendingTeamRebriefs(ctx context.Context) ([]app.RebriefDeploymentRequest, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT c.request_json FROM team_continuations c JOIN team_rebriefs r ON r.request_scope=c.request_scope AND r.request_id=c.request_id AND r.deployment_id=c.deployment_id JOIN team_deployments d ON d.id=c.deployment_id WHERE c.kind='rebrief' AND r.state=? AND d.state NOT IN (?,?) ORDER BY r.created_at,r.request_id`, model.TeamRebriefDelivering, model.DeploymentStopped, model.DeploymentStandingDown)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var requests []app.RebriefDeploymentRequest
	for rows.Next() {
		var encoded []byte
		if err = rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var request app.RebriefDeploymentRequest
		if err = json.Unmarshal(encoded, &request); err != nil {
			return nil, err
		}
		requests = append(requests, request)
	}
	return requests, rows.Err()
}
