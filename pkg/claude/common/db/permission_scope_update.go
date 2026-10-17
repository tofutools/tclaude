package db

import (
	"database/sql"
	"errors"
	"time"
)

// PermissionRow is the stored state of one (holder, slug) permission row, as
// a ScopeUpdate sees it: Exists is false when there is no row.
type PermissionRow struct {
	Exists bool
	Effect string // PermEffectGrant or PermEffectDeny; groups only grant
	Scope  string
}

// ScopeUpdate decides a row's new scope from its current state. It returns
// the new canonical scope JSON, keep to leave the row as it is, or remove to
// delete it; an error aborts the whole update with nothing written.
type ScopeUpdate func(slug string, cur PermissionRow) (scope string, keep, remove bool, err error)

// ErrHolderRetired: the agent or group is retired or archived, so it can no
// longer hold grants.
var ErrHolderRetired = errors.New("retired or archived")

// UpdateAgentPermissionScopes reads and rewrites agentID's grant rows for
// slugs in one transaction, so a merge into a row cannot lose a concurrent
// write and a refusal on one slug leaves the others unwritten. A row written
// is always a grant.
func UpdateAgentPermissionScopes(agentID string, slugs []string, grantedBy string, update ScopeUpdate) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var retired dbTimestamp
	switch err := tx.QueryRow(`SELECT retired_at FROM agents WHERE agent_id = ?`, agentID).Scan(&retired); {
	case errors.Is(err, sql.ErrNoRows):
		return sql.ErrNoRows
	case err != nil:
		return err
	case retired.valid:
		return ErrHolderRetired
	}
	for _, slug := range slugs {
		var cur PermissionRow
		switch err := tx.QueryRow(`SELECT effect, scope_json FROM agent_permissions WHERE agent_id = ? AND slug = ?`, agentID, slug).Scan(&cur.Effect, &cur.Scope); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			cur.Exists = true
		}
		scope, keep, remove, err := update(slug, cur)
		switch {
		case err != nil:
			return err
		case keep:
		case remove:
			if _, err := tx.Exec(`DELETE FROM agent_permissions WHERE agent_id = ? AND slug = ?`, agentID, slug); err != nil {
				return err
			}
		default:
			if _, err := tx.Exec(`INSERT INTO agent_permissions
				(agent_id, slug, effect, granted_at, granted_by, scope_json) VALUES (?, ?, ?, ?, ?, ?)
				ON CONFLICT(agent_id, slug) DO UPDATE SET
					effect = excluded.effect, granted_at = excluded.granted_at,
					granted_by = excluded.granted_by, scope_json = excluded.scope_json`,
				agentID, slug, PermEffectGrant, dbTime(time.Now()), grantedBy, scope); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// UpdateAgentGroupPermissionScopes is the group twin of
// UpdateAgentPermissionScopes: it rewrites some of a group's grants without
// replacing the others, and bumps the route generation for a route slug as
// the whole-list replace does.
func UpdateAgentGroupPermissionScopes(groupID int64, slugs []string, grantedBy string, update ScopeUpdate) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var archived dbTimestamp
	switch err := tx.QueryRow(`SELECT archived_at FROM agent_groups WHERE id = ?`, groupID).Scan(&archived); {
	case errors.Is(err, sql.ErrNoRows):
		return sql.ErrNoRows
	case err != nil:
		return err
	case archived.valid:
		return ErrHolderRetired
	}
	routes := false
	for _, slug := range slugs {
		cur := PermissionRow{Effect: PermEffectGrant}
		switch err := tx.QueryRow(`SELECT scope_json FROM agent_group_permissions WHERE group_id = ? AND slug = ?`, groupID, slug).Scan(&cur.Scope); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			cur.Exists = true
		}
		scope, keep, remove, err := update(slug, cur)
		switch {
		case err != nil:
			return err
		case keep:
			continue
		case remove:
			if _, err := tx.Exec(`DELETE FROM agent_group_permissions WHERE group_id = ? AND slug = ?`, groupID, slug); err != nil {
				return err
			}
		default:
			if _, err := tx.Exec(`INSERT INTO agent_group_permissions
				(group_id, slug, granted_at, granted_by, scope_json) VALUES (?, ?, ?, ?, ?)
				ON CONFLICT(group_id, slug) DO UPDATE SET
					granted_at = excluded.granted_at, granted_by = excluded.granted_by, scope_json = excluded.scope_json`,
				groupID, slug, dbTime(time.Now()), grantedBy, scope); err != nil {
				return err
			}
		}
		routes = routes || slug == "routes.publish" || slug == "routes.consume"
	}
	if routes {
		if _, err := tx.Exec(`UPDATE agent_groups SET route_generation = route_generation + 1 WHERE id = ?`, groupID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
