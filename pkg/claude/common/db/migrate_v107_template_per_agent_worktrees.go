package db

import (
	"database/sql"
	"fmt"
)

// migrateV106toV107 adds the template-level default for the existing
// per-agent-worktree deploy option. False preserves the historical behaviour:
// every template opens the deploy dialog with the option disabled unless the
// human explicitly enables it for that run.
func migrateV106toV107(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v106→v107: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTemplates, err := txTableExists(tx, "group_templates")
	if err != nil {
		return fmt.Errorf("migrate v106→v107 (probe group_templates): %w", err)
	}
	if haveTemplates {
		if err := addColumnIfMissing(tx, "group_templates", "per_agent_worktrees",
			`ALTER TABLE group_templates ADD COLUMN per_agent_worktrees INTEGER NOT NULL DEFAULT 0`); err != nil {
			return fmt.Errorf("migrate v106→v107: %w", err)
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 107`); err != nil {
		return fmt.Errorf("migrate v106→v107 (version): %w", err)
	}
	return tx.Commit()
}
