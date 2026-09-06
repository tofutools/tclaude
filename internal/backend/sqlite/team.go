package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Store) CreateTeamDeployment(ctx context.Context, deployment model.TeamDeployment, group model.Group, agents []model.Agent) (model.TeamDeployment, bool, error) {
	if prior, err := s.TeamDeployment(ctx, deployment.ID); err == nil {
		if prior.Definition != deployment.Definition || prior.GroupID != deployment.GroupID || prior.Mission != deployment.Mission || !reflect.DeepEqual(prior.Members, deployment.Members) {
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
	for _, agent := range agents {
		if _, err = tx.ExecContext(ctx, `INSERT INTO agents(id,name,harness,model,working_directory,approval,sandbox,primary_execution_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, agent.ID, agent.Name, agent.Desired.Harness, agent.Desired.Model, agent.Desired.WorkingDirectory, agent.Desired.Approval, agent.Desired.Sandbox, agent.PrimaryExecutionID, agent.Revision, nanos(agent.CreatedAt), nanos(agent.UpdatedAt)); err != nil {
			return model.TeamDeployment{}, false, classify(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO groups(id,name,owner_agent_id,revision,created_at,updated_at) VALUES(?,?,?,?,?,?)`, group.ID, group.Name, group.OwnerAgentID, group.Revision, nanos(group.CreatedAt), nanos(group.UpdatedAt)); err != nil {
		return model.TeamDeployment{}, false, classify(err)
	}
	for position, member := range group.Members {
		if _, err = tx.ExecContext(ctx, `INSERT INTO group_members(group_id,agent_id,position) VALUES(?,?,?)`, group.ID, member, position); err != nil {
			return model.TeamDeployment{}, false, classify(err)
		}
	}
	definition, _ := json.Marshal(deployment.Definition)
	closure, _ := json.Marshal(deployment.DependencyClosure)
	parameters, _ := json.Marshal(deployment.Parameters)
	members, _ := json.Marshal(deployment.Members)
	rules, _ := json.Marshal(deployment.AutomationRuleIDs)
	if _, err = tx.ExecContext(ctx, `INSERT INTO team_deployments(id,definition_json,dependency_closure_json,mission,parameters_json,group_id,members_json,automation_rule_ids_json,work_run_id,advisory_phase,state,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, deployment.ID, definition, closure, deployment.Mission, parameters, deployment.GroupID, members, rules, deployment.WorkRunID, deployment.AdvisoryPhase, deployment.State, deployment.Revision, nanos(deployment.CreatedAt), nanos(deployment.UpdatedAt)); err != nil {
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

func (s *Store) TeamDeployment(ctx context.Context, id model.DeploymentID) (model.TeamDeployment, error) {
	var deployment model.TeamDeployment
	var definition, closure, parameters, members, rules []byte
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id,definition_json,dependency_closure_json,mission,parameters_json,group_id,members_json,automation_rule_ids_json,work_run_id,advisory_phase,state,revision,created_at,updated_at FROM team_deployments WHERE id=?`, id).Scan(&deployment.ID, &definition, &closure, &deployment.Mission, &parameters, &deployment.GroupID, &members, &rules, &deployment.WorkRunID, &deployment.AdvisoryPhase, &deployment.State, &deployment.Revision, &created, &updated)
	if err != nil {
		return deployment, classify(err)
	}
	if err = unmarshalMany([][]byte{definition, closure, parameters, members, rules}, []any{&deployment.Definition, &deployment.DependencyClosure, &deployment.Parameters, &deployment.Members, &deployment.AutomationRuleIDs}); err != nil {
		return deployment, err
	}
	deployment.CreatedAt, deployment.UpdatedAt = fromNanos(created), fromNanos(updated)
	return deployment, nil
}

func (s *Store) PendingTeamDeployments(ctx context.Context) ([]model.TeamDeployment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM team_deployments WHERE state IN (?,?,?) ORDER BY created_at,id`, model.DeploymentDeploying, model.DeploymentPartial, model.DeploymentStandingDown)
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
