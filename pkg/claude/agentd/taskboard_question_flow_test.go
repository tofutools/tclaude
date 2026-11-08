package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type tbQuestionTask struct {
	tbTask
	Question *struct {
		Multi   bool `json:"multi"`
		Options []struct {
			Key   string `json:"key"`
			Label string `json:"label"`
		} `json:"options"`
		Recommend string `json:"recommend"`
		Answer    *struct {
			Picked []string `json:"picked"`
			Text   string   `json:"text"`
		} `json:"answer"`
	} `json:"question"`
	Waiting bool `json:"waiting"`
}

func tbDecodeQuestion(t *testing.T, code int, body string, want int) tbQuestionTask {
	t.Helper()
	require.Equal(t, want, code, body)
	var out tbQuestionTask
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	return out
}

// An agent asks the operator a question that its task waits on; the
// operator answers with a pick and a note: the task closes as done, the
// waiting task reopens, and the asker gets the answer by mail and on the
// answer route.
func TestTaskboardQuestionAskAndAnswer(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "the lanes"})
	mine := tbDecodeQuestion(t, rec.Code, rec.Body.String(), http.StatusCreated)

	q := map[string]any{"options": []map[string]any{
		{"label": "Amber", "description": "warm"}, {"label": "Teal"}, {"label": "No tint"}}, "recommend": "Amber"}
	rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "Lane colour?", "question": q, "blocks": mine.ID})
	asked := tbDecodeQuestion(t, rec.Code, rec.Body.String(), http.StatusCreated)
	require.NotNil(t, asked.Question)
	assert.Equal(t, []string{"amber", "teal", "no-tint"}, []string{asked.Question.Options[0].Key, asked.Question.Options[1].Key, asked.Question.Options[2].Key})
	assert.Equal(t, "amber", asked.Question.Recommend)
	assert.Nil(t, asked.Question.Answer)

	// Bad questions are refused.
	for _, bad := range []map[string]any{
		{"options": []map[string]any{{"label": "only one"}}},
		{"options": []map[string]any{{"label": "a"}, {"label": "a"}}},
		{"multi": true},
		{"options": []map[string]any{{"label": "a"}, {"label": "b"}}, "recommend": "c"},
	} {
		rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "bad", "question": bad})
		assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	}
	rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "to a peer", "to": h.peer, "question": q})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// Not yet answered; a short wait returns without an answer.
	rec = h.call(h.worker, http.MethodGet, "/v1/taskboard/tasks/"+asked.ID+"/answer?wait=300ms", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"answered":false`)
	rec = h.call("", http.MethodGet, "/v1/taskboard/tasks/"+asked.ID, nil)
	assert.True(t, tbDecodeQuestion(t, rec.Code, rec.Body.String(), http.StatusOK).Waiting, "the asker polled just now")

	// The asker does not answer; a peer without assign does not either; a
	// pick that is no option, or two on a pick-one question, is refused.
	code, body := h.post(h.worker, asked.ID, "answer", map[string]any{"picked": []string{"amber"}})
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = h.post(h.peer, asked.ID, "answer", map[string]any{"picked": []string{"amber"}})
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = h.post("", asked.ID, "answer", map[string]any{"picked": []string{"purple"}})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, body = h.post("", asked.ID, "answer", map[string]any{"picked": []string{"amber", "teal"}})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, body = h.post("", asked.ID, "answer", map[string]any{})
	assert.Equal(t, http.StatusBadRequest, code, body)

	// A waiter blocked on the answer sees it as soon as it lands.
	got := make(chan string, 1)
	go func() {
		rec := h.call(h.worker, http.MethodGet, "/v1/taskboard/tasks/"+asked.ID+"/answer?wait=20s", nil)
		got <- rec.Body.String()
	}()
	code, body = h.post("", asked.ID, "answer", map[string]any{"picked": []string{"Amber"}, "text": "dim it in dark mode"})
	answered := tbDecodeQuestion(t, code, body, http.StatusOK)
	assert.Equal(t, "done", answered.Status)
	require.NotNil(t, answered.Question.Answer)
	assert.Equal(t, []string{"amber"}, answered.Question.Answer.Picked)
	select {
	case s := <-got:
		assert.Contains(t, s, `"answered":true`)
	case <-time.After(15 * time.Second):
		t.Fatal("the waiting poll did not return with the answer")
	}

	code, body = h.post("", asked.ID, "answer", map[string]any{"text": "again"})
	assert.Equal(t, http.StatusConflict, code, "answered once: %s", body)

	got2, err := db.GetGroupTask(mine.ID)
	require.NoError(t, err)
	assert.Equal(t, db.GroupTaskOpen, got2.Status, "the waiting task reopens")
	subjects := strings.Join(tbInboxSubjects(t, h.worker), "\n")
	assert.Contains(t, subjects, "[taskboard] answered: crew · "+asked.ID)
	msgs, err := db.ListInboxForActor(h.worker, tbAgentID(t, h.worker), 50)
	require.NoError(t, err)
	var mail string
	for _, m := range msgs {
		if strings.Contains(m.Subject, "answered:") {
			mail = m.Body
		}
	}
	assert.Contains(t, mail, "Picked: Amber")
	assert.Contains(t, mail, "Said: dim it in dark mode")
	assert.Contains(t, mail, `"picked":["amber"]`)
}

// A question without options is answered in words; a plain task has no
// answer route.
func TestTaskboardQuestionInWords(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "What should the export cover?", "question": map[string]any{}})
	asked := tbDecodeQuestion(t, rec.Code, rec.Body.String(), http.StatusCreated)
	code, body := h.post("", asked.ID, "answer", map[string]any{"picked": []string{"x"}})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, body = h.post("", asked.ID, "answer", map[string]any{"text": "orders and refunds"})
	got := tbDecodeQuestion(t, code, body, http.StatusOK)
	assert.Equal(t, "orders and refunds", got.Question.Answer.Text)

	plain := h.add("plain", "next")
	code, body = h.post("", plain.ID, "answer", map[string]any{"text": "x"})
	assert.Equal(t, http.StatusConflict, code, body)
}
