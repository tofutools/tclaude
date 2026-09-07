package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const processSnippetSchema = `
CREATE TABLE IF NOT EXISTS process_snippets(id TEXT PRIMARY KEY,name TEXT NOT NULL,selection BLOB NOT NULL,revision INTEGER NOT NULL,created_at INTEGER NOT NULL,updated_at INTEGER NOT NULL,deleted INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS process_snippet_requests(scope TEXT NOT NULL,request_id TEXT NOT NULL,intent BLOB NOT NULL,result BLOB NOT NULL,PRIMARY KEY(scope,request_id));`

func (s *Store) ListProcessSnippets(ctx context.Context) ([]model.ProcessSnippet, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,selection,revision,created_at,updated_at FROM process_snippets WHERE deleted=0 ORDER BY name,id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.ProcessSnippet{}
	for rows.Next() {
		var v model.ProcessSnippet
		var created, updated int64
		var selection []byte
		if err = rows.Scan(&v.ID, &v.Name, &selection, &v.Revision, &created, &updated); err != nil {
			return nil, err
		}
		v.Selection = selection
		v.CreatedAt = fromNanos(created)
		v.UpdatedAt = fromNanos(updated)
		out = append(out, v)
	}
	return out, rows.Err()
}
func (s *Store) WriteProcessSnippet(ctx context.Context, in app.ProcessSnippetRequest, now time.Time) (model.ProcessSnippet, error) {
	var out model.ProcessSnippet
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer func() { _ = tx.Rollback() }()
	// Receipts bind intent, caller scope and exact revision, including after deletion.
	intent, err := json.Marshal(struct {
		ID, Action, Name string
		Selection        json.RawMessage
		Revision         model.Revision
	}{in.ID, in.Action, in.Name, in.Selection, in.ExpectedRevision})
	if err != nil {
		return out, err
	}
	var prior, stored []byte
	err = tx.QueryRowContext(ctx, `SELECT intent,result FROM process_snippet_requests WHERE scope=? AND request_id=?`, requestScope(in.Context.Principal), in.Context.RequestID).Scan(&prior, &stored)
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
	if in.Action == "create" {
		var count, total int
		if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(LENGTH(selection)),0) FROM process_snippets WHERE deleted=0`).Scan(&count, &total); err != nil {
			return out, err
		}
		if count >= 1000 || total+len(in.Selection) > 8<<20 {
			return out, app.ErrConflict
		}
		out = model.ProcessSnippet{ID: in.ID, Name: in.Name, Selection: in.Selection, Revision: 1, CreatedAt: now, UpdatedAt: now}
		_, err = tx.ExecContext(ctx, `INSERT INTO process_snippets(id,name,selection,revision,created_at,updated_at) VALUES(?,?,?,?,?,?)`, out.ID, out.Name, []byte(out.Selection), out.Revision, nanos(now), nanos(now))
	} else {
		var created, updated int64
		var selection []byte
		err = tx.QueryRowContext(ctx, `SELECT id,name,selection,revision,created_at,updated_at,deleted FROM process_snippets WHERE id=?`, in.ID).Scan(&out.ID, &out.Name, &selection, &out.Revision, &created, &updated, &out.Deleted)
		if err != nil {
			return out, classify(err)
		}
		if out.Deleted || out.Revision != in.ExpectedRevision {
			return out, app.ErrConflict
		}
		out.Selection = selection
		out.CreatedAt = fromNanos(created)
		out.UpdatedAt = now
		out.Revision++
		if in.Action == "delete" {
			out.Deleted = true
		} else {
			out.Name = in.Name
		}
		_, err = tx.ExecContext(ctx, `UPDATE process_snippets SET name=?,revision=?,updated_at=?,deleted=? WHERE id=?`, out.Name, out.Revision, nanos(now), out.Deleted, out.ID)
	}
	if err != nil {
		return out, classify(err)
	}
	if app.ValidateProcessSelection(out.Selection) != nil {
		out.Selection = nil
	}
	stored, err = json.Marshal(out)
	if err != nil {
		return out, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO process_snippet_requests(scope,request_id,intent,result) VALUES(?,?,?,?)`, requestScope(in.Context.Principal), in.Context.RequestID, intent, stored); err != nil {
		return out, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return out, err
	}
	return out, tx.Commit()
}
