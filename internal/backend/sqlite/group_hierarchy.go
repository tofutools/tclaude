package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"time"
)

const groupHierarchySchema = `CREATE TABLE IF NOT EXISTS group_parents(group_id TEXT PRIMARY KEY REFERENCES groups(id),parent_id TEXT NOT NULL REFERENCES groups(id));
CREATE TABLE IF NOT EXISTS group_parent_requests(scope TEXT NOT NULL,request_id TEXT NOT NULL,intent BLOB NOT NULL,result BLOB NOT NULL,PRIMARY KEY(scope,request_id));`

func (s *Store) SetGroupParent(ctx context.Context, in app.SetGroupParentRequest, at time.Time) (model.Group, error) {
	var out model.Group
	if in.Context.Principal.Kind != model.PrincipalOperator {
		return out, app.ErrUnauthorized
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	intent, err := json.Marshal(struct {
		ID, Parent model.GroupID
		Revision   model.Revision
	}{in.ID, in.ParentGroupID, in.ExpectedRevision})
	if err != nil {
		return out, err
	}
	var prior, stored []byte
	err = tx.QueryRowContext(ctx, `SELECT intent,result FROM group_parent_requests WHERE scope=? AND request_id=?`, requestScope(in.Context.Principal), in.Context.RequestID).Scan(&prior, &stored)
	if err == nil {
		if string(prior) != string(intent) {
			return out, app.ErrConflict
		}
		err = json.Unmarshal(stored, &out)
		return out, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, err
	}
	out, err = readGroup(ctx, tx, in.ID)
	if err != nil {
		return out, err
	}
	if out.Revision != in.ExpectedRevision {
		return model.Group{}, app.ErrConflict
	}
	rows, err := tx.QueryContext(ctx, `SELECT g.id,COALESCE(p.parent_id,'') FROM groups g LEFT JOIN group_parents p ON p.group_id=g.id WHERE g.tombstoned=0`)
	if err != nil {
		return out, err
	}
	groups := []model.Group{}
	for rows.Next() {
		var g model.Group
		if err = rows.Scan(&g.ID, &g.ParentGroupID); err != nil {
			_ = rows.Close()
			return out, err
		}
		if g.ID == in.ID {
			g.ParentGroupID = in.ParentGroupID
		}
		groups = append(groups, g)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return out, err
	}
	if model.ValidateGroupHierarchy(groups) != nil {
		return model.Group{}, app.ErrInvalid
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM group_parents WHERE group_id=?`, in.ID); err != nil {
		return out, err
	}
	if in.ParentGroupID != "" {
		if _, err = tx.ExecContext(ctx, `INSERT INTO group_parents(group_id,parent_id) VALUES(?,?)`, in.ID, in.ParentGroupID); err != nil {
			return out, classify(err)
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE groups SET revision=revision+1,updated_at=? WHERE id=?`, nanos(at), in.ID); err != nil {
		return out, err
	}
	out.ParentGroupID = in.ParentGroupID
	out.Revision++
	out.UpdatedAt = at
	stored, err = json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO group_parent_requests(scope,request_id,intent,result) VALUES(?,?,?,?)`, requestScope(in.Context.Principal), in.Context.RequestID, intent, stored); err != nil {
		return out, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
