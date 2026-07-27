package db

import (
	"database/sql"
	"fmt"
)

// migrateV163toV164 gives every tclaude-layer OpenCode actor an explicit
// durable state-allocation mode. Existing actors retain the shared XDG state
// they previously used; only actors allocated after this migration receive a
// private root.
func migrateV163toV164(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v163→v164 (OpenCode agent state): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS opencode_agent_state_allocations (
			agent_id  TEXT PRIMARY KEY,
			mode      TEXT NOT NULL CHECK (mode IN ('private', 'legacy-shared')),
			state_root TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			CHECK (
				(mode = 'private' AND state_root <> '') OR
				(mode = 'legacy-shared' AND state_root = '')
			)
		);
	`); err != nil {
		return fmt.Errorf("migrate v163→v164 (OpenCode agent state): create: %w", err)
	}

	var sessionColumns, conversationColumns, resumeProfileColumns int
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('sessions')
		WHERE name IN ('conv_id', 'harness', 'sandbox_implementation')
	`).Scan(&sessionColumns); err != nil {
		return fmt.Errorf("migrate v163→v164 (OpenCode agent state): probe sessions: %w", err)
	}
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('agent_conversations')
		WHERE name IN ('conv_id', 'agent_id')
	`).Scan(&conversationColumns); err != nil {
		return fmt.Errorf(
			"migrate v163→v164 (OpenCode agent state): probe conversations: %w", err)
	}
	if err := tx.QueryRow(`
		SELECT COUNT(*) FROM pragma_table_info('conversation_resume_profiles')
		WHERE name IN ('conv_id', 'profile_json')
	`).Scan(&resumeProfileColumns); err != nil {
		return fmt.Errorf(
			"migrate v163→v164 (OpenCode agent state): probe resume profiles: %w", err)
	}
	// Migration-heal fixtures may intentionally carry only the columns needed
	// by the much older step they exercise. Such a DB has no OpenCode actor
	// surface to grandfather, so create the new table and skip the data pass.
	if sessionColumns == 3 && conversationColumns == 2 {
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO opencode_agent_state_allocations
			(agent_id, mode, state_root, created_at)
			SELECT DISTINCT ac.agent_id, 'legacy-shared', '',
				strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			FROM sessions s
			JOIN agent_conversations ac ON ac.conv_id = s.conv_id
			WHERE s.harness = 'opencode'
			  AND s.sandbox_implementation = 'tclaude-layer'
			  AND ac.agent_id <> '';
		`); err != nil {
			return fmt.Errorf("migrate v163→v164 (OpenCode agent state): backfill: %w", err)
		}
	}
	// Durable resume profiles outlive session rows. Include their fallback
	// launch posture so stopped or retired actors retain the shared state they
	// used before this migration instead of being mistaken for new actors.
	if conversationColumns == 2 && resumeProfileColumns == 2 {
		if _, err := tx.Exec(`
			INSERT OR IGNORE INTO opencode_agent_state_allocations
			(agent_id, mode, state_root, created_at)
			SELECT DISTINCT ac.agent_id, 'legacy-shared', '',
				strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			FROM conversation_resume_profiles crp
			JOIN agent_conversations ac ON ac.conv_id = crp.conv_id
			WHERE json_valid(crp.profile_json) = 1
			  AND json_extract(crp.profile_json, '$.harness') = 'opencode'
			  AND json_extract(
				crp.profile_json,
				'$.fallback_relaunch.sandbox_implementation'
			  ) = 'tclaude-layer'
			  AND ac.agent_id <> '';
		`); err != nil {
			return fmt.Errorf(
				"migrate v163→v164 (OpenCode agent state): backfill resume profiles: %w", err)
		}
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 164`); err != nil {
		return fmt.Errorf("migrate v163→v164 (OpenCode agent state): version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate v163→v164 (OpenCode agent state): commit: %w", err)
	}
	return nil
}
