package sqlite

import (
	"context"
	"database/sql"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func requireWorkDirectoryTrustSources(ctx context.Context, tx *sql.Tx, trust *model.WorkDirectoryTrust) error {
	if trust == nil {
		return nil
	}
	for id, expected := range trust.AgentRevisions {
		var revision model.Revision
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM agents WHERE id=?`, id).Scan(&revision); err != nil {
			return classify(err)
		}
		if revision != expected {
			return app.ErrConflict
		}
	}
	for id, expected := range trust.WorkspaceRevisions {
		var revision model.Revision
		if err := tx.QueryRowContext(ctx, `SELECT revision FROM workspaces WHERE id=?`, id).Scan(&revision); err != nil {
			return classify(err)
		}
		if revision != expected {
			return app.ErrConflict
		}
		if err := requireNoWorkspaceRemovalTx(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}
