package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Permission-override effects — the value stored in
// agent_permissions.effect (schema v38+). A row with effect=grant ADDS
// the slug on top of the config.json defaults; effect=deny SUBTRACTS
// it, overriding a default grant for that one conv. The absence of a
// row is the third, neutral state: "inherit the default."
const (
	PermEffectGrant = "grant"
	PermEffectDeny  = "deny"
)

// AgentPermission is a row in agent_permissions — a per-conv permission
// override. Lives in SQLite (rather than ~/.tclaude/config.json) so the
// daemon can grant/revoke without JSON-file rewrites. DefaultPermissions
// for *all* agents still live in config.json, since those describe
// baseline trust the human curates explicitly.
//
// Effect is "grant" or "deny" (see PermEffectGrant / PermEffectDeny).
type AgentPermission struct {
	ConvID    string
	Slug      string
	Effect    string
	ScopeJSON string
	GrantedAt time.Time
	GrantedBy string
	// BirthSource is the spawn profile (and role) a birth write took this row
	// from, nil when it was not written at birth from a profile or has been
	// written since.
	BirthSource *PermissionBirthSource
}

// PermissionBirthSource names where a birth-time permission row came from:
// the spawn profile whose tier won the slug, and the role when the profile
// conferred it through a role. Stored as JSON in agent_permissions.birth_source.
//
// Only a birth write records one, and every other write of the row resets it,
// so a row carrying one is exactly what that profile wrote and nobody changed
// since. Re-applying the profile may touch such rows and no others: explicit,
// operator, generator, imported and cloned rows never carry one, and sudo
// grants live in agent_sudo_grants. A federation identity rotation that
// rewrites a scope clears it too. The one move that keeps it is a teleport
// re-mint (RemintFederationClone), which carries the same conversation, rows
// and launch layers to a new agent ID unchanged.
type PermissionBirthSource struct {
	ProfileID int64  `json:"profile_id"`
	Role      string `json:"role,omitempty"`
}

func encodePermissionBirthSource(src *PermissionBirthSource) (string, error) {
	if src == nil || src.ProfileID <= 0 {
		return "", nil
	}
	raw, err := json.Marshal(src)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func decodePermissionBirthSource(raw string) *PermissionBirthSource {
	if raw == "" {
		return nil
	}
	var src PermissionBirthSource
	if json.Unmarshal([]byte(raw), &src) != nil || src.ProfileID <= 0 {
		return nil
	}
	return &src
}

// AgentGroupPermission is one live group grant. ScopeJSON is empty for an
// unscoped grant.
type AgentGroupPermission struct {
	GroupID   int64
	GroupName string
	Slug      string
	ScopeJSON string
	GrantedAt time.Time
	GrantedBy string
	// Retained marks a grant the agent holds through a group it was moved out
	// of with its permissions kept (agent_retained_group_permissions), not
	// through membership.
	Retained bool
}

// permissionGroupSources is the (group_id, agent_id, retained) relation the
// group permission tier reads: an agent's memberships, plus the groups whose
// permissions it kept when an operator moved it out of them. A kept group
// grants exactly what membership would, still bound to that group.
const permissionGroupSources = `SELECT group_id, agent_id, 0 AS retained FROM agent_group_members
	UNION ALL
	SELECT r.group_id, r.agent_id, 1 AS retained FROM agent_retained_group_permissions r
	WHERE NOT EXISTS (SELECT 1 FROM agent_group_members m WHERE m.group_id = r.group_id AND m.agent_id = r.agent_id)`

// ListAgentGroupPermissions returns the sorted additive grants attached to a
// group. Group grants are live membership policy, not birth-time agent rows.
func ListAgentGroupPermissions(groupID int64) ([]string, error) {
	grants, err := ListAgentGroupPermissionRows(groupID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(grants))
	for _, grant := range grants {
		out = append(out, grant.Slug)
	}
	return out, nil
}

// ListAgentGroupPermissionRows returns the group's sorted grants including
// their optional persisted scopes.
func ListAgentGroupPermissionRows(groupID int64) ([]AgentGroupPermission, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT slug, scope_json, granted_at, granted_by
		FROM agent_group_permissions WHERE group_id = ? ORDER BY slug`, groupID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AgentGroupPermission{}
	for rows.Next() {
		var grant AgentGroupPermission
		var grantedAt dbTimestamp
		if err := rows.Scan(&grant.Slug, &grant.ScopeJSON, &grantedAt, &grant.GrantedBy); err != nil {
			return nil, err
		}
		grant.GroupID = groupID
		grant.GrantedAt = grantedAt.Time()
		out = append(out, grant)
	}
	return out, rows.Err()
}

// ReplaceAgentGroupPermissions atomically replaces a group's additive grants.
func ReplaceAgentGroupPermissions(groupID int64, slugs []string, grantedBy string) error {
	return replaceAgentGroupPermissionGrants(groupID, UnscopedGrants(slugs), grantedBy, true)
}

// ReplaceAgentGroupPermissionGrants atomically replaces a group's additive
// grants, including their scopes. A structured grant's supplied scope is
// authoritative; a legacy bare slug preserves an existing narrowing, while
// an explicitly supplied empty scope deliberately clears it.
func ReplaceAgentGroupPermissionGrants(groupID int64, grants []PermissionGrant, grantedBy string) error {
	return replaceAgentGroupPermissionGrants(groupID, grants, grantedBy, false)
}

func replaceAgentGroupPermissionGrants(groupID int64, grants []PermissionGrant, grantedBy string, preserveScopes bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_groups WHERE id = ?`, groupID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return sql.ErrNoRows
	}
	var hadRouteCapability int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_group_permissions WHERE group_id = ? AND slug IN ('routes.publish', 'routes.consume')`, groupID).Scan(&hadRouteCapability); err != nil {
		return err
	}
	// Bare-slug callers cannot express scopes. Preserve each retained scope for
	// those legacy callers so an unrelated edit cannot silently widen it. The
	// scoped replacement path below supplies authoritative scopes instead.
	existingScopes := map[string]string{}
	needsExistingScopes := preserveScopes
	for _, grant := range grants {
		if grant.Scope == "" && !grant.ScopeSpecified {
			needsExistingScopes = true
			break
		}
	}
	if needsExistingScopes {
		rows, err := tx.Query(`SELECT slug, scope_json FROM agent_group_permissions WHERE group_id = ?`, groupID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var slug, scopeJSON string
			if err := rows.Scan(&slug, &scopeJSON); err != nil {
				_ = rows.Close()
				return err
			}
			existingScopes[slug] = scopeJSON
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM agent_group_permissions WHERE group_id = ?`, groupID); err != nil {
		return err
	}
	now := dbTime(time.Now())
	seen := map[string]bool{}
	for _, grant := range grants {
		slug := grant.Slug
		if seen[slug] {
			continue
		}
		seen[slug] = true
		scopeJSON := grant.Scope
		if preserveScopes || (grant.Scope == "" && !grant.ScopeSpecified) {
			scopeJSON = existingScopes[slug]
		}
		if _, err := tx.Exec(`INSERT INTO agent_group_permissions
			(group_id, slug, granted_at, granted_by, scope_json) VALUES (?, ?, ?, ?, ?)`,
			groupID, slug, now, grantedBy, scopeJSON); err != nil {
			return err
		}
	}
	if hadRouteCapability > 0 || seen["routes.publish"] || seen["routes.consume"] {
		if _, err := tx.Exec(`UPDATE agent_groups SET route_generation = route_generation + 1 WHERE id = ?`, groupID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListAgentGroupPermissionRowsForConv returns every active group grant for an
// actor, preserving one row per group so a later resolver can union scopes
// within the group tier. Phase 1 uses the rows only for provenance display.
func ListAgentGroupPermissionRowsForConv(convID string) ([]AgentGroupPermission, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil || agentID == "" {
		return nil, err
	}
	return queryRows(d, func(rows rowScanner) (AgentGroupPermission, error) {
		var grant AgentGroupPermission
		var grantedAt dbTimestamp
		if err := rows.Scan(&grant.GroupID, &grant.GroupName, &grant.Slug, &grant.ScopeJSON, &grantedAt, &grant.GrantedBy, &grant.Retained); err != nil {
			return AgentGroupPermission{}, err
		}
		grant.GrantedAt = grantedAt.Time()
		return grant, nil
	}, `
		SELECT gp.group_id, g.name, gp.slug, gp.scope_json, gp.granted_at, gp.granted_by, gm.retained
		FROM agent_group_permissions gp
		JOIN agent_groups g ON g.id = gp.group_id AND g.archived_at IS NULL
		JOIN (`+permissionGroupSources+`) gm ON gm.group_id = gp.group_id
		JOIN agents a ON a.agent_id = gm.agent_id AND a.retired_at IS NULL
		WHERE gm.agent_id = ?
		ORDER BY gp.slug, gp.group_id`, agentID)
}

// IsAgentGroupRouteEnabled reports whether a group has either route
// capability in its own policy. Group membership remains ordinary unless the
// group deliberately opts into the route contract.
func IsAgentGroupRouteEnabled(groupID int64, publishSlug, consumeSlug string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM agent_group_permissions
		WHERE group_id = ? AND slug IN (?, ?)`, groupID, publishSlug, consumeSlug).Scan(&n)
	return n > 0, err
}

// AgentPermissionOverride returns the tri-state per-conv override for
// (convID, slug): effect is "grant" or "deny" with ok=true when a row
// exists, or ("", false) when there is no override and the slug falls
// through to the config defaults.
func AgentPermissionOverride(convID, slug string) (effect string, ok bool, err error) {
	db, err := Open()
	if err != nil {
		return "", false, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return "", false, err
	}
	if agentID == "" {
		return "", false, nil
	}
	err = db.QueryRow(`SELECT effect FROM agent_permissions WHERE agent_id = ? AND slug = ?`,
		agentID, slug).Scan(&effect)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return effect, true, nil
}

// AgentPermissionOverrideByAgentID returns the override row for
// (agentID, slug) including its scope, or (nil, nil) when the agent has no
// row for that slug. It reads the stable actor directly — no conv resolution,
// no enrollment — for callers that captured identity at an earlier security
// boundary and must not resurrect a stale generation.
func AgentPermissionOverrideByAgentID(agentID, slug string) (*AgentPermission, error) {
	if agentID == "" {
		return nil, nil
	}
	db, err := Open()
	if err != nil {
		return nil, err
	}
	row := AgentPermission{Slug: slug}
	var grantedAt dbTimestamp
	err = db.QueryRow(`SELECT effect, scope_json, granted_at, granted_by
		FROM agent_permissions WHERE agent_id = ? AND slug = ?`, agentID, slug).
		Scan(&row.Effect, &row.ScopeJSON, &grantedAt, &row.GrantedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.GrantedAt = grantedAt.Time()
	return &row, nil
}

// ListAgentPermissionsForConv returns every GRANTED slug for this conv,
// alphabetised. Deny-effect rows are excluded — this preserves the
// historical "additive grants" semantics the CLI / dashboard grants
// view relies on. Returns an empty slice (not nil) for a conv with no
// grants.
func ListAgentPermissionsForConv(convID string) ([]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return []string{}, nil
	}
	rows, err := db.Query(`SELECT slug FROM agent_permissions WHERE agent_id = ? AND effect = ? ORDER BY slug`,
		agentID, PermEffectGrant)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListAgentPermissionOverridesForConv returns the full tri-state
// override map for this conv — slug → "grant" | "deny". Convs with no
// overrides return an empty (non-nil) map. Used by the dashboard's
// permanent-permission editor to pre-populate the modal.
func ListAgentPermissionOverridesForConv(convID string) (map[string]string, error) {
	rows, err := ListAgentPermissionOverrideRowsForConv(convID)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, row := range rows {
		out[row.Slug] = row.Effect
	}
	return out, nil
}

// ListAgentPermissionOverrideRowsForConv returns the full per-agent override
// rows including optional scope storage.
func ListAgentPermissionOverrideRowsForConv(convID string) ([]AgentPermission, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return nil, err
	}
	if agentID == "" {
		return []AgentPermission{}, nil
	}
	rows, err := db.Query(`SELECT slug, effect, scope_json, granted_at, granted_by, birth_source
		FROM agent_permissions WHERE agent_id = ? ORDER BY slug`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AgentPermission{}
	for rows.Next() {
		var row AgentPermission
		var grantedAt dbTimestamp
		var birthSource string
		if err := rows.Scan(&row.Slug, &row.Effect, &row.ScopeJSON, &grantedAt, &row.GrantedBy, &birthSource); err != nil {
			return nil, err
		}
		row.ConvID = convID
		row.GrantedAt = grantedAt.Time()
		row.BirthSource = decodePermissionBirthSource(birthSource)
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListAllAgentPermissions returns the full GRANT table, grouped by
// conv-id. Deny-effect rows are excluded — see ListAgentPermissionsForConv.
// Convs with no grants are absent from the map. Used by the dashboard /
// permissions ls view.
func ListAllAgentPermissions() (map[string][]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	// Keyed by the actor's CURRENT conv for display (the dashboard renders per
	// live conv); the table is agent-keyed, so resolve through agents.
	rows, err := db.Query(`SELECT ag.current_conv_id, p.slug
		FROM agent_permissions p JOIN agents ag ON ag.agent_id = p.agent_id
		WHERE p.effect = ? ORDER BY ag.current_conv_id, p.slug`,
		PermEffectGrant)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string][]string{}
	for rows.Next() {
		var c, s string
		if err := rows.Scan(&c, &s); err != nil {
			return nil, err
		}
		out[c] = append(out[c], s)
	}
	return out, rows.Err()
}

// ListAllAgentPermissionOverrides returns every per-conv override
// (grant AND deny), nested conv-id → slug → effect. Convs with no
// overrides are absent. Used by the dashboard snapshot so the per-agent
// editor can render the current state without a round-trip per agent.
func ListAllAgentPermissionOverrides() (map[string]map[string]string, error) {
	db, err := Open()
	if err != nil {
		return nil, err
	}
	// The result is folded into maps, so row order has no observable meaning.
	// Omitting ORDER BY avoids a temp sort over the whole permission table on
	// the dashboard's 2-second snapshot path.
	rows, err := db.Query(`SELECT ag.current_conv_id, p.slug, p.effect
		FROM agent_permissions p JOIN agents ag ON ag.agent_id = p.agent_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]string{}
	for rows.Next() {
		var c, s, e string
		if err := rows.Scan(&c, &s, &e); err != nil {
			return nil, err
		}
		if out[c] == nil {
			out[c] = map[string]string{}
		}
		out[c][s] = e
	}
	return out, rows.Err()
}

// ListAllAgentPermissionScopes returns only non-empty scopes, nested by the
// actor's current conversation and slug. It is the display companion to the
// back-compatible effect-only Overrides map.
func ListAllAgentPermissionScopes() (map[string]map[string]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT ag.current_conv_id, p.slug, p.scope_json
		FROM agent_permissions p JOIN agents ag ON ag.agent_id = p.agent_id
		WHERE p.scope_json <> ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]string{}
	for rows.Next() {
		var convID, slug, scopeJSON string
		if err := rows.Scan(&convID, &slug, &scopeJSON); err != nil {
			return nil, err
		}
		if out[convID] == nil {
			out[convID] = map[string]string{}
		}
		out[convID][slug] = scopeJSON
	}
	return out, rows.Err()
}

// GrantAgentPermission writes a grant-effect override for (convID, slug).
// Idempotent, and an UPSERT — granting a slug that currently carries a
// deny override flips it back to grant. grantedBy is informational
// ("<human>" or a granter's conv-id); empty is fine.
func GrantAgentPermission(convID, slug, grantedBy string) error {
	return SetAgentPermissionOverride(convID, slug, PermEffectGrant, grantedBy)
}

// GrantAgentPermissionWithScope writes a grant-effect override with its
// already-validated canonical scope JSON. Empty remains the unscoped form.
func GrantAgentPermissionWithScope(convID, slug, scopeJSON, grantedBy string) error {
	return SetAgentPermissionOverrideWithScope(convID, slug, PermEffectGrant, scopeJSON, grantedBy)
}

// SetAgentPermissionOverride writes the tri-state override row for
// (convID, slug) with the given effect ("grant" or "deny"). UPSERT:
// re-running with a different effect flips the row. To return the slug
// to its default, use RevokeAgentPermission (which deletes the row).
func SetAgentPermissionOverride(convID, slug, effect, grantedBy string) error {
	return SetAgentPermissionOverrideWithScope(convID, slug, effect, "", grantedBy)
}

// SetAgentPermissionOverrideWithScope is the scoped storage twin. Callers are
// responsible for validating and canonicalizing scopeJSON before this layer.
func SetAgentPermissionOverrideWithScope(convID, slug, effect, scopeJSON, grantedBy string) error {
	return setAgentPermissionOverrideForConv(convID, slug, effect, scopeJSON, grantedBy, nil)
}

// SetAgentBirthPermissionOverride is the birth-time write: the same row as
// SetAgentPermissionOverrideWithScope, recording the spawn profile it came
// from (nil when the request, a role the request named, or worker defaults
// set it).
func SetAgentBirthPermissionOverride(convID, slug, effect, scopeJSON, grantedBy string, source *PermissionBirthSource) error {
	return setAgentPermissionOverrideForConv(convID, slug, effect, scopeJSON, grantedBy, source)
}

func setAgentPermissionOverrideForConv(convID, slug, effect, scopeJSON, grantedBy string, source *PermissionBirthSource) error {
	if effect != PermEffectGrant && effect != PermEffectDeny {
		return fmt.Errorf("invalid permission effect %q (want %q or %q)", effect, PermEffectGrant, PermEffectDeny)
	}
	// Holding a permission override (grant or deny) makes the conv an agent —
	// a deny is still per-agent permission config. EnsureAgentForConv mints /
	// links the stable actor; we then key the override on agent_id (JOH-26) so
	// it survives conv rotations without a rekey.
	if _, _, err := EnsureAgentForConv(convID, "grant"); err != nil {
		return err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return fmt.Errorf("SetAgentPermissionOverride: no actor for conv %s", convID)
	}
	return setAgentPermissionOverrideByAgentID(agentID, slug, effect, scopeJSON, grantedBy, source)
}

// SetAgentPermissionOverrideByAgentID writes a permission override directly
// against an existing, active stable actor. Unlike the conv-scoped writer, it
// never enrolls or relinks a conversation. Callers that captured identity at
// an earlier security boundary use this form so a stale generation cannot be
// resurrected as a different actor while a decision is pending.
func SetAgentPermissionOverrideByAgentID(agentID, slug, effect, grantedBy string) error {
	return SetAgentPermissionOverrideByAgentIDWithScope(agentID, slug, effect, "", grantedBy)
}

func SetAgentPermissionOverrideByAgentIDWithScope(agentID, slug, effect, scopeJSON, grantedBy string) error {
	return setAgentPermissionOverrideByAgentID(agentID, slug, effect, scopeJSON, grantedBy, nil)
}

func setAgentPermissionOverrideByAgentID(agentID, slug, effect, scopeJSON, grantedBy string, source *PermissionBirthSource) error {
	birthSource, err := encodePermissionBirthSource(source)
	if err != nil {
		return err
	}
	if effect != PermEffectGrant && effect != PermEffectDeny {
		return fmt.Errorf("invalid permission effect %q (want %q or %q)", effect, PermEffectGrant, PermEffectDeny)
	}
	if effect == PermEffectDeny && scopeJSON != "" {
		return fmt.Errorf("deny permission override %q cannot carry a scope", slug)
	}
	if agentID == "" {
		return fmt.Errorf("SetAgentPermissionOverrideByAgentID: agentID is required")
	}
	db, err := Open()
	if err != nil {
		return err
	}
	res, err := db.Exec(`INSERT INTO agent_permissions
		(agent_id, slug, effect, granted_at, granted_by, scope_json, birth_source)
		SELECT ?, ?, ?, ?, ?, ?, ? FROM agents WHERE agent_id = ? AND retired_at IS NULL
		ON CONFLICT(agent_id, slug) DO UPDATE SET
			effect       = excluded.effect,
			granted_at   = excluded.granted_at,
			granted_by   = excluded.granted_by,
			scope_json   = excluded.scope_json,
			birth_source = excluded.birth_source`,
		agentID, slug, effect, dbTime(time.Now()), grantedBy, scopeJSON, birthSource, agentID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		var retiredAt dbTimestamp
		switch err := db.QueryRow(`SELECT retired_at FROM agents WHERE agent_id = ?`, agentID).Scan(&retiredAt); {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("SetAgentPermissionOverrideByAgentID: agent %s does not exist", agentID)
		case err != nil:
			return err
		case retiredAt.valid:
			return fmt.Errorf("SetAgentPermissionOverrideByAgentID: agent %s is retired", agentID)
		default:
			return fmt.Errorf("SetAgentPermissionOverrideByAgentID: agent %s is not active", agentID)
		}
	}
	return nil
}

// RevokeAgentPermission removes a single (convID, slug). Idempotent.
// Returns the number of rows deleted (0 if there was nothing to remove).
func RevokeAgentPermission(convID, slug string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM agent_permissions WHERE agent_id = ? AND slug = ?`, agentID, slug)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ApplyAgentPermissionOverrides atomically applies a set of permission effects
// for convID. When clearGranterDenies is true, deny rows previously authored by
// grantedBy are removed in the same transaction before the requested effects
// are applied. That lets a policy generator unwind stale denies without a
// temporary capability gap or a half-cleared state on write failure.
//
// When preserveSameEffectProvenance is true, reapplying the same effect keeps
// the existing row's audit provenance. A generator must not relabel a
// human-authored deny as its own merely because it independently wants the
// same deny; preserving the original granted_by also ensures a later
// generator-scoped cleanup cannot erase operator policy. Fresh-agent callers
// can pass false to replace birth-time provenance with their own audit label.
func ApplyAgentPermissionOverrides(convID string, overrides map[string]PermissionOverride, grantedBy string, clearGranterDenies, preserveSameEffectProvenance bool) error {
	for slug, override := range overrides {
		if override.Effect != PermEffectGrant && override.Effect != PermEffectDeny {
			return fmt.Errorf("invalid permission effect %q for %s (want %q or %q)", override.Effect, slug, PermEffectGrant, PermEffectDeny)
		}
		if override.Effect == PermEffectDeny && override.Scope != "" {
			return fmt.Errorf("deny permission override %q cannot carry a scope", slug)
		}
	}
	d, err := Open()
	if err != nil {
		return err
	}
	if _, _, err := EnsureAgentForConv(convID, "grant"); err != nil {
		return err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return err
	}
	if agentID == "" {
		return fmt.Errorf("ApplyAgentPermissionOverrides: no actor for conv %s", convID)
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if clearGranterDenies {
		if _, err := tx.Exec(`DELETE FROM agent_permissions
			WHERE agent_id = ? AND granted_by = ? AND effect = ?`, agentID, grantedBy, PermEffectDeny); err != nil {
			return err
		}
	}
	now := dbTime(time.Now())
	// scope_json travels with the effect on both arms. Hard-coding '' here
	// (as this did before scopes existed) silently WIDENED a re-applied
	// scoped grant to an unscoped one — the exact direction a permission
	// write must never drift in.
	upsert := `INSERT INTO agent_permissions
		(agent_id, slug, effect, granted_at, granted_by, scope_json)
		SELECT ?, ?, ?, ?, ?, ? FROM agents WHERE agent_id = ? AND retired_at IS NULL
		ON CONFLICT(agent_id, slug) DO UPDATE SET
			effect = excluded.effect,
			granted_at = excluded.granted_at,
			granted_by = excluded.granted_by,
			scope_json = excluded.scope_json,
			birth_source = ''`
	// Either arm resets birth_source: a row written here is no longer only
	// what a spawn profile put there, so a profile re-apply must leave it.
	if preserveSameEffectProvenance {
		upsert = `INSERT INTO agent_permissions
			(agent_id, slug, effect, granted_at, granted_by, scope_json)
			SELECT ?, ?, ?, ?, ?, ? FROM agents WHERE agent_id = ? AND retired_at IS NULL
			ON CONFLICT(agent_id, slug) DO UPDATE SET
				effect = excluded.effect,
				scope_json = excluded.scope_json,
				granted_at = CASE
					WHEN agent_permissions.effect = excluded.effect THEN agent_permissions.granted_at
					ELSE excluded.granted_at
				END,
				granted_by = CASE
					WHEN agent_permissions.effect = excluded.effect THEN agent_permissions.granted_by
					ELSE excluded.granted_by
				END,
				birth_source = ''`
	}
	for slug, override := range overrides {
		res, err := tx.Exec(upsert, agentID, slug, override.Effect, now, grantedBy, override.Scope, agentID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return fmt.Errorf("ApplyAgentPermissionOverrides: agent %s is retired", agentID)
		}
	}
	return tx.Commit()
}

// RevokeAllAgentPermissionsForConv drops every per-conv permission
// row for convID. Bulk cleanup variant for the conv-delete path.
// Idempotent — returns the number of rows removed.
func RevokeAllAgentPermissionsForConv(convID string) (int64, error) {
	db, err := Open()
	if err != nil {
		return 0, err
	}
	agentID, err := AgentIDForConv(convID)
	if err != nil {
		return 0, err
	}
	if agentID == "" {
		return 0, nil
	}
	res, err := db.Exec(`DELETE FROM agent_permissions WHERE agent_id = ?`, agentID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListPeerWrittenGroupPermissionScopes returns the stored scopes of the
// group permission rows a federation peer wrote (granted_by "peer:<id>"),
// so a peer's group rename or delete can be refused when it would move what
// such a row names.
func ListPeerWrittenGroupPermissionScopes() ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT scope_json FROM agent_group_permissions WHERE granted_by LIKE 'peer:%' AND scope_json != ''`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return nil, err
		}
		out = append(out, scope)
	}
	return out, rows.Err()
}
