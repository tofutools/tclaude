package sqlite

import (
	"context"
	"database/sql"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"time"
)

const groupCapacitySchema = `CREATE TABLE IF NOT EXISTS group_capacity(group_id TEXT PRIMARY KEY REFERENCES groups(id),max_active_members INTEGER NOT NULL CHECK(max_active_members > 0));`

func (s *Store) SetGroupCapacity(ctx context.Context, in app.SetGroupCapacityRequest, at time.Time) (model.Group, error) {
	if in.Principal.Kind != model.PrincipalOperator {
		return model.Group{}, app.ErrUnauthorized
	}
	if !model.ValidGroupCapacity(in.MaxActiveMembers) {
		return model.Group{}, app.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Group{}, err
	}
	defer func() { _ = tx.Rollback() }()
	group, err := readGroup(ctx, tx, in.ID)
	if err != nil {
		return model.Group{}, err
	}
	if group.Revision != in.ExpectedRevision {
		return model.Group{}, app.ErrConflict
	}
	if in.MaxActiveMembers == 0 {
		_, err = tx.ExecContext(ctx, `DELETE FROM group_capacity WHERE group_id=?`, in.ID)
	} else {
		_, err = tx.ExecContext(ctx, `INSERT INTO group_capacity(group_id,max_active_members) VALUES(?,?) ON CONFLICT(group_id) DO UPDATE SET max_active_members=excluded.max_active_members`, in.ID, in.MaxActiveMembers)
	}
	if err != nil {
		return model.Group{}, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE groups SET revision=revision+1,updated_at=? WHERE id=?`, nanos(at), in.ID); err != nil {
		return model.Group{}, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.Group{}, err
	}
	group.MaxActiveMembers = in.MaxActiveMembers
	group.Revision++
	group.UpdatedAt = at
	return group, tx.Commit()
}

// Check the resulting membership inside the mutation transaction. Callers skip
// this only when no new active membership is being admitted, so an over-limit
// group can still remove members and edit its name or order.
func requireGroupCapacity(ctx context.Context, tx *sql.Tx, id model.GroupID) error {
	var exceeded bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_capacity c WHERE c.group_id=? AND c.max_active_members < (SELECT COUNT(*) FROM group_members gm JOIN agents a ON a.id=gm.agent_id WHERE gm.group_id=c.group_id AND a.lifecycle_state=?))`, id, model.AgentActive).Scan(&exceeded)
	if err != nil {
		return err
	}
	if exceeded {
		return app.ErrConflict
	}
	return nil
}

func requireReactivationCapacity(ctx context.Context, tx *sql.Tx, id model.AgentID) error {
	var exceeded bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_members own JOIN group_capacity c ON c.group_id=own.group_id WHERE own.agent_id=? AND c.max_active_members < (SELECT COUNT(*) FROM group_members gm JOIN agents a ON a.id=gm.agent_id WHERE gm.group_id=c.group_id AND a.lifecycle_state=?))`, id, model.AgentActive).Scan(&exceeded)
	if err != nil {
		return err
	}
	if exceeded {
		return app.ErrConflict
	}
	return nil
}
