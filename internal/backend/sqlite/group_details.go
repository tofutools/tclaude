package sqlite

import (
	"context"
	"encoding/json"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"time"
)

const groupDetailsSchema = `CREATE TABLE IF NOT EXISTS group_details(group_id TEXT PRIMARY KEY REFERENCES groups(id),record BLOB NOT NULL);`

func (s *Store) SetGroupDetails(ctx context.Context, in app.SetGroupDetailsRequest, at time.Time) (model.Group, error) {
	if in.Principal.Kind != model.PrincipalOperator {
		return model.Group{}, app.ErrUnauthorized
	}
	if model.ValidateGroupDetails(in.Details) != nil {
		return model.Group{}, app.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Group{}, err
	}
	defer func() { _ = tx.Rollback() }()
	group, err := readGroup(ctx, tx, in.ID)
	if err != nil {
		return group, err
	}
	if group.Revision != in.ExpectedRevision {
		return model.Group{}, app.ErrConflict
	}
	if in.Details == (model.GroupDetails{}) {
		_, err = tx.ExecContext(ctx, `DELETE FROM group_details WHERE group_id=?`, in.ID)
		group.Details = nil
	} else {
		data, _ := json.Marshal(in.Details)
		_, err = tx.ExecContext(ctx, `INSERT INTO group_details(group_id,record) VALUES(?,?) ON CONFLICT(group_id) DO UPDATE SET record=excluded.record`, in.ID, data)
		group.Details = &in.Details
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
	group.Revision++
	group.UpdatedAt = at
	return group, tx.Commit()
}
