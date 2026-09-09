package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/tofutools/tclaude/internal/backend/app"
)

// ImportConfigurations commits the selected catalog entries and the retry receipt together.
func (s *Store) ImportConfigurations(ctx context.Context, w app.ConfigurationBundleWrite) (app.ConfigurationImportResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return app.ConfigurationImportResult{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var fingerprint string
	var data []byte
	err = tx.QueryRowContext(ctx, `SELECT fingerprint,result FROM configuration_bundle_requests WHERE request_id=?`, w.RequestID).Scan(&fingerprint, &data)
	if err == nil {
		if fingerprint != w.Fingerprint {
			return app.ConfigurationImportResult{}, app.ErrConflict
		}
		var result app.ConfigurationImportResult
		err = json.Unmarshal(data, &result)
		result.Repeated = true
		return result, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return app.ConfigurationImportResult{}, err
	}
	if err = requireImportedProfileAliases(ctx, tx, w.Profiles); err != nil {
		return app.ConfigurationImportResult{}, err
	}
	result := app.ConfigurationImportResult{Profiles: []app.ConfigurationProfileResult{}}
	for _, profile := range w.Profiles {
		if profile.Profile.Archived {
			if err = requireProfileUnselected(ctx, tx, profile.Profile.ID); err != nil {
				return app.ConfigurationImportResult{}, err
			}
		}
		saved, err := saveConfigurationProfileTx(ctx, tx, profile, true)
		if err != nil {
			return app.ConfigurationImportResult{}, err
		}
		result.Profiles = append(result.Profiles, saved)
	}
	data, err = json.Marshal(result)
	if err != nil {
		return app.ConfigurationImportResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO configuration_bundle_requests(request_id,fingerprint,result) VALUES(?,?,?)`, w.RequestID, w.Fingerprint, data); err != nil {
		return app.ConfigurationImportResult{}, classify(err)
	}
	if err = tx.Commit(); err != nil {
		return app.ConfigurationImportResult{}, err
	}
	return result, nil
}
