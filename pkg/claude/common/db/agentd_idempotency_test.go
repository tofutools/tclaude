package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestClaimAgentdRequestPrunesExpiredRecords(t *testing.T) {
	setupTestDB(t)
	now := time.Date(2026, 7, 14, 12, 0, 0, 0, time.UTC)
	d, err := Open()
	require.NoError(t, err)

	_, err = d.Exec(`INSERT INTO agentd_idempotency
		(request_key, fingerprint, owner_id, state, status, headers_json, response_body, created_at, expires_at)
		VALUES
			('expired-pending', 'expired-pending-fingerprint', 'daemon-a', 'pending', 0, '', NULL, ?, ?),
			('expired-completed', 'expired-completed-fingerprint', 'daemon-a', 'completed', 201, '{}', 'done', ?, ?),
			('live-pending', 'live-pending-fingerprint', 'daemon-a', 'pending', 0, '', NULL, ?, ?),
			('live-completed', 'live-completed-fingerprint', 'daemon-a', 'completed', 201, '{}', 'done', ?, ?)`,
		now.Add(-time.Hour).Unix(), now.Unix(),
		now.Add(-time.Hour).Unix(), now.Add(-time.Second).Unix(),
		now.Add(-time.Hour).Unix(), now.Add(time.Second).Unix(),
		now.Add(-time.Hour).Unix(), now.Add(time.Second).Unix())
	require.NoError(t, err)

	_, claimed, err := ClaimAgentdRequest(
		"new", "new-fingerprint", "daemon-b", now, now.Add(30*time.Minute))
	require.NoError(t, err)
	require.True(t, claimed)

	rows, err := d.Query(`SELECT request_key FROM agentd_idempotency ORDER BY request_key`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	var keys []string
	for rows.Next() {
		var key string
		require.NoError(t, rows.Scan(&key))
		keys = append(keys, key)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"live-completed", "live-pending", "new"}, keys)
}
