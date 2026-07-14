package db

import (
	"fmt"
	"time"
)

// DashboardSessionGrace is one restart-handoff credential. TokenHash is a
// SHA-256 digest of the HttpOnly cookie, never the replayable cookie itself.
type DashboardSessionGrace struct {
	TokenHash string
	ExpiresAt time.Time
}

// PreserveDashboardSessionGrace records a cookie digest for a bounded restart
// grace period. Expired rows are pruned in the same transaction so repeated
// clean restarts cannot grow the store without bound.
func PreserveDashboardSessionGrace(tokenHash string, expiresAt, now time.Time) error {
	if tokenHash == "" {
		return fmt.Errorf("dashboard session token hash is empty")
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
	if _, err := tx.Exec(`DELETE FROM dashboard_session_grace WHERE expires_at <= ?`, now.Unix()); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO dashboard_session_grace (token_hash, expires_at, created_at)
		VALUES (?, ?, ?)
		ON CONFLICT(token_hash) DO UPDATE SET expires_at = excluded.expires_at`,
		tokenHash, expiresAt.Unix(), now.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

// ListDashboardSessionGrace returns the unexpired restart-handoff digests and
// deletes expired rows atomically. The returned slice is safe to cache in
// memory for the daemon lifetime; callers still enforce each row's ExpiresAt.
func ListDashboardSessionGrace(now time.Time) ([]DashboardSessionGrace, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM dashboard_session_grace WHERE expires_at <= ?`, now.Unix()); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT token_hash, expires_at
		FROM dashboard_session_grace ORDER BY expires_at`)
	if err != nil {
		return nil, err
	}
	var out []DashboardSessionGrace
	for rows.Next() {
		var item DashboardSessionGrace
		var expiresUnix int64
		if err := rows.Scan(&item.TokenHash, &expiresUnix); err != nil {
			_ = rows.Close()
			return nil, err
		}
		item.ExpiresAt = time.Unix(expiresUnix, 0).UTC()
		out = append(out, item)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
