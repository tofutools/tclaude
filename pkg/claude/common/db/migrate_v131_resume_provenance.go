package db

import (
	"database/sql"
	"fmt"
)

// migrateV130toV131 adds the durable physical directory/repository identity
// used by offline managed-agent resume. Existing rows intentionally remain
// empty: a pathname alone cannot be upgraded into trustworthy provenance after
// the pane that owned its cwd has exited.
func migrateV130toV131(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v130→v131: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "sessions")
	if err != nil {
		return fmt.Errorf("migrate v130→v131 (probe sessions): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'resume_provenance'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v130→v131 (probe sessions.resume_provenance): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE sessions ADD COLUMN resume_provenance TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v130→v131 (add sessions.resume_provenance): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 131`); err != nil {
		return fmt.Errorf("migrate v130→v131 (version): %w", err)
	}
	return tx.Commit()
}
