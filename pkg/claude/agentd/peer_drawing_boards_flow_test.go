package agentd_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A restricted peer reaches this node's collab drawing boards through the drawing board's
// group, by the group siblings of the global drawing board slugs: it lists and reads
// with groups.resources.read, and edits, retitles, creates and moves with
// groups.resources.write there. A drawing board in no group, or in a group it holds
// nothing in, is no peer's; deleting one, its snapshot, live view, events and
// undo stay this node's own dashboard's.
func TestPeerDrawingBoardsWithinTheirGroups(t *testing.T) {
	h := newPeerProcessHarness(t)
	sec := h.f.HaveGroup("secret")
	other := h.f.HaveGroup("other")
	drawingBoard := func(id string, groupID int64) {
		_, err := db.CreateCollabDrawingBoard(id, id, "operator", 500)
		require.NoError(t, err)
		if groupID > 0 {
			require.NoError(t, db.SetCollabDrawingBoardGroup(id, groupID))
		}
	}
	drawingBoard("plan", h.group.ID)
	drawingBoard("hidden", sec.ID)
	drawingBoard("loose", 0)
	outsider := havePerformerAgent(t, h.f, "secret", "sec-conv", "sec", "dev", "idle")
	_, err := db.SetCollabDrawingBoardParticipant("plan", outsider, false)
	require.NoError(t, err)
	list := func() []string {
		t.Helper()
		code, body := h.call("GET", "/api/drawing-boards", nil)
		require.Equal(t, 200, code, body)
		var got struct {
			DrawingBoards []struct{ ID string } `json:"drawingBoards"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &got))
		var ids []string
		for _, b := range got.DrawingBoards {
			ids = append(ids, b.ID)
		}
		return ids
	}
	edit := map[string]any{"elements": []map[string]any{rect("card", 1, 1, 0)}}
	// The page reads its access to a group's drawing boards from the peer view's
	// per-group grants, so they must list both slugs.
	viewGrants := func() []any {
		t.Helper()
		code, body := h.call("GET", "/api/groups", nil)
		require.Equal(t, 200, code, body)
		var got struct {
			PeerView struct {
				Groups map[string][]any `json:"groups"`
			} `json:"peer_view"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &got))
		return got.PeerView.Groups["crew"]
	}

	code, body := h.call("GET", "/api/drawing-boards", nil)
	require.Equal(t, 403, code, "no grant: %s", body)
	h.grant(t, agentd.PermGroupsResourcesRead, "group=crew")
	require.Contains(t, viewGrants(), agentd.PermGroupsResourcesRead, "the page shows the group's drawing boards read-only")
	require.Equal(t, []string{"plan"}, list(), "only a drawing board in a group the peer reads")
	code, body = h.call("GET", "/api/drawing-boards/plan", nil)
	require.Equal(t, 200, code, body)
	var read map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &read))
	require.NotContains(t, body, outsider, "an invited agent of another group")
	require.NotContains(t, read, "presence", "who is on the drawing board and the operator's view stay local")
	require.NotContains(t, read, "group_members", "the roster takes groups.roster.read")
	require.JSONEq(t, `[]`, string(read["participants"]), "invited agents may be any group's")
	for _, id := range []string{"hidden", "loose", "nope"} {
		code, body = h.call("GET", "/api/drawing-boards/"+id, nil)
		require.Equal(t, 404, code, "%s: %s", id, body)
	}
	code, body = h.call("PATCH", "/api/drawing-boards/plan", edit)
	require.Equal(t, 403, code, "reading is not editing: %s", body)
	code, body = h.call("POST", "/api/drawing-boards", map[string]any{"title": "new", "group": "crew"})
	require.Equal(t, 403, code, body)

	h.grant(t, agentd.PermGroupsRosterRead, "group=crew")
	code, body = h.call("GET", "/api/drawing-boards/plan", nil)
	require.Equal(t, 200, code, body)
	require.Contains(t, body, `"group_members"`, "shown with the roster grant")

	h.grant(t, agentd.PermGroupsResourcesWrite, "group=crew")
	require.Contains(t, viewGrants(), agentd.PermGroupsResourcesWrite, "the page lets the peer edit them")
	code, body = h.call("PATCH", "/api/drawing-boards/plan", edit)
	require.Equal(t, 200, code, body)
	elems, err := db.ListCollabDrawingBoardElements("plan", -1)
	require.NoError(t, err)
	require.Len(t, elems, 1)
	b, err := db.GetCollabDrawingBoard("plan")
	require.NoError(t, err)
	require.Equal(t, "operator@"+h.peer.id.ID(), b.UpdatedBy, "authored as the peer's operator")
	code, body = h.call("PATCH", "/api/drawing-boards/hidden", edit)
	require.Equal(t, 404, code, body)
	code, body = h.call("POST", "/api/drawing-boards/plan/title", map[string]any{"title": "Plan"})
	require.Equal(t, 200, code, body)

	// Created only in a group it writes; moved only between two such groups,
	// never out of every group.
	for _, g := range []string{"", "secret"} {
		code, body = h.call("POST", "/api/drawing-boards", map[string]any{"title": "new", "group": g})
		require.Equal(t, 403, code, "%q: %s", g, body)
	}
	code, body = h.call("POST", "/api/drawing-boards", map[string]any{"id": "hidden", "title": "notes", "group": "crew"})
	require.Equal(t, 201, code, "the peer's id is not used, so it probes nothing: %s", body)
	for _, g := range []string{"", "other"} {
		code, body = h.call("POST", "/api/drawing-boards/notes/group", map[string]any{"group": g})
		require.Equal(t, 403, code, "%q: %s", g, body)
	}
	h.grant(t, agentd.PermGroupsResourcesWrite, "group=other")
	code, body = h.call("POST", "/api/drawing-boards/notes/group", map[string]any{"group": "other"})
	require.Equal(t, 200, code, body)
	moved, err := db.GetCollabDrawingBoard("notes")
	require.NoError(t, err)
	require.Equal(t, other.ID, moved.GroupID)
	require.ElementsMatch(t, []string{"plan", "notes"}, list(), "write implies read, as it does locally")

	for _, req := range []struct{ method, path string }{
		{"DELETE", "/api/drawing-boards/plan"}, {"GET", "/api/drawing-boards/plan/snapshot"}, {"POST", "/api/drawing-boards/plan/undo"},
		{"POST", "/api/drawing-boards/plan/events"}, {"POST", "/api/drawing-boards/plan/view"}, {"POST", "/api/drawing-boards/plan/participants"},
	} {
		code, body = h.call(req.method, req.path, map[string]any{})
		require.Equal(t, 403, code, "%s %s stays local: %s", req.method, req.path, body)
	}
	_, err = db.GetCollabDrawingBoard("plan")
	require.NoError(t, err, "not deleted")

	h.trustUnrestricted(t)
	require.ElementsMatch(t, []string{"plan", "hidden", "loose", "notes"}, list(), "unrestricted trust lists as this node's operator does")
	code, body = h.call("PATCH", "/api/drawing-boards/loose", map[string]any{"elements": []map[string]any{rect("dot", 1, 2, 0)}})
	require.Equal(t, 200, code, body)
	loose, err := db.GetCollabDrawingBoard("loose")
	require.NoError(t, err)
	require.Equal(t, "operator@"+h.peer.id.ID(), loose.UpdatedBy, "unrestricted edits are the peer's too, not this node's operator's")
}

// A drawing board this node's operator locked is not a remote operator's to change,
// whatever its trust, until it is unlocked; reading it still works.
func TestPeerDrawingBoardsHonorOperatorLocks(t *testing.T) {
	h := newPeerProcessHarness(t)
	_, err := db.CreateCollabDrawingBoard("plan", "plan", "operator", 500)
	require.NoError(t, err)
	require.NoError(t, db.SetCollabDrawingBoardGroup("plan", h.group.ID))
	require.NoError(t, db.SetGroupResourceLocked(db.GroupResourceDrawingBoard, "plan", true))
	code, body := h.call("PATCH", "/api/drawing-boards/plan", map[string]any{"elements": []map[string]any{}})
	require.Equal(t, 404, code, "a drawing board the peer cannot read stays unknown, locked or not: %s", body)
	h.f.HaveGroup("other")
	h.grant(t, agentd.PermGroupsResourcesWrite, "group=crew")
	h.grant(t, agentd.PermGroupsResourcesWrite, "group=other")
	changes := []struct {
		method, path string
		body         map[string]any
	}{
		{"PATCH", "/api/drawing-boards/plan", map[string]any{"elements": []map[string]any{rect("card", 1, 1, 0)}}},
		{"POST", "/api/drawing-boards/plan/title", map[string]any{"title": "Mine"}},
		{"POST", "/api/drawing-boards/plan/group", map[string]any{"group": "other"}},
	}
	refused := func(why string) {
		t.Helper()
		for _, c := range changes {
			code, body := h.call(c.method, c.path, c.body)
			require.Equal(t, 403, code, "%s %s, %s: %s", c.method, c.path, why, body)
			require.Contains(t, body, "resource_locked")
		}
		b, err := db.GetCollabDrawingBoard("plan")
		require.NoError(t, err)
		require.Equal(t, "plan", b.Title)
		require.Zero(t, b.ElementCount)
		require.Equal(t, "crew", b.GroupName, "not moved")
	}
	refused("restricted")
	code, body = h.call("GET", "/api/drawing-boards/plan", nil)
	require.Equal(t, 200, code, "a locked drawing board still reads: %s", body)
	h.trustUnrestricted(t)
	refused("unrestricted")

	require.NoError(t, db.SetGroupResourceLocked(db.GroupResourceDrawingBoard, "plan", false))
	for _, c := range changes {
		code, body = h.call(c.method, c.path, c.body)
		require.Equal(t, 200, code, "unlocked: %s %s: %s", c.method, c.path, body)
	}
}
