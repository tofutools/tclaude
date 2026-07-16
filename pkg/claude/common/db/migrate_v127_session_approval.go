package db

import (
	"database/sql"
	"fmt"
)

// migrateV126toV127 records the launch-time approval posture on sessions so
// agent-initiated spawns can enforce approval lineage just as sandbox lineage
// already uses sessions.sandbox_mode. Empty approval_policy is the legacy or
// direct-session sentinel; v128 reconstructs only the rows whose durable
// provenance makes their historical effective policy deterministic.
func migrateV126toV127(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v126→v127 (session approval posture): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var haveTable int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'sessions'`).Scan(&haveTable); err != nil {
		return fmt.Errorf("migrate v126→v127 (session approval posture): probe table: %w", err)
	}
	if haveTable > 0 {
		for _, col := range []struct {
			name string
			sql  string
		}{
			{"approval_policy", `ALTER TABLE sessions ADD COLUMN approval_policy TEXT NOT NULL DEFAULT ''`},
			{"approval_auto_review", `ALTER TABLE sessions ADD COLUMN approval_auto_review INTEGER NOT NULL DEFAULT 0`},
		} {
			var haveColumn int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = ?`, col.name).Scan(&haveColumn); err != nil {
				return fmt.Errorf("migrate v126→v127 (session approval posture): probe %s: %w", col.name, err)
			}
			if haveColumn == 0 {
				if _, err := tx.Exec(col.sql); err != nil {
					return fmt.Errorf("migrate v126→v127 (session approval posture): add %s: %w", col.name, err)
				}
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 127`); err != nil {
		return fmt.Errorf("migrate v126→v127 (session approval posture): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v126→v127 (session approval posture): commit: %w", err)
	}
	return nil
}
