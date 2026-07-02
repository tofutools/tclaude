package db

import (
	"database/sql"
	"fmt"
)

// migrateV84toV85 adds `sessions.last_statusline_json` — the verbatim raw JSON
// of the most recent statusline callback the harness sent for this session,
// overwritten on every render (one column, latest-wins — NOT an append log).
// The statusbar hook already parses this stdin into StatusLineInput, but Go's
// decoder silently drops fields the struct doesn't name, so a newly-shipped
// usage bucket (e.g. Fable 5's separate limit) would be invisible. Persisting
// the raw bytes keeps unknown fields intact for direct inspection of the DB —
// the diagnostic this column exists for.
//
// Additive + idempotent (the v76–v84 convention): the table is probed so a
// partial-schema heal DB without `sessions` is a clean skip; the ADD COLUMN is
// guarded by a pragma_table_info probe so a half-applied run converges on re-run
// instead of wedging on "duplicate column". TEXT NOT NULL DEFAULT '' back-fills
// every existing row to "no snapshot yet" with no data pass. One transaction
// with the version bump; nothing is dropped or rewritten.
//
// The column is write-only by design: tclaude stores it verbatim and never reads
// it back in code (it is inspected by hand off the DB), so there is no getter.
func migrateV84toV85(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v84→v85: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "sessions")
	if err != nil {
		return fmt.Errorf("migrate v84→v85 (probe sessions): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'last_statusline_json'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v84→v85 (probe sessions.last_statusline_json): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE sessions ADD COLUMN last_statusline_json TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v84→v85 (add sessions.last_statusline_json): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 85`); err != nil {
		return fmt.Errorf("migrate v84→v85 (version): %w", err)
	}
	return tx.Commit()
}
