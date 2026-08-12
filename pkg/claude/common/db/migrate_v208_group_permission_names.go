package db

import (
	"database/sql"
	"fmt"
)

var groupPermissionRenames = map[string]string{
	"groups.rm":                  "groups.delete",
	"groups.stop":                "groups.members.stop",
	"groups.resume":              "groups.members.resume",
	"groups.retire":              "groups.members.retire",
	"groups.spawn":               "groups.members.spawn",
	"groups.own":                 "groups.owners.manage",
	"member.add":                 "groups.members.add",
	"member.remove":              "groups.members.remove",
	"member.redesignate":         "groups.members.update",
	"groups.description":         "groups.settings.description",
	"groups.default-dir":         "groups.settings.default-dir",
	"groups.default-context":     "groups.settings.default-context",
	"groups.default-spawn-group": "groups.settings.default-spawn-target",
	"groups.default-profile":     "groups.settings.default-profile",
	"groups.max-members":         "groups.settings.max-members",
	"groups.notifications":       "groups.settings.notifications",
	"groups.remote-control":      "groups.settings.remote-control-policy",
	"groups.permissions":         "groups.settings.member-permissions",
	"groups.owner-scopes":        "groups.settings.owner-scopes",
	"groups.link.rm":             "groups.link.remove",
}

// CanonicalPermissionSlug returns the current spelling for a permission slug
// carried by an external legacy artifact. Database and config migrations handle
// installed state; group archives can be imported long after those migrations
// ran, so their contents need the same compatibility map at import time.
func CanonicalPermissionSlug(slug string) string {
	if canonical, ok := semanticProxyPermissionRenames[slug]; ok {
		return canonical
	}
	if canonical, ok := groupPermissionRenames[slug]; ok {
		return canonical
	}
	return slug
}

// migrateV207toV208 moves the group-permission vocabulary to namespaces that
// distinguish group objects, their members, owners, and settings. Every
// durable permission-bearing surface moves in one transaction so an upgrade
// cannot leave standing authority split across legacy and canonical names.
func migrateV207toV208(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v207→v208 (group permission names): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

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
			return fmt.Errorf("migrate v207→v208 (inspect %s): %w", spec.table, probeErr)
		}
		if !exists {
			continue
		}
		parentExists, probeErr := txTableExists(tx, spec.parentTable)
		if probeErr != nil {
			return fmt.Errorf("migrate v207→v208 (inspect %s): %w", spec.parentTable, probeErr)
		}
		if !parentExists {
			continue
		}
		for oldSlug, newSlug := range groupPermissionRenames {
			if _, err = tx.Exec(`DELETE FROM `+spec.table+` AS legacy
				WHERE slug = ? AND EXISTS (
					SELECT 1 FROM `+spec.table+` AS canonical
					WHERE canonical.`+spec.owner+` = legacy.`+spec.owner+` AND canonical.slug = ?
				)`, oldSlug, newSlug); err != nil {
				return fmt.Errorf("migrate v207→v208 (%s collision %s): %w", spec.table, oldSlug, err)
			}
		}
		if err = renameSlugColumn(tx, spec.table, "slug", groupPermissionRenames); err != nil {
			return fmt.Errorf("migrate v207→v208 (%s): %w", spec.table, err)
		}
	}

	for _, spec := range []struct{ table, column string }{
		{"agent_sudo_grants", "slug"},
		{"access_requests", "perm"},
	} {
		if err = renameSlugColumnIfPresent(tx, spec.table, spec.column, groupPermissionRenames); err != nil {
			return fmt.Errorf("migrate v207→v208 (%s.%s): %w", spec.table, spec.column, err)
		}
	}

	for _, spec := range []struct{ table, column string }{
		{"roles", "permissions"},
		{"group_template_agents", "permissions"},
	} {
		if err = migratePermissionJSONColumn(tx, spec.table, spec.column, func(value string) (string, bool, error) {
			return renamePermissionGrantList(value, groupPermissionRenames)
		}); err != nil {
			return fmt.Errorf("migrate v207→v208 (%s.%s): %w", spec.table, spec.column, err)
		}
	}
	for _, spec := range []struct{ table, column string }{
		{"pending_spawns", "permission_overrides"},
		{"spawn_profiles", "permission_overrides"},
		{"agent_groups", "owner_scopes_json"},
		{"group_templates", "owner_scopes_json"},
	} {
		if err = migratePermissionJSONColumn(tx, spec.table, spec.column, func(value string) (string, bool, error) {
			return renamePermissionMap(value, groupPermissionRenames)
		}); err != nil {
			return fmt.Errorf("migrate v207→v208 (%s.%s): %w", spec.table, spec.column, err)
		}
	}
	if err = migratePermissionJSONColumn(tx, "group_template_agents", "profile_inline", func(value string) (string, bool, error) {
		return renameInlineProfilePermissions(value, groupPermissionRenames)
	}); err != nil {
		return fmt.Errorf("migrate v207→v208 (group_template_agents.profile_inline): %w", err)
	}

	if _, err = tx.Exec(`UPDATE schema_version SET version = 208`); err != nil {
		return fmt.Errorf("migrate v207→v208 (version): %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migrate v207→v208 (commit): %w", err)
	}
	return nil
}
