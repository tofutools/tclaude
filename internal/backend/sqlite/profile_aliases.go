package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func configurationAliasNamespace(ctx context.Context, tx *sql.Tx) (map[model.ConfigurationProfileID]model.ConfigurationProfile, error) {
	rows, err := tx.QueryContext(ctx, `SELECT record FROM configuration_profiles`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := map[model.ConfigurationProfileID]model.ConfigurationProfile{}
	for rows.Next() {
		var data []byte
		if err = rows.Scan(&data); err != nil {
			return nil, err
		}
		var profile model.ConfigurationProfile
		if err = json.Unmarshal(data, &profile); err != nil {
			return nil, err
		}
		profiles[profile.ID] = profile
	}
	return profiles, rows.Err()
}

func checkConfigurationAliases(profile model.ConfigurationProfile, namespace map[model.ConfigurationProfileID]model.ConfigurationProfile) error {
	handles := map[string]bool{profile.Name: true}
	for _, alias := range profile.Aliases {
		if handles[alias] {
			return app.ErrConflict
		}
		handles[alias] = true
	}
	for id, other := range namespace {
		if id == profile.ID {
			continue
		}
		// Existing duplicate primary names remain addressable by ID. Aliases cannot
		// acquire a primary name or alias, and a rename cannot acquire another alias.
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
	return nil
}

func requireAvailableProfileAliases(ctx context.Context, tx *sql.Tx, profile model.ConfigurationProfile) error {
	namespace, err := configurationAliasNamespace(ctx, tx)
	if err != nil {
		return err
	}
	return checkConfigurationAliases(profile, namespace)
}

// Validate the complete result before any write, so acquiring an alias from a
// selected profile is independent of whether its releasing entry comes first.
func requireImportedProfileAliases(ctx context.Context, tx *sql.Tx, writes []app.ConfigurationProfileWrite) error {
	namespace, err := configurationAliasNamespace(ctx, tx)
	if err != nil {
		return err
	}
	for _, write := range writes {
		profile := write.Profile
		if !write.AliasesSet {
			profile.Aliases = namespace[profile.ID].Aliases
		}
		namespace[profile.ID] = profile
	}
	for _, write := range writes {
		if err = checkConfigurationAliases(namespace[write.Profile.ID], namespace); err != nil {
			return err
		}
	}
	return nil
}
