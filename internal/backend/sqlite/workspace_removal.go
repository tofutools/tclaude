package sqlite

import (
	"context"
	"database/sql"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"time"
)

const workspaceRemovalSchema = `CREATE TABLE IF NOT EXISTS workspace_removal_intents(operation_id TEXT PRIMARY KEY REFERENCES operations(id),expected_revision INTEGER NOT NULL,destructive INTEGER NOT NULL);`

func checkWorkspaceRemovalIntent(ctx context.Context, tx *sql.Tx, id model.OperationID, in app.RemoveCheckoutRequest) error {
	var revision model.Revision
	var destructive bool
	if err := tx.QueryRowContext(ctx, `SELECT expected_revision,destructive FROM workspace_removal_intents WHERE operation_id=?`, id).Scan(&revision, &destructive); err != nil {
		return classify(err)
	}
	if revision != in.ExpectedRevision || destructive != in.Destructive {
		return app.ErrConflict
	}
	return nil
}

// Completed/admitted receipts are reachable before current workspace preflight.
// This lookup never invokes the native removal effect again.
func (s *Store) FindWorkspaceRemoval(ctx context.Context, in app.RemoveCheckoutRequest, at time.Time) (app.WorkspaceEffectAdmissionResult, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.WorkspaceEffectAdmissionResult{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: in.Context.Principal, Action: model.ActionRemoveWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: in.WorkspaceID}}, at)
	if err != nil {
		return app.WorkspaceEffectAdmissionResult{}, false, err
	}
	if !decision.Allowed {
		return app.WorkspaceEffectAdmissionResult{}, false, app.ErrUnauthorized
	}
	out, found, err := resourceAdmissionByRequest(ctx, tx, model.Operation{RequestID: in.Context.RequestID, Principal: in.Context.Principal, Kind: model.OperationRemoveWorkspace}, model.Workspace{ID: in.WorkspaceID})
	if err == nil && found {
		err = checkWorkspaceRemovalIntent(ctx, tx, out.Operation.ID, in)
	}
	return out, found, err
}
