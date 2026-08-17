package db

import (
	"errors"
	"strings"
	"time"
)

// auto_permit.go carries the per-agent auto-permit opt-in set (schema v220):
// the named permission-prompt conditions an operator has pre-consented to for
// one agent, which the daemon's auto-permit sweep may answer on their behalf.
//
// Rows are keyed on the stable agent_id, so consent follows the actor across a
// reincarnate / `/clear` conv rotation. The condition name is opaque here — the
// vocabulary lives in the daemon's compile-time condition registry, and a name
// no build recognizes is inert (nothing matches it), the same way an
// unregistered permission slug is.
//
// This store is DB + CLI + dashboard only; nothing in it ever reaches a tmux
// pane, so the validation below is storage hygiene rather than an injection
// guard. The keystrokes the sweep injects are compile-time constants from the
// condition registry, never anything read back from here.

// MaxAutoPermitConditionLen bounds one stored condition name. The registry's
// own names are short kebab-case slugs; the cap only keeps a malformed write
// from storing an unbounded string.
const MaxAutoPermitConditionLen = 64

// MaxAutoPermitConditions caps how many conditions one agent may opt into. The
// registry is small and deliberately narrow (see the daemon's condition
// registry), so this is a sanity bound, not a policy.
const MaxAutoPermitConditions = 32

// AutoPermitOptIn is one stored opt-in.
type AutoPermitOptIn struct {
	AgentID   string
	Condition string
	// GrantedBy is a display snapshot of who consented — "human" for an
	// operator call, or the caller's conv-id / title for a manager-pattern
	// (--target) call. Denormalized at write time so the row stays readable
	// after the granter is renamed or retired.
	GrantedBy string
	CreatedAt time.Time
}

// NormalizeAutoPermitCondition trims a condition name and reports the cleaned
// value. It is the single place the name policy lives, shared by the write ops
// and the boundary validators: non-empty after trim, within
// MaxAutoPermitConditionLen, lowercase kebab-case ([a-z0-9-]).
//
// The charset is deliberately tighter than the tag charset: a condition name is
// a registry key, not operator prose, and keeping it to an unambiguous slug
// means a stored name compares byte-for-byte against the registry with no
// case-folding or whitespace questions.
func NormalizeAutoPermitCondition(condition string) (string, error) {
	c := strings.TrimSpace(condition)
	if c == "" {
		return "", errors.New("condition is empty")
	}
	if len(c) > MaxAutoPermitConditionLen {
		return "", errors.New("condition is too long")
	}
	for _, r := range c {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return "", errors.New("condition must be lower-case kebab-case ([a-z0-9-])")
		}
	}
	return c, nil
}

// ListAgentAutoPermits returns one agent's opt-ins, sorted by condition name. An
// agent with no opt-ins yields an empty slice, nil — the same shape callers get
// for "none", so they needn't special-case it.
func ListAgentAutoPermits(agentID string) ([]AutoPermitOptIn, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, errors.New("ListAgentAutoPermits: agent_id required")
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id, condition, granted_by, created_at
		FROM agent_auto_permit WHERE agent_id = ? ORDER BY condition`, agentID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AutoPermitOptIn{}
	for rows.Next() {
		var row AutoPermitOptIn
		var created dbTimestamp
		if err := rows.Scan(&row.AgentID, &row.Condition, &row.GrantedBy, &created); err != nil {
			return nil, err
		}
		row.CreatedAt = created.Time()
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListAllAutoPermits returns every stored opt-in, grouped by agent_id. It is the
// sweep's one read per tick: the table is tiny (opt-in is off by default and
// deliberately narrow), so one scan beats a per-agent query on every tick.
func ListAllAutoPermits() (map[string]map[string]bool, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT agent_id, condition FROM agent_auto_permit`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var agentID, condition string
		if err := rows.Scan(&agentID, &condition); err != nil {
			return nil, err
		}
		if out[agentID] == nil {
			out[agentID] = map[string]bool{}
		}
		out[agentID][condition] = true
	}
	return out, rows.Err()
}

// SetAgentAutoPermit records an opt-in. Re-consenting to a condition already
// stored refreshes granted_by / created_at, so the row always names the most
// recent consent rather than the first one.
//
// Enforces MaxAutoPermitConditions on the RESULTING set, so a write can't push
// an agent past the cap.
func SetAgentAutoPermit(agentID, condition, grantedBy string, now time.Time) error {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return errors.New("SetAgentAutoPermit: agent_id required")
	}
	clean, err := NormalizeAutoPermitCondition(condition)
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
	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM agent_auto_permit
		WHERE agent_id = ? AND condition <> ?`, agentID, clean).Scan(&existing); err != nil {
		return err
	}
	if existing+1 > MaxAutoPermitConditions {
		return errors.New("too many auto-permit conditions")
	}
	if _, err := tx.Exec(`INSERT INTO agent_auto_permit (agent_id, condition, granted_by, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(agent_id, condition) DO UPDATE SET
			granted_by = excluded.granted_by,
			created_at = excluded.created_at`,
		agentID, clean, strings.TrimSpace(grantedBy), dbTime(now)); err != nil {
		return err
	}
	return tx.Commit()
}

// ClearAgentAutoPermit removes an opt-in and reports whether a row was actually
// removed, so a caller can distinguish "revoked" from "was never on" without a
// second read.
func ClearAgentAutoPermit(agentID, condition string) (bool, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return false, errors.New("ClearAgentAutoPermit: agent_id required")
	}
	clean, err := NormalizeAutoPermitCondition(condition)
	if err != nil {
		return false, err
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM agent_auto_permit WHERE agent_id = ? AND condition = ?`,
		agentID, clean)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
