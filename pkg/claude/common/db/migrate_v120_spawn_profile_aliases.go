package db

import (
	"database/sql"
	"fmt"
)

// migrateV119toV120 adds alternate handles for spawn profiles. Aliases live in
// the same namespace as primary profile names and point at the stable profile
// row id, so a rename never detaches an alias-backed lookup.
func migrateV119toV120(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v119→v120 (spawn profile aliases): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS spawn_profile_aliases (
			alias TEXT PRIMARY KEY,
			profile_id INTEGER NOT NULL REFERENCES spawn_profiles(id) ON DELETE CASCADE
		);
		CREATE INDEX IF NOT EXISTS idx_spawn_profile_aliases_profile
			ON spawn_profile_aliases(profile_id);

		CREATE TRIGGER IF NOT EXISTS spawn_profile_name_not_alias_insert
		BEFORE INSERT ON spawn_profiles
		WHEN EXISTS (SELECT 1 FROM spawn_profile_aliases WHERE alias = NEW.name)
		BEGIN
			SELECT RAISE(ABORT, 'spawn profile handle already exists');
		END;

		CREATE TRIGGER IF NOT EXISTS spawn_profile_name_not_alias_update
		BEFORE UPDATE OF name ON spawn_profiles
		WHEN EXISTS (SELECT 1 FROM spawn_profile_aliases WHERE alias = NEW.name)
		BEGIN
			SELECT RAISE(ABORT, 'spawn profile handle already exists');
		END;

		CREATE TRIGGER IF NOT EXISTS spawn_profile_alias_not_name_insert
		BEFORE INSERT ON spawn_profile_aliases
		WHEN EXISTS (SELECT 1 FROM spawn_profiles WHERE name = NEW.alias)
		BEGIN
			SELECT RAISE(ABORT, 'spawn profile handle already exists');
		END;

		CREATE TRIGGER IF NOT EXISTS spawn_profile_alias_not_name_update
		BEFORE UPDATE OF alias ON spawn_profile_aliases
		WHEN EXISTS (SELECT 1 FROM spawn_profiles WHERE name = NEW.alias)
		BEGIN
			SELECT RAISE(ABORT, 'spawn profile handle already exists');
		END;

		UPDATE schema_version SET version = 120;
	`); err != nil {
		return fmt.Errorf("migrate v119→v120 (spawn profile aliases): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v119→v120 (spawn profile aliases): commit: %w", err)
	}
	return nil
}
