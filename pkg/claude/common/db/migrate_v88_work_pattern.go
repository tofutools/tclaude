package db

import (
	"database/sql"
	"fmt"
)

// migrateV87toV88 adds `group_templates.work_pattern` — the template's
// default work pattern (JOH-336): a JSON array of ordered, routed
// briefing messages ({send_to, value}) delivered after a template's
// whole roster has spawned. send_to names a roster agent or "all";
// value may carry a {{task}} placeholder replaced with the
// per-instantiation task at delivery. '' (the back-fill) and "[]" both
// read back as "no pattern", so existing templates keep today's
// spawn-only behaviour.
//
// HISTORY: this migration was born as v86→v87 on the task-forces
// integration branch, in parallel with main's v86→v87 (which adds
// sessions.subagents_json), and was renumbered to v88 when the branches
// merged. A DB stamped 87 by a task-forces build therefore has
// work_pattern but NOT subagents_json — and will never re-run v86→v87 —
// so this migration also re-runs the subagents_json guard. Both adds
// are probe-guarded, so every arrival order converges: a main-stamped
// v87 DB gains work_pattern here (its subagents guard no-ops), a
// task-forces-stamped v87 DB gains subagents_json here (its
// work_pattern guard no-ops), and a fresh DB gets both from schema.sql.
//
// Additive + idempotent (the v76–v86 convention): tables are probed so
// a partial-schema heal DB is a clean skip; the ADD COLUMNs are guarded
// by pragma_table_info probes so a half-applied run converges on re-run
// instead of wedging on "duplicate column". TEXT NOT NULL DEFAULT ''
// back-fills every existing row with no data pass. One transaction with
// the version bump; nothing is dropped or rewritten.
func migrateV87toV88(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v87→v88: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	haveTable, err := txTableExists(tx, "group_templates")
	if err != nil {
		return fmt.Errorf("migrate v87→v88 (probe group_templates): %w", err)
	}
	if haveTable {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('group_templates') WHERE name = 'work_pattern'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v87→v88 (probe group_templates.work_pattern): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE group_templates ADD COLUMN work_pattern TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v87→v88 (add group_templates.work_pattern): %w", err)
			}
		}
	}

	// Self-heal for the parallel-v87 history above: a task-forces-stamped
	// v87 DB skipped main's v86→v87, so ensure sessions.subagents_json
	// here too (same probes as migrateV86toV87 — a no-op everywhere else).
	haveSessions, err := txTableExists(tx, "sessions")
	if err != nil {
		return fmt.Errorf("migrate v87→v88 (probe sessions): %w", err)
	}
	if haveSessions {
		var have int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'subagents_json'`,
		).Scan(&have); err != nil {
			return fmt.Errorf("migrate v87→v88 (probe sessions.subagents_json): %w", err)
		}
		if have == 0 {
			if _, err := tx.Exec(
				`ALTER TABLE sessions ADD COLUMN subagents_json TEXT NOT NULL DEFAULT ''`,
			); err != nil {
				return fmt.Errorf("migrate v87→v88 (add sessions.subagents_json): %w", err)
			}
		}
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 88`); err != nil {
		return fmt.Errorf("migrate v87→v88 (version): %w", err)
	}
	return tx.Commit()
}
