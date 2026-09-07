package sqlite

import (
	"context"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Store) ExecutionFileTarget(ctx context.Context, principal model.Principal, id model.ExecutionID, at time.Time) (model.Execution, error) {
	var empty model.Execution
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return empty, err
	}
	defer func() { _ = tx.Rollback() }()
	execution, err := executionTx(ctx, tx, id)
	if err != nil {
		return empty, err
	}
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: principal, Action: model.ActionReadExecutionFile, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: id}}, at)
	if err != nil {
		return empty, err
	}
	if !decision.Allowed {
		return empty, app.ErrUnauthorized
	}
	if err = terminalFileExecutionEligible(ctx, tx, execution); err != nil {
		return empty, err
	}
	return execution, nil
}
