package db

import (
	"database/sql"
	"fmt"
)

// migrateV271toV272 adds conv_cost_inheritance: for a conversation minted from
// another conversation's copied history (a clone, a cross-node bundle import,
// a Codex thread fork), the instant up to which the transcript's usage belongs
// to the source. Harnesses whose cost is summed over the whole transcript
// (Codex, Gemini, OpenCode) skip usage recorded at or before it, so the copied
// history's spend is not counted a second time under the new conversation.
func migrateV271toV272(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v271→v272 (conv cost inheritance): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS conv_cost_inheritance (
		conv_id         TEXT PRIMARY KEY,
		inherited_until INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("migrate v271→v272 (conv cost inheritance): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 272`); err != nil {
		return fmt.Errorf("migrate v271→v272 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v271→v272 (commit): %w", err)
	}
	return nil
}
