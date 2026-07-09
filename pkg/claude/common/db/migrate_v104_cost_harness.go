package db

import (
	"database/sql"
	"fmt"
)

// migrateV103toV104 adds session_cost_daily.harness — the coding harness
// ("claude", "codex", …) denormalised onto each cost-history row. Costs history
// intentionally outlives sessions rows, so the dashboard cannot rely on a live
// sessions lookup when filtering historical spend by harness.
func migrateV103toV104(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v103→v104: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "session_cost_daily")
	if err != nil {
		return fmt.Errorf("migrate v103→v104 (probe session_cost_daily): %w", err)
	}
	if haveTable {
		var haveCol int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('session_cost_daily') WHERE name = 'harness'`,
		).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v103→v104 (probe column): %w", err)
		}
		if haveCol == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE session_cost_daily ADD COLUMN harness TEXT NOT NULL DEFAULT 'claude'`,
			); err != nil {
				return fmt.Errorf("migrate v103→v104 (add column): %w", err)
			}
		}

		haveSessions, err := txTableExists(tx, "sessions")
		if err != nil {
			return fmt.Errorf("migrate v103→v104 (probe sessions): %w", err)
		}
		if haveSessions {
			if _, err := tx.Exec(`
				UPDATE session_cost_daily SET harness = (
					SELECT COALESCE(NULLIF(s.harness, ''), 'claude')
					FROM sessions s WHERE s.id = session_cost_daily.session_id
				)
				WHERE session_id IN (SELECT id FROM sessions WHERE COALESCE(NULLIF(harness, ''), 'claude') <> '')
			`); err != nil {
				return fmt.Errorf("migrate v103→v104 (backfill from sessions): %w", err)
			}
		}

		haveConvIndex, err := txTableExists(tx, "conv_index")
		if err != nil {
			return fmt.Errorf("migrate v103→v104 (probe conv_index): %w", err)
		}
		if haveConvIndex {
			convBackfillSQL := `
				UPDATE session_cost_daily SET harness = (
					SELECT COALESCE(NULLIF(c.harness, ''), 'claude')
					FROM conv_index c WHERE c.conv_id = session_cost_daily.conv_id
				)
				WHERE conv_id IN (SELECT conv_id FROM conv_index WHERE COALESCE(NULLIF(harness, ''), 'claude') <> '')
			`
			if haveSessions {
				convBackfillSQL = `
					UPDATE session_cost_daily SET harness = (
						SELECT COALESCE(NULLIF(c.harness, ''), 'claude')
						FROM conv_index c WHERE c.conv_id = session_cost_daily.conv_id
					)
					WHERE session_id NOT IN (SELECT id FROM sessions)
					  AND conv_id IN (SELECT conv_id FROM conv_index WHERE COALESCE(NULLIF(harness, ''), 'claude') <> '')
				`
			}
			if _, err := tx.Exec(convBackfillSQL); err != nil {
				return fmt.Errorf("migrate v103→v104 (backfill from conv_index): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 104`); err != nil {
		return fmt.Errorf("migrate v103→v104 (version): %w", err)
	}
	return tx.Commit()
}
