package db

import (
	"database/sql"
	"fmt"
)

// migrateV308toV309 adds a dismissed state to decisions put to the operator
// (human_messages with options): dismissed_at, dismissed_by ("operator" or
// "agent", who withdrew it) and dismiss_reason. A dismissed decision leaves
// the "to decide" queue but stays in Messages, marked dismissed.
//
// Additive and idempotent: each column is probed. dismissed_at is NULL on
// every existing row, so nothing already in the queue changes.
func migrateV308toV309(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v308→v309: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	haveTable, err := txTableExists(tx, "human_messages")
	if err != nil {
		return fmt.Errorf("migrate v308→v309 (probe human_messages): %w", err)
	}
	if haveTable {
		for _, col := range []struct{ name, ddl string }{
			{"dismissed_at", `ALTER TABLE human_messages ADD COLUMN dismissed_at INTEGER`},
			{"dismissed_by", `ALTER TABLE human_messages ADD COLUMN dismissed_by TEXT NOT NULL DEFAULT ''`},
			{"dismiss_reason", `ALTER TABLE human_messages ADD COLUMN dismiss_reason TEXT NOT NULL DEFAULT ''
				CHECK(length(CAST(dismiss_reason AS BLOB)) <= 2048)`},
		} {
			var have int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('human_messages') WHERE name = ?`, col.name).Scan(&have); err != nil {
				return fmt.Errorf("migrate v308→v309 (probe %s): %w", col.name, err)
			}
			if have == 0 {
				if _, err := tx.Exec(col.ddl); err != nil {
					return fmt.Errorf("migrate v308→v309 (add %s): %w", col.name, err)
				}
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 309`); err != nil {
		return fmt.Errorf("migrate v308→v309 (version): %w", err)
	}
	return tx.Commit()
}
