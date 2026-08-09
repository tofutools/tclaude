package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

var semanticProxyPermissionRenames = map[string]string{
	"git.read":     "proxy.git.read",
	"git.push":     "proxy.git.push",
	"github.read":  "proxy.github.read",
	"github.write": "proxy.github.write",
	"linear.read":  "proxy.linear.read",
	"linear.write": "proxy.linear.write",
}

// migrateV201toV202 gives every semantic-proxy permission one common namespace.
// Besides live grants, permission slugs occur in saved launch policy and
// template/role blueprints; all of those must move together or a later spawn
// would silently resurrect an obsolete slug.
func migrateV201toV202(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return fmt.Errorf("migrate v201→v202 (semantic-proxy permissions): begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// The two durable grant tables have uniqueness constraints. If a user has
	// already authored the new spelling, keep that row authoritative and remove
	// the legacy collision before renaming the remaining rows.
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
			return fmt.Errorf("migrate v201→v202 (inspect %s): %w", spec.table, probeErr)
		}
		if !exists {
			continue
		}
		// Historical half-schema heal fixtures can contain a child table whose
		// declared FK parent is absent. SQLite rejects DML against that child
		// even when it is empty, so leave the orphan alone; a real v201 database
		// always has both sides.
		parentExists, probeErr := txTableExists(tx, spec.parentTable)
		if probeErr != nil {
			return fmt.Errorf("migrate v201→v202 (inspect %s): %w", spec.parentTable, probeErr)
		}
		if !parentExists {
			continue
		}
		for oldSlug, newSlug := range semanticProxyPermissionRenames {
			if _, err = tx.Exec(`DELETE FROM `+spec.table+` AS legacy
				WHERE slug = ? AND EXISTS (
					SELECT 1 FROM `+spec.table+` AS canonical
					WHERE canonical.`+spec.owner+` = legacy.`+spec.owner+` AND canonical.slug = ?
				)`, oldSlug, newSlug); err != nil {
				return fmt.Errorf("migrate v201→v202 (%s collision %s): %w", spec.table, oldSlug, err)
			}
		}
		if err = renameSlugColumn(tx, spec.table, "slug"); err != nil {
			return fmt.Errorf("migrate v201→v202 (%s): %w", spec.table, err)
		}
	}

	for _, spec := range []struct{ table, column string }{
		{"agent_sudo_grants", "slug"},
		{"access_requests", "perm"},
	} {
		if err = renameSlugColumnIfPresent(tx, spec.table, spec.column); err != nil {
			return fmt.Errorf("migrate v201→v202 (%s.%s): %w", spec.table, spec.column, err)
		}
	}

	for _, spec := range []struct{ table, column string }{
		{"roles", "permissions"},
		{"group_template_agents", "permissions"},
	} {
		if err = migratePermissionJSONColumn(tx, spec.table, spec.column, renamePermissionGrantList); err != nil {
			return fmt.Errorf("migrate v201→v202 (%s.%s): %w", spec.table, spec.column, err)
		}
	}
	for _, spec := range []struct{ table, column string }{
		{"pending_spawns", "permission_overrides"},
		{"spawn_profiles", "permission_overrides"},
		{"agent_groups", "owner_scopes_json"},
		{"group_templates", "owner_scopes_json"},
	} {
		if err = migratePermissionJSONColumn(tx, spec.table, spec.column, renamePermissionMap); err != nil {
			return fmt.Errorf("migrate v201→v202 (%s.%s): %w", spec.table, spec.column, err)
		}
	}
	if err = migratePermissionJSONColumn(tx, "group_template_agents", "profile_inline", renameInlineProfilePermissions); err != nil {
		return fmt.Errorf("migrate v201→v202 (group_template_agents.profile_inline): %w", err)
	}

	if _, err = tx.Exec(`UPDATE schema_version SET version = 202`); err != nil {
		return fmt.Errorf("migrate v201→v202 (version): %w", err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migrate v201→v202 (commit): %w", err)
	}
	return nil
}

func renameSlugColumnIfPresent(tx *sql.Tx, table, column string) error {
	exists, err := txTableExists(tx, table)
	if err != nil || !exists {
		return err
	}
	return renameSlugColumn(tx, table, column)
}

func renameSlugColumn(tx *sql.Tx, table, column string) error {
	for oldSlug, newSlug := range semanticProxyPermissionRenames {
		if _, err := tx.Exec(`UPDATE `+table+` SET `+column+` = ? WHERE `+column+` = ?`, newSlug, oldSlug); err != nil {
			return err
		}
	}
	return nil
}

type permissionJSONTransform func(string) (string, bool, error)

func migratePermissionJSONColumn(tx *sql.Tx, table, column string, transform permissionJSONTransform) error {
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
	rows, err := tx.Query(`SELECT rowid, ` + column + ` FROM ` + table)
	if err != nil {
		return err
	}
	type update struct {
		rowID int64
		value string
	}
	var updates []update
	for rows.Next() {
		var rowID int64
		var value string
		if err := rows.Scan(&rowID, &value); err != nil {
			_ = rows.Close()
			return err
		}
		next, changed, err := transform(value)
		if err != nil {
			// These JSON columns have always degraded malformed values to an
			// empty policy at read time. Preserve that fail-closed contract: a
			// corrupt optional blueprint must not brick the whole database
			// upgrade merely because it cannot participate in this rename.
			continue
		}
		if changed {
			updates = append(updates, update{rowID: rowID, value: next})
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, u := range updates {
		if _, err := tx.Exec(`UPDATE `+table+` SET `+column+` = ? WHERE rowid = ?`, u.value, u.rowID); err != nil {
			return err
		}
	}
	return nil
}

func renamePermissionGrantList(value string) (string, bool, error) {
	if value == "" {
		return value, false, nil
	}
	var grants []PermissionGrant
	if err := json.Unmarshal([]byte(value), &grants); err != nil {
		return "", false, err
	}
	existing := make(map[string]bool, len(grants))
	for _, grant := range grants {
		existing[grant.Slug] = true
	}
	changed := false
	out := make([]PermissionGrant, 0, len(grants))
	for _, grant := range grants {
		newSlug, legacy := semanticProxyPermissionRenames[grant.Slug]
		if !legacy {
			out = append(out, grant)
			continue
		}
		changed = true
		if existing[newSlug] {
			continue
		}
		grant.Slug = newSlug
		existing[newSlug] = true
		out = append(out, grant)
	}
	if !changed {
		return value, false, nil
	}
	b, err := json.Marshal(out)
	return string(b), true, err
}

func renamePermissionMap(value string) (string, bool, error) {
	if value == "" {
		return value, false, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &entries); err != nil {
		return "", false, err
	}
	changed := renameRawPermissionMap(entries)
	if !changed {
		return value, false, nil
	}
	b, err := json.Marshal(entries)
	return string(b), true, err
}

func renameRawPermissionMap(entries map[string]json.RawMessage) bool {
	changed := false
	for oldSlug, newSlug := range semanticProxyPermissionRenames {
		value, ok := entries[oldSlug]
		if !ok {
			continue
		}
		if _, canonicalExists := entries[newSlug]; !canonicalExists {
			entries[newSlug] = value
		}
		delete(entries, oldSlug)
		changed = true
	}
	return changed
}

func renameInlineProfilePermissions(value string) (string, bool, error) {
	if value == "" {
		return value, false, nil
	}
	var profile map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value), &profile); err != nil {
		return "", false, err
	}
	raw, ok := profile["permission_overrides"]
	if !ok {
		return value, false, nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return "", false, err
	}
	if !renameRawPermissionMap(entries) {
		return value, false, nil
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return "", false, err
	}
	profile["permission_overrides"] = raw
	b, err := json.Marshal(profile)
	return string(b), true, err
}
