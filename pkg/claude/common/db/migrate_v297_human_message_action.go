package db

import (
	"database/sql"
	"fmt"
)

// A human notification can carry one action link (tcl-99t8tr): a button that
// names an action from a fixed list, never a path or a command. action holds
// that name ("terminal", "board:<id>", "pr:<url>", "tab:<name>"), and the
// daemon works out the target from it when the operator clicks.
func migrateV296toV297(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Some partial test schemas never had the table.
	haveHumanMessages, err := txTableExists(tx, "human_messages")
	if err != nil {
		return fmt.Errorf("migrate v296→v297 (probe human_messages): %w", err)
	}
	if haveHumanMessages {
		if _, err := tx.Exec(`ALTER TABLE human_messages ADD COLUMN action TEXT NOT NULL DEFAULT ''`); err != nil {
			return fmt.Errorf("migrate v296→v297 (add human_messages.action): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 297`); err != nil {
		return err
	}
	return tx.Commit()
}
