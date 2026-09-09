package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// The current restriction is checked inside publication, after exact receipts
// have been handled. A concurrent profile edit cannot authorize a fresh member.
func requireProfileCreationTx(ctx context.Context, tx *sql.Tx, principal model.Principal, profileID model.ConfigurationProfileID) error {
	if app.OperatorProfileCaller(principal) {
		return nil
	}
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_profiles WHERE id=?`, profileID).Scan(&raw); err != nil {
		return classify(err)
	}
	var profile model.ConfigurationProfile
	if err := json.Unmarshal(raw, &profile); err != nil {
		return err
	}
	if err := app.ConfigurationProfileCreationAllowed(profile, principal); err != nil {
		return err
	}
	var defaults model.ConfigurationDefaults
	err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_defaults WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &defaults); err != nil {
		return err
	}
	if defaults.Global == nil || defaults.Global.ProfileID == profileID {
		return nil
	}
	if err := tx.QueryRowContext(ctx, `SELECT record FROM configuration_profiles WHERE id=?`, defaults.Global.ProfileID).Scan(&raw); err != nil {
		return classify(err)
	}
	if err := json.Unmarshal(raw, &profile); err != nil {
		return err
	}
	return app.ConfigurationProfileCreationAllowed(profile, principal)
}
