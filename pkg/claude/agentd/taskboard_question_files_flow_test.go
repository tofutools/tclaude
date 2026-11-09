package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Files shown with a question (tk_72dff9f8): `ask-operator --attach` sends
// them with the question; they become the task's own files and the question
// lists them, after the task's earlier files --attach-existing names, so
// the answer dialog previews them with it.

type tbQuestionWithFiles struct {
	N             int     `json:"n"`
	AttachmentIDs []int64 `json:"attachment_ids"`
}

type tbAskedWithFiles struct {
	Task struct {
		tbTaskWithFiles
		Questions []tbQuestionWithFiles `json:"questions"`
		Question  *tbQuestionWithFiles  `json:"question"`
	} `json:"task"`
	Question tbQuestionWithFiles `json:"question"`
}

func fileIDs(files []tbFile, names ...string) []int64 {
	out := []int64{}
	for _, n := range names {
		for _, f := range files {
			if f.Name == n {
				out = append(out, f.ID)
			}
		}
	}
	return out
}

func TestTaskboardQuestionWithFiles(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	mine := h.task(h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Epics mock"}), http.StatusCreated)
	h.fileTask(h.upload(h.worker, "/v1/taskboard/tasks/"+mine.ID+"/attachments", nil, [2]string{"earlier.png", string(tbPNG)}), http.StatusCreated)
	asks := "/v1/taskboard/tasks/" + mine.ID + "/questions"

	// A name the task has no file by: refused, and nothing sent with it lands.
	rec := h.upload(h.worker, asks, map[string]any{"title": "Which layout?", "attach_existing": []string{"#9999"}},
		[2]string{"mock.png", string(tbPNG)})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "no_attachment")

	rec = h.upload(h.worker, asks, map[string]any{"title": "Which layout?", "body": "Both mocks are attached.",
		"options": []map[string]any{{"label": "A"}, {"label": "B"}}, "attach_existing": []string{"EARLIER.png"}},
		[2]string{"mock.png", string(tbPNG)}, [2]string{"notes.md", "# notes"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var asked tbAskedWithFiles
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &asked))
	files := asked.Task.Attachments
	assert.Equal(t, []string{"earlier.png", "mock.png", "notes.md"}, fileNames(files), "the files are the task's own")
	assert.Equal(t, fileIDs(files, "earlier.png", "mock.png", "notes.md"), asked.Question.AttachmentIDs,
		"the question shows the named file, then the ones sent with it")
	got := h.showQ(t, mine.ID)
	require.Len(t, got.Questions, 1)
	var shown struct {
		Questions []tbQuestionWithFiles `json:"questions"`
	}
	rec = h.call("", http.MethodGet, "/v1/taskboard/tasks/"+mine.ID, nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &shown))
	assert.Equal(t, asked.Question.AttachmentIDs, shown.Questions[0].AttachmentIDs, "the board reads them back")

	// A JSON ask names earlier files only.
	code, body := h.askOn(h.worker, mine.ID, map[string]any{"title": "And the colour?", "attach_existing": []string{"#" + jsonInt(fileIDs(files, "mock.png")[0])}})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Contains(t, body, `"attachment_ids":[`+jsonInt(fileIDs(files, "mock.png")[0])+`]`)
	// The ids are the daemon's to resolve, never the body's.
	code, body = h.askOn(h.worker, mine.ID, map[string]any{"title": "Smuggled?", "attachment_ids": []int64{424242}})
	require.Equal(t, http.StatusCreated, code, body)
	assert.NotContains(t, body, "424242")

	// A question task: its files are its question's.
	rec = h.upload(h.worker, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Pick a mock", "to": "operator",
		"question": map[string]any{"kind": "question"}}, [2]string{"a.png", string(tbPNG)})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var qt struct {
		tbTaskWithFiles
		Question *tbQuestionWithFiles `json:"question"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &qt))
	require.NotNil(t, qt.Question)
	assert.Equal(t, fileIDs(qt.Attachments, "a.png"), qt.Question.AttachmentIDs)
}

func jsonInt(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// A linked node asks this node's operator with files under the grants it
// has for attaching: directly when small, staged otherwise.
func TestPeerTaskboardQuestionWithFiles(t *testing.T) {
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
	asks := "/v1/taskboard/tasks/" + task.ID + "/questions"
	q := map[string]any{"title": "Which mock?"}

	direct := func(files ...[2]string) (int, string) {
		body, ct := taskMultipart(t, q, files...)
		req := httptest.NewRequest(http.MethodPost, asks, bytes.NewReader(body))
		req.Header.Set("Content-Type", ct)
		rec := testharness.Serve(view, req)
		return rec.Code, rec.Body.String()
	}
	staged := func(files ...[2]string) (int, string) {
		body, ct := taskMultipart(t, q, files...)
		code, out := h.call("POST", "/api/peer-uploads", nil)
		require.Equal(t, 200, code, out)
		var opened struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &opened))
		req := httptest.NewRequest(http.MethodPost, "/api/peer-uploads/chunk?id="+opened.ID+"&offset=0", bytes.NewReader(body))
		rec := testharness.Serve(view, req)
		require.Equal(t, 200, rec.Code, rec.Body.String())
		return h.finishUpload(t, opened.ID, asks, ct)
	}

	h.grant(t, agentd.PermTaskboardRead, "group=crew")
	code, body := direct([2]string{"a.png", string(tbPNG)})
	assert.Equal(t, http.StatusForbidden, code, body)

	h.grant(t, agentd.PermTaskboardWrite, "group=crew")
	// A JSON ask, and one with files.
	code, body = h.call("POST", asks, q)
	require.Equal(t, http.StatusCreated, code, body)
	code, body = direct([2]string{"a.png", string(tbPNG)})
	require.Equal(t, http.StatusCreated, code, body)
	var asked tbAskedWithFiles
	require.NoError(t, json.Unmarshal([]byte(body), &asked))
	assert.Equal(t, fileIDs(asked.Task.Attachments, "a.png"), asked.Question.AttachmentIDs)

	big := "\x89PNG\r\n\x1a\n" + strings.Repeat("0123456789abcdef", 4<<10) // 64 KiB, past one peer view request
	code, body = staged([2]string{"big.png", big})
	require.Equal(t, http.StatusCreated, code, body)
	require.NoError(t, json.Unmarshal([]byte(body), &asked))
	assert.Equal(t, 3, asked.Question.N)
	assert.Equal(t, fileIDs(asked.Task.Attachments, "big.png"), asked.Question.AttachmentIDs)
}
