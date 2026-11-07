package agentd_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type tbComment struct {
	ID          int64    `json:"id"`
	Kind        string   `json:"kind"`
	Body        string   `json:"body"`
	Key         string   `json:"key"`
	Author      string   `json:"author"`
	EditedAt    string   `json:"edited_at"`
	DeletedAt   string   `json:"deleted_at"`
	Attachments []tbFile `json:"attachments"`
}

type tbEntry struct {
	ID      int64  `json:"id"`
	Actor   string `json:"actor"`
	Kind    string `json:"kind"`
	Action  string `json:"action"`
	Changes []struct {
		Field, Before, After string
	} `json:"changes"`
	Comment      *tbComment `json:"comment"`
	CommentID    int64      `json:"comment_id"`
	PreviousBody string     `json:"previous_body"`
}

type tbActivityTask struct {
	tbTask
	CommentCount int    `json:"comment_count"`
	LastEditedBy string `json:"last_edited_by"`
	LastEditedAt string `json:"last_edited_at"`
}

func (h *tbAssignHarness) comment(conv, id string, body any) (int, tbComment) {
	h.t.Helper()
	rec := h.call(conv, http.MethodPost, "/v1/taskboard/tasks/"+id+"/comments", body)
	var out struct {
		Comment tbComment `json:"comment"`
	}
	if rec.Code < 300 {
		require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out), rec.Body.String())
	}
	return rec.Code, out.Comment
}

func (h *tbAssignHarness) activity(conv, id string) []tbEntry {
	h.t.Helper()
	rec := h.call(conv, http.MethodGet, "/v1/taskboard/tasks/"+id+"/activity", nil)
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Entries []tbEntry `json:"entries"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Entries
}

func entriesOf(entries []tbEntry, kind string) []tbEntry {
	var out []tbEntry
	for _, e := range entries {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// Comments: someone else's comment mails the assignee and the agent that
// created the task; a key makes a repeat idempotent; only the author edits
// (the earlier body stays) or deletes (a tombstone stays); an outsider
// reads nothing.
func TestTaskboardCommentsLifecycle(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	const outsider = "tb-outsider-4444"
	h.agent(t, outsider)
	created := h.task(h.call(h.peer, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "wire the lanes"}), http.StatusCreated)
	code, body := h.post(h.lead, created.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	workerMail := len(tbInboxSubjects(t, h.worker))
	creatorMail := len(tbInboxSubjects(t, h.peer))

	// The operator comments: the assignee and the agent creator are mailed.
	code, c := h.comment("", created.ID, map[string]any{"body": "Use **the** new API.", "key": "k1"})
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "comment", c.Kind)
	assert.Equal(t, "human:operator", c.Author)
	require.Len(t, tbInboxSubjects(t, h.worker), workerMail+1)
	assert.Contains(t, tbInboxSubjects(t, h.worker)[0], "comment by")
	require.Len(t, tbInboxSubjects(t, h.peer), creatorMail+1)

	// The same key and body again is the same comment, and mails nobody.
	code, again := h.comment("", created.ID, map[string]any{"body": "Use **the** new API.", "key": "k1"})
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, c.ID, again.ID)
	assert.Len(t, tbInboxSubjects(t, h.worker), workerMail+1)
	code, _ = h.comment("", created.ID, map[string]any{"body": "something else", "key": "k1"})
	assert.Equal(t, http.StatusConflict, code)
	// The same key is another author's own.
	code, _ = h.comment(h.worker, created.ID, map[string]any{"body": "on it", "key": "k1"})
	assert.Equal(t, http.StatusCreated, code)
	// The assignee's own comment mails the creator, not the assignee.
	assert.Len(t, tbInboxSubjects(t, h.worker), workerMail+1)
	assert.Len(t, tbInboxSubjects(t, h.peer), creatorMail+2)

	code, _ = h.comment(h.worker, created.ID, map[string]any{"body": "  "})
	assert.Equal(t, http.StatusBadRequest, code)
	code, _ = h.comment(h.worker, created.ID, map[string]any{"body": strings.Repeat("x", 16<<10+1)})
	assert.Equal(t, http.StatusBadRequest, code)

	// Only the author edits; the earlier body stays in the stream.
	path := "/v1/taskboard/tasks/" + created.ID + "/comments/"
	cid := tbID(c.ID)
	rec := h.call(h.worker, http.MethodPatch, path+cid, map[string]any{"body": "hijacked"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	rec = h.call("", http.MethodPatch, path+cid, map[string]any{"body": "Use the v2 API."})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	entries := h.activity(h.worker, created.ID)
	edits := entriesOf(entries, "comment_edited")
	require.Len(t, edits, 1)
	assert.Equal(t, c.ID, edits[0].CommentID)
	assert.Equal(t, "Use **the** new API.", edits[0].PreviousBody)
	comments := entriesOf(entries, "comment")
	require.Len(t, comments, 2)
	assert.Equal(t, "Use the v2 API.", comments[0].Comment.Body)
	assert.NotEmpty(t, comments[0].Comment.EditedAt)

	// Only the author deletes; a tombstone stays, its bodies gone.
	rec = h.call(h.worker, http.MethodDelete, path+cid, nil)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = h.call("", http.MethodDelete, path+cid, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	entries = h.activity("", created.ID)
	comments = entriesOf(entries, "comment")
	require.Len(t, comments, 2)
	assert.Empty(t, comments[0].Comment.Body)
	assert.NotEmpty(t, comments[0].Comment.DeletedAt)
	assert.Empty(t, entriesOf(entries, "comment_edited")[0].PreviousBody)
	rec = h.call("", http.MethodPatch, path+cid, map[string]any{"body": "back"})
	assert.Equal(t, http.StatusConflict, rec.Code)
	rec = h.call("", http.MethodDelete, path+"999999", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	view := tbActivityTask{}
	rec = h.call(h.worker, http.MethodGet, "/v1/taskboard/tasks/"+created.ID, nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	assert.Equal(t, 1, view.CommentCount)

	// An agent outside the group reads nothing and comments nothing.
	rec = h.call(outsider, http.MethodGet, "/v1/taskboard/tasks/"+created.ID+"/activity", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	code, _ = h.comment(outsider, created.ID, map[string]any{"body": "hi"})
	assert.Equal(t, http.StatusNotFound, code)
}

// A comment carries files: they are the comment's, not the task's, are read
// through the task's file route, and go when the comment is deleted.
func TestTaskboardCommentFiles(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	task := h.add("draw the icon", "")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormField("comment")
	require.NoError(t, err)
	_, _ = part.Write([]byte(`{"body":"see the shot","key":"web-1"}`))
	fw, err := mw.CreateFormFile("file", "shot.png")
	require.NoError(t, err)
	_, _ = fw.Write(tbPNG)
	require.NoError(t, mw.Close())
	req := httptest.NewRequest(http.MethodPost, "/v1/taskboard/tasks/"+task.ID+"/comments", bytes.NewReader(buf.Bytes()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := testharness.Serve(h.f.Mux, agentd.AsAgentPeer(req, h.worker))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var out struct {
		Comment tbComment `json:"comment"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Len(t, out.Comment.Attachments, 1)
	file := out.Comment.Attachments[0]
	assert.Equal(t, "shot.png", file.Name)

	// Not among the task's own files, but readable through its file route.
	got := h.fileTask(h.call(h.peer, http.MethodGet, "/v1/taskboard/tasks/"+task.ID, nil), http.StatusOK)
	assert.Empty(t, got.Attachments)
	fileURL := "/v1/taskboard/tasks/" + task.ID + "/attachments/" + tbID(file.ID)
	rec = h.call(h.peer, http.MethodGet, fileURL, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, tbPNG, rec.Body.Bytes())
	// Nor detachable on its own.
	rec = h.call("", http.MethodDelete, fileURL, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	rec = h.call(h.worker, http.MethodDelete, "/v1/taskboard/tasks/"+task.ID+"/comments/"+tbID(out.Comment.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = h.call(h.peer, http.MethodGet, fileURL, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

// Changes are structured: a field edit records before and after, a no-op
// records nothing, resource actions are named, and close reasons are typed
// comments. The view names its last editor.
func TestTaskboardActivityRecordsChanges(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	task := h.add("old title", "")
	rec := h.call(h.lead, http.MethodPatch, "/v1/taskboard/tasks/"+task.ID, map[string]any{"title": "new title"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	before := len(h.activity("", task.ID))
	rec = h.call(h.lead, http.MethodPatch, "/v1/taskboard/tasks/"+task.ID, map[string]any{"title": "new title"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Len(t, h.activity("", task.ID), before, "a no-op edit records nothing")

	code, body := h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post("", task.ID, "status", map[string]any{"status": "done", "reason": "shipped in #12"})
	require.Equal(t, http.StatusOK, code, body)

	entries := h.activity("", task.ID)
	var titled bool
	for _, e := range entries {
		for _, c := range e.Changes {
			if c.Field == "title" {
				titled = true
				assert.Equal(t, "old title", c.Before)
				assert.Equal(t, "new title", c.After)
				assert.Equal(t, "agent:"+tbAgentID(t, h.lead), e.Actor)
			}
		}
	}
	assert.True(t, titled, "the title change is recorded: %+v", entries)
	assigned := entriesOf(entries, "assigned")
	require.Len(t, assigned, 1)
	assert.Equal(t, "assigned", assigned[0].Action)
	var reason *tbComment
	for _, e := range entriesOf(entries, "comment") {
		if e.Comment.Kind == "done_reason" {
			reason = e.Comment
		}
	}
	require.NotNil(t, reason, "the done reason is a typed comment: %+v", entries)
	assert.Equal(t, "shipped in #12", reason.Body)

	var view tbActivityTask
	rec = h.call("", http.MethodGet, "/v1/taskboard/tasks/"+task.ID, nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &view))
	assert.Equal(t, "agent:"+tbAgentID(t, h.lead), view.LastEditedBy)
	assert.NotEmpty(t, view.LastEditedAt)
	// The history route answers the same stream.
	rec = h.call("", http.MethodGet, "/v1/taskboard/tasks/"+task.ID+"/events", nil)
	assert.Contains(t, rec.Body.String(), `"done_reason"`)
}

func tbID(id int64) string { return strconv.FormatInt(id, 10) }

// A member holding only taskboard.own comments on any task of its group's
// board, someone else's included (commenting is not claiming); an explicit
// deny refuses it, and own never reaches another group's board.
func TestTaskboardOwnOnlyMemberComments(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	theirs := h.add("the operator's task", "next")
	code, body := h.post(h.lead, theirs.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	// The peer may not change the worker's task, but may comment on it.
	rec := h.call(h.peer, http.MethodPatch, "/v1/taskboard/tasks/"+theirs.ID, map[string]any{"title": "renamed"})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, c := h.comment(h.peer, theirs.ID, map[string]any{"body": "I saw this fail too"})
	require.Equal(t, http.StatusCreated, code)
	assert.Equal(t, "agent:"+tbAgentID(t, h.peer), c.Author)
	// An unassigned task too, and the author edits its own comment.
	idea := h.add("an idea", "")
	code, _ = h.comment(h.peer, idea.ID, map[string]any{"body": "worth doing"})
	require.Equal(t, http.StatusCreated, code)
	rec = h.call(h.peer, http.MethodPatch, "/v1/taskboard/tasks/"+theirs.ID+"/comments/"+tbID(c.ID), map[string]any{"body": "I saw this fail twice"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// An explicit deny wins.
	require.NoError(t, db.SetAgentPermissionOverride(h.peer, agentd.PermTaskboardOwn, db.PermEffectDeny, "test"))
	code, _ = h.comment(h.peer, theirs.ID, map[string]any{"body": "still here"})
	assert.Equal(t, http.StatusForbidden, code)

	// own does not reach a group the agent is not a member of.
	const outsider = "tb-own-outsider-5555"
	h.agent(t, outsider)
	h.f.HaveGroup("elsewhere")
	h.f.HaveMember("elsewhere", outsider)
	require.NoError(t, db.SetAgentPermissionOverride(outsider, agentd.PermTaskboardOwn, db.PermEffectGrant, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(outsider, agentd.PermTaskboardRead, db.PermEffectGrant, "test"))
	code, _ = h.comment(outsider, theirs.ID, map[string]any{"body": "from outside"})
	assert.Equal(t, http.StatusForbidden, code)
}
