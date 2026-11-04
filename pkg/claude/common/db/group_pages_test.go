package db

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A group keeps its most recent closed pages; one the operator locked is never
// trimmed, and a save does not replace it.
func TestGroupPagesKeepLockedPages(t *testing.T) {
	setupTestDB(t)
	gid, err := CreateAgentGroup("crew", "")
	require.NoError(t, err)
	save := func(id, name string) {
		require.NoError(t, SaveOpenGroupPage(GroupPage{ID: id, GroupID: gid, Owner: "agt_a", Name: name, Kind: "port"}))
	}
	save("pg_locked", "keep")
	require.NoError(t, SetGroupPageLocked("pg_locked", true))
	require.NoError(t, CloseGroupPage("pg_locked", "closed"))
	for i := range GroupPageClosedKept + 2 {
		id := fmt.Sprintf("pg_%03d", i)
		save(id, fmt.Sprintf("p%d", i))
		require.NoError(t, CloseGroupPage(id, "closed"))
	}
	rows, err := ListGroupPages()
	require.NoError(t, err)
	assert.Len(t, rows, GroupPageClosedKept+1, "the kept closed pages, and the locked one")
	locked, err := GetGroupPage("pg_locked")
	require.NoError(t, err)
	require.NotNil(t, locked)

	// Another page saved under the locked one's name does not replace it.
	assert.Error(t, SaveOpenGroupPage(GroupPage{ID: "pg_new", GroupID: gid, Owner: "agt_a", Name: "keep", Kind: "port"}))
	locked, err = GetGroupPage("pg_locked")
	require.NoError(t, err)
	assert.NotNil(t, locked)
}
