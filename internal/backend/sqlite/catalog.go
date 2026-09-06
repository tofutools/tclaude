package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

const configurationCatalogSchema = `
CREATE TABLE IF NOT EXISTS configuration_defaults(id INTEGER PRIMARY KEY CHECK(id=1), record BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS configuration_defaults_requests(request_id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, result BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS configuration_profiles(id TEXT PRIMARY KEY, record BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS configuration_profile_revisions(profile_id TEXT NOT NULL, revision_id TEXT NOT NULL, record BLOB NOT NULL, PRIMARY KEY(profile_id,revision_id));
CREATE TABLE IF NOT EXISTS configuration_profile_requests(request_id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, result BLOB NOT NULL);
`

func (s *Store) SaveConfigurationProfile(ctx context.Context, w app.ConfigurationProfileWrite) (app.ConfigurationProfileResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.ConfigurationProfileResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var fingerprint string
	var payload []byte
	err = tx.QueryRowContext(ctx, `SELECT fingerprint,result FROM configuration_profile_requests WHERE request_id=?`, w.RequestID).Scan(&fingerprint, &payload)
	if err == nil {
		if fingerprint != w.RequestFingerprint {
			return app.ConfigurationProfileResult{}, app.ErrConflict
		}
		var prior app.ConfigurationProfileResult
		err = json.Unmarshal(payload, &prior)
		return prior, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.ConfigurationProfileResult{}, err
	}
	var current model.ConfigurationProfile
	err = tx.QueryRowContext(ctx, `SELECT record FROM configuration_profiles WHERE id=?`, w.Profile.ID).Scan(&payload)
	if err == nil {
		if err = json.Unmarshal(payload, &current); err != nil {
			return app.ConfigurationProfileResult{}, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return app.ConfigurationProfileResult{}, err
	}
	if current.Revision != w.ExpectedRevision {
		return app.ConfigurationProfileResult{}, app.ErrConflict
	}
	w.Profile.Revision = current.Revision + 1
	w.Profile.CreatedAt = current.CreatedAt
	w.Profile.UpdatedAt = w.At
	if current.Revision == 0 {
		w.Profile.CreatedAt = w.At
	}
	revisionJSON, err := json.Marshal(w.Revision)
	if err != nil {
		return app.ConfigurationProfileResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO configuration_profile_revisions(profile_id,revision_id,record) VALUES(?,?,?)`, w.Profile.ID, w.Revision.Ref.RevisionID, revisionJSON); err != nil {
		return app.ConfigurationProfileResult{}, classify(err)
	}
	profileJSON, err := json.Marshal(w.Profile)
	if err != nil {
		return app.ConfigurationProfileResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO configuration_profiles(id,record) VALUES(?,?) ON CONFLICT(id) DO UPDATE SET record=excluded.record`, w.Profile.ID, profileJSON); err != nil {
		return app.ConfigurationProfileResult{}, err
	}
	result := app.ConfigurationProfileResult{Profile: w.Profile, Revision: w.Revision}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return app.ConfigurationProfileResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO configuration_profile_requests(request_id,fingerprint,result) VALUES(?,?,?)`, w.RequestID, w.RequestFingerprint, resultJSON); err != nil {
		return app.ConfigurationProfileResult{}, classify(err)
	}
	if err = tx.Commit(); err != nil {
		return app.ConfigurationProfileResult{}, err
	}
	return result, nil
}

func (s *Store) ConfigurationProfile(ctx context.Context, id model.ConfigurationProfileID, revision model.ConfigurationProfileRevisionID) (app.ConfigurationProfileResult, error) {
	// One transaction keeps the current pointer and its selected immutable revision coherent.
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return app.ConfigurationProfileResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var result app.ConfigurationProfileResult
	var data []byte
	if err = tx.QueryRowContext(ctx, `SELECT record FROM configuration_profiles WHERE id=?`, id).Scan(&data); err != nil {
		return result, classify(err)
	}
	if err = json.Unmarshal(data, &result.Profile); err != nil {
		return result, err
	}
	if revision == "" {
		revision = result.Profile.CurrentRevisionID
	}
	if err = tx.QueryRowContext(ctx, `SELECT record FROM configuration_profile_revisions WHERE profile_id=? AND revision_id=?`, id, revision).Scan(&data); err != nil {
		return result, classify(err)
	}
	if err = json.Unmarshal(data, &result.Revision); err != nil {
		return result, err
	}
	return result, tx.Commit()
}
func (s *Store) ConfigurationProfiles(ctx context.Context) ([]model.ConfigurationProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT record FROM configuration_profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []model.ConfigurationProfile{}
	for rows.Next() {
		var data []byte
		var profile model.ConfigurationProfile
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(data, &profile); err != nil {
			return nil, err
		}
		result = append(result, profile)
	}
	return result, rows.Err()
}

func configurationProfileJSON(ref *model.ConfigurationProfileRef) []byte {
	if ref == nil {
		return nil
	}
	data, _ := json.Marshal(ref)
	return data
}

func (s *Store) ConfigurationDefaults(ctx context.Context) (model.ConfigurationDefaults, error) {
	var out model.ConfigurationDefaults
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT record FROM configuration_defaults WHERE id=1`).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(data, &out)
	return out, err
}
func (s *Store) SaveConfigurationDefaults(ctx context.Context, w app.ConfigurationDefaultsWrite) (model.ConfigurationDefaults, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ConfigurationDefaults{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var old model.ConfigurationDefaults
	var data []byte
	var fingerprint string
	err = tx.QueryRowContext(ctx, `SELECT fingerprint,result FROM configuration_defaults_requests WHERE request_id=?`, w.RequestID).Scan(&fingerprint, &data)
	if err == nil {
		if fingerprint != w.RequestFingerprint {
			return old, app.ErrConflict
		}
		err = json.Unmarshal(data, &old)
		return old, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return old, err
	}
	err = tx.QueryRowContext(ctx, `SELECT record FROM configuration_defaults WHERE id=1`).Scan(&data)
	if err == nil {
		if err = json.Unmarshal(data, &old); err != nil {
			return old, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return old, err
	}
	if old.Revision != w.ExpectedRevision {
		return old, app.ErrConflict
	}
	w.Defaults.Revision = old.Revision + 1
	data, err = json.Marshal(w.Defaults)
	if err != nil {
		return old, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO configuration_defaults(id,record) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET record=excluded.record`, data); err != nil {
		return old, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO configuration_defaults_requests(request_id,fingerprint,result) VALUES(?,?,?)`, w.RequestID, w.RequestFingerprint, data); err != nil {
		return old, classify(err)
	}
	if err = tx.Commit(); err != nil {
		return old, err
	}
	return w.Defaults, nil
}
