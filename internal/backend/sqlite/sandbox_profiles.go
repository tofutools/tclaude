package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sandboxpolicy"
)

func (s *Store) SaveSandboxProfile(ctx context.Context, req app.SaveSandboxProfileRequest, revisionID model.SandboxProfileRevisionID, now time.Time) (app.SandboxProfileResult, error) {
	if err := app.ValidateSandboxProfileSave(req); err != nil {
		return app.SandboxProfileResult{}, err
	}
	if err := revisionID.Validate(); err != nil {
		return app.SandboxProfileResult{}, app.ErrInvalid
	}
	hash, err := sandboxpolicy.ContentHash(req.Policy)
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	// The generated revision identity and invocation time are not authored intent.
	// The displayed CAS revision is: mutating it under the same request must fail.
	intent, err := json.Marshal(struct {
		ID         model.SandboxProfileID
		Expected   model.Revision
		Name, Hash string
	}{req.ID, req.ExpectedRevision, req.Name, hash})
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var oldIntent, oldResult []byte
	err = tx.QueryRowContext(ctx, `SELECT intent,result FROM sandbox_profile_requests WHERE request_scope=? AND request_id=?`, requestScope(req.Context.Principal), req.Context.RequestID).Scan(&oldIntent, &oldResult)
	if err == nil {
		if !bytes.Equal(intent, oldIntent) {
			return app.SandboxProfileResult{}, app.ErrConflict
		}
		var result app.SandboxProfileResult
		if err = json.Unmarshal(oldResult, &result); err != nil {
			return result, err
		}
		result.Repeated = true
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.SandboxProfileResult{}, err
	}
	ref := model.SandboxProfileRef{ProfileID: req.ID, RevisionID: revisionID, ContentHash: hash}
	// Check the current include graph in this transaction, before publishing the
	// fresh revision. A missing include cannot leave a partial profile or receipt.
	reader := sandboxRevisionReader{query: tx, proposedRef: ref, proposed: req.Policy}
	if _, err = sandboxpolicy.ResolveCurrent(ctx, ref, reader); err != nil {
		if errors.Is(err, sandboxpolicy.ErrInvalidClosure) {
			return app.SandboxProfileResult{}, fmt.Errorf("%w: %v", app.ErrInvalid, err)
		}
		return app.SandboxProfileResult{}, err
	}
	profile := model.SandboxProfile{ID: req.ID, Name: req.Name, HeadRevisionID: revisionID, Revision: req.ExpectedRevision + 1, CreatedAt: now, UpdatedAt: now}
	if req.ExpectedRevision > 0 {
		var data []byte
		if err = tx.QueryRowContext(ctx, `SELECT document FROM sandbox_profiles WHERE id=?`, req.ID).Scan(&data); err != nil {
			return app.SandboxProfileResult{}, classify(err)
		}
		var previous model.SandboxProfile
		if err = json.Unmarshal(data, &previous); err != nil {
			return app.SandboxProfileResult{}, err
		}
		if previous.Archived || previous.Revision != req.ExpectedRevision {
			return app.SandboxProfileResult{}, app.ErrConflict
		}
		profile.CreatedAt = previous.CreatedAt
		profile.Imported = previous.Imported
	}
	revision := model.SandboxProfileRevision{Ref: ref, Number: profile.Revision, Policy: req.Policy, Author: req.Context.Principal, RequestID: req.Context.RequestID, CreatedAt: now}
	result := app.SandboxProfileResult{Profile: profile, Revision: revision}
	profileData, err := json.Marshal(profile)
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	revisionData, err := json.Marshal(revision)
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	resultData, err := json.Marshal(result)
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	if req.ExpectedRevision == 0 {
		var inserted sql.Result
		inserted, err = tx.ExecContext(ctx, `INSERT INTO sandbox_profiles(id,name,head_revision_id,archived,revision,document) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, profile.ID, profile.Name, revisionID, false, profile.Revision, profileData)
		if err == nil {
			count, countErr := inserted.RowsAffected()
			if countErr != nil {
				return app.SandboxProfileResult{}, countErr
			}
			if count == 0 {
				return app.SandboxProfileResult{}, app.ErrConflict
			}
		}
	} else {
		var update sql.Result
		update, err = tx.ExecContext(ctx, `UPDATE sandbox_profiles SET name=?,head_revision_id=?,revision=?,document=? WHERE id=? AND revision=? AND archived=0`, profile.Name, revisionID, profile.Revision, profileData, profile.ID, req.ExpectedRevision)
		if err == nil {
			var count int64
			count, err = update.RowsAffected()
			if err == nil && count != 1 {
				return app.SandboxProfileResult{}, app.ErrConflict
			}
		}
	}
	if err != nil {
		return app.SandboxProfileResult{}, classify(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sandbox_profile_revisions(id,profile_id,number,content_hash,document) VALUES(?,?,?,?,?)`, revisionID, profile.ID, revision.Number, hash, revisionData); err != nil {
		return app.SandboxProfileResult{}, classify(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sandbox_profile_requests(request_scope,request_id,intent,result) VALUES(?,?,?,?)`, requestScope(req.Context.Principal), req.Context.RequestID, intent, resultData); err != nil {
		return app.SandboxProfileResult{}, classify(err)
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.SandboxProfileResult{}, err
	}
	return result, tx.Commit()
}

type sandboxQuery interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}
type sandboxRevisionReader struct {
	query       sandboxQuery
	proposedRef model.SandboxProfileRef
	proposed    model.SandboxPolicy
}

func (r sandboxRevisionReader) ReadSandboxRevision(ctx context.Context, ref model.SandboxProfileRef) (model.SandboxPolicy, error) {
	if ref == r.proposedRef {
		return r.proposed, nil
	}
	var data []byte
	if err := r.query.QueryRowContext(ctx, `SELECT document FROM sandbox_profile_revisions WHERE id=?`, ref.RevisionID).Scan(&data); err != nil {
		return model.SandboxPolicy{}, classify(err)
	}
	var revision model.SandboxProfileRevision
	if err := json.Unmarshal(data, &revision); err != nil {
		return model.SandboxPolicy{}, err
	}
	if revision.Ref != ref {
		return model.SandboxPolicy{}, app.ErrConflict
	}
	return revision.Policy, nil
}
func (s *Store) ReadSandboxRevision(ctx context.Context, ref model.SandboxProfileRef) (model.SandboxPolicy, error) {
	return (sandboxRevisionReader{query: s.db}).ReadSandboxRevision(ctx, ref)
}
func (s *Store) SandboxProfile(ctx context.Context, id model.SandboxProfileID) (app.SandboxProfileResult, error) {
	var profileData, revisionData []byte
	err := s.db.QueryRowContext(ctx, `SELECT p.document,r.document FROM sandbox_profiles p JOIN sandbox_profile_revisions r ON r.id=p.head_revision_id WHERE p.id=?`, id).Scan(&profileData, &revisionData)
	if err != nil {
		return app.SandboxProfileResult{}, classify(err)
	}
	var result app.SandboxProfileResult
	if err = json.Unmarshal(profileData, &result.Profile); err != nil {
		return result, err
	}
	err = json.Unmarshal(revisionData, &result.Revision)
	return result, err
}
func (s *Store) ListSandboxProfiles(ctx context.Context, includeArchived bool) ([]model.SandboxProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT document FROM sandbox_profiles WHERE (? OR archived=0) ORDER BY name,id`, includeArchived)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := []model.SandboxProfile{}
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		var profile model.SandboxProfile
		if err = json.Unmarshal(data, &profile); err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	return profiles, rows.Err()
}

func (s *Store) SetSandboxProfileArchived(ctx context.Context, req app.SetSandboxProfileArchivedRequest, now time.Time) (app.SandboxProfileResult, error) {
	if err := app.ValidateSandboxArchive(req); err != nil {
		return app.SandboxProfileResult{}, err
	}
	intent, err := json.Marshal(struct {
		Action   string
		ID       model.SandboxProfileID
		Expected model.Revision
		Archived bool
	}{"archive", req.ID, req.ExpectedRevision, req.Archived})
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var oldIntent, oldResult []byte
	err = tx.QueryRowContext(ctx, `SELECT intent,result FROM sandbox_profile_requests WHERE request_scope=? AND request_id=?`, requestScope(req.Context.Principal), req.Context.RequestID).Scan(&oldIntent, &oldResult)
	if err == nil {
		if !bytes.Equal(intent, oldIntent) {
			return app.SandboxProfileResult{}, app.ErrConflict
		}
		var result app.SandboxProfileResult
		if err = json.Unmarshal(oldResult, &result); err != nil {
			return result, err
		}
		result.Repeated = true
		return result, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.SandboxProfileResult{}, err
	}
	var profileData, revisionData []byte
	err = tx.QueryRowContext(ctx, `SELECT p.document,r.document FROM sandbox_profiles p JOIN sandbox_profile_revisions r ON r.id=p.head_revision_id WHERE p.id=?`, req.ID).Scan(&profileData, &revisionData)
	if err != nil {
		return app.SandboxProfileResult{}, classify(err)
	}
	var result app.SandboxProfileResult
	if err = json.Unmarshal(profileData, &result.Profile); err != nil {
		return result, err
	}
	if err = json.Unmarshal(revisionData, &result.Revision); err != nil {
		return result, err
	}
	if result.Profile.Revision != req.ExpectedRevision {
		return app.SandboxProfileResult{}, app.ErrConflict
	}
	result.Profile.Archived = req.Archived
	result.Profile.Revision++
	result.Profile.UpdatedAt = now
	profileData, err = json.Marshal(result.Profile)
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	resultData, err := json.Marshal(result)
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	update, err := tx.ExecContext(ctx, `UPDATE sandbox_profiles SET archived=?,revision=?,document=? WHERE id=? AND revision=?`, req.Archived, result.Profile.Revision, profileData, req.ID, req.ExpectedRevision)
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	count, err := update.RowsAffected()
	if err != nil {
		return app.SandboxProfileResult{}, err
	}
	if count != 1 {
		return app.SandboxProfileResult{}, app.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO sandbox_profile_requests(request_scope,request_id,intent,result) VALUES(?,?,?,?)`, requestScope(req.Context.Principal), req.Context.RequestID, intent, resultData); err != nil {
		return app.SandboxProfileResult{}, classify(err)
	}
	if req.Archived {
		if err = clearSandboxDefaultAssignments(ctx, tx, req.ID, ""); err != nil {
			return app.SandboxProfileResult{}, err
		}
	}
	if err = bumpTx(ctx, tx); err != nil {
		return app.SandboxProfileResult{}, err
	}
	return result, tx.Commit()
}

func (r sandboxRevisionReader) CurrentSandboxRef(ctx context.Context, id model.SandboxProfileID) (model.SandboxProfileRef, error) {
	if id == r.proposedRef.ProfileID {
		return r.proposedRef, nil
	}
	var data []byte
	err := r.query.QueryRowContext(ctx, `SELECT r.document FROM sandbox_profiles p JOIN sandbox_profile_revisions r ON r.id=p.head_revision_id WHERE p.id=?`, id).Scan(&data)
	if err != nil {
		return model.SandboxProfileRef{}, classify(err)
	}
	var revision model.SandboxProfileRevision
	if err = json.Unmarshal(data, &revision); err != nil {
		return model.SandboxProfileRef{}, err
	}
	return revision.Ref, nil
}
func (s *Store) CurrentSandboxRef(ctx context.Context, id model.SandboxProfileID) (model.SandboxProfileRef, error) {
	return (sandboxRevisionReader{query: s.db}).CurrentSandboxRef(ctx, id)
}
