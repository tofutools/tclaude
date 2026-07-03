package db

import (
	"database/sql"
	"fmt"
)

// migrateV91toV92 stands up the advisory process runtime (JOH-242): a
// declarative, ordered process spec on the template plus per-group advisory
// phase state at runtime. It is EXPLICITLY advisory — no gates, no
// phase-scoped permissions, no rule engine; the runtime only records and
// surfaces the phase a live group is in.
//
// Three additive schema changes:
//
//   - group_templates.process — the template's process spec (JSON: an ORDERED
//     list of {name, roles, criteria} phases; '' / empty = no process). Rides
//     the same TEXT-JSON convention as work_pattern (v88). The group snapshots
//     this at instantiate so the runtime is self-contained (template edits /
//     deletes don't disturb a live group's process).
//   - group_process_state — one row per group that has a process: the
//     snapshotted phase list, the current phase name, and when it was entered.
//     group_id is the PK (a group has at most one process state). No process =
//     no row (absence = feature off, degrade everywhere).
//   - group_process_transitions — the append-only phase-change log (from, to,
//     at, actor). Indexed on group_id for the per-group history read. Ordered
//     by id, never by `at` (the RFC3339Nano lexical-sort hazard).
//
// Both state tables are keyed to the live group by group_id and are cleaned up
// explicitly inside DeleteAgentGroup's transaction (the deploy-meta cleanup
// path — deploy meta lives on the agent_groups row and dies with it; this
// state lives in sibling tables and needs an explicit sweep).
//
// Additive + idempotent (the v76–v91 convention): CREATE TABLE IF NOT EXISTS
// is a clean re-run, and the process ADD COLUMN is guarded by a sqlite_master
// table probe AND a pragma_table_info column probe so a half-applied run
// converges instead of wedging on "duplicate column". One transaction with the
// version bump; nothing is dropped or rewritten.
func migrateV91toV92(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v91→v92: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// process column on group_templates — the blueprint's process spec. Guarded
	// on the table existing (a minimally-seeded migration-heal DB may not have
	// created it) AND the column being absent (converge on re-run).
	var haveTemplates int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'group_templates'`,
	).Scan(&haveTemplates); err != nil {
		return fmt.Errorf("migrate v91→v92 (probe group_templates): %w", err)
	}
	if haveTemplates > 0 {
		var haveCol int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('group_templates') WHERE name = 'process'`,
		).Scan(&haveCol); err != nil {
			return fmt.Errorf("migrate v91→v92 (probe group_templates.process): %w", err)
		}
		if haveCol == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE group_templates ADD COLUMN process TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v91→v92 (add group_templates.process): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS group_process_state (
			group_id         INTEGER PRIMARY KEY,
			process          TEXT NOT NULL DEFAULT '[]',
			current_phase    TEXT NOT NULL DEFAULT '',
			phase_started_at TEXT NOT NULL
		);

		CREATE TABLE IF NOT EXISTS group_process_transitions (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id   INTEGER NOT NULL,
			from_phase TEXT NOT NULL DEFAULT '',
			to_phase   TEXT NOT NULL,
			at         TEXT NOT NULL,
			actor      TEXT NOT NULL DEFAULT ''
		);
		CREATE INDEX IF NOT EXISTS idx_group_process_transitions_group
			ON group_process_transitions(group_id);
	`); err != nil {
		return fmt.Errorf("migrate v91→v92 (create process state): %w", err)
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 92`); err != nil {
		return fmt.Errorf("migrate v91→v92 (version): %w", err)
	}
	return tx.Commit()
}
