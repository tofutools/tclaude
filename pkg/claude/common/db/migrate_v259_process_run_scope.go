package db

import (
	"database/sql"
	"fmt"
)

// migrateV258toV259 binds every process run to a context scope: none (the
// pre-scope behaviour), a group, or an agent. The binding is fixed at start
// and never re-scoped silently; scope_state flips to 'orphaned' when the
// bound group or agent goes away, which parks the run for a human.
//
// Groups are referenced by id (names are renameable), agents by their
// stable agent_id (it survives reincarnate and compact). owner is the actor
// ref that started the run; existing rows backfill it from their run_created
// evidence row, which has always recorded the creating actor.
func migrateV258toV259(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v258→v259 (process run scope): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	columns := []struct{ name, ddl string }{
		{"scope_kind", `ALTER TABLE process_runs ADD COLUMN scope_kind TEXT NOT NULL DEFAULT 'none'
			CHECK(scope_kind IN ('none', 'group', 'agent'))`},
		{"scope_group_id", `ALTER TABLE process_runs ADD COLUMN scope_group_id INTEGER NOT NULL DEFAULT 0
			CHECK(scope_group_id >= 0)`},
		{"scope_agent_id", `ALTER TABLE process_runs ADD COLUMN scope_agent_id TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(scope_agent_id AS BLOB)) <= 128)`},
		{"owner", `ALTER TABLE process_runs ADD COLUMN owner TEXT NOT NULL DEFAULT ''
			CHECK(length(CAST(owner AS BLOB)) <= 256)`},
		{"scope_state", `ALTER TABLE process_runs ADD COLUMN scope_state TEXT NOT NULL DEFAULT 'bound'
			CHECK(scope_state IN ('bound', 'orphaned'))`},
	}
	for _, column := range columns {
		var present int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('process_runs') WHERE name = ?`, column.name).Scan(&present); err != nil {
			return fmt.Errorf("migrate v258→v259 (inspect process_runs): %w", err)
		}
		if present == 0 {
			if _, err := tx.Exec(column.ddl); err != nil {
				return fmt.Errorf("migrate v258→v259 (add %s): %w", column.name, err)
			}
		}
	}
	if _, err := tx.Exec(`
		UPDATE process_runs SET owner = COALESCE((
			SELECT substr(actor, 1, 256) FROM process_run_events
			 WHERE run_id = process_runs.id AND sequence = 1 AND kind = 'run_created'
		), '')
		WHERE owner = '';
		CREATE INDEX IF NOT EXISTS idx_process_runs_scope_group
			ON process_runs(scope_group_id, id) WHERE scope_kind = 'group';
		CREATE INDEX IF NOT EXISTS idx_process_runs_scope_agent
			ON process_runs(scope_agent_id, id) WHERE scope_kind = 'agent';
		CREATE INDEX IF NOT EXISTS idx_process_runs_scope_recent
			ON process_runs(scope_kind, scope_group_id, scope_agent_id, updated_at)
			WHERE scope_kind != 'none';
	`); err != nil {
		return fmt.Errorf("migrate v258→v259 (backfill owner, index scope): %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 259`); err != nil {
		return fmt.Errorf("migrate v258→v259 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v258→v259 (commit): %w", err)
	}
	return nil
}
