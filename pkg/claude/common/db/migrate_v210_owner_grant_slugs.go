package db

import (
	"database/sql"
	"fmt"
)

// ownerGrantSlugRenames applies only to owner_scopes_json. The agent.* slugs
// remain valid global permissions everywhere else; only ownership moved to
// dedicated group-scoped siblings.
var ownerGrantSlugRenames = map[string]string{
	"agent.reincarnate":    "groups.members.reincarnate",
	"agent.compact":        "groups.members.compact",
	"agent.interrupt":      "groups.members.interrupt",
	"agent.rename":         "groups.members.rename",
	"agent.clone":          "groups.members.clone",
	"agent.context-info":   "groups.members.context-info",
	"agent.task":           "groups.members.task",
	"agent.pr":             "groups.members.pr",
	"agent.tags":           "groups.members.tags",
	"agent.schedule":       "groups.members.schedule",
	"agent.stop":           "groups.members.stop",
	"agent.resume":         "groups.members.resume",
	"agent.delete":         "groups.members.delete",
	"agent.promote":        "groups.members.promote",
	"agent.retire":         "groups.members.retire",
	"agent.remote-control": "groups.members.remote-control",
	"agent.inbox-watch":    "groups.members.inbox-watch",
}

// migrateV209toV210 preserves owner-grant constraints from existing
// installations after ownership moves from global agent.* permissions to
// group-scoped groups.members.* siblings. Other permission-bearing columns are
// intentionally untouched because the global agent.* vocabulary still exists.
func migrateV209toV210(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v209→v210 (owner grant slugs): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, spec := range []struct{ table, column string }{
		{"agent_groups", "owner_scopes_json"},
		{"group_templates", "owner_scopes_json"},
	} {
		if err = migratePermissionJSONColumn(tx, spec.table, spec.column, func(value string) (string, bool, error) {
			return renamePermissionMap(value, ownerGrantSlugRenames)
		}); err != nil {
			return fmt.Errorf("migrate v209→v210 (%s.%s): %w", spec.table, spec.column, err)
		}
	}
	if _, err = tx.Exec(`UPDATE schema_version SET version = 210`); err != nil {
		return fmt.Errorf("migrate v209→v210 (version): %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migrate v209→v210 (commit): %w", err)
	}
	return nil
}
