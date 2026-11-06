package db

import "database/sql"

// migrateV314toV315 adds the peer owner of a cron job a linked node scheduled
// here (S7b): owner_peer is that node's instance ID and owner_peer_agent the
// agent of it that scheduled the job, ” for its operator. Such a job has no
// owner_agent and is never operator_authored; it sends messages only, under
// the peer's grants, rechecked at every fire.
func migrateV314toV315(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// A sparse database with no cron subsystem (old healing tests, and
	// installations recovered from that shape) has nothing to change.
	var hasCronJobs int
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='agent_cron_jobs')`).Scan(&hasCronJobs); err != nil {
		return err
	}
	if hasCronJobs == 0 {
		if _, err := tx.Exec(`UPDATE schema_version SET version = 315`); err != nil {
			return err
		}
		return tx.Commit()
	}
	for _, stmt := range []string{
		`ALTER TABLE agent_cron_jobs ADD COLUMN owner_peer TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE agent_cron_jobs ADD COLUMN owner_peer_agent TEXT NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS idx_agent_cron_jobs_owner_peer ON agent_cron_jobs(owner_peer) WHERE owner_peer <> ''`,
		`UPDATE schema_version SET version = 315`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
