package db

import (
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Re-applying an edited spawn profile's grants to an agent that holds a copy
// of it (tcl-fg2kqk). A re-apply may only change the rows that profile wrote
// at birth and nobody has changed since: the rows whose birth_source names
// it (PermissionBirthSource). Every other row (explicit, operator, generator,
// cloned, another profile's) is left alone, and sudo grants live elsewhere.

// BirthPermissionChange is one slug a re-apply would write.
type BirthPermissionChange struct {
	Slug string
	// Want is the profile's value for the slug now; nil removes the row.
	Want *PermissionOverride
	// Source is the birth source the written row records (the profile, and
	// the role when a role grants it).
	Source PermissionBirthSource
	// MayAdd allows inserting a row when the agent has none for the slug.
	MayAdd bool
}

// Outcomes of one BirthPermissionChange.
const (
	BirthPermissionChanged = "changed"
	BirthPermissionRemoved = "removed"
	BirthPermissionAdded   = "added"
	// BirthPermissionKept: the agent's row is not the profile's (explicit,
	// or changed since its spawn), so it stays.
	BirthPermissionKept = "kept"
	// BirthPermissionNotAdded: the agent has no row and the change may not
	// add one.
	BirthPermissionNotAdded = "not added"
)

// BirthPermissionResult is what happened to one change.
type BirthPermissionResult struct {
	Slug    string
	Outcome string
	// From is the row's value before, To after ("" for none).
	From, To string
}

// BirthPermissionReapply is the grant half of re-applying a profile, written
// with the spawn copy (SpawnCopyOutside.BirthPermissions). Results is filled
// in as it is written.
type BirthPermissionReapply struct {
	ProfileID int64
	Changes   []BirthPermissionChange
	GrantedBy string
	Results   []BirthPermissionResult
}

// reapplyBirthPermissionsTx applies r's changes to agentID's permission rows:
// a row whose birth_source names r.ProfileID is updated (keeping a birth
// source) or removed, a missing row is added when the change allows it, and
// any other row is kept. Each write matches on the source it read, so a row
// changed by someone else meanwhile is not overwritten.
func reapplyBirthPermissionsTx(tx *sql.Tx, agentID string, r *BirthPermissionReapply) error {
	if r == nil || len(r.Changes) == 0 {
		return nil
	}
	if r.ProfileID <= 0 {
		return errors.New("re-apply grants: profile required")
	}
	now := dbTime(time.Now())
	r.Results = make([]BirthPermissionResult, 0, len(r.Changes))
	for _, c := range r.Changes {
		res, err := reapplyBirthPermissionTx(tx, agentID, r.ProfileID, c, r.GrantedBy, now)
		if err != nil {
			return fmt.Errorf("re-apply %s: %w", c.Slug, err)
		}
		r.Results = append(r.Results, res)
	}
	return nil
}

// PlanAgentBirthPermissions is what reapplyBirthPermissionsTx would do to
// agentID's rows, for the re-apply preview: the same writes in a transaction
// that is rolled back.
func PlanAgentBirthPermissions(agentID string, r BirthPermissionReapply) ([]BirthPermissionResult, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := reapplyBirthPermissionsTx(tx, strings.TrimSpace(agentID), &r); err != nil {
		return nil, err
	}
	return r.Results, nil
}

func reapplyBirthPermissionTx(tx *sql.Tx, agentID string, profileID int64, c BirthPermissionChange, grantedBy string, now driver.Valuer) (BirthPermissionResult, error) {
	res := BirthPermissionResult{Slug: c.Slug, To: overrideWord(c.Want)}
	var effect, scope, rawSource string
	err := tx.QueryRow(`SELECT effect, scope_json, birth_source FROM agent_permissions WHERE agent_id = ? AND slug = ?`,
		agentID, c.Slug).Scan(&effect, &scope, &rawSource)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if c.Want == nil || !c.MayAdd {
			res.Outcome, res.To = BirthPermissionNotAdded, ""
			return res, nil
		}
		if err := checkOverride(*c.Want); err != nil {
			return res, err
		}
		source, err := encodePermissionBirthSource(&c.Source)
		if err != nil {
			return res, err
		}
		if _, err := tx.Exec(`INSERT INTO agent_permissions
			(agent_id, slug, effect, granted_at, granted_by, scope_json, birth_source)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			agentID, c.Slug, c.Want.Effect, now, grantedBy, c.Want.Scope, source); err != nil {
			return res, err
		}
		res.Outcome = BirthPermissionAdded
		return res, nil
	case err != nil:
		return res, err
	}
	current := PermissionOverride{Effect: effect, Scope: scope}
	res.From = overrideWord(&current)
	src := decodePermissionBirthSource(rawSource)
	if src == nil || src.ProfileID != profileID {
		res.Outcome, res.To = BirthPermissionKept, res.From
		return res, nil
	}
	if c.Want == nil {
		if _, err := tx.Exec(`DELETE FROM agent_permissions WHERE agent_id = ? AND slug = ? AND birth_source = ?`,
			agentID, c.Slug, rawSource); err != nil {
			return res, err
		}
		res.Outcome = BirthPermissionRemoved
		return res, nil
	}
	if err := checkOverride(*c.Want); err != nil {
		return res, err
	}
	source, err := encodePermissionBirthSource(&c.Source)
	if err != nil {
		return res, err
	}
	if _, err := tx.Exec(`UPDATE agent_permissions
		SET effect = ?, scope_json = ?, granted_at = ?, granted_by = ?, birth_source = ?
		WHERE agent_id = ? AND slug = ? AND birth_source = ?`,
		c.Want.Effect, c.Want.Scope, now, grantedBy, source, agentID, c.Slug, rawSource); err != nil {
		return res, err
	}
	res.Outcome = BirthPermissionChanged
	return res, nil
}

func checkOverride(o PermissionOverride) error {
	if o.Effect != PermEffectGrant && o.Effect != PermEffectDeny {
		return fmt.Errorf("invalid permission effect %q", o.Effect)
	}
	if o.Effect == PermEffectDeny && o.Scope != "" {
		return errors.New("a deny cannot carry a scope")
	}
	return nil
}

func overrideWord(o *PermissionOverride) string {
	if o == nil {
		return ""
	}
	if o.Scope != "" {
		return o.Effect + " " + o.Scope
	}
	return o.Effect
}

// AgentBirthPermissionParts reports which parts of profileID's birth access
// took effect on agentID: hasOverrides when a row the profile's own
// permission overrides wrote still carries its source, hasRoles when a row
// one of its roles granted does. A re-apply adds new slugs only to a part
// that took effect, because a part a higher tier or the spawn request
// replaced never applied.
func AgentBirthPermissionParts(agentID string, profileID int64) (hasOverrides, hasRoles bool, err error) {
	d, err := Open()
	if err != nil {
		return false, false, err
	}
	rows, err := d.Query(`SELECT birth_source FROM agent_permissions WHERE agent_id = ? AND birth_source <> ''`, strings.TrimSpace(agentID))
	if err != nil {
		return false, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return false, false, err
		}
		src := decodePermissionBirthSource(raw)
		if src == nil || src.ProfileID != profileID {
			continue
		}
		if src.Role == "" {
			hasOverrides = true
		} else {
			hasRoles = true
		}
	}
	return hasOverrides, hasRoles, rows.Err()
}
