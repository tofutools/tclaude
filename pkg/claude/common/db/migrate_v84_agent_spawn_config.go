package db

import (
	"database/sql"
	"fmt"
)

// migrateV83toV84 adds `agents.initial_spawn_config` — a verbatim JSON snapshot
// of the spawn request the agent was born from (agent.SpawnRequest: name, role,
// model, effort, harness, sandbox, approval, remote-control, worktree, …). It is
// the durable, agent-level record of "what this agent was spawned with" that the
// schema previously lacked: model/effort live only on the per-generation session
// row (rotated + live-tracked), pending_spawns is transient, and agent_spawn_history
// is just lineage. Written once at spawn enrollment; never consumed by resume
// (that path reads live state) — purely an inspection/audit aid.
//
// Additive + idempotent (the v76–v83 convention): the table is probed so a
// partial-schema heal DB without `agents` is a clean skip; the ADD COLUMN is
// guarded by a pragma_table_info probe so a half-applied run converges on re-run
// instead of wedging on "duplicate column". NOT NULL DEFAULT '' back-fills every
// existing row to "no recorded config" with no data pass. One transaction with
// the version bump; nothing is dropped or rewritten.
func migrateV83toV84(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v83→v84: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "agents")
	if err != nil {
		return fmt.Errorf("migrate v83→v84 (probe agents): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('agents') WHERE name = 'initial_spawn_config'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v83→v84 (probe agents.initial_spawn_config): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE agents ADD COLUMN initial_spawn_config TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v83→v84 (add agents.initial_spawn_config): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 84`); err != nil {
		return fmt.Errorf("migrate v83→v84 (version): %w", err)
	}
	return tx.Commit()
}
