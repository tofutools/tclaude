package db

import "database/sql"

// migrateV289toV290 adds dashboard_marks and dashboard_captures: what an agent
// drew on the operator's view and how each mark ended, and the captures it
// kept. Both outlive the agent and a compaction, which is their point, so they
// carry no foreign key to it; their handlers bound how many rows are kept.
func migrateV289toV290(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS dashboard_marks (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 agent_id TEXT NOT NULL,
 agent_name TEXT NOT NULL DEFAULT '',
 kind TEXT NOT NULL,
 spec TEXT NOT NULL,
 tip TEXT NOT NULL DEFAULT '',
 state TEXT NOT NULL,
 ended_by TEXT NOT NULL DEFAULT '',
 reply TEXT NOT NULL DEFAULT '',
 replied_at INTEGER,
 created_at INTEGER NOT NULL,
 expires_at INTEGER NOT NULL,
 ended_at INTEGER
) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_dashboard_marks_state ON dashboard_marks(state, id)`,
		`CREATE TABLE IF NOT EXISTS dashboard_captures (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 agent_id TEXT NOT NULL,
 agent_name TEXT NOT NULL DEFAULT '',
 label TEXT NOT NULL DEFAULT '',
 context TEXT NOT NULL DEFAULT '',
 size INTEGER NOT NULL,
 created_at INTEGER NOT NULL
) STRICT`,
		`UPDATE schema_version SET version = 290`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
