package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// JOH-283 (S-283): the by_agent / anchor_agent_id companions are already
// dual-written; these tests pin that the READ paths now SURFACE them on the
// row structs (previously the queries/structs selected only the conv form).
// agent_id is the durable actor; the conv form stays as the snapshot.

// TestSurfaceByAgent_GroupLinks: ListAgentGroupLinks / GetAgentGroupLinkByID /
// ListAllAgentGroupLinks return the by_agent companion of the link creator.
func TestSurfaceByAgent_GroupLinks(t *testing.T) {
	setupTestDB(t)
	creator, _, err := EnsureAgentForConv("creatorConv", "spawn")
	require.NoError(t, err)

	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")
	id, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "creatorConv")
	require.NoError(t, err)

	got, err := GetAgentGroupLinkByID(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "creatorConv", got.ByConv, "conv snapshot preserved")
	assert.Equal(t, creator, got.ByAgent, "GetAgentGroupLinkByID surfaces by_agent")

	out, err := ListAgentGroupLinks(a, LinkOut)
	require.NoError(t, err)
	require.Len(t, out, 1)
	assert.Equal(t, creator, out[0].ByAgent, "ListAgentGroupLinks surfaces by_agent")

	all, err := ListAllAgentGroupLinks()
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, creator, all[0].ByAgent, "ListAllAgentGroupLinks surfaces by_agent")
}

// TestSurfaceByAgent_GroupLinks_HumanCreatorEmpty: a human/un-enrolled creator
// (empty byConv, or a conv with no agent) leaves ByAgent empty — only ByConv
// is meaningful then.
func TestSurfaceByAgent_GroupLinks_HumanCreatorEmpty(t *testing.T) {
	setupTestDB(t)
	a, _ := CreateAgentGroup("alpha", "")
	b, _ := CreateAgentGroup("beta", "")
	id, err := InsertAgentGroupLink(a, b, LinkModeMembersToMembers, "")
	require.NoError(t, err)

	got, err := GetAgentGroupLinkByID(id)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "", got.ByAgent, "human creator leaves by_agent empty")
}

// TestSurfaceByAgent_TransferLog: ListTransferLog returns the by_agent
// companion of the export/import caller.
func TestSurfaceByAgent_TransferLog(t *testing.T) {
	setupTestDB(t)
	caller, _, err := EnsureAgentForConv("callerConv", "spawn")
	require.NoError(t, err)

	_, err = InsertTransferLog(TransferLogEntry{
		Kind: TransferKindExport, FormatVersion: 2,
		SourceGroup: "g", ResultGroup: "g", ByConv: "callerConv",
	})
	require.NoError(t, err)

	entries, err := ListTransferLog(0)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "callerConv", entries[0].ByConv, "conv snapshot preserved")
	assert.Equal(t, caller, entries[0].ByAgent, "ListTransferLog surfaces by_agent")
}

// TestSurfaceByAgent_HeadAlias: GetHeadAlias / ListHeadAliases return both the
// by_agent (who set it) and anchor_agent_id (the anchored actor) companions.
// Surfacing anchor_agent_id must NOT change anchor resolution — Head still
// derives from AnchorConvID via the succession chain (KEEP-2).
func TestSurfaceByAgent_HeadAlias(t *testing.T) {
	setupTestDB(t)
	anchorAgent, _, err := EnsureAgentForConv("anchorConv", "spawn")
	require.NoError(t, err)
	setter, _, err := EnsureAgentForConv("setterConv", "spawn")
	require.NoError(t, err)

	require.NoError(t, SetHeadAlias("myhandle", "anchorConv", "setterConv"))

	got, err := GetHeadAlias("myhandle")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "anchorConv", got.AnchorConvID, "anchor conv snapshot preserved")
	assert.Equal(t, "setterConv", got.ByConv, "by conv snapshot preserved")
	assert.Equal(t, anchorAgent, got.AnchorAgentID, "GetHeadAlias surfaces anchor_agent_id")
	assert.Equal(t, setter, got.ByAgent, "GetHeadAlias surfaces by_agent")

	list, err := ListHeadAliases()
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, anchorAgent, list[0].AnchorAgentID, "ListHeadAliases surfaces anchor_agent_id")
	assert.Equal(t, setter, list[0].ByAgent, "ListHeadAliases surfaces by_agent")
}

// TestSurfaceByAgent_GroupRenames: ListAgentGroupRenames returns the by_agent
// companion of the renamer.
func TestSurfaceByAgent_GroupRenames(t *testing.T) {
	setupTestDB(t)
	renamer, _, err := EnsureAgentForConv("renamerConv", "spawn")
	require.NoError(t, err)

	g, err := CreateAgentGroup("g1", "")
	require.NoError(t, err)
	_, err = RenameAgentGroup("g1", "g2", "renamerConv")
	require.NoError(t, err)

	renames, err := ListAgentGroupRenames(g)
	require.NoError(t, err)
	require.NotEmpty(t, renames)
	assert.Equal(t, "renamerConv", renames[0].ByConv, "conv snapshot preserved")
	assert.Equal(t, renamer, renames[0].ByAgent, "ListAgentGroupRenames surfaces by_agent")
}
