package db

import (
	"database/sql"
	"fmt"
)

// migrateV107toV108 adds a durable cancellation marker to the nudge delivery
// queue. Before this migration a message queued for an agent that was later
// retired (or deleted) stayed "undelivered" forever: no drain could ever reach
// its pane, so the stale-queue watchdog re-warned about it on every daemon
// start until someone hand-edited the row. The reaper's orphan sweep now
// stamps nudge_cancelled_at (+ a human-readable reason) on such rows, which
// removes them from every undelivered predicate while leaving the message
// itself readable in the recipient's inbox. Reinstating a retired agent clears
// the marker so its queued mail flows again.
func migrateV107toV108(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v107→v108: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "agent_messages")
	if err != nil {
		return fmt.Errorf("migrate v107→v108 (probe agent_messages): %w", err)
	}
	if haveTable {
		for _, col := range []struct {
			name string
			sql  string
		}{
			{"nudge_cancelled_at", `ALTER TABLE agent_messages ADD COLUMN nudge_cancelled_at TEXT NOT NULL DEFAULT ''`},
			{"nudge_cancel_reason", `ALTER TABLE agent_messages ADD COLUMN nudge_cancel_reason TEXT NOT NULL DEFAULT ''`},
		} {
			var haveCol int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('agent_messages') WHERE name = ?`, col.name,
			).Scan(&haveCol); err != nil {
				return fmt.Errorf("migrate v107→v108 (probe %s): %w", col.name, err)
			}
			if haveCol == 0 {
				if _, err := tx.Exec(col.sql); err != nil {
					return fmt.Errorf("migrate v107→v108 (add %s): %w", col.name, err)
				}
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 108`); err != nil {
		return fmt.Errorf("migrate v107→v108 (version): %w", err)
	}
	return tx.Commit()
}
