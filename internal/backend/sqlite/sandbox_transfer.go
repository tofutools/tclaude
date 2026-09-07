package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
)

func (s *Store) ImportSandboxProfiles(ctx context.Context, req app.ImportSandboxProfilesRequest, now time.Time) (app.SandboxImportResult, error) {
	result, intent, err := app.PrepareSandboxImport(ctx, req, now)
	if err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.SandboxImportResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var oldIntent, oldResult []byte
	err = tx.QueryRowContext(ctx, `SELECT intent,result FROM sandbox_profile_requests WHERE request_scope=? AND request_id=?`, requestScope(req.Context.Principal), req.Context.RequestID).Scan(&oldIntent, &oldResult)
	if err == nil {
		if !bytes.Equal(intent, oldIntent) {
			return app.SandboxImportResult{}, app.ErrConflict
		}
		if err = json.Unmarshal(oldResult, &result); err != nil {
			return app.SandboxImportResult{}, err
		}
		result.Repeated = true
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.SandboxImportResult{}, err
	}
	for _, entry := range result.Profiles {
		profileData, err := json.Marshal(entry.Profile)
		if err != nil {
			return app.SandboxImportResult{}, err
		}
		revisionData, err := json.Marshal(entry.Revision)
		if err != nil {
			return app.SandboxImportResult{}, err
		}
		inserted, err := tx.ExecContext(ctx, `INSERT INTO sandbox_profiles(id,name,head_revision_id,archived,revision,document) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, entry.Profile.ID, entry.Profile.Name, entry.Revision.Ref.RevisionID, false, 1, profileData)
		if err != nil {
			return app.SandboxImportResult{}, err
		}
		if err = ensureSandboxImportInsert(inserted); err != nil {
			return app.SandboxImportResult{}, err
		}
		inserted, err = tx.ExecContext(ctx, `INSERT INTO sandbox_profile_revisions(id,profile_id,number,content_hash,document) VALUES(?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, entry.Revision.Ref.RevisionID, entry.Profile.ID, 1, entry.Revision.Ref.ContentHash, revisionData)
		if err != nil {
			return app.SandboxImportResult{}, err
		}
		if err = ensureSandboxImportInsert(inserted); err != nil {
			return app.SandboxImportResult{}, err
		}
	}
	data, err := json.Marshal(result)
	if err != nil {
		return app.SandboxImportResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sandbox_profile_requests(request_scope,request_id,intent,result) VALUES(?,?,?,?)`, requestScope(req.Context.Principal), req.Context.RequestID, intent, data); err != nil {
		return app.SandboxImportResult{}, err
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.SandboxImportResult{}, err
	}
	return result, tx.Commit()
}
func ensureSandboxImportInsert(result sql.Result) error {
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return app.ErrConflict
	}
	return nil
}
