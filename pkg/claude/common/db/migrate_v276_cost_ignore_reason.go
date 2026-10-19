package db

import (
	"database/sql"
	"fmt"
)

// migrateV275toV276 gives an ignored conversation a reason: "" for the
// operator's own Ignore, "billed_to:<gateway>@<peer>" for one whose model
// calls a peer's gateway paid for (usage sharing), which that peer counts.
func migrateV275toV276(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v275→v276 (cost ignore reason): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`ALTER TABLE cost_ignored_convs ADD COLUMN reason TEXT NOT NULL DEFAULT ''`,
		`UPDATE schema_version SET version = 276`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v275→v276 (cost ignore reason): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v275→v276 (commit): %w", err)
	}
	return nil
}
