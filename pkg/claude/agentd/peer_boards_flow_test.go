package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A restricted peer reaches this node's collab boards through the board's
// group, by the group siblings of the global board slugs: it lists and reads
// with groups.resources.read, and edits, retitles, creates and moves with
// groups.resources.write there. A board in no group, or in a group it holds
// nothing in, is no peer's; deleting one, its snapshot, live view, events and
// undo stay this node's own dashboard's.
func TestPeerBoardsWithinTheirGroups(t *testing.T) {
	h := newPeerProcessHarness(t)
	sec := h.f.HaveGroup("secret")
	other := h.f.HaveGroup("other")
	board := func(id string, groupID int64) {
		_, err := db.CreateCollabBoard(id, id, "operator", 500)
		require.NoError(t, err)
		if groupID > 0 {
			require.NoError(t, db.SetCollabBoardGroup(id, groupID))
		}
	}
	board("plan", h.group.ID)
	board("hidden", sec.ID)
	board("loose", 0)
	list := func() []string {
		t.Helper()
		code, body := h.call("GET", "/api/boards", nil)
		require.Equal(t, 200, code, body)
		var got struct {
			Boards []struct{ ID string } `json:"boards"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &got))
		var ids []string
		for _, b := range got.Boards {
			ids = append(ids, b.ID)
		}
		return ids
	}
	edit := map[string]any{"elements": []map[string]any{rect("card", 1, 1, 0)}}

	code, body := h.call("GET", "/api/boards", nil)
	require.Equal(t, 403, code, "no grant: %s", body)
	h.grant(t, agentd.PermGroupsResourcesRead, "group=crew")
	require.Equal(t, []string{"plan"}, list(), "only a board in a group the peer reads")
	code, body = h.call("GET", "/api/boards/plan", nil)
	require.Equal(t, 200, code, body)
	for _, id := range []string{"hidden", "loose", "nope"} {
		code, body = h.call("GET", "/api/boards/"+id, nil)
		require.Equal(t, 404, code, "%s: %s", id, body)
	}
	code, body = h.call("PATCH", "/api/boards/plan", edit)
	require.Equal(t, 403, code, "reading is not editing: %s", body)
	code, body = h.call("POST", "/api/boards", map[string]any{"title": "new", "group": "crew"})
	require.Equal(t, 403, code, body)

	h.grant(t, agentd.PermGroupsResourcesWrite, "group=crew")
	code, body = h.call("PATCH", "/api/boards/plan", edit)
	require.Equal(t, 200, code, body)
	elems, err := db.ListCollabBoardElements("plan", -1)
	require.NoError(t, err)
	require.Len(t, elems, 1)
	b, err := db.GetCollabBoard("plan")
	require.NoError(t, err)
	require.Equal(t, "operator@"+h.peer.id.ID(), b.UpdatedBy, "authored as the peer's operator")
	code, body = h.call("PATCH", "/api/boards/hidden", edit)
	require.Equal(t, 404, code, body)
	code, body = h.call("POST", "/api/boards/plan/title", map[string]any{"title": "Plan"})
	require.Equal(t, 200, code, body)

	// Created only in a group it writes; moved only between two such groups,
	// never out of every group.
	for _, g := range []string{"", "secret"} {
		code, body = h.call("POST", "/api/boards", map[string]any{"title": "new", "group": g})
		require.Equal(t, 403, code, "%q: %s", g, body)
	}
	code, body = h.call("POST", "/api/boards", map[string]any{"title": "notes", "group": "crew"})
	require.Equal(t, 201, code, body)
	for _, g := range []string{"", "other"} {
		code, body = h.call("POST", "/api/boards/notes/group", map[string]any{"group": g})
		require.Equal(t, 403, code, "%q: %s", g, body)
	}
	h.grant(t, agentd.PermGroupsResourcesWrite, "group=other")
	code, body = h.call("POST", "/api/boards/notes/group", map[string]any{"group": "other"})
	require.Equal(t, 200, code, body)
	moved, err := db.GetCollabBoard("notes")
	require.NoError(t, err)
	require.Equal(t, other.ID, moved.GroupID)
	require.ElementsMatch(t, []string{"plan", "notes"}, list(), "write implies read, as it does locally")

	for _, req := range []struct{ method, path string }{
		{"DELETE", "/api/boards/plan"}, {"GET", "/api/boards/plan/snapshot"}, {"POST", "/api/boards/plan/undo"},
		{"POST", "/api/boards/plan/events"}, {"POST", "/api/boards/plan/view"}, {"POST", "/api/boards/plan/participants"},
	} {
		code, body = h.call(req.method, req.path, map[string]any{})
		require.Equal(t, 403, code, "%s %s stays local: %s", req.method, req.path, body)
	}
	_, err = db.GetCollabBoard("plan")
	require.NoError(t, err, "not deleted")

	h.trustUnrestricted(t)
	require.ElementsMatch(t, []string{"plan", "hidden", "loose", "notes"}, list(), "unrestricted trust lists as this node's operator does")
}
