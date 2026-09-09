package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// Check the mutable sources that supplied an inherited member configuration in
// the same transaction as group membership publication. Completed request
// receipts are handled before this check and retain their original result.
func requireProfileResolutionCurrent(ctx context.Context, tx *sql.Tx, resolved app.ResolvedProfileConfiguration) error {
	if resolved.DefaultsRevision == nil {
		return app.ErrConflict
	}
	var data []byte
	var defaults model.ConfigurationDefaults
	err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_defaults WHERE id=1`).Scan(&data)
	if err == nil {
		if err := json.Unmarshal(data, &defaults); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if defaults.Revision != *resolved.DefaultsRevision {
		return app.ErrConflict
	}
	if resolved.GlobalProfile == nil {
		return nil
	}
	ref := resolved.GlobalProfile
	if defaults.Global == nil || defaults.Global.ProfileID != ref.ProfileID {
		return app.ErrConflict
	}
	if err := requireActiveConfigurationProfileTx(ctx, tx, ref); err != nil {
		return err
	}
	if err := requireEnabledConfigurationProfileTx(ctx, tx, ref.ProfileID); err != nil {
		return err
	}
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
	return nil
}
