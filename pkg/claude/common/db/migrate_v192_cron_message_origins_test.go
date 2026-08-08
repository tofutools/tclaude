package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV191ToV192CreatesCronMessageOrigins(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	const target = "legacy-cron-buffer-target"
	_, _, err = EnsureAgentForConv(target, "test")
	require.NoError(t, err)
	jobID, err := InsertAgentCronJob(&AgentCronJob{
		Name: "legacy-job", OwnerConv: target,
		TargetKind: CronTargetConv, TargetConv: target,
		IntervalSeconds: 600, Subject: "status", Body: "report status", Enabled: true,
	})
	require.NoError(t, err)
	oldID, err := InsertAgentMessage(&AgentMessage{
		FromConv: target, ToConv: target,
		Subject: "[cron:legacy-job] status", Body: "report status",
	})
	require.NoError(t, err)
	claimedID, err := InsertAgentMessage(&AgentMessage{
		FromConv: target, ToConv: target,
		Subject: "[cron:legacy-job] status", Body: "report status",
	})
	require.NoError(t, err)
	_, claimed, err := ClaimAgentMessageNudge(claimedID, time.Now())
	require.NoError(t, err)
	require.True(t, claimed)
	latestID, err := InsertAgentMessage(&AgentMessage{
		FromConv: target, ToConv: target,
		Subject: "[cron:legacy-job] status", Body: "report status",
	})
	require.NoError(t, err)
	unrelatedID, err := InsertAgentMessage(&AgentMessage{
		FromConv: target, ToConv: target,
		Subject: "not cron", Body: "report status",
	})
	require.NoError(t, err)

	_, err = d.Exec(`DROP TABLE agent_cron_messages; UPDATE schema_version SET version = 191`)
	require.NoError(t, err)
	require.NoError(t, migrateV191toV192(d))
	require.NoError(t, migrateV191toV192(d), "migration is idempotent")
	assert.Equal(t, 192, schemaVersion(d))

	var have int
	require.NoError(t, d.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'agent_cron_messages'`,
	).Scan(&have))
	assert.Equal(t, 1, have)

	old, err := GetAgentMessage(oldID)
	require.NoError(t, err)
	assert.Nil(t, old, "upgrade removes the older legacy buffered tick")
	for _, id := range []int64{claimedID, latestID, unrelatedID} {
		message, getErr := GetAgentMessage(id)
		require.NoError(t, getErr)
		assert.NotNil(t, message)
	}
	origin, err := AgentMessageCronJobID(latestID)
	require.NoError(t, err)
	assert.Equal(t, jobID, origin)
	claimedOrigin, err := AgentMessageCronJobID(claimedID)
	require.NoError(t, err)
	assert.Equal(t, jobID, claimedOrigin, "migration tags a stale claimed tick before startup releases it")
	released, err := ReleaseAllAgentMessageNudgeClaims()
	require.NoError(t, err)
	assert.EqualValues(t, 1, released)
	claimedMessage, err := GetAgentMessage(claimedID)
	require.NoError(t, err)
	assert.Nil(t, claimedMessage, "startup claim release removes the now-unclaimed superseded tick")
}
