package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMigrateV99toV100_FreshSchema builds a fresh DB through the full migrate()
// chain and asserts access_requests exists. The literal currentVersion
// tripwire moved forward to the v101 test.
func TestMigrateV99toV100_FreshSchema(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	require.Equal(t, currentVersion, ver, "fresh DB migrates to currentVersion")

	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'access_requests'`).Scan(&have))
	assert.Equal(t, 1, have, "fresh schema has access_requests")
}

func TestMigrateV99toV100_AddsAccessRequests(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err, "Open")

	mustExec(t, d, `DROP TABLE access_requests`)
	mustExec(t, d, `UPDATE schema_version SET version = 99`)

	require.NoError(t, migrateV99toV100(d), "v99→v100")

	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'access_requests'`).Scan(&have))
	assert.Equal(t, 1, have, "access_requests table added")

	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 100, ver, "version advanced")

	require.NoError(t, migrateV99toV100(d), "v99→v100 re-run is a clean no-op")
}

func TestAccessRequests_RoundTripHandledHistory(t *testing.T) {
	setupTestDB(t)
	created := mustTime(t, "2026-07-05T10:00:00Z")
	decided := mustTime(t, "2026-07-05T10:00:05Z")

	require.NoError(t, UpsertAccessRequest(&AccessRequest{
		ID:              "ar-1",
		Perm:            "human.notify",
		ConvID:          "conv-a",
		AgentID:         "agt_captured_caller",
		ConvTitle:       "tester",
		Path:            "POST /v1/notify-human",
		BodyPreview:     `{"subject":"hi"}`,
		TargetGroup:     "tclaude",
		TargetConvID:    "conv-b",
		TargetConvTitle: "target",
		AutoGrantable:   true,
		Status:          "approved",
		CreatedAt:       created,
		DecidedAt:       decided,
	}))
	// A later lifecycle write from a legacy caller must not erase the stable
	// identity already captured on the pending/request write.
	require.NoError(t, UpsertAccessRequest(&AccessRequest{
		ID:            "ar-1",
		Perm:          "human.notify",
		ConvID:        "conv-a",
		ConvTitle:     "tester",
		AutoGrantable: true,
		Status:        "approved",
		CreatedAt:     created,
		DecidedAt:     decided,
	}))
	require.NoError(t, UpsertAccessRequest(&AccessRequest{
		ID:        "ar-pending",
		Perm:      "self.rename",
		ConvID:    "conv-p",
		Status:    AccessRequestStatusPending,
		CreatedAt: created,
	}))

	rows, err := ListRecentHandledAccessRequests(10)
	require.NoError(t, err)
	require.Len(t, rows, 1, "pending rows are not returned as handled history")
	got := rows[0]
	assert.Equal(t, "ar-1", got.ID)
	assert.Equal(t, "human.notify", got.Perm)
	assert.Equal(t, "conv-a", got.ConvID)
	assert.Equal(t, "agt_captured_caller", got.AgentID,
		"explicitly captured identity must not be re-resolved from mutable conv metadata")
	assert.Equal(t, "tester", got.ConvTitle)
	assert.Equal(t, "approved", got.Status)
	assert.True(t, got.AutoGrantable)
	assert.Equal(t, created, got.CreatedAt)
	assert.Equal(t, decided, got.DecidedAt)
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	require.NoError(t, err)
	return v
}
