package db

import (
	"database/sql"
	"time"
)

// CarriedSudoGrant is a temporary grant stored for all future Stewards. Its
// expiry is the original one: a new Steward never restarts the clock.
type CarriedSudoGrant struct {
	ID        int64
	Slug      string
	ScopeJSON string
	GrantedAt time.Time
	ExpiresAt time.Time
	GrantedBy string
	Reason    string
}

// ListStewardCarriedPermissions returns the standing grants and denies stored
// for all future Stewards, by slug.
func ListStewardCarriedPermissions() (map[string]PermissionOverride, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT slug, effect, scope_json FROM steward_carried_permissions`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]PermissionOverride{}
	for rows.Next() {
		var slug string
		var override PermissionOverride
		if err := rows.Scan(&slug, &override.Effect, &override.Scope); err != nil {
			return nil, err
		}
		out[slug] = override
	}
	return out, rows.Err()
}

// SetStewardCarriedPermission stores or replaces one standing grant or deny.
func SetStewardCarriedPermission(slug string, override PermissionOverride, by string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO steward_carried_permissions (slug, effect, scope_json, granted_by, granted_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(slug) DO UPDATE SET effect = excluded.effect, scope_json = excluded.scope_json,
			granted_by = excluded.granted_by, granted_at = excluded.granted_at`,
		slug, override.Effect, override.Scope, by, dbTime(time.Now()))
	return err
}

// DeleteStewardCarriedPermission stops giving a slug to future Stewards.
func DeleteStewardCarriedPermission(slug string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM steward_carried_permissions WHERE slug = ?`, slug)
	return err
}

// CarrySudoGrant stores an instance's active sudo grant for all future
// Stewards and links the two rows.
func CarrySudoGrant(grantID int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.Exec(`INSERT INTO steward_carried_sudo (slug, scope_json, granted_at, expires_at, granted_by, reason)
		SELECT slug, scope_json, granted_at, expires_at, granted_by, reason
		FROM agent_sudo_grants WHERE id = ? AND revoked_at IS NULL`, grantID)
	if err != nil {
		return err
	}
	carriedID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE agent_sudo_grants SET carried_id = ? WHERE id = ?`, carriedID, grantID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListActiveStewardCarriedSudo returns the stored temporary grants still in
// time and not revoked.
func ListActiveStewardCarriedSudo() ([]*CarriedSudoGrant, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, slug, scope_json, granted_at, expires_at, granted_by, reason
		FROM steward_carried_sudo WHERE revoked_at IS NULL AND expires_at > ?
		ORDER BY expires_at ASC`, dbTime(time.Now()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*CarriedSudoGrant
	for rows.Next() {
		var g CarriedSudoGrant
		var grantedAt, expiresAt dbTimestamp
		if err := rows.Scan(&g.ID, &g.Slug, &g.ScopeJSON, &grantedAt, &expiresAt, &g.GrantedBy, &g.Reason); err != nil {
			return nil, err
		}
		g.GrantedAt, g.ExpiresAt = grantedAt.Time(), expiresAt.Time()
		out = append(out, &g)
	}
	return out, rows.Err()
}

// InheritStewardCarriedSudo gives one instance a stored grant, with its
// original expiry. Instance rows are ordinary sudo grants from here on.
func InheritStewardCarriedSudo(agentID string, g *CarriedSudoGrant, by string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	return insertSudoGrantForAgent(d, &SudoGrant{
		Slug: g.Slug, ScopeJSON: g.ScopeJSON, GrantedAt: g.GrantedAt, ExpiresAt: g.ExpiresAt,
		GrantedBy: by, Reason: g.Reason, CarriedID: g.ID,
	}, agentID)
}

// endCarriedSudo revokes the stored grants behind the instance rows where
// applies, so an operator's revoke also holds for later Stewards. Retiring a
// Steward does not come through here: it ends only the instance's rows.
func endCarriedSudo(tx *sql.Tx, where string, args ...any) error {
	_, err := tx.Exec(`UPDATE steward_carried_sudo SET revoked_at = ?
		WHERE revoked_at IS NULL AND id IN (SELECT carried_id FROM agent_sudo_grants
			WHERE carried_id > 0 AND revoked_at IS NULL AND `+where+`)`,
		append([]any{dbTime(time.Now())}, args...)...)
	return err
}
