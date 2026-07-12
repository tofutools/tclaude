package db

import (
	"database/sql"
	"fmt"
)

// migrateV113toV114 adds declarations for daemon-created, per-agent writable
// directories whose paths are injected through named environment variables.
func migrateV113toV114(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v113→v114 (sandbox profile agent directories): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var haveColumn int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sandbox_profiles') WHERE name = 'agent_directories_json'`).Scan(&haveColumn); err != nil {
		return fmt.Errorf("migrate v113→v114 (probe sandbox_profiles.agent_directories_json): %w", err)
	}
	if haveColumn == 0 {
		if _, err := tx.Exec(`ALTER TABLE sandbox_profiles ADD COLUMN agent_directories_json TEXT NOT NULL DEFAULT '[]'`); err != nil {
			return fmt.Errorf("migrate v113→v114 (add sandbox_profiles.agent_directories_json): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 114`); err != nil {
		return fmt.Errorf("migrate v113→v114 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v113→v114 (commit): %w", err)
	}
	return nil
}
