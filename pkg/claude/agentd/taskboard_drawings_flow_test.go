package agentd_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image/png"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type tbDrawingRef struct {
	Board    string   `json:"board"`
	Version  int64    `json:"version"`
	Elements []string `json:"elements"`
	Crop     struct {
		X, Y, Width, Height float64
	} `json:"crop"`
}

type tbDrawingFile struct {
	tbFile
	Drawing *tbDrawingRef `json:"drawing"`
}

type tbDrawingReply struct {
	Attachment tbDrawingFile `json:"attachment"`
	Replaced   bool          `json:"replaced"`
	Comment    int64         `json:"comment"`
}

// Scenario: a group member draws on its group's drawing board and puts the
// drawing on a task: one call renders it as a PNG the task previews, with a
// reference back to the board. Exporting the same part again updates that
// file; a part of the board is a file of its own; a comment can carry one.
// It needs both read access to the board and the right to change the task.
func TestTaskboardDrawingsAttachAnExportWithItsBoard(t *testing.T) {
	h := newTaskboardFlow(t)
	const member, outsider = "tb-draw-member-1111", "tb-draw-outsider-2222"
	h.agent(t, member)
	h.agent(t, outsider)
	h.f.HaveGroup("crew")
	h.f.HaveGroup("elsewhere")
	h.f.HaveMember("crew", member)
	h.f.HaveMember("elsewhere", outsider)
	h.groupWrites("crew")

	require.Equal(t, http.StatusCreated, h.call(member, http.MethodPost, "/v1/drawing-boards", map[string]any{"id": "arch", "title": "Architecture", "group": "crew"}).Code)
	rec := h.call(member, http.MethodPatch, "/v1/drawing-boards/arch", map[string]any{"elements": []any{rect("api", 1, 1, 0), rect("db", 1, 2, 200)}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	dash := agentd.BuildDashboardHandlerForTest()
	require.Equal(t, http.StatusCreated, testharness.Serve(dash, drawingBoardDashReq(t, http.MethodPost, "/api/drawing-boards", map[string]any{"id": "private", "title": "Private"})).Code)

	task := h.task(h.call(member, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "explain the design"}), http.StatusCreated)
	drawings := "/v1/taskboard/tasks/" + task.ID + "/drawings"
	attach := func(conv string, body map[string]any, want int) tbDrawingReply {
		t.Helper()
		rec := h.call(conv, http.MethodPost, drawings, body)
		require.Equal(t, want, rec.Code, rec.Body.String())
		var out tbDrawingReply
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		return out
	}

	first := attach(member, map[string]any{"board": "arch"}, http.StatusCreated)
	file := first.Attachment
	assert.Equal(t, "arch.png", file.Name)
	assert.Equal(t, "image/png", file.ContentType)
	assert.True(t, file.Image)
	require.NotNil(t, file.Drawing, "the file says which board it shows")
	assert.Equal(t, "arch", file.Drawing.Board)
	assert.Equal(t, int64(1), file.Drawing.Version)
	assert.Greater(t, file.Drawing.Crop.Width, 210.0, "the crop covers both boxes")
	rec = h.call(member, http.MethodGet, fmt.Sprintf("/v1/taskboard/tasks/%s/attachments/%d", task.ID, file.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	_, err := png.Decode(bytes.NewReader(rec.Body.Bytes()))
	require.NoError(t, err, "the task's file is the rendered PNG")

	// The board changes; the same export again updates the file in place.
	rec = h.call(member, http.MethodPatch, "/v1/drawing-boards/arch", map[string]any{"elements": []any{rect("cache", 1, 3, 400)}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	again := attach(member, map[string]any{"board": "arch"}, http.StatusOK)
	assert.True(t, again.Replaced)
	assert.Equal(t, file.ID, again.Attachment.ID)
	assert.Equal(t, "arch.png", again.Attachment.Name)
	assert.Equal(t, int64(2), again.Attachment.Drawing.Version)
	assert.NotEqual(t, file.SHA256, again.Attachment.SHA256)

	// A part of the board is a file of its own, and is updated on its own.
	part := attach(member, map[string]any{"board": "arch", "elements": []string{"db", "api"}, "name": "storage"}, http.StatusCreated)
	assert.Equal(t, "storage.png", part.Attachment.Name)
	assert.ElementsMatch(t, []string{"api", "db"}, part.Attachment.Drawing.Elements)
	assert.True(t, attach(member, map[string]any{"board": "arch", "elements": []string{"api", "db"}}, http.StatusOK).Replaced,
		"the same elements in another order are the same part")
	files, err := db.ListGroupTaskAttachments(task.ID)
	require.NoError(t, err)
	assert.Len(t, files, 2)

	// A comment carries a drawing of its own.
	commented := attach(member, map[string]any{"board": "arch", "comment": "here is the storage layer", "elements": []string{"db"}}, http.StatusCreated)
	assert.NotZero(t, commented.Comment)
	require.NotNil(t, commented.Attachment.Drawing)
	assert.Equal(t, []string{"db"}, commented.Attachment.Drawing.Elements)

	// With a key, a retried comment is the first one, not a second
	// (tk_2d31bef9); the same key with another comment is refused.
	keyed := map[string]any{"board": "arch", "comment": "the api side", "elements": []string{"api"}, "key": "web-1"}
	keyedFirst := attach(member, keyed, http.StatusCreated)
	keyedAgain := attach(member, keyed, http.StatusOK)
	assert.Equal(t, keyedFirst.Comment, keyedAgain.Comment, "the retry answers the first comment")
	assert.Equal(t, keyedFirst.Attachment.ID, keyedAgain.Attachment.ID, "with its file")
	comments, err := db.ListGroupTaskCommentAttachments(task.ID)
	require.NoError(t, err)
	assert.Len(t, comments, 2, "two comments carry drawings: the unkeyed one and the keyed one, once")
	rec = h.call(member, http.MethodPost, drawings, map[string]any{"board": "arch", "comment": "something else", "key": "web-1"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	rec = h.call(member, http.MethodPost, "/v1/taskboard/tasks/"+task.ID+"/comments", map[string]any{"body": "plain", "key": "web-3"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = h.call(member, http.MethodPost, drawings, map[string]any{"board": "arch", "comment": "plain", "key": "web-3"})
	assert.Equal(t, http.StatusConflict, rec.Code, "a plain comment's key is not a drawing's: %s", rec.Body.String())
	rec = h.call(member, http.MethodPost, drawings, map[string]any{"board": "arch", "key": "web-2"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a key goes with a comment: %s", rec.Body.String())

	// replace names a drawing on the task; a plain file is not one.
	plain := h.fileTask(h.upload(member, "/v1/taskboard/tasks/"+task.ID+"/attachments", nil, [2]string{"notes.txt", "x"}), http.StatusCreated)
	var notes int64
	for _, f := range plain.Attachments {
		if f.Name == "notes.txt" {
			notes = f.ID
		}
	}
	rec = h.call(member, http.MethodPost, drawings, map[string]any{"board": "arch", "replace": notes})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.True(t, attach(member, map[string]any{"board": "arch", "elements": []string{"db"}, "replace": file.ID}, http.StatusOK).Replaced)

	// Refusals: a board the member cannot read, a name that is not a PNG, a
	// task outside the caller's group.
	rec = h.call(member, http.MethodPost, drawings, map[string]any{"board": "private"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = h.call(member, http.MethodPost, drawings, map[string]any{"board": "arch", "name": "x.svg"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	rec = h.call(outsider, http.MethodPost, drawings, map[string]any{"board": "arch"})
	assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())

	// The operator attaches any board to any task.
	op := h.call("", http.MethodPost, drawings, map[string]any{"board": "private"})
	assert.Equal(t, http.StatusUnprocessableEntity, op.Code, "an empty board has nothing to draw: %s", op.Body.String())
	assert.Equal(t, http.StatusCreated, h.call("", http.MethodPost, drawings, map[string]any{"board": "arch"}).Code,
		"the whole-board file now shows only db (replace above), so the whole board is a new file")

	// The dashboard's own server serves the route too: the editor's File ›
	// Export › Attach to a task… posts there.
	rec = testharness.Serve(dash, dashReq(t, http.MethodPost, drawings, map[string]any{"board": "arch", "elements": []string{"api"}}))
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}
