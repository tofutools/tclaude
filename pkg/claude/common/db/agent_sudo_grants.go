package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SudoGrant is a row in agent_sudo_grants. Active = ExpiresAt is in
// the future AND RevokedAt is zero. Audit-friendly fields
// (GrantedBy, Reason) carry context for forensics.
type SudoGrant struct {
	ID         int64
	ConvID     string
	Slug       string
	GrantedAt  time.Time
	ExpiresAt  time.Time
	GrantedBy  string
	Reason     string
	RevokedAt  time.Time
}

// IsActive returns true when the grant is still in force at `now`:
// not yet expired AND not revoked.
func (g *SudoGrant) IsActive(now time.Time) bool {
	if g == nil {
		return false
	}
	if !g.RevokedAt.IsZero() {
		return false
	}
	return g.ExpiresAt.After(now)
}

// InsertSudoGrant records one (conv, slug) elevation. Caller is
// responsible for setting GrantedAt / ExpiresAt explicitly so a
// multi-slug bundle from a single popup approval ends up with
// matching timestamps.
func InsertSudoGrant(g *SudoGrant) (int64, error) {
	if g == nil {
		return 0, errors.New("InsertSudoGrant: nil grant")
	}
	if strings.TrimSpace(g.ConvID) == "" {
		return 0, errors.New("InsertSudoGrant: conv_id required")
	}
	if strings.TrimSpace(g.Slug) == "" {
		return 0, errors.New("InsertSudoGrant: slug required")
	}
	if g.ExpiresAt.IsZero() {
		return 0, errors.New("InsertSudoGrant: expires_at required")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	if g.GrantedAt.IsZero() {
		g.GrantedAt = time.Now()
	}
	res, err := d.Exec(`INSERT INTO agent_sudo_grants
		(conv_id, slug, granted_at, expires_at, granted_by, reason, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		g.ConvID, g.Slug,
		g.GrantedAt.Format(time.RFC3339Nano),
		g.ExpiresAt.Format(time.RFC3339Nano),
		g.GrantedBy, g.Reason,
		formatTimeOrEmpty(g.RevokedAt))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetSudoGrant returns one row by id, or nil if absent.
func GetSudoGrant(id int64) (*SudoGrant, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT id, conv_id, slug, granted_at, expires_at, granted_by, reason, revoked_at
		FROM agent_sudo_grants WHERE id = ?`, id)
	g, err := scanSudoGrant(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return g, err
}

// HasActiveSudoGrant returns true when convID currently holds an
// active grant for slug. The hot path called from requirePermission;
// indexed lookup, no scan.
func HasActiveSudoGrant(convID, slug string) (bool, error) {
	if convID == "" || slug == "" {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	cutoff := time.Now().Format(time.RFC3339Nano)
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM agent_sudo_grants
		WHERE conv_id = ? AND slug = ? AND revoked_at = '' AND expires_at > ?`,
		convID, slug, cutoff).Scan(&n)
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// LookupActiveSudoGrantID returns the id of an active grant for
// (convID, slug), or 0 if none. Used by the audit-string composer in
// agentd: when requirePermission only passed because of a sudo grant,
// downstream `granted_by` is annotated with the grant id so forensic
// queries can answer "what did agent X do during the elevation
// window?".
//
// If multiple active grants for the pair exist (re-request before the
// first expired), returns the soonest-to-expire — same ordering
// `sudo ls` uses, so the audit string ties to the row the human is
// most likely to act on first.
func LookupActiveSudoGrantID(convID, slug string) (int64, error) {
	if convID == "" || slug == "" {
		return 0, nil
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Format(time.RFC3339Nano)
	var id int64
	err = d.QueryRow(`SELECT id FROM agent_sudo_grants
		WHERE conv_id = ? AND slug = ? AND revoked_at = '' AND expires_at > ?
		ORDER BY expires_at ASC LIMIT 1`,
		convID, slug, cutoff).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// ListActiveSudoGrants returns every active grant for convID ordered
// by expires_at ascending (soonest expiring first — useful for the
// CLI's `sudo ls` which shows the user what's about to fall off).
func ListActiveSudoGrants(convID string) ([]*SudoGrant, error) {
	if convID == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Format(time.RFC3339Nano)
	rows, err := d.Query(`SELECT id, conv_id, slug, granted_at, expires_at, granted_by, reason, revoked_at
		FROM agent_sudo_grants
		WHERE conv_id = ? AND revoked_at = '' AND expires_at > ?
		ORDER BY expires_at ASC`, convID, cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*SudoGrant
	for rows.Next() {
		g, err := scanSudoGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// ListAllActiveSudoGrants returns every active grant across all
// convs. Used by the human "sudo ls --all" path and the eventual
// dashboard panel. Ordered by conv_id then expires_at so the
// rendering groups grants per agent.
func ListAllActiveSudoGrants() ([]*SudoGrant, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Format(time.RFC3339Nano)
	rows, err := d.Query(`SELECT id, conv_id, slug, granted_at, expires_at, granted_by, reason, revoked_at
		FROM agent_sudo_grants
		WHERE revoked_at = '' AND expires_at > ?
		ORDER BY conv_id ASC, expires_at ASC`, cutoff)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*SudoGrant
	for rows.Next() {
		g, err := scanSudoGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeSudoGrant marks one grant as revoked-now. Returns the number
// of rows updated (0 when the id doesn't exist or was already
// revoked).
func RevokeSudoGrant(id int64) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now().Format(time.RFC3339Nano)
	res, err := d.Exec(`UPDATE agent_sudo_grants SET revoked_at = ?
		WHERE id = ? AND revoked_at = ''`, now, id)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RevokeSudoGrantsByConv stamps revoked_at on every still-active row
// for convID. Idempotent — already-revoked rows are skipped. Returns
// the number of newly-revoked rows.
func RevokeSudoGrantsByConv(convID string) (int64, error) {
	if convID == "" {
		return 0, errors.New("RevokeSudoGrantsByConv: conv_id required")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now().Format(time.RFC3339Nano)
	res, err := d.Exec(`UPDATE agent_sudo_grants SET revoked_at = ?
		WHERE conv_id = ? AND revoked_at = ''`, now, convID)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// RevokeAllActiveSudoGrants is the nuclear option — marks every
// still-active row revoked-now. Returns the row count for the human
// to verify before they nuke prod.
func RevokeAllActiveSudoGrants() (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now().Format(time.RFC3339Nano)
	res, err := d.Exec(`UPDATE agent_sudo_grants SET revoked_at = ?
		WHERE revoked_at = ''`, now)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanSudoGrant(s rowScanner) (*SudoGrant, error) {
	var g SudoGrant
	var grantedAt, expiresAt, revokedAt string
	if err := s.Scan(&g.ID, &g.ConvID, &g.Slug,
		&grantedAt, &expiresAt, &g.GrantedBy, &g.Reason, &revokedAt); err != nil {
		return nil, err
	}
	g.GrantedAt = parseTimeOrZero(grantedAt)
	g.ExpiresAt = parseTimeOrZero(expiresAt)
	g.RevokedAt = parseTimeOrZero(revokedAt)
	return &g, nil
}

// PurgeExpiredSudoGrants hard-deletes rows whose expires_at is older
// than `olderThan`. Returns the row count. Optional housekeeping for
// long-running daemons; correctness doesn't depend on it because the
// active-grants probe filters by expires_at on every check.
func PurgeExpiredSudoGrants(olderThan time.Time) (int64, error) {
	if olderThan.IsZero() {
		return 0, fmt.Errorf("PurgeExpiredSudoGrants: olderThan required")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	cutoff := olderThan.Format(time.RFC3339Nano)
	res, err := d.Exec(`DELETE FROM agent_sudo_grants WHERE expires_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}
