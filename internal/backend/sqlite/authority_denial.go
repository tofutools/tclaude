package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const denialSelect = `SELECT id,subject_kind,subject_id,action,revision,created_at,updated_at FROM authority_denials`

func scanDenial(row scanner) (model.AuthorityDenial, error) {
	var d model.AuthorityDenial
	var kind, id string
	var created, updated int64
	err := row.Scan(&d.ID, &kind, &id, &d.Action, &d.Revision, &created, &updated)
	d.Subject = makeSubject(kind, id)
	d.CreatedAt, d.UpdatedAt = fromNanos(created), fromNanos(updated)
	return d, classify(err)
}
func (s *Store) PutDenial(ctx context.Context, denial model.AuthorityDenial, expected model.Revision) (model.AuthorityDenial, error) {
	if err := app.ValidateAuthorityDenial(denial); err != nil {
		return model.AuthorityDenial{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.AuthorityDenial{}, err
	}
	defer func() { _ = tx.Rollback() }()
	kind, id := subjectParts(denial.Subject)
	if expected == 0 {
		_, err = tx.ExecContext(ctx, `INSERT INTO authority_denials(id,subject_kind,subject_id,action,revision,created_at,updated_at) VALUES(?,?,?,?,1,?,?)`, denial.ID, kind, id, denial.Action, nanos(denial.CreatedAt), nanos(denial.UpdatedAt))
	} else {
		var result sql.Result
		result, err = tx.ExecContext(ctx, `UPDATE authority_denials SET subject_kind=?,subject_id=?,action=?,revision=revision+1,updated_at=? WHERE id=? AND revision=?`, kind, id, denial.Action, nanos(denial.UpdatedAt), denial.ID, expected)
		if err == nil {
			if n, _ := result.RowsAffected(); n != 1 {
				return model.AuthorityDenial{}, app.ErrConflict
			}
		}
	}
	if err != nil {
		return model.AuthorityDenial{}, classify(err)
	}
	saved, err := scanDenial(tx.QueryRowContext(ctx, denialSelect+` WHERE id=?`, denial.ID))
	if err != nil {
		return model.AuthorityDenial{}, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return model.AuthorityDenial{}, err
	}
	return saved, tx.Commit()
}
func (s *Store) DeleteDenial(ctx context.Context, id model.DenialID, expected model.Revision) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.ExecContext(ctx, `DELETE FROM authority_denials WHERE id=? AND revision=?`, id, expected)
	if err != nil {
		return classify(err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return app.ErrConflict
	}
	if err = bumpTx(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}
func deniedAuthority(ctx context.Context, q queryer, subject model.AuthoritySubject, request model.AuthorityRequest) (model.AuthorityDecision, bool, error) {
	kind, id := subjectParts(subject)
	denial, err := scanDenial(q.QueryRowContext(ctx, denialSelect+` WHERE subject_kind=? AND subject_id=? AND action=?`, kind, id, request.Action))
	if errors.Is(err, app.ErrNotFound) {
		return model.AuthorityDecision{}, false, nil
	}
	if err != nil {
		return model.AuthorityDecision{}, false, err
	}
	return model.AuthorityDecision{Action: request.Action, Resource: request.Resource, SourceKind: model.AuthorityDenied, SourceID: string(denial.ID), Revision: denial.Revision}, true, nil
}
