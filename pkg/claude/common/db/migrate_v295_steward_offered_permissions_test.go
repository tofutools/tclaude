package db

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// A Steward already in place gets an empty record to catch up from. Only the
// scribe Steward scope counts, not a look-alike group or another member.
func TestMigrateV295OpensARecordForExistingStewards(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `DROP TABLE steward_offered_permissions`)
	steward, err := CreateAgentGroup("steward", ScribeGroupDescr)
	require.NoError(t, err)
	other, err := CreateAgentGroup("not-steward", ScribeGroupDescr)
	require.NoError(t, err)
	join := `INSERT INTO agent_group_members(group_id, agent_id, role, descr, joined_at) VALUES(?, ?, '', ?, 1)`
	mustExec(t, d, join, steward, "agt_real", "Reusable scribe scope: steward/")
	mustExec(t, d, join, steward, "agt_member", "something else")
	mustExec(t, d, join, other, "agt_lookalike", "Reusable scribe scope: steward/")
	mustExec(t, d, `UPDATE schema_version SET version = 294`)

	require.NoError(t, migrateV294toV295(d))

	offered, err := ListStewardOfferedPermissions("agt_real")
	require.NoError(t, err)
	require.Equal(t, map[string]bool{StewardRecordOpened: true}, offered)
	for _, agentID := range []string{"agt_member", "agt_lookalike"} {
		offered, err = ListStewardOfferedPermissions(agentID)
		require.NoError(t, err)
		require.Empty(t, offered)
	}
}
