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
CREATE TABLE IF NOT EXISTS configuration_bundle_requests(request_id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, result BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS configuration_profile_lifecycle_requests(request_id TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, result BLOB NOT NULL);
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
	if prior, found, err := configurationProfileReceipt(ctx, tx, w.RequestID, w.RequestFingerprint); found || err != nil {
		return prior, err
	}
	result, err := saveConfigurationProfileTx(ctx, tx, w, false)
	if err != nil {
		return app.ConfigurationProfileResult{}, err
	}
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

func saveConfigurationProfileTx(ctx context.Context, tx *sql.Tx, w app.ConfigurationProfileWrite, allowArchived bool) (app.ConfigurationProfileResult, error) {
	var payload []byte
	var current model.ConfigurationProfile
	err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_profiles WHERE id=?`, w.Profile.ID).Scan(&payload)
	if err == nil {
		if err = json.Unmarshal(payload, &current); err != nil {
			return app.ConfigurationProfileResult{}, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return app.ConfigurationProfileResult{}, err
	}
	if (!allowArchived && current.Archived) || current.Revision != w.ExpectedRevision {
		return app.ConfigurationProfileResult{}, app.ErrConflict
	}
	if current.Revision != 0 && !allowArchived {
		w.Profile.Disabled, w.Profile.DisabledReason = current.Disabled, current.DisabledReason
	}
	if !w.AliasesSet {
		w.Profile.Aliases = current.Aliases
	}
	// Imports validate all projected handles together before calling this writer.
	if !allowArchived {
		if err := requireAvailableProfileAliases(ctx, tx, w.Profile); err != nil {
			return app.ConfigurationProfileResult{}, err
		}
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
	if w.Defaults.Global != nil {
		if err := requireCurrentConfigurationDefaultTx(ctx, tx, w.Defaults.Global, ""); err != nil {
			return old, err
		}
	}
	for harness, ref := range w.Defaults.Harnesses {
		if err := requireCurrentConfigurationDefaultTx(ctx, tx, &ref, harness); err != nil {
			return old, err
		}
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

func requireActiveConfigurationProfileTx(ctx context.Context, tx *sql.Tx, ref *model.ConfigurationProfileRef) error {
	if ref == nil {
		return nil
	}
	var data []byte
	if err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_profiles WHERE id=?`, ref.ProfileID).Scan(&data); err != nil {
		return classify(err)
	}
	var profile model.ConfigurationProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return err
	}
	if profile.Archived {
		return app.ErrConflict
	}
	if err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_profile_revisions WHERE profile_id=? AND revision_id=?`, ref.ProfileID, ref.RevisionID).Scan(&data); err != nil {
		return classify(err)
	}
	var revision model.ConfigurationProfileRevision
	if err := json.Unmarshal(data, &revision); err != nil {
		return err
	}
	if revision.Ref != *ref {
		return app.ErrConflict
	}
	return nil
}
func (s *Store) SetConfigurationProfileArchived(ctx context.Context, w app.ConfigurationProfileArchiveWrite) (model.ConfigurationProfile, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ConfigurationProfile{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var profile model.ConfigurationProfile
	var fingerprint string
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT fingerprint,result FROM configuration_profile_lifecycle_requests WHERE request_id=?`, w.RequestID).Scan(&fingerprint, &data)
	if err == nil {
		if fingerprint != w.Fingerprint {
			return profile, app.ErrConflict
		}
		err = json.Unmarshal(data, &profile)
		return profile, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return profile, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT record FROM configuration_profiles WHERE id=?`, w.ID).Scan(&data); err != nil {
		return profile, classify(err)
	}
	if err = json.Unmarshal(data, &profile); err != nil {
		return profile, err
	}
	if profile.Revision != w.ExpectedRevision {
		return profile, app.ErrConflict
	}
	if w.Archived {
		if err = requireProfileUnselected(ctx, tx, w.ID); err != nil {
			return profile, err
		}
	}
	profile.Archived = w.Archived
	profile.Revision++
	profile.UpdatedAt = w.At
	data, err = json.Marshal(profile)
	if err != nil {
		return profile, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE configuration_profiles SET record=? WHERE id=?`, data, w.ID); err != nil {
		return profile, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO configuration_profile_lifecycle_requests(request_id,fingerprint,result) VALUES(?,?,?)`, w.RequestID, w.Fingerprint, data); err != nil {
		return profile, classify(err)
	}
	if err = tx.Commit(); err != nil {
		return profile, err
	}
	return profile, nil
}

func requireProfileUnselected(ctx context.Context, tx *sql.Tx, id model.ConfigurationProfileID) error {
	var data []byte
	var selected int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_configurations WHERE profile_id=?`, id).Scan(&selected); err != nil {
		return err
	}
	if selected > 0 {
		return app.ErrConflict
	}
	err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_defaults WHERE id=1`).Scan(&data)
	if err == nil {
		var defaults model.ConfigurationDefaults
		if err = json.Unmarshal(data, &defaults); err != nil {
			return err
		}
		if defaults.Global != nil && defaults.Global.ProfileID == id {
			return app.ErrConflict
		}
		for _, ref := range defaults.Harnesses {
			if ref.ProfileID == id {
				return app.ErrConflict
			}
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func (s *Store) FindConfigurationProfileWrite(ctx context.Context, id model.RequestID, fingerprint string) (app.ConfigurationProfileResult, bool, error) {
	return configurationProfileReceipt(ctx, s.db, id, fingerprint)
}

func configurationProfileReceipt(ctx context.Context, q queryer, id model.RequestID, expected string) (app.ConfigurationProfileResult, bool, error) {
	var fingerprint string
	var payload []byte
	err := q.QueryRowContext(ctx, `SELECT fingerprint,result FROM configuration_profile_requests WHERE request_id=?`, id).Scan(&fingerprint, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return app.ConfigurationProfileResult{}, false, nil
	}
	if err != nil {
		return app.ConfigurationProfileResult{}, false, err
	}
	if fingerprint != expected {
		return app.ConfigurationProfileResult{}, true, app.ErrConflict
	}
	var prior app.ConfigurationProfileResult
	err = json.Unmarshal(payload, &prior)
	return prior, true, err
}

func (s *Store) FindConfigurationDefaultsWrite(ctx context.Context, id model.RequestID, expected string) (model.ConfigurationDefaults, bool, error) {
	var result model.ConfigurationDefaults
	var fingerprint string
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT fingerprint,result FROM configuration_defaults_requests WHERE request_id=?`, id).Scan(&fingerprint, &data)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if fingerprint != expected {
		return result, true, app.ErrConflict
	}
	err = json.Unmarshal(data, &result)
	return result, true, err
}

func requireCurrentConfigurationDefaultTx(ctx context.Context, tx *sql.Tx, ref *model.ConfigurationProfileRef, harness string) error {
	if err := requireActiveConfigurationProfileTx(ctx, tx, ref); err != nil {
		return err
	}
	if ref == nil {
		return nil
	}
	var data []byte
	if err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_profiles WHERE id=?`, ref.ProfileID).Scan(&data); err != nil {
		return classify(err)
	}
	var profile model.ConfigurationProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return err
	}
	if profile.CurrentRevisionID != ref.RevisionID {
		return app.ErrConflict
	}
	if harness != "" {
		if err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_profile_revisions WHERE profile_id=? AND revision_id=?`, ref.ProfileID, ref.RevisionID).Scan(&data); err != nil {
			return classify(err)
		}
		var revision model.ConfigurationProfileRevision
		if err := json.Unmarshal(data, &revision); err != nil {
			return err
		}
		if authored := revision.AuthoredHarness(); authored != "" && authored != harness {
			return app.ErrConflict
		}
	}
	return nil
}
