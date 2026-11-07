package db

import (
	"database/sql"
	"fmt"
)

// drawingBoardPermissionRenames moves the collab drawing-board slugs off the
// bare "boards" name, which now reads as the group task board.
var drawingBoardPermissionRenames = map[string]string{
	"boards.read":  "drawing-boards.read",
	"boards.write": "drawing-boards.write",
}

// migrateV319toV320 renames collab boards to drawing boards in storage: the
// tables, their board_id columns, indexes and the session trigger, the
// permission slugs, and the "board" kind wherever a row stores it (resource
// locks, resource=board:<id> scopes, a message's board:<id> action). It is one
// transaction, so an upgrade never leaves a grant or a lock on the old name.
func migrateV319toV320(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v319→v320 (drawing boards): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, stmt := range []string{
		`DROP TRIGGER IF EXISTS collab_board_sessions_agent_retired`,
		`DROP INDEX IF EXISTS idx_collab_boards_group`,
		`DROP INDEX IF EXISTS idx_collab_board_elements_server_version`,
		`DROP INDEX IF EXISTS idx_collab_board_participants_agent`,
		`DROP INDEX IF EXISTS idx_collab_board_history_stack`,
		`DROP INDEX IF EXISTS idx_collab_board_sessions_agent`,
		`ALTER TABLE collab_boards RENAME TO collab_drawing_boards`,
		`ALTER TABLE collab_board_elements RENAME TO collab_drawing_board_elements`,
		`ALTER TABLE collab_board_participants RENAME TO collab_drawing_board_participants`,
		`ALTER TABLE collab_board_history RENAME TO collab_drawing_board_history`,
		`ALTER TABLE collab_board_sessions RENAME TO collab_drawing_board_sessions`,
		`ALTER TABLE collab_drawing_board_elements RENAME COLUMN board_id TO drawing_board_id`,
		`ALTER TABLE collab_drawing_board_participants RENAME COLUMN board_id TO drawing_board_id`,
		`ALTER TABLE collab_drawing_board_history RENAME COLUMN board_id TO drawing_board_id`,
		`ALTER TABLE collab_drawing_board_sessions RENAME COLUMN board_id TO drawing_board_id`,
		`CREATE INDEX IF NOT EXISTS idx_collab_drawing_boards_group ON collab_drawing_boards(group_id)`,
		`CREATE INDEX IF NOT EXISTS idx_collab_drawing_board_elements_server_version ON collab_drawing_board_elements(drawing_board_id, server_version)`,
		`CREATE INDEX IF NOT EXISTS idx_collab_drawing_board_participants_agent ON collab_drawing_board_participants(agent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_collab_drawing_board_history_stack ON collab_drawing_board_history(drawing_board_id, actor, stack, id)`,
		`CREATE INDEX IF NOT EXISTS idx_collab_drawing_board_sessions_agent ON collab_drawing_board_sessions(agent_id)`,
		`CREATE TRIGGER IF NOT EXISTS collab_drawing_board_sessions_agent_retired
 AFTER UPDATE OF retired_at ON agents WHEN NEW.retired_at IS NOT NULL BEGIN
 DELETE FROM collab_drawing_board_sessions WHERE agent_id = NEW.agent_id;
END`,
	} {
		if _, err = tx.Exec(stmt); err != nil {
			return fmt.Errorf("migrate v319→v320 (%.60s): %w", stmt, err)
		}
	}

	// The board kind, wherever a row stores it.
	for _, spec := range []struct{ table, stmt string }{
		{"group_resource_locks", `UPDATE group_resource_locks SET kind = 'drawing_board' WHERE kind = 'board'`},
		{"human_messages", `UPDATE human_messages SET action = 'drawing_board:' || substr(action, 7) WHERE action LIKE 'board:%'`},
	} {
		exists, probeErr := txTableExists(tx, spec.table)
		if probeErr != nil {
			return fmt.Errorf("migrate v319→v320 (inspect %s): %w", spec.table, probeErr)
		}
		if !exists {
			continue
		}
		if _, err = tx.Exec(spec.stmt); err != nil {
			return fmt.Errorf("migrate v319→v320 (%s): %w", spec.table, err)
		}
	}
	for _, table := range []string{"agent_permissions", "agent_group_permissions", "agent_sudo_grants",
		"steward_carried_permissions", "steward_carried_sudo"} {
		if err = renameBoardScopes(tx, table, "scope_json"); err != nil {
			return fmt.Errorf("migrate v319→v320 (%s scopes): %w", table, err)
		}
	}
	for _, spec := range []struct{ table, column string }{
		{"pending_spawns", "permission_overrides"},
		{"spawn_profiles", "permission_overrides"},
		{"agent_groups", "owner_scopes_json"},
		{"group_templates", "owner_scopes_json"},
		{"group_template_agents", "profile_inline"},
	} {
		if err = renameBoardScopes(tx, spec.table, spec.column); err != nil {
			return fmt.Errorf("migrate v319→v320 (%s.%s scopes): %w", spec.table, spec.column, err)
		}
	}

	// The slugs, on every permission-bearing surface (as v202 and v208 did).
	for _, spec := range []struct {
		table       string
		owner       string
		parentTable string
	}{
		{"agent_permissions", "agent_id", "agents"},
		{"agent_group_permissions", "group_id", "agent_groups"},
	} {
		exists, probeErr := txTableExists(tx, spec.table)
		if probeErr != nil {
			return fmt.Errorf("migrate v319→v320 (inspect %s): %w", spec.table, probeErr)
		}
		if !exists {
			continue
		}
		parentExists, probeErr := txTableExists(tx, spec.parentTable)
		if probeErr != nil {
			return fmt.Errorf("migrate v319→v320 (inspect %s): %w", spec.parentTable, probeErr)
		}
		if !parentExists {
			continue
		}
		for oldSlug, newSlug := range drawingBoardPermissionRenames {
			if _, err = tx.Exec(`DELETE FROM `+spec.table+` AS legacy
				WHERE slug = ? AND EXISTS (
					SELECT 1 FROM `+spec.table+` AS canonical
					WHERE canonical.`+spec.owner+` = legacy.`+spec.owner+` AND canonical.slug = ?
				)`, oldSlug, newSlug); err != nil {
				return fmt.Errorf("migrate v319→v320 (%s collision %s): %w", spec.table, oldSlug, err)
			}
		}
		if err = renameSlugColumn(tx, spec.table, "slug", drawingBoardPermissionRenames); err != nil {
			return fmt.Errorf("migrate v319→v320 (%s): %w", spec.table, err)
		}
	}
	for _, spec := range []struct{ table, column string }{
		{"agent_sudo_grants", "slug"},
		{"access_requests", "perm"},
		{"steward_carried_permissions", "slug"},
		{"steward_carried_sudo", "slug"},
	} {
		if err = renameSlugColumnIfPresent(tx, spec.table, spec.column, drawingBoardPermissionRenames); err != nil {
			return fmt.Errorf("migrate v319→v320 (%s.%s): %w", spec.table, spec.column, err)
		}
	}
	for _, spec := range []struct{ table, column string }{
		{"roles", "permissions"},
		{"group_template_agents", "permissions"},
	} {
		if err = migratePermissionJSONColumn(tx, spec.table, spec.column, func(value string) (string, bool, error) {
			return renamePermissionGrantList(value, drawingBoardPermissionRenames)
		}); err != nil {
			return fmt.Errorf("migrate v319→v320 (%s.%s): %w", spec.table, spec.column, err)
		}
	}
	for _, spec := range []struct{ table, column string }{
		{"pending_spawns", "permission_overrides"},
		{"spawn_profiles", "permission_overrides"},
		{"agent_groups", "owner_scopes_json"},
		{"group_templates", "owner_scopes_json"},
	} {
		if err = migratePermissionJSONColumn(tx, spec.table, spec.column, func(value string) (string, bool, error) {
			return renamePermissionMap(value, drawingBoardPermissionRenames)
		}); err != nil {
			return fmt.Errorf("migrate v319→v320 (%s.%s): %w", spec.table, spec.column, err)
		}
	}
	if err = migratePermissionJSONColumn(tx, "group_template_agents", "profile_inline", func(value string) (string, bool, error) {
		return renameInlineProfilePermissions(value, drawingBoardPermissionRenames)
	}); err != nil {
		return fmt.Errorf("migrate v319→v320 (group_template_agents.profile_inline): %w", err)
	}

	if _, err = tx.Exec(`UPDATE schema_version SET version = 320`); err != nil {
		return fmt.Errorf("migrate v319→v320 (version): %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migrate v319→v320 (commit): %w", err)
	}
	return nil
}

// renameBoardScopes rewrites resource=board:<id> scope matchers in a JSON
// column. The kind only ever appears as a quoted matcher's prefix, so a text
// replace of `"board:` is exact.
func renameBoardScopes(tx *sql.Tx, table, column string) error {
	exists, err := txTableExists(tx, table)
	if err != nil || !exists {
		return err
	}
	var present int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, table, column).Scan(&present); err != nil {
		return err
	}
	if present == 0 {
		return nil
	}
	_, err = tx.Exec(`UPDATE ` + table + ` SET ` + column + ` = replace(` + column + `, '"board:', '"drawing_board:')
		WHERE instr(` + column + `, '"board:') > 0`)
	return err
}
