package db

import (
	"database/sql"
	"fmt"
)

// migrateV104toV105 adds the durable process command id carried by a spawned
// agent. The unique non-empty index is the restart idempotency boundary: one
// process command can own at most one agent even when agentd restarts between
// spawn and command-dispatched persistence.
func migrateV104toV105(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v104→v105: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"agents", "pending_spawns"} {
		haveTable, probeErr := txTableExists(tx, table)
		if probeErr != nil {
			return fmt.Errorf("migrate v104→v105 (probe %s): %w", table, probeErr)
		}
		if !haveTable {
			continue
		}
		var haveColumn int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('` + table + `') WHERE name = 'process_command_id'`).Scan(&haveColumn); err != nil {
			return fmt.Errorf("migrate v104→v105 (probe %s column): %w", table, err)
		}
		if haveColumn == 0 {
			if _, err := tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN process_command_id TEXT NOT NULL DEFAULT ''`); err != nil {
				return fmt.Errorf("migrate v104→v105 (add %s column): %w", table, err)
			}
		}
		indexName := "idx_" + table + "_process_command"
		if _, err := tx.Exec(`CREATE UNIQUE INDEX IF NOT EXISTS ` + indexName + ` ON ` + table + `(process_command_id) WHERE process_command_id <> ''`); err != nil {
			return fmt.Errorf("migrate v104→v105 (%s command index): %w", table, err)
		}
	}
	haveHumanMessages, err := txTableExists(tx, "human_messages")
	if err != nil {
		return fmt.Errorf("migrate v104→v105 (probe human_messages): %w", err)
	}
	if haveHumanMessages {
		for _, column := range []string{"process_run_id", "process_node_id", "process_command_id"} {
			var haveColumn int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('human_messages') WHERE name = ?`, column).Scan(&haveColumn); err != nil {
				return fmt.Errorf("migrate v104→v105 (probe human_messages.%s): %w", column, err)
			}
			if haveColumn == 0 {
				if _, err := tx.Exec(`ALTER TABLE human_messages ADD COLUMN ` + column + ` TEXT NOT NULL DEFAULT ''`); err != nil {
					return fmt.Errorf("migrate v104→v105 (add human_messages.%s): %w", column, err)
				}
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 105`); err != nil {
		return fmt.Errorf("migrate v104→v105 (version): %w", err)
	}
	return tx.Commit()
}
