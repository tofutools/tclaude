package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A saved page keeps its newest mark batches up to the cap, keeps its mark
// sequence when the marks are cleared, and a page with no saved row keeps
// nothing.
func TestGroupPageCollabKeepsNewestMarks(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	require.NoError(t, SaveOpenGroupPage(GroupPage{ID: "pg_a", GroupID: gid, Owner: "agt_a", Name: "app", Kind: "port"}))
	for seq := int64(1); seq <= 5; seq++ {
		require.NoError(t, AddGroupPageMarks("pg_a", seq, fmt.Sprintf(`{"seq":%d}`, seq), 3))
	}
	require.NoError(t, SaveGroupPagePatches("pg_a", 2, 7, `[{"n":7}]`))
	c, err := LoadGroupPageCollab("pg_a")
	require.NoError(t, err)
	assert.Equal(t, &GroupPageCollab{MarkSeq: 5, PatchSeq: 7, Batches: []string{`{"seq":3}`, `{"seq":4}`, `{"seq":5}`}, Patches: `[{"n":7}]`}, c)

	require.NoError(t, ClearGroupPageMarks("pg_a"))
	c, err = LoadGroupPageCollab("pg_a")
	require.NoError(t, err)
	assert.Empty(t, c.Batches)
	assert.Equal(t, int64(5), c.MarkSeq, "the sequence stays")

	require.NoError(t, ClearGroupPageCollab("pg_a"))
	c, err = LoadGroupPageCollab("pg_a")
	require.NoError(t, err)
	assert.Nil(t, c, "a page shown from another source starts again")

	require.NoError(t, AddGroupPageMarks("pg_private", 1, `{}`, 3))
	require.NoError(t, SaveGroupPagePatches("pg_private", 1, 1, `[]`))
	c, err = LoadGroupPageCollab("pg_private")
	require.NoError(t, err)
	assert.Nil(t, c, "a page with no saved row keeps nothing")
}
