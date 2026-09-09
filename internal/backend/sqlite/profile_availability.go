package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func requireEnabledConfigurationProfileTx(ctx context.Context, q queryer, id model.ConfigurationProfileID) error {
	var data []byte
	if err := q.QueryRowContext(ctx, `SELECT record FROM configuration_profiles WHERE id=?`, id).Scan(&data); err != nil {
		return classify(err)
	}
	var profile model.ConfigurationProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return err
	}
	return app.ConfigurationProfileEnabled(profile)
}
func (s *Store) SetConfigurationProfileAvailability(ctx context.Context, w app.ConfigurationProfileAvailabilityWrite) (model.ConfigurationProfile, error) {
	var profile model.ConfigurationProfile
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return profile, err
	}
	defer func() { _ = tx.Rollback() }()
	var data []byte
	var fingerprint string
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
	profile.Disabled = w.Disabled
	if w.Reason != nil {
		profile.DisabledReason = *w.Reason
	}
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
		return profile, err
	}
	if err = tx.Commit(); err != nil {
		return profile, err
	}
	return profile, nil
}
