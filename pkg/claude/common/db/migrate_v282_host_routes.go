package db

import "database/sql"

const hostRoutesSchema = `CREATE TABLE IF NOT EXISTS agent_host_routes (
 id TEXT PRIMARY KEY,
 publisher_agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
 publisher_conv_id TEXT NOT NULL,
 publisher_launch_generation TEXT NOT NULL,
 name TEXT NOT NULL,
 transport TEXT NOT NULL,
 target TEXT NOT NULL,
 state TEXT NOT NULL CHECK(state IN ('ready','withdrawn','publisher-lost')),
 created_at INTEGER NOT NULL,
 withdrawn_at INTEGER,
 withdraw_reason TEXT NOT NULL DEFAULT '',
 UNIQUE(publisher_agent_id, name)
) STRICT`

func migrateV281toV282(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(hostRoutesSchema); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE INDEX IF NOT EXISTS idx_agent_host_routes_publisher ON agent_host_routes(publisher_conv_id, state)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 282`); err != nil {
		return err
	}
	return tx.Commit()
}
