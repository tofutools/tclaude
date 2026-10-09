package db

import (
	"database/sql"
	"fmt"
)

// migrateV234toV235 adds agent_startup_snapshots: the per-agent facts a
// compaction or /clear boundary needs to re-inject the startup context the
// agent was born with.
//
// Only what cannot be re-read live is stored. The group's startup context is
// deliberately absent — it is read from the group row at re-injection time so
// operator edits reach agents already running. The profile/role startup
// context is stored as it resolved at spawn, because the tier inputs (the
// named profile, the role, the defaults in force) are not persisted anywhere
// else. include_group_ctx records the spawn's include_group_context choice:
// an agent spawned without the group context must not have it injected later.
func migrateV234toV235(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate v234→v235 (agent startup snapshots): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS agent_startup_snapshots (
			agent_id          TEXT PRIMARY KEY,
			spawn_group_id    INTEGER NOT NULL DEFAULT 0,
			spawned_by_agent  TEXT NOT NULL DEFAULT '',
			include_group_ctx INTEGER NOT NULL DEFAULT 1,
			profile_context   TEXT NOT NULL DEFAULT '',
			worktree_path     TEXT NOT NULL DEFAULT '',
			worktree_branch   TEXT NOT NULL DEFAULT '',
			brief_message_id  INTEGER NOT NULL DEFAULT 0,
			created_at        INTEGER NOT NULL
		) STRICT;
	`); err != nil {
		return fmt.Errorf("migrate v234→v235 (agent startup snapshots): create: %w", err)
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 235`); err != nil {
		return fmt.Errorf("migrate v234→v235 (version): %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v234→v235 (commit): %w", err)
	}
	return nil
}
