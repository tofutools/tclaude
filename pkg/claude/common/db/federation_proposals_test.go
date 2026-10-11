package db

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func TestFederationGroupIdentitySurvivesRenameAndNeverReusesDeletedGroup(t *testing.T) {
	setupTestDB(t)
	g, err := CreateAgentGroup("original", "")
	require.NoError(t, err)
	id, err := FederationGroupIdentity(g)
	require.NoError(t, err)
	require.True(t, proto.ValidGroupIdentity(id))
	_, err = RenameAgentGroup("original", "renamed", "")
	require.NoError(t, err)
	renamed, err := FederationGroupByIdentity(id)
	require.NoError(t, err)
	require.Equal(t, "renamed", renamed.Name)
	require.NoError(t, DeleteAgentGroup("renamed"))
	deleted, err := FederationGroupByIdentity(id)
	require.NoError(t, err)
	require.Nil(t, deleted)
	replacement, err := CreateAgentGroup("renamed", "")
	require.NoError(t, err)
	newID, err := FederationGroupIdentity(replacement)
	require.NoError(t, err)
	require.NotEqual(t, id, newID)
}

func TestMigrateV253toV254BackfillsGroupIdentitiesAndCatalogReadsDoNotWrite(t *testing.T) {
	setupTestDB(t)
	existing, err := CreateAgentGroup("before-upgrade", "")
	require.NoError(t, err)
	d, err := Open()
	require.NoError(t, err)
	// Restore the exact pre-proposal schema while retaining an existing group.
	_, err = d.Exec(`DROP TRIGGER agent_groups_federation_identity;
 DROP TABLE federation_link_mail; DROP TABLE federation_proposal_events;
 DROP TABLE federation_proposal_item_decisions; DROP TABLE federation_group_links;
 DROP TABLE federation_peer_proposals; DROP TABLE federation_group_identities;
 UPDATE schema_version SET version=253;`)
	require.NoError(t, err)
	require.NoError(t, migrateV253toV254(d))
	before, err := FederationGroupIdentity(existing)
	require.NoError(t, err)
	require.True(t, proto.ValidGroupIdentity(before))
	newGroup, err := CreateAgentGroup("after-upgrade", "")
	require.NoError(t, err)
	after, err := FederationGroupIdentity(newGroup)
	require.NoError(t, err)
	require.True(t, proto.ValidGroupIdentity(after))
	require.NotEqual(t, before, after)
	generation := StatusSnapshotGeneration()
	for range 20 {
		id, err := FederationGroupIdentity(existing)
		require.NoError(t, err)
		require.Equal(t, before, id)
		require.Equal(t, generation, StatusSnapshotGeneration(), "identity lookup must not invalidate a shared gather")
	}
}
