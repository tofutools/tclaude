package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"time"
)

func requireSelectedWorkspaceTx(ctx context.Context, tx *sql.Tx, selected model.WorkspaceSelection, path string, principal model.Principal, at time.Time) (model.Workspace, error) {
	workspace, err := scanWorkspace(tx.QueryRowContext(ctx, `SELECT id,intent_json,state,observation_json,resource_owner,resource_version,resource_payload,revision,created_at,updated_at FROM workspaces WHERE id=?`, selected.WorkspaceID))
	if err != nil {
		return workspace, err
	}
	if workspace.State != model.WorkspaceAvailable || workspace.Observation.ActualPath == "" || workspace.Observation.ActualPath != path || (selected.ExpectedRevision != 0 && workspace.Revision != selected.ExpectedRevision) {
		return workspace, app.ErrConflict
	}
	if err := requireNoWorkspaceRemovalTx(ctx, tx, workspace.ID); err != nil {
		return workspace, err
	}
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: principal, Action: model.ActionInspectWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: workspace.ID}}, at)
	if err != nil {
		return workspace, err
	}
	if !decision.Allowed {
		return workspace, app.ErrUnauthorized
	}
	return workspace, nil
}

// A saved checkout choice follows the agent across ordinary starts and restarts.
// A use is published with each execution, before any native preparation.
func admitAgentWorkspaceUseTx(ctx context.Context, tx *sql.Tx, in app.LaunchAdmission) error {
	if in.AgentID == "" {
		return nil
	}
	var id model.WorkspaceID
	var path string
	err := tx.QueryRowContext(ctx, `SELECT workspace_id,working_directory FROM agent_workspaces WHERE agent_id=?`, in.AgentID).Scan(&id, &path)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	executionPath := in.Execution.Spec.WorkingDirectory
	if in.ProvenWorkingDirectory == executionPath && in.Authority.RequestedConfiguration != nil {
		executionPath = in.Authority.RequestedConfiguration.WorkingDirectory
	}
	if path != executionPath {
		return app.ErrConflict
	}
	if _, err := requireSelectedWorkspaceTx(ctx, tx, model.WorkspaceSelection{WorkspaceID: id}, path, in.Operation.Principal, in.Operation.CreatedAt); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO workspace_uses(id,workspace_id,execution_id,work_run_id,created_at) VALUES(?,?,?,'',?)`, "use_"+string(in.Execution.ID), id, in.Execution.ID, nanos(in.Execution.CreatedAt))
	return err
}
