package db

import (
	"database/sql"
	"fmt"
)

// migrateV105toV106 separates an in-flight nudge claim from successful
// delivery and persists retry history. Before this migration flushQueue used
// delivered_at itself as the claim, so a tmux failure (or daemon restart while
// send-keys was stuck) permanently consumed a nudge that never landed.
func migrateV105toV106(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v105→v106: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "agent_messages")
	if err != nil {
		return fmt.Errorf("migrate v105→v106 (probe agent_messages): %w", err)
	}
	if haveTable {
		for _, col := range []struct {
			name string
			sql  string
		}{
			{"nudge_claimed_at", `ALTER TABLE agent_messages ADD COLUMN nudge_claimed_at TEXT NOT NULL DEFAULT ''`},
			{"nudge_attempted_at", `ALTER TABLE agent_messages ADD COLUMN nudge_attempted_at TEXT NOT NULL DEFAULT ''`},
			{"nudge_attempts", `ALTER TABLE agent_messages ADD COLUMN nudge_attempts INTEGER NOT NULL DEFAULT 0`},
		} {
			var haveCol int
			if err := tx.QueryRow(
				`SELECT COUNT(*) FROM pragma_table_info('agent_messages') WHERE name = ?`, col.name,
			).Scan(&haveCol); err != nil {
				return fmt.Errorf("migrate v105→v106 (probe %s): %w", col.name, err)
			}
			if haveCol == 0 {
				if _, err := tx.Exec(col.sql); err != nil {
					return fmt.Errorf("migrate v105→v106 (add %s): %w", col.name, err)
				}
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 106`); err != nil {
		return fmt.Errorf("migrate v105→v106 (version): %w", err)
	}
	return tx.Commit()
}
