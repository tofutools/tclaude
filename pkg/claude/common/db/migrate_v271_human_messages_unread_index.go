package db

import (
	"database/sql"
	"fmt"
)

// migrateV270toV271 indexes unread human messages. The dashboard snapshot
// reads every unread notification on each poll, and read_at sits after the
// (possibly long) body in the row, so without this the unread filter walked
// the whole table, bodies included. The partial index holds only unread rows.
func migrateV270toV271(d *sql.DB) error {
	// A partial test fixture may predate human_messages; nothing to index.
	have, err := tableExists(d, "human_messages")
	if err != nil {
		return fmt.Errorf("migrate v270→v271 (human messages unread index): %w", err)
	}
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v270→v271 (human messages unread index): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if have {
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_human_messages_unread
			ON human_messages(id) WHERE read_at IS NULL`); err != nil {
			return fmt.Errorf("migrate v270→v271 (human messages unread index): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 271`); err != nil {
		return fmt.Errorf("migrate v270→v271 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v270→v271 (commit): %w", err)
	}
	return nil
}
