package db

import (
	"database/sql"
	"fmt"
)

// migrateV280toV281 widens session_cost_daily's primary key from
// (session_id, day) to (session_id, conv_id, day). Claude Code's
// total_cost_usd belongs to the running process and keeps climbing when the
// conversation changes under it (/clear, rename). Under the old key the
// writer relabelled the day's row to the new conversation, moving that day's
// pre-switch spend with it; under the new key the old conversation's row stops
// at its pre-switch cumulative and the new one gets its own row. Existing rows
// are unique on (session_id, day), so the copy cannot collide.
func migrateV280toV281(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v280→v281 (cost daily conv key): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	haveTable, err := txTableExists(tx, "session_cost_daily")
	if err != nil {
		return fmt.Errorf("migrate v280→v281 (probe session_cost_daily): %w", err)
	}
	if haveTable {
		for _, stmt := range []string{
			`CREATE TABLE session_cost_daily_v281 (
				session_id       TEXT NOT NULL,
				day              TEXT NOT NULL,
				conv_id          TEXT NOT NULL DEFAULT '',
				cost_usd         REAL NOT NULL DEFAULT 0,
				updated_at       INTEGER,
				virtual_cost_usd REAL NOT NULL DEFAULT 0,
				model            TEXT NOT NULL DEFAULT '',
				agent_id         TEXT NOT NULL DEFAULT '',
				harness          TEXT NOT NULL DEFAULT 'claude',
				PRIMARY KEY (session_id, conv_id, day)
			) STRICT`,
			`INSERT INTO session_cost_daily_v281
				(session_id, day, conv_id, cost_usd, updated_at, virtual_cost_usd, model, agent_id, harness)
				SELECT session_id, day, conv_id, cost_usd, updated_at, virtual_cost_usd, model, agent_id, harness
				FROM session_cost_daily`,
			`DROP TABLE session_cost_daily`,
			`ALTER TABLE session_cost_daily_v281 RENAME TO session_cost_daily`,
			`CREATE INDEX IF NOT EXISTS idx_session_cost_daily_day ON session_cost_daily(day)`,
			`CREATE INDEX IF NOT EXISTS idx_session_cost_daily_walk
				ON session_cost_daily(
					COALESCE(NULLIF(conv_id, ''), session_id), day, updated_at, session_id
				)`,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("migrate v280→v281 (cost daily conv key): %w", err)
			}
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 281`); err != nil {
		return fmt.Errorf("migrate v280→v281 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v280→v281 (commit): %w", err)
	}
	return nil
}
