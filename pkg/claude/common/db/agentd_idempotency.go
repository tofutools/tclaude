package db

import (
	"fmt"
	"time"
)

const (
	IdempotencyPending   = "pending"
	IdempotencyCompleted = "completed"
)

// AgentdIdempotencyRecord is one durable mutating-request outcome.
type AgentdIdempotencyRecord struct {
	RequestKey   string
	Fingerprint  string
	OwnerID      string
	State        string
	Status       int
	HeadersJSON  string
	ResponseBody []byte
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// ClaimAgentdRequest reserves requestKey for one daemon instance. Claimed is
// true only for the caller that inserted the pending row; all other callers
// receive the existing record and must replay, wait, or report ambiguity.
func ClaimAgentdRequest(requestKey, fingerprint, ownerID string, now, expiresAt time.Time) (record AgentdIdempotencyRecord, claimed bool, err error) {
	if requestKey == "" || fingerprint == "" || ownerID == "" {
		return record, false, fmt.Errorf("idempotency key, fingerprint, and owner are required")
	}
	d, err := Open()
	if err != nil {
		return record, false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return record, false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM agentd_idempotency WHERE expires_at <= ?`, now.Unix()); err != nil {
		return record, false, err
	}
	res, err := tx.Exec(`INSERT OR IGNORE INTO agentd_idempotency
		(request_key, fingerprint, owner_id, state, created_at, expires_at)
		VALUES (?, ?, ?, 'pending', ?, ?)`,
		requestKey, fingerprint, ownerID, now.Unix(), expiresAt.Unix())
	if err != nil {
		return record, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return record, false, err
	}
	record, err = scanAgentdIdempotency(tx.QueryRow(`SELECT request_key, fingerprint, owner_id,
		state, status, headers_json, response_body, created_at, expires_at
		FROM agentd_idempotency WHERE request_key = ?`, requestKey))
	if err != nil {
		return record, false, err
	}
	if err := tx.Commit(); err != nil {
		return record, false, err
	}
	return record, n == 1, nil
}

// GetAgentdRequest returns a reservation or completed response by key.
func GetAgentdRequest(requestKey string) (AgentdIdempotencyRecord, error) {
	d, err := Open()
	if err != nil {
		return AgentdIdempotencyRecord{}, err
	}
	return scanAgentdIdempotency(d.QueryRow(`SELECT request_key, fingerprint, owner_id,
		state, status, headers_json, response_body, created_at, expires_at
		FROM agentd_idempotency WHERE request_key = ?`, requestKey))
}

// CompleteAgentdRequest atomically turns this daemon instance's pending claim
// into a replayable response.
func CompleteAgentdRequest(requestKey, ownerID string, status int, headersJSON string, body []byte) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`UPDATE agentd_idempotency
		SET state = 'completed', status = ?, headers_json = ?, response_body = ?
		WHERE request_key = ? AND owner_id = ? AND state = 'pending'`,
		status, headersJSON, body, requestKey, ownerID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("idempotency request %q is no longer owned by this daemon", requestKey)
	}
	return nil
}

func scanAgentdIdempotency(row rowScanner) (AgentdIdempotencyRecord, error) {
	var record AgentdIdempotencyRecord
	var createdUnix, expiresUnix int64
	if err := row.Scan(&record.RequestKey, &record.Fingerprint, &record.OwnerID,
		&record.State, &record.Status, &record.HeadersJSON, &record.ResponseBody,
		&createdUnix, &expiresUnix); err != nil {
		return AgentdIdempotencyRecord{}, err
	}
	record.CreatedAt = time.Unix(createdUnix, 0).UTC()
	record.ExpiresAt = time.Unix(expiresUnix, 0).UTC()
	return record, nil
}
