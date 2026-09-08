package sqlite

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"slices"
	"time"
)

func (s *Store) UpdateGroup(ctx context.Context, in app.UpdateGroupRequest, at time.Time) (model.Group, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Group{}, err
	}
	defer func() { _ = tx.Rollback() }()
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: in.Context, Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: in.ID}}, at)
	if err != nil {
		return model.Group{}, err
	}
	if !decision.Allowed {
		return model.Group{}, app.ErrUnauthorized
	}
	group, err := readGroup(ctx, tx, in.ID)
	if err != nil {
		return model.Group{}, err
	}
	if group.Revision != in.ExpectedRevision {
		return model.Group{}, app.ErrConflict
	}
	freshActive := false
	for _, id := range in.Members {
		var lifecycle model.AgentLifecycleState
		var retained bool
		err = tx.QueryRowContext(ctx, `SELECT lifecycle_state,EXISTS(SELECT 1 FROM group_members WHERE group_id=? AND agent_id=agents.id) FROM agents WHERE id=?`, in.ID, id).Scan(&lifecycle, &retained)
		if err != nil {
			return model.Group{}, classify(err)
		}
		if lifecycle == model.AgentActive && !retained {
			freshActive = true
		}
		if lifecycle != model.AgentActive && !retained {
			return model.Group{}, app.ErrConflict
		}
	}
	for _, owner := range group.OwnerAgentIDs {
		if !slices.Contains(in.Members, owner) {
			return model.Group{}, app.ErrConflict
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE groups SET name=?,revision=revision+1,updated_at=? WHERE id=?`, in.Name, nanos(at), in.ID); err != nil {
		return model.Group{}, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM group_members WHERE group_id=?`, in.ID); err != nil {
		return model.Group{}, err
	}
	for position, id := range in.Members {
		if _, err = tx.ExecContext(ctx, `INSERT INTO group_members(group_id,agent_id,position) VALUES(?,?,?)`, in.ID, id, position); err != nil {
			return model.Group{}, classify(err)
		}
	}
	if freshActive {
		if err = requireGroupCapacity(ctx, tx, in.ID); err != nil {
			return model.Group{}, err
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.Group{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Group{}, err
	}
	group.Name = in.Name
	group.Members = append([]model.AgentID(nil), in.Members...)
	group.Revision++
	group.UpdatedAt = at
	return group, nil
}
