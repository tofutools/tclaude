package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const presentationSchema = `CREATE TABLE IF NOT EXISTS presentation_preferences(id INTEGER PRIMARY KEY CHECK(id=1),value_json BLOB NOT NULL,revision INTEGER NOT NULL);`

func (s *Store) ReadPresentation(ctx context.Context) (model.PresentationPreferences, error) {
	var data []byte
	var p model.PresentationPreferences
	err := s.db.QueryRowContext(ctx, `SELECT value_json,revision FROM presentation_preferences WHERE id=1`).Scan(&data, &p.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return model.DefaultPresentation(), nil
	}
	if err != nil {
		return p, err
	}
	revision := p.Revision
	err = json.Unmarshal(data, &p)
	p.Revision = revision
	return p, err
}
func (s *Store) PutPresentation(ctx context.Context, p model.PresentationPreferences, expected model.Revision) (model.PresentationPreferences, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return p, err
	}
	defer func() { _ = tx.Rollback() }()
	p.Revision = expected + 1
	data, err := json.Marshal(p)
	if err != nil {
		return p, err
	}
	var result sql.Result
	if expected == 0 {
		result, err = tx.ExecContext(ctx, `INSERT INTO presentation_preferences(id,value_json,revision) VALUES(1,?,1) ON CONFLICT(id) DO NOTHING`, data)
	} else {
		result, err = tx.ExecContext(ctx, `UPDATE presentation_preferences SET value_json=?,revision=? WHERE id=1 AND revision=?`, data, p.Revision, expected)
	}
	if err != nil {
		return p, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return p, err
	}
	if n != 1 {
		return p, app.ErrConflict
	}
	if err = bumpTx(ctx, tx); err != nil {
		return p, err
	}
	return p, tx.Commit()
}
