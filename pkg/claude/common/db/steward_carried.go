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
	UsesLimit int64
	UsesLeft  int64
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

// ReplaceStewardCarriedPermissions makes the stored set exactly next, in one
// transaction. A row whose grant did not change keeps its author and time.
func ReplaceStewardCarriedPermissions(next map[string]PermissionOverride, by string) error {
	current, err := ListStewardCarriedPermissions()
	if err != nil {
		return err
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
	for slug := range current {
		if _, kept := next[slug]; kept {
			continue
		}
		if _, err := tx.Exec(`DELETE FROM steward_carried_permissions WHERE slug = ?`, slug); err != nil {
			return err
		}
	}
	for slug, override := range next {
		if current[slug] == override {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO steward_carried_permissions (slug, effect, scope_json, granted_by, granted_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(slug) DO UPDATE SET effect = excluded.effect, scope_json = excluded.scope_json,
				granted_by = excluded.granted_by, granted_at = excluded.granted_at`,
			slug, override.Effect, override.Scope, by, dbTime(time.Now())); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	res, err := tx.Exec(`INSERT INTO steward_carried_sudo (slug, scope_json, granted_at, expires_at, granted_by, reason, uses_limit, uses_left)
		SELECT slug, scope_json, granted_at, expires_at, granted_by, reason, uses_limit, uses_left
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
	rows, err := d.Query(`SELECT id, slug, scope_json, granted_at, expires_at, granted_by, reason, uses_limit, uses_left
		FROM steward_carried_sudo WHERE revoked_at IS NULL AND expires_at > ? AND (uses_limit = 0 OR uses_left > 0)
		ORDER BY expires_at ASC`, dbTime(time.Now()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*CarriedSudoGrant
	for rows.Next() {
		var g CarriedSudoGrant
		var grantedAt, expiresAt dbTimestamp
		if err := rows.Scan(&g.ID, &g.Slug, &g.ScopeJSON, &grantedAt, &expiresAt, &g.GrantedBy, &g.Reason, &g.UsesLimit, &g.UsesLeft); err != nil {
			return nil, err
		}
		g.GrantedAt, g.ExpiresAt = grantedAt.Time(), expiresAt.Time()
		out = append(out, &g)
	}
	return out, rows.Err()
}

// endAllCarriedSudo ends every stored grant. With no Steward running nothing
// links to them, and revoke-all still has to reach them. Returns those no
// running instance holds, so the caller's count covers each grant once.
func endAllCarriedSudo() (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE steward_carried_sudo SET revoked_at = ?
		WHERE revoked_at IS NULL AND id NOT IN (SELECT carried_id FROM agent_sudo_grants
			WHERE carried_id > 0 AND revoked_at IS NULL)`, dbTime(time.Now()))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
		GrantedBy: by, Reason: g.Reason, CarriedID: g.ID, UsesLimit: g.UsesLimit, UsesLeft: g.UsesLeft,
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

// StewardRecordOpened is the row every Steward's record starts with. Alone, it
// is a record with nothing in it yet.
const StewardRecordOpened = ""

// StewardRecordPromoted marks a record opened by a handover rather than a summon.
const StewardRecordPromoted = "promoted"

// OfferStewardPermissions records that a Steward's levels have given it these
// slugs, once and all together. What the operator does with them afterwards is
// theirs.
func OfferStewardPermissions(agentID string, slugs ...string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, slug := range slugs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO steward_offered_permissions (agent_id, slug) VALUES (?, ?)`, agentID, slug); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListStewardOfferedPermissions returns what a Steward's levels have given it.
func ListStewardOfferedPermissions(agentID string) (map[string]bool, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT slug FROM steward_offered_permissions WHERE agent_id = ?`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]bool{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		out[slug] = true
	}
	return out, rows.Err()
}
