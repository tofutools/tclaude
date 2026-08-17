package db

import (
	"database/sql"
	"fmt"
)

// migrateV219toV220 adds the per-agent auto-permit opt-in set: the named
// permission-prompt conditions an operator has pre-consented to for one agent,
// so the daemon may answer them on the operator's behalf.
//
// Keyed on the stable agent_id (not a conv-id) so an opt-in survives a
// reincarnate / `/clear` conv rotation the same way tags and task refs do — the
// consent is about the actor, not about one generation of its conversation.
// ON DELETE CASCADE ties the row's lifetime to the agent it consents for.
//
// The condition name is a compile-time constant from the daemon's condition
// registry; storing it as free text keeps the schema stable when a later build
// renames or retires a condition (an unknown name is simply inert, exactly like
// an unregistered permission slug).
//
// Answers are NOT stored here. Each auto-answer is written to audit_log with
// actor_kind = system, which is the operator's existing "what happened on my
// behalf" surface; a second private store would only be a trail that the
// dashboard's audit view does not show.
func migrateV219toV220(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v219→v220 (auto-permit opt-ins): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_auto_permit (
			agent_id   TEXT NOT NULL REFERENCES agents(agent_id) ON DELETE CASCADE,
			condition  TEXT NOT NULL,
			granted_by TEXT NOT NULL DEFAULT '',
			created_at INTEGER NOT NULL,
			PRIMARY KEY (agent_id, condition)
		) STRICT;
	`); err != nil {
		return fmt.Errorf("migrate v219→v220 (auto-permit opt-ins): create: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 220`); err != nil {
		return fmt.Errorf("migrate v219→v220 (auto-permit opt-ins): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v219→v220 (auto-permit opt-ins): commit: %w", err)
	}
	return nil
}
