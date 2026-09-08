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

// Library removal never rewrites an immutable revision or an admitted pin.
func (s *Store) SetDefinitionArchived(ctx context.Context, req app.SetDefinitionArchivedRequest, now time.Time) (model.Definition, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Definition{}, err
	}
	defer func() { _ = tx.Rollback() }()
	decision, err := authorizeTx(ctx, tx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionManageDefinition, Resource: model.ResourceSelector{Kind: model.ResourceDefinition, DefinitionID: req.ID}}, now)
	if err != nil {
		return model.Definition{}, err
	}
	if !decision.Allowed {
		return model.Definition{}, app.ErrUnauthorized
	}
	var priorID model.DefinitionID
	var priorRevision model.Revision
	var priorArchived bool
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT definition_id,expected_revision,archived,result_json FROM definition_archive_requests WHERE request_scope=? AND request_id=?`, requestScope(req.Context.Principal), req.Context.RequestID).Scan(&priorID, &priorRevision, &priorArchived, &data)
	if err == nil {
		if priorID != req.ID || priorRevision != req.ExpectedRevision || priorArchived != req.Archived {
			return model.Definition{}, app.ErrConflict
		}
		var prior model.Definition
		err = json.Unmarshal(data, &prior)
		return prior, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return model.Definition{}, err
	}
	var definition model.Definition
	var created, updated int64
	err = tx.QueryRowContext(ctx, `SELECT id,name,kind,head_revision_id,tombstoned,revision,created_at,updated_at FROM definitions WHERE id=?`, req.ID).Scan(&definition.ID, &definition.Name, &definition.Kind, &definition.HeadRevisionID, &definition.Tombstoned, &definition.Revision, &created, &updated)
	if err != nil {
		return definition, classify(err)
	}
	if definition.Revision != req.ExpectedRevision {
		return definition, app.ErrConflict
	}
	definition.CreatedAt, definition.UpdatedAt = fromNanos(created), fromNanos(updated)
	if definition.Tombstoned != req.Archived {
		definition.Tombstoned = req.Archived
		definition.Revision++
		definition.UpdatedAt = now
		if _, err = tx.ExecContext(ctx, `UPDATE definitions SET tombstoned=?,revision=?,updated_at=? WHERE id=?`, definition.Tombstoned, definition.Revision, nanos(now), definition.ID); err != nil {
			return definition, err
		}
		if err = bumpTx(ctx, tx); err != nil {
			return definition, err
		}
	}
	data, err = json.Marshal(definition)
	if err != nil {
		return definition, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO definition_archive_requests(request_scope,request_id,definition_id,expected_revision,archived,result_json) VALUES(?,?,?,?,?,?)`, requestScope(req.Context.Principal), req.Context.RequestID, req.ID, req.ExpectedRevision, req.Archived, data); err != nil {
		return definition, err
	}
	return definition, tx.Commit()
}
