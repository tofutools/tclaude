package db

import (
	"database/sql"
	"fmt"
)

// migrateV102toV103 adds agent_prs: an explicit, agent-authored list of PRs
// the dashboard should surface alongside the best-effort branch/statusline PR
// discovery. Rows are keyed by (agent_id, PR URL) so two collaborating agents
// can present the same PR independently, while a retry of
// `tclaude agent present-pr` by the same agent updates the same row instead of
// creating noise.
func migrateV102toV103(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v102→v103: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_prs (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			agent_id    TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			pr_url      TEXT NOT NULL,
			summary     TEXT NOT NULL DEFAULT '',
			state       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL,
			UNIQUE(agent_id, pr_url)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_prs_agent ON agent_prs(agent_id);
		CREATE INDEX IF NOT EXISTS idx_agent_prs_state_updated ON agent_prs(state, updated_at);
	`); err != nil {
		return fmt.Errorf("migrate v102→v103 (create agent_prs): %w", err)
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 103`); err != nil {
		return fmt.Errorf("migrate v102→v103 (version): %w", err)
	}
	return tx.Commit()
}
