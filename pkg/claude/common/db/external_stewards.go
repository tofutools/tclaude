package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ExternalStewardConvPrefix starts the synthetic conv-id of an external
// steward. No harness writes such a conversation: the id exists because every
// permission gate, the inbox and the audit trail are keyed by conv.
const ExternalStewardConvPrefix = "ext-"

// ExternalStewardVia labels the agents row of an external steward.
const ExternalStewardVia = "external-steward"

// ErrExternalStewardNameTaken reports a name already used by a principal.
var ErrExternalStewardNameTaken = errors.New("external steward name is taken")

// IsExternalStewardConv reports whether convID belongs to an external steward.
// It only ever narrows what a conv may do, so a prefix is enough here: the
// authentication decision reads the credential rows, never this.
func IsExternalStewardConv(convID string) bool {
	return strings.HasPrefix(convID, ExternalStewardConvPrefix)
}

// ExternalStewardCredential is one credential, without its hash.
type ExternalStewardCredential struct {
	TokenID    string
	AgentID    string
	CreatedAt  time.Time
	CreatedBy  string
	ExpiresAt  time.Time
	RevokedAt  time.Time
	LastUsedAt time.Time
}

// Live reports whether the credential can still authenticate at now.
func (c ExternalStewardCredential) Live(now time.Time) bool {
	return c.RevokedAt.IsZero() && now.Before(c.ExpiresAt)
}

// ExternalSteward is one principal and its credentials, newest first.
type ExternalSteward struct {
	AgentID     string
	ConvID      string
	Name        string
	CreatedAt   time.Time
	CreatedBy   string
	Retired     bool
	Credentials []ExternalStewardCredential
}

// ExternalStewardAuth is what authenticating a credential needs.
type ExternalStewardAuth struct {
	ExternalStewardCredential
	Hash         []byte
	ConvID       string
	AgentRetired bool
}

// CreateExternalSteward adds the principal and its first credential together,
// so a failure leaves neither. grants are written as ordinary standing grants.
func CreateExternalSteward(name, convID, by, tokenID string, tokenHash []byte, expiresAt time.Time, grants []string, granter string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	tx, err := d.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var taken int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM external_stewards WHERE name = ?`, name).Scan(&taken); err != nil {
		return "", err
	}
	if taken > 0 {
		return "", ErrExternalStewardNameTaken
	}
	now := time.Now()
	agentID := newAgentID()
	if err := insertAgentTx(tx, agentID, convID, ExternalStewardVia, now); err != nil {
		return "", err
	}
	if err := linkConvTx(tx, convID, agentID, ConvRoleHead, ExternalStewardVia, now); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE agents SET pending_name = ? WHERE agent_id = ?`, name, agentID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO external_stewards (agent_id, name, created_at, created_by) VALUES (?, ?, ?, ?)`,
		agentID, name, dbTime(now), by); err != nil {
		return "", err
	}
	if err := insertExternalStewardCredentialTx(tx, agentID, by, tokenID, tokenHash, now, expiresAt); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	for _, slug := range grants {
		if err := SetAgentPermissionOverride(convID, slug, PermEffectGrant, granter); err != nil {
			return agentID, fmt.Errorf("grant %s: %w", slug, err)
		}
	}
	return agentID, nil
}

func insertExternalStewardCredentialTx(q dbExecQuerier, agentID, by, tokenID string, tokenHash []byte, now, expiresAt time.Time) error {
	_, err := q.Exec(`INSERT INTO external_steward_credentials
		(token_id, agent_id, token_hash, created_at, created_by, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		tokenID, agentID, tokenHash, dbTime(now), by, dbTime(expiresAt))
	return err
}

// AddExternalStewardCredential renews a principal with a new credential. With
// revokeOthers the old ones end in the same transaction.
func AddExternalStewardCredential(agentID, by, tokenID string, tokenHash []byte, expiresAt time.Time, revokeOthers bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	if revokeOthers {
		if _, err := tx.Exec(`UPDATE external_steward_credentials SET revoked_at = ?
			WHERE agent_id = ? AND revoked_at IS NULL`, dbTime(now), agentID); err != nil {
			return err
		}
	}
	if err := insertExternalStewardCredentialTx(tx, agentID, by, tokenID, tokenHash, now, expiresAt); err != nil {
		return err
	}
	return tx.Commit()
}

// RevokeExternalStewardCredentials ends the principal's credentials, or just
// tokenID when that is set, and returns how many ended.
func RevokeExternalStewardCredentials(agentID, tokenID string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	query := `UPDATE external_steward_credentials SET revoked_at = ? WHERE agent_id = ? AND revoked_at IS NULL`
	args := []any{dbTime(time.Now()), agentID}
	if tokenID != "" {
		query += ` AND token_id = ?`
		args = append(args, tokenID)
	}
	res, err := d.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LookupExternalStewardCredential returns nil when tokenID names no credential.
func LookupExternalStewardCredential(tokenID string) (*ExternalStewardAuth, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var auth ExternalStewardAuth
	var created, expires int64
	var revoked, used sql.NullInt64
	err = d.QueryRow(`SELECT c.token_id, c.agent_id, c.token_hash, c.created_at, c.created_by, c.expires_at,
			c.revoked_at, c.last_used_at, a.current_conv_id, a.retired_at IS NOT NULL
		FROM external_steward_credentials c JOIN agents a ON a.agent_id = c.agent_id
		WHERE c.token_id = ?`, tokenID).Scan(&auth.TokenID, &auth.AgentID, &auth.Hash, &created, &auth.CreatedBy,
		&expires, &revoked, &used, &auth.ConvID, &auth.AgentRetired)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	auth.CreatedAt, auth.ExpiresAt = time.Unix(0, created), time.Unix(0, expires)
	auth.RevokedAt, auth.LastUsedAt = externalStewardTime(revoked), externalStewardTime(used)
	return &auth, nil
}

func externalStewardTime(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.Unix(0, v.Int64)
}

// TouchExternalStewardCredential records use, at most once per interval.
func TouchExternalStewardCredential(tokenID string, now time.Time, interval time.Duration) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE external_steward_credentials SET last_used_at = ?
		WHERE token_id = ? AND (last_used_at IS NULL OR last_used_at <= ?)`,
		dbTime(now), tokenID, now.Add(-interval).UnixNano())
	return err
}

// GetExternalStewardByName returns nil when no principal has the name.
func GetExternalStewardByName(name string) (*ExternalSteward, error) {
	all, err := ListExternalStewards()
	if err != nil {
		return nil, err
	}
	for i := range all {
		if all[i].Name == name {
			return &all[i], nil
		}
	}
	return nil, nil
}

// ListExternalStewards returns every principal, oldest first.
func ListExternalStewards() ([]ExternalSteward, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT s.agent_id, a.current_conv_id, s.name, s.created_at, s.created_by, a.retired_at IS NOT NULL
		FROM external_stewards s JOIN agents a ON a.agent_id = s.agent_id ORDER BY s.created_at, s.name`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ExternalSteward
	byAgent := map[string]int{}
	for rows.Next() {
		var s ExternalSteward
		var created int64
		if err := rows.Scan(&s.AgentID, &s.ConvID, &s.Name, &created, &s.CreatedBy, &s.Retired); err != nil {
			return nil, err
		}
		s.CreatedAt = time.Unix(0, created)
		byAgent[s.AgentID] = len(out)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	credRows, err := d.Query(`SELECT token_id, agent_id, created_at, created_by, expires_at, revoked_at, last_used_at
		FROM external_steward_credentials ORDER BY created_at DESC, token_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = credRows.Close() }()
	for credRows.Next() {
		var c ExternalStewardCredential
		var created, expires int64
		var revoked, used sql.NullInt64
		if err := credRows.Scan(&c.TokenID, &c.AgentID, &created, &c.CreatedBy, &expires, &revoked, &used); err != nil {
			return nil, err
		}
		c.CreatedAt, c.ExpiresAt = time.Unix(0, created), time.Unix(0, expires)
		c.RevokedAt, c.LastUsedAt = externalStewardTime(revoked), externalStewardTime(used)
		if i, ok := byAgent[c.AgentID]; ok {
			out[i].Credentials = append(out[i].Credentials, c)
		}
	}
	return out, credRows.Err()
}
