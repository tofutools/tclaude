package db

import "database/sql"

// migrateV288toV289 adds collab_board_sessions: which agents are working on a
// board, whether or not a dashboard has it open. A row is keyed by the stable
// agent id, so it outlives a reincarnation, and it has no expiry. The trigger
// ends an agent's sessions on every path that retires it, and the foreign keys
// end them with the board or the agent row.
func migrateV288toV289(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS collab_board_sessions (
 board_id TEXT NOT NULL REFERENCES collab_boards(id) ON DELETE CASCADE,
 agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
 started_at INTEGER NOT NULL,
 last_active_at INTEGER NOT NULL,
 PRIMARY KEY(board_id, agent_id)
) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_collab_board_sessions_agent ON collab_board_sessions(agent_id)`,
		`CREATE TRIGGER IF NOT EXISTS collab_board_sessions_agent_retired
 AFTER UPDATE OF retired_at ON agents WHEN NEW.retired_at IS NOT NULL BEGIN
 DELETE FROM collab_board_sessions WHERE agent_id = NEW.agent_id;
END`,
		`UPDATE schema_version SET version = 289`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	return tx.Commit()
}
