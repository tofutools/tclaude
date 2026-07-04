package db

import (
	"database/sql"
	"fmt"
)

// migrateV95toV96 adds the per-agent tag set: a new `agent_tags` table
// (agent_id, tag) whose rows label an actor with a small set of short
// strings — free-form operator tags plus the auto-stamped `tf:<template>`
// marker that records which task-force / template deployment spawned the
// agent (JOH-380). Keyed by the stable agent_id (not a group-membership
// row), so a tag follows the actor across groups, reincarnations and
// /clear rotations — the same rationale the per-agent task-ref link
// (v95) is stored per-agent.
//
// The table carries an ON DELETE CASCADE foreign key to agents(agent_id):
// foreign_keys is enforced on every connection (see db.go's DSN), so the
// actor-scoped `DELETE FROM agents` in DeleteAgentByConvID drops an
// actor's tags automatically — no explicit teardown step, the same way
// agent_conversations cascades. A tag row can therefore never outlive its
// agent.
//
// Idempotent: CREATE TABLE / CREATE INDEX IF NOT EXISTS converge on
// re-run after a partial apply. One transaction with the version bump;
// nothing existing is dropped or rewritten.
func migrateV95toV96(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v95→v96 (agent_tags): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_tags (
			agent_id TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			tag      TEXT NOT NULL,
			PRIMARY KEY (agent_id, tag)
		);
		CREATE INDEX IF NOT EXISTS idx_agent_tags_tag ON agent_tags(tag);
	`); err != nil {
		return fmt.Errorf("migrate v95→v96 (create agent_tags): %w", err)
	}

	if _, err := tx.Exec(`UPDATE schema_version SET version = 96`); err != nil {
		return fmt.Errorf("migrate v95→v96 (version): %w", err)
	}
	return tx.Commit()
}
