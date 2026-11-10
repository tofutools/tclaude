package db

import (
	"database/sql"
	"fmt"
)

// A human notification can put a decision to the operator (tcl-5hnxii):
// options is a JSON array of 2–6 short choices, recommended the 1-based
// option the agent suggests (0 none), and decision / decided_at record the
// operator's pick so the card shows as answered everywhere.
func migrateV285toV286(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	haveHumanMessages, err := txTableExists(tx, "human_messages")
	if err != nil {
		return fmt.Errorf("migrate v285→v286 (probe human_messages): %w", err)
	}
	if haveHumanMessages {
		for _, col := range []struct{ name, def string }{
			{"options", `TEXT NOT NULL DEFAULT ''`},
			{"recommended", `INTEGER NOT NULL DEFAULT 0`},
			{"decision", `TEXT NOT NULL DEFAULT ''`},
			{"decided_at", `INTEGER`},
		} {
			var have int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('human_messages') WHERE name = ?`, col.name).Scan(&have); err != nil {
				return fmt.Errorf("migrate v285→v286 (probe human_messages.%s): %w", col.name, err)
			}
			if have == 0 {
				if _, err := tx.Exec(`ALTER TABLE human_messages ADD COLUMN ` + col.name + ` ` + col.def); err != nil {
					return fmt.Errorf("migrate v285→v286 (add human_messages.%s): %w", col.name, err)
				}
			}
		}
	}
	if haveHumanMessages {
		// Every snapshot asks for the open decisions, usually none.
		if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_human_messages_undecided
			ON human_messages(id) WHERE options != '' AND decision = ''`); err != nil {
			return fmt.Errorf("migrate v285→v286 (index): %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 286`); err != nil {
		return fmt.Errorf("migrate v285→v286 (version): %w", err)
	}
	return tx.Commit()
}
