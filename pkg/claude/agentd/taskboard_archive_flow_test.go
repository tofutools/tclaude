package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func (h *tbAssignHarness) listIDs(query string) []string {
	rec := h.call("", http.MethodGet, "/v1/groups/crew/taskboard"+query, nil)
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Tasks []struct {
			ID string `json:"id"`
		} `json:"tasks"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	ids := []string{}
	for _, t := range out.Tasks {
		ids = append(ids, t.ID)
	}
	return ids
}

// A closed task archived by hand leaves the history and is listed only
// under ?archived=1; unarchive brings it back into the history, and a live
// task is dropped on the way in. Someone else's task needs the drop gate.
func TestTaskboardArchiveAndUnarchive(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	done := h.add("shipped", "next")
	code, body := h.post("", done.ID, "status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)
	require.Contains(t, h.listIDs("?all=1"), done.ID)

	code, body = h.post("", done.ID, "archive", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.NotContains(t, h.listIDs("?all=1"), done.ID, "an archived task leaves the history")
	assert.Equal(t, []string{done.ID}, h.listIDs("?archived=1"))
	code, body = h.post("", done.ID, "archive", nil)
	assert.Equal(t, http.StatusOK, code, "archiving an archived task is a no-op: %s", body)

	code, body = h.post("", done.ID, "unarchive", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, h.listIDs("?all=1"), done.ID)
	got, err := db.GetGroupTask(done.ID)
	require.NoError(t, err)
	assert.Equal(t, db.GroupTaskDone, got.Status, "unarchive keeps it done")
	code, body = h.post("", done.ID, "unarchive", nil)
	assert.Equal(t, http.StatusConflict, code, body)

	// A live task is dropped and archived; one held by an agent needs the
	// right to drop it.
	live := h.add("not needed", "")
	code, body = h.post(h.lead, live.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post(h.peer, live.ID, "archive", map[string]any{"reason": "obsolete"})
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = h.post(h.worker, live.ID, "archive", map[string]any{"reason": "obsolete"})
	require.Equal(t, http.StatusOK, code, body)
	got, err = db.GetGroupTask(live.ID)
	require.NoError(t, err)
	assert.Equal(t, db.GroupTaskDropped, got.Status)
	assert.False(t, got.ArchivedAt.IsZero())
	assert.NotContains(t, h.listIDs("?all=1"), live.ID)

	// Reopening an archived task puts it back on the board.
	code, body = h.post("", live.ID, "status", map[string]any{"status": "open"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, h.listIDs(""), live.ID)
}

// Archive done archives every done task, or those closed longer ago than
// an age; dropped tasks and live ones stay. It takes taskboard.write.
func TestTaskboardArchiveDone(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	old := h.add("long done", "next")
	recent := h.add("just done", "next")
	dropped := h.add("dropped", "")
	live := h.add("still open", "next")
	for _, id := range []string{old.ID, recent.ID} {
		code, body := h.post("", id, "status", map[string]any{"status": "done"})
		require.Equal(t, http.StatusOK, code, body)
	}
	code, body := h.post("", dropped.ID, "status", map[string]any{"status": "dropped"})
	require.Equal(t, http.StatusOK, code, body)
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE group_tasks SET closed_at = ? WHERE id = ?`, time.Now().Add(-10*24*time.Hour).UnixNano(), old.ID)
	require.NoError(t, err)

	archiveDone := func(conv string, body map[string]any) (int, []string) {
		rec := h.call(conv, http.MethodPost, "/v1/groups/crew/taskboard/archive-done", body)
		var out struct {
			Archived []string `json:"archived"`
		}
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		}
		return rec.Code, out.Archived
	}
	code, _ = archiveDone(h.worker, nil)
	assert.Equal(t, http.StatusForbidden, code, "a member without taskboard.write")
	code, _ = archiveDone("", map[string]any{"older_than": "soon"})
	assert.Equal(t, http.StatusBadRequest, code)

	code, ids := archiveDone("", map[string]any{"older_than": "7d"})
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []string{old.ID}, ids)
	code, ids = archiveDone("", nil)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, []string{recent.ID}, ids)
	assert.ElementsMatch(t, []string{old.ID, recent.ID}, h.listIDs("?archived=1"))
	all := h.listIDs("?all=1")
	assert.Contains(t, all, dropped.ID, "a dropped task stays in the history")
	assert.Contains(t, all, live.ID)
}
