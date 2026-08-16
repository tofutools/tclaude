package db

import (
	"database/sql"
	"fmt"
)

// migrateV213toV214 adds scheduled managed workers. Message jobs retain their
// historical defaults; only rows explicitly marked action_kind=spawn use the
// new payload. trigger_workers becomes the shared managed-worker ledger by
// accepting either trigger-rule/firing provenance or cron-job/run provenance.
func migrateV213toV214(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v213→v214 (cron spawn): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// Some old healing tests (and installations recovered from their same
	// pre-cron partial shape) legitimately have no cron subsystem at all. Keep
	// advancing that sparse database; there is no cron state to transform.
	var hasCronJobs int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='agent_cron_jobs')`).Scan(&hasCronJobs); err != nil {
		return fmt.Errorf("migrate v213→v214 (cron spawn): inspect cron schema: %w", err)
	}
	if hasCronJobs == 0 {
		if _, err = tx.Exec(`UPDATE schema_version SET version=214`); err != nil {
			return fmt.Errorf("migrate v213→v214 (cron spawn): sparse version: %w", err)
		}
		if err = tx.Commit(); err != nil {
			return fmt.Errorf("migrate v213→v214 (cron spawn): sparse commit: %w", err)
		}
		return nil
	}
	for _, stmt := range []string{
		`ALTER TABLE agent_cron_jobs ADD COLUMN action_kind TEXT NOT NULL DEFAULT 'message' CHECK (action_kind IN ('message','spawn'))`,
		`ALTER TABLE agent_cron_jobs ADD COLUMN spawn_profile TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_cron_jobs ADD COLUMN spawn_role_refs_json TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE agent_cron_jobs ADD COLUMN spawn_name_template TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_cron_jobs ADD COLUMN spawn_instruction_template TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_cron_jobs ADD COLUMN spawn_concurrency_policy TEXT NOT NULL DEFAULT 'Forbid' CHECK (spawn_concurrency_policy IN ('Forbid','Replace','Allow'))`,
		`ALTER TABLE agent_cron_jobs ADD COLUMN spawn_max_live_workers INTEGER NOT NULL DEFAULT 1 CHECK (spawn_max_live_workers > 0)`,
		`ALTER TABLE agent_cron_jobs ADD COLUMN spawn_worker_deadline_seconds INTEGER NOT NULL DEFAULT 0 CHECK (spawn_worker_deadline_seconds >= 0)`,
		`ALTER TABLE agent_cron_runs ADD COLUMN worker_id INTEGER`,
		`ALTER TABLE agent_cron_runs ADD COLUMN worker_agent TEXT NOT NULL DEFAULT ''`,
	} {
		if _, err = tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v213→v214 (cron spawn): %w", err)
		}
	}
	if _, err = tx.Exec(`
		ALTER TABLE trigger_workers RENAME TO trigger_workers_v213;
		CREATE TABLE trigger_workers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			rule_id INTEGER REFERENCES trigger_rules(id) ON DELETE SET NULL,
			firing_id INTEGER REFERENCES trigger_firings(id) ON DELETE SET NULL,
			cron_job_id INTEGER REFERENCES agent_cron_jobs(id) ON DELETE SET NULL,
			cron_run_id INTEGER REFERENCES agent_cron_runs(id) ON DELETE SET NULL,
			action_index INTEGER NOT NULL,
			agent_id TEXT NOT NULL UNIQUE,
			conv_id TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'reserved' CHECK (state IN ('reserved','pending','live','failed','exited','deadline_exceeded','replaced','interrupted')),
			pending_label TEXT NOT NULL DEFAULT '',
			deadline_at INTEGER,
			created_at INTEGER NOT NULL,
			completed_at INTEGER,
			detail TEXT NOT NULL DEFAULT '',
			CHECK (rule_id IS NULL OR cron_job_id IS NULL)
		) STRICT;
		INSERT INTO trigger_workers
			(id,rule_id,firing_id,action_index,agent_id,conv_id,state,pending_label,deadline_at,created_at,completed_at,detail)
		SELECT id,rule_id,firing_id,action_index,agent_id,conv_id,state,pending_label,deadline_at,created_at,completed_at,detail
		FROM trigger_workers_v213;
		DROP TABLE trigger_workers_v213;
		CREATE INDEX idx_trigger_workers_rule_live ON trigger_workers(rule_id,state);
		CREATE INDEX idx_trigger_workers_cron_live ON trigger_workers(cron_job_id,state);
	`); err != nil {
		return fmt.Errorf("migrate v213→v214 (cron spawn): rebuild workers: %w", err)
	}
	if _, err = tx.Exec(`UPDATE schema_version SET version=214`); err != nil {
		return fmt.Errorf("migrate v213→v214 (cron spawn): version: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migrate v213→v214 (cron spawn): commit: %w", err)
	}
	return nil
}
