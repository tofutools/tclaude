package db

import (
	"database/sql"
	"fmt"
)

func migrateV204toV205(d *sql.DB) error {
	_, err := d.Exec(`
		CREATE TABLE IF NOT EXISTS codex_native_permission_profiles (
			generation   TEXT PRIMARY KEY,
			profile_name TEXT NOT NULL UNIQUE,
			profile_toml TEXT NOT NULL,
			created_at   TEXT NOT NULL
		) STRICT;
		UPDATE schema_version SET version = 205;
	`)
	if err != nil {
		return fmt.Errorf("migrate v204→v205 (Codex native permission profiles): %w", err)
	}
	return nil
}
