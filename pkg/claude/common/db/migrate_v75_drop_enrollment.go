package db

import (
	"database/sql"
	"fmt"
)

// migrateV74toV75 drops agent_enrollment — the conv-keyed "this conv is an
// agent" table that the stable actor layer (agents + agent_conversations,
// JOH-26) fully superseded. By v75 every production reader has moved to the
// agents table (PR3c): the identity/roster role is the agents table's, the
// tri-state probe is db.AgentState, retire state is agents.retired_at, the
// spawn-time display fallback is agents.pending_name, and the
// succession-derived generations are agent_conversations rows. backfillAgents
// (run at v72) already seeded the actor layer from agent_enrollment, so dropping
// the table loses nothing.
//
// Ordering: this is the LAST link in the chain, so every earlier migration that
// references agent_enrollment (it is created at v30, read by the v30/v72
// backfills) still sees the table when IT runs. A fresh DB walks the whole chain
// — create at v30, drop here — which is correct.
//
// Guarded + idempotent: DROP … IF EXISTS converges whether or not the table is
// present, so a re-run (or a partial-schema heal) is a clean no-op. Nothing
// FK-references agent_enrollment, so the drop cascades nothing. A VACUUM INTO
// snapshot is written first — the same convenience insurance the v73/v74
// destructive migrations write (the operator's own backups remain the ultimate
// safety net).
func migrateV74toV75(db *sql.DB) error {
	vacuumBackup(db, ".pre-v75.bak")

	if _, err := db.Exec(`DROP INDEX IF EXISTS idx_agent_enrollment_active`); err != nil {
		return fmt.Errorf("migrate v74→v75 (drop index): %w", err)
	}
	if _, err := db.Exec(`DROP TABLE IF EXISTS agent_enrollment`); err != nil {
		return fmt.Errorf("migrate v74→v75 (drop table): %w", err)
	}
	if _, err := db.Exec(`UPDATE schema_version SET version = 75`); err != nil {
		return fmt.Errorf("migrate v74→v75 (version): %w", err)
	}
	return nil
}
