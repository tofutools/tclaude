package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

// The same transaction fences aliases against all primary names and aliases.
func requireAvailableProfileAliases(ctx context.Context, tx *sql.Tx, profile model.ConfigurationProfile) error {
	handles := map[string]bool{profile.Name: true}
	for _, alias := range profile.Aliases {
		if handles[alias] {
			return app.ErrConflict
		}
		handles[alias] = true
	}
	rows, err := tx.QueryContext(ctx, `SELECT record FROM configuration_profiles WHERE id<>?`, profile.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return err
		}
		var other model.ConfigurationProfile
		if err = json.Unmarshal(data, &other); err != nil {
			return err
		}
		// Existing duplicate primary names remain addressable by ID, but must not
		// become ambiguous aliases. Renames cannot acquire another profile's alias.
		for _, alias := range profile.Aliases {
			if alias == other.Name {
				return app.ErrConflict
			}
		}
		for _, alias := range other.Aliases {
			if handles[alias] {
				return app.ErrConflict
			}
		}
	}
	return rows.Err()
}
