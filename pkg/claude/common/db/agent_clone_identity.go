package db

import (
	"errors"
	"fmt"
	"time"
)

// CloneIdentity is the identity a clone inherits from its source: group
// memberships (with role and description), per-agent permission overrides
// (with scope) and, optionally, group ownerships.
type CloneIdentity struct {
	Members  []*AgentGroupMember
	Perms    []AgentPermission
	OwnedIDs []int64
}

// CopyCloneIdentity writes identity onto the active agent agentID in one
// transaction: either every row lands or none does, so a clone never comes up
// with only part of its source's groups or permissions. It is add-only on the
// clone and never touches the source's rows.
func CopyCloneIdentity(agentID string, identity CloneIdentity, grantedBy string) error {
	if agentID == "" {
		return errors.New("CopyCloneIdentity: agentID is required")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agents WHERE agent_id = ? AND retired_at IS NULL`, agentID).Scan(&active); err != nil {
		return err
	}
	if active == 0 {
		return fmt.Errorf("CopyCloneIdentity: agent %s is not active", agentID)
	}
	now := dbTime(time.Now())
	for _, m := range identity.Members {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO agent_group_members
			(group_id, agent_id, role, descr, joined_at) VALUES (?, ?, ?, ?, ?)`,
			m.GroupID, agentID, m.Role, m.Descr, now); err != nil {
			return fmt.Errorf("join group %d: %w", m.GroupID, err)
		}
		if _, err := tx.Exec(`UPDATE agent_groups SET route_generation = route_generation + 1 WHERE id = ?`, m.GroupID); err != nil {
			return err
		}
	}
	for _, p := range identity.Perms {
		if p.Effect != PermEffectGrant && p.Effect != PermEffectDeny {
			return fmt.Errorf("invalid permission effect %q for %s", p.Effect, p.Slug)
		}
		if p.Effect == PermEffectDeny && p.ScopeJSON != "" {
			return fmt.Errorf("deny permission override %q cannot carry a scope", p.Slug)
		}
		if _, err := tx.Exec(`INSERT INTO agent_permissions
			(agent_id, slug, effect, granted_at, granted_by, scope_json)
			VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(agent_id, slug) DO UPDATE SET
				effect     = excluded.effect,
				granted_at = excluded.granted_at,
				granted_by = excluded.granted_by,
				scope_json = excluded.scope_json`,
			agentID, p.Slug, p.Effect, now, grantedBy, p.ScopeJSON); err != nil {
			return fmt.Errorf("copy permission %s: %w", p.Slug, err)
		}
	}
	for _, groupID := range identity.OwnedIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO agent_group_owners
			(group_id, agent_id, granted_at, granted_by) VALUES (?, ?, ?, ?)`,
			groupID, agentID, now, grantedBy); err != nil {
			return fmt.Errorf("copy ownership of group %d: %w", groupID, err)
		}
	}
	return tx.Commit()
}
