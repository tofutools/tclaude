package db

import (
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"testing"
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
