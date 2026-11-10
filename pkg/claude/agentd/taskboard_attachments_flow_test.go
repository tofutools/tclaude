package agentd_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// tbFile is a file of a task on the wire.
type tbFile struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
	Image       bool   `json:"image"`
	AddedBy     string `json:"added_by"`
}

type tbTaskWithFiles struct {
	tbTask
	Attachments []tbFile `json:"attachments"`
}

// tbPNG is a tiny file named .png; its bytes need not decode.
var tbPNG = []byte("\x89PNG\r\n\x1a\nfake image bytes")

// taskMultipart builds a request body of a "task" part (nil: none) and
// "file" parts, name → bytes, in order.
func taskMultipart(t *testing.T, task any, files ...[2]string) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if task != nil {
		raw, err := json.Marshal(task)
		require.NoError(t, err)
		w, err := mw.CreateFormField("task")
		require.NoError(t, err)
		_, err = w.Write(raw)
		require.NoError(t, err)
	}
	for _, f := range files {
		w, err := mw.CreateFormFile("file", f[0])
		require.NoError(t, err)
		_, err = w.Write([]byte(f[1]))
		require.NoError(t, err)
	}
	require.NoError(t, mw.Close())
	return buf.Bytes(), mw.FormDataContentType()
}

func (h *taskboardHarness) upload(conv, path string, task any, files ...[2]string) *httptest.ResponseRecorder {
	h.t.Helper()
	body, ct := taskMultipart(h.t, task, files...)
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", ct)
	if conv == "" {
		req = agentd.AsHumanPeer(req)
	} else {
		req = agentd.AsAgentPeer(req, conv)
	}
	return testharness.Serve(h.f.Mux, req)
}

func (h *taskboardHarness) fileTask(rec *httptest.ResponseRecorder, want int) tbTaskWithFiles {
	h.t.Helper()
	require.Equal(h.t, want, rec.Code, "body=%s", rec.Body.String())
	var out tbTaskWithFiles
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func fileNames(files []tbFile) []string {
	out := []string{}
	for _, f := range files {
		out = append(out, f.Name)
	}
	return out
}

// A member creates a task with files in one request, attaches and detaches
// more, and reads them back; the caps and the type allow-list hold, history
// records each file, and an agent outside the group cannot see them.
func TestTaskboardAttachmentsLifecycle(t *testing.T) {
	h := newTaskboardFlow(t)
	const member, outsider = "tb-att-member-1111", "tb-att-outsider-2222"
	h.agent(t, member)
	h.agent(t, outsider)
	h.f.HaveGroup("crew")
	h.f.HaveGroup("elsewhere")
	h.f.HaveMember("crew", member)
	h.f.HaveMember("elsewhere", outsider)
	h.groupWrites("crew")

	created := h.fileTask(h.upload(member, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "fix the layout", "stage": "next"},
		[2]string{"shot.png", string(tbPNG)}, [2]string{"notes.md", "# notes\n"}), http.StatusCreated)
	assert.Equal(t, "next", created.Stage)
	require.Len(t, created.Attachments, 2)
	assert.Equal(t, []string{"shot.png", "notes.md"}, fileNames(created.Attachments))
	shot := created.Attachments[0]
	assert.Equal(t, "image/png", shot.ContentType)
	assert.True(t, shot.Image)
	assert.Equal(t, int64(len(tbPNG)), shot.Size)
	assert.Contains(t, shot.AddedBy, "agent:")

	// The bytes: an image inline, sandboxed; the JSON form for peers.
	fileURL := fmt.Sprintf("/v1/taskboard/tasks/%s/attachments/%d", created.ID, shot.ID)
	rec := h.call(member, http.MethodGet, fileURL, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Equal(t, tbPNG, rec.Body.Bytes())
	assert.Equal(t, "image/png", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "inline")
	assert.Equal(t, "nosniff", rec.Header().Get("X-Content-Type-Options"))
	assert.Contains(t, rec.Header().Get("Content-Security-Policy"), "sandbox")
	rec = h.call(member, http.MethodGet, fmt.Sprintf("/v1/taskboard/tasks/%s/attachments/%d", created.ID, created.Attachments[1].ID), nil)
	assert.Contains(t, rec.Header().Get("Content-Disposition"), "attachment", "a non-image is a download")
	rec = h.call(member, http.MethodGet, fileURL+"?format=json", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var asJSON struct {
		Name string `json:"name"`
		Data []byte `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &asJSON))
	assert.Equal(t, "shot.png", asJSON.Name)
	assert.Equal(t, tbPNG, asJSON.Data)

	// Another file of the same name is renamed; the list shows all three.
	more := h.fileTask(h.upload(member, "/v1/taskboard/tasks/"+created.ID+"/attachments", nil,
		[2]string{"Shot.png", "other image"}), http.StatusCreated)
	assert.Equal(t, []string{"shot.png", "notes.md", "2-Shot.png"}, fileNames(more.Attachments))
	rec = h.call(member, http.MethodGet, "/v1/taskboard/tasks/"+created.ID+"/attachments", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var listed struct {
		Attachments []tbFile `json:"attachments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	assert.Len(t, listed.Attachments, 3)

	// Refusals: a type off the allow-list, an empty file, past the cap, no
	// files at all.
	rec = h.upload(member, "/v1/taskboard/tasks/"+created.ID+"/attachments", nil, [2]string{"page.html", "<script>"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "type not allowed")
	rec = h.upload(member, "/v1/taskboard/tasks/"+created.ID+"/attachments", nil, [2]string{"empty.txt", ""})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var eight [][2]string
	for i := range 8 {
		eight = append(eight, [2]string{fmt.Sprintf("f%d.txt", i), "x"})
	}
	rec = h.upload(member, "/v1/taskboard/tasks/"+created.ID+"/attachments", nil, eight...)
	assert.Equal(t, http.StatusConflict, rec.Code, "3 + 8 is past the cap of 10: %s", rec.Body.String())
	rec = h.upload(member, "/v1/taskboard/tasks/"+created.ID+"/attachments", nil)
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// An agent outside the group sees neither the task nor its files.
	rec = h.call(outsider, http.MethodGet, fileURL, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	rec = h.upload(outsider, "/v1/taskboard/tasks/"+created.ID+"/attachments", nil, [2]string{"x.txt", "x"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
	// Another task's id does not reach this file.
	other := h.task(h.call(member, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "other"}), http.StatusCreated)
	rec = h.call(member, http.MethodGet, fmt.Sprintf("/v1/taskboard/tasks/%s/attachments/%d", other.ID, shot.ID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// Detach; history records every file in and out.
	after := h.fileTask(h.call(member, http.MethodDelete, fileURL, nil), http.StatusOK)
	assert.Equal(t, []string{"notes.md", "2-Shot.png"}, fileNames(after.Attachments))
	rec = h.call(member, http.MethodDelete, fileURL, nil)
	assert.Equal(t, http.StatusNotFound, rec.Code, "already detached")
	events, err := db.ListGroupTaskEvents(created.ID)
	require.NoError(t, err)
	var trail []string
	for _, e := range events {
		if e.Kind == db.GroupTaskEventAttached || e.Kind == db.GroupTaskEventDetached {
			trail = append(trail, e.Kind+" "+e.Detail)
		}
	}
	assert.Equal(t, []string{"attached shot.png", "attached notes.md", "attached 2-Shot.png", "detached shot.png"}, trail)

	// An import takes no files.
	rec = h.upload(member, "/v1/groups/crew/taskboard/tasks", map[string]any{"lines": []string{"a"}}, [2]string{"x.txt", "x"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// A clone shares the original's files; a blob goes once nothing names it,
// after the grace a fresh blob gets.
func TestTaskboardAttachmentsCloneAndCollect(t *testing.T) {
	h := newTaskboardFlow(t)
	h.f.HaveGroup("crew")
	h.f.HaveGroup("squad")
	created := h.fileTask(h.upload("", "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "with a file"},
		[2]string{"log.txt", "the log"}), http.StatusCreated)
	require.Len(t, created.Attachments, 1)
	sha := created.Attachments[0].SHA256
	blob := filepath.Join(agentd.TaskboardFilesDirForTest(), sha[:2], sha)
	require.FileExists(t, blob)

	rec := h.call("", http.MethodPost, "/v1/taskboard/tasks/"+created.ID+"/move-to-group", map[string]any{"group": "squad", "clone": true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var cloned struct {
		Task tbTaskWithFiles `json:"task"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cloned))
	require.Len(t, cloned.Task.Attachments, 1, "the clone carries the file")
	assert.Equal(t, sha, cloned.Task.Attachments[0].SHA256, "and shares its bytes")

	// Detached from the original, the clone still names it: it stays.
	h.fileTask(h.call("", http.MethodDelete, fmt.Sprintf("/v1/taskboard/tasks/%s/attachments/%d", created.ID, created.Attachments[0].ID), nil), http.StatusOK)
	later := time.Now().Add(time.Hour)
	agentd.CollectTaskBlobsForTest(later)
	require.FileExists(t, blob)

	// Detached from the clone too: within the grace it stays, past it it goes.
	h.fileTask(h.call("", http.MethodDelete, fmt.Sprintf("/v1/taskboard/tasks/%s/attachments/%d", cloned.Task.ID, cloned.Task.Attachments[0].ID), nil), http.StatusOK)
	agentd.CollectTaskBlobsForTest(time.Now())
	require.FileExists(t, blob, "a fresh blob keeps its grace")
	agentd.CollectTaskBlobsForTest(later)
	_, err := os.Stat(blob)
	assert.True(t, os.IsNotExist(err), "nothing names it any more: %v", err)
}

// An ask's files reach the asked member as its mail's attachments, and an
// assignment's brief names the task's files and how to fetch them.
func TestTaskboardAttachmentsAskAndBrief(t *testing.T) {
	h := newTaskboardFlow(t)
	const member = "tb-att-ask-3333"
	t.Cleanup(agentd.SetOperatorMessageAttachmentBasesForTest(t.TempDir(), t.TempDir()))
	h.agent(t, member)
	h.f.HaveGroup("crew")
	h.f.HaveMember("crew", member)
	h.groupWrites("crew")

	rec := h.upload("", "/v1/groups/crew/taskboard/ask", map[string]any{"to": member, "prompt": "turn this mock into tasks"},
		[2]string{"mock.png", string(tbPNG)})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var asked struct {
		MessageID int64 `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &asked))
	atts, err := db.ListAgentMessageAttachments(asked.MessageID)
	require.NoError(t, err)
	require.Len(t, atts, 1)
	assert.Equal(t, "mock.png", atts[0].Filename)
	msg, err := db.GetAgentMessage(asked.MessageID)
	require.NoError(t, err)
	assert.Contains(t, msg.Body, "mock.png")
	assert.Contains(t, msg.Body, "--attach")

	task := h.fileTask(h.upload("", "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "build it"},
		[2]string{"spec.md", "spec"}), http.StatusCreated)
	rec = h.call("", http.MethodPost, "/v1/taskboard/tasks/"+task.ID+"/assign", map[string]any{"to": member})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rows, err := db.ListAgentMessagesForConv(member, 10)
	require.NoError(t, err)
	var brief string
	for _, m := range rows {
		if strings.Contains(m.Body, task.ID) && strings.Contains(m.Body, "Assigned to you") {
			brief = m.Body
		}
	}
	require.NotEmpty(t, brief, "the assignee got its brief")
	assert.Contains(t, brief, "Files: spec.md")
	assert.Contains(t, brief, "taskboard attachments "+task.ID+" --save")
}

// A linked node attaches, reads and detaches a task's files under its
// taskboard grants: reads as base64 JSON, small files directly, larger ones
// (and a task moved in with files) through its staged upload.
func TestPeerTaskboardAttachmentsUnderGrants(t *testing.T) {
	fh := newFedHarness(t)
	cfg, err := config.Load()
	require.NoError(t, err)
	cfg.Features = &config.FeaturesConfig{Taskboard: new(true)}
	require.NoError(t, config.Save(cfg))
	fh.f.HaveGroup("crew")
	h := &peerProcessHarness{fedHarness: fh}
	view := agentd.PeerViewHandler(fh.peer.id.ID())
	h.call = func(method, path string, body any) (int, string) {
		t.Helper()
		rec := testharness.Serve(view, testharness.JSONRequest(t, method, path, body))
		return rec.Code, rec.Body.String()
	}
	rec := fedHuman(t, fh.f, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "crew plan"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var task tbTask
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &task))
	attachPath := "/v1/taskboard/tasks/" + task.ID + "/attachments"

	direct := func(task any, files ...[2]string) (int, string) {
		body, ct := taskMultipart(t, task, files...)
		req := httptest.NewRequest(http.MethodPost, attachPath, bytes.NewReader(body))
		req.Header.Set("Content-Type", ct)
		rec := testharness.Serve(view, req)
		return rec.Code, rec.Body.String()
	}
	staged := func(path string, task any, files ...[2]string) (int, string) {
		t.Helper()
		body, ct := taskMultipart(t, task, files...)
		code, out := h.call("POST", "/api/peer-uploads", nil)
		require.Equal(t, 200, code, out)
		var opened struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &opened))
		req := httptest.NewRequest(http.MethodPost, "/api/peer-uploads/chunk?id="+opened.ID+"&offset=0", bytes.NewReader(body))
		rec := testharness.Serve(view, req)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		return h.finishUpload(t, opened.ID, path, ct)
	}

	// Read only: the list, but no attaching either way.
	h.grant(t, agentd.PermTaskboardRead, "group=crew")
	code, body := direct(nil, [2]string{"a.txt", "a"})
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = staged(attachPath, nil, [2]string{"a.txt", "a"})
	assert.Equal(t, http.StatusForbidden, code, body)

	// Write: a small file directly, a larger one staged.
	h.grant(t, agentd.PermTaskboardWrite, "group=crew")
	code, body = direct(nil, [2]string{"a.txt", "small"})
	require.Equal(t, http.StatusCreated, code, body)
	big := strings.Repeat("0123456789abcdef", 4<<10) // 64 KiB, past one peer view request
	code, body = staged(attachPath, nil, [2]string{"big.log", big})
	require.Equal(t, http.StatusCreated, code, body)
	var withFiles tbTaskWithFiles
	require.NoError(t, json.Unmarshal([]byte(body), &withFiles))
	require.Equal(t, []string{"a.txt", "big.log"}, fileNames(withFiles.Attachments))
	assert.Contains(t, withFiles.Attachments[1].AddedBy, "peer: ", "the peer's own change")

	// Read back as JSON (the peer view carries JSON only), then detach.
	code, body = h.call("GET", fmt.Sprintf("%s/%d", attachPath, withFiles.Attachments[1].ID), nil)
	require.Equal(t, http.StatusOK, code)
	var got struct {
		Data []byte `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, big, string(got.Data))
	code, body = h.call("DELETE", fmt.Sprintf("%s/%d", attachPath, withFiles.Attachments[0].ID), nil)
	require.Equal(t, http.StatusOK, code, body)

	// A task moved in from the peer with its files, under taskboard.transfer.
	transfer := "/v1/groups/crew/taskboard/transfer-in"
	in := map[string]any{"title": "moved with files", "origin": map[string]any{"group": "bobs", "task": "tk_00000001"}}
	code, body = staged(transfer, in, [2]string{"shot.png", string(tbPNG)})
	assert.Equal(t, http.StatusForbidden, code, "transfer-in takes taskboard.transfer: %s", body)
	h.grant(t, agentd.PermTaskboardTransfer, "group=crew")
	code, body = staged(transfer, in, [2]string{"shot.png", string(tbPNG)})
	require.Equal(t, http.StatusCreated, code, body)
	var moved tbTaskWithFiles
	require.NoError(t, json.Unmarshal([]byte(body), &moved))
	assert.Equal(t, "moved with files", moved.Title)
	assert.Equal(t, []string{"shot.png"}, fileNames(moved.Attachments))
}

// A task's files carry the viewer verdicts a notification's do, from their
// bytes: a real image is previewable, a Markdown file renders, and a text
// file or a PDF has no viewer (the dashboard offers it as before).
func TestTaskboardAttachmentsViewerVerdicts(t *testing.T) {
	h := newTaskboardFlow(t)
	h.f.HaveGroup("crew")
	created := h.fileTask(h.upload("", "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "with files"},
		[2]string{"shot.png", string(tbPNG)}, [2]string{"plan.md", "# Plan\n\n- one\n"},
		[2]string{"log.txt", "a log"}, [2]string{"spec.pdf", "%PDF-1.4\n%%EOF\n"}, [2]string{"fake.png", "not an image"}), http.StatusCreated)

	rec := h.call("", http.MethodGet, "/v1/taskboard/tasks/"+created.ID+"/attachments", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Attachments []struct {
			Name        string `json:"name"`
			Previewable bool   `json:"previewable"`
			Markdown    bool   `json:"markdown"`
			Video       bool   `json:"video"`
		} `json:"attachments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	got := map[string][3]bool{}
	for _, a := range out.Attachments {
		got[a.Name] = [3]bool{a.Previewable, a.Markdown, a.Video}
	}
	assert.Equal(t, map[string][3]bool{
		"shot.png": {true, false, false},
		"plan.md":  {false, true, false},
		"log.txt":  {false, false, false},
		"spec.pdf": {false, false, false},
		"fake.png": {false, false, false}, // claimed an image, its bytes are not
	}, got)
}
