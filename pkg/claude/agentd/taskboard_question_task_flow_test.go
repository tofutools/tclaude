package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Questions on any task (tk_8b2c86f7): an entry of the task's activity,
// numbered per task, answered right under it.

type tbAskedQuestion struct {
	N        int    `json:"n"`
	Title    string `json:"title"`
	State    string `json:"state"`
	AskedBy  string `json:"asked_by"`
	Blocks   bool   `json:"blocks"`
	EventID  int64  `json:"event_id"`
	AnswerID int64  `json:"answer_event_id"`
	Answer   *struct {
		Picked []string `json:"picked"`
		Text   string   `json:"text"`
	} `json:"answer"`
}

type tbTaskWithQuestions struct {
	tbTask
	Questions     []tbAskedQuestion `json:"questions"`
	OpenQuestions int               `json:"open_questions"`
	Question      *tbAskedQuestion  `json:"question"`
	Assignee      *struct {
		AgentID string `json:"agent_id"`
	} `json:"assignee"`
}

func (v tbTaskWithQuestions) AssigneeID() string {
	if v.Assignee == nil {
		return ""
	}
	return v.Assignee.AgentID
}

func (h *tbAssignHarness) askOn(conv, id string, body map[string]any) (int, string) {
	rec := h.call(conv, http.MethodPost, "/v1/taskboard/tasks/"+id+"/questions", body)
	return rec.Code, rec.Body.String()
}

func (h *tbAssignHarness) showQ(t *testing.T, id string) tbTaskWithQuestions {
	t.Helper()
	rec := h.call("", http.MethodGet, "/v1/taskboard/tasks/"+id, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out tbTaskWithQuestions
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// tbActivity reads a task's activity as the wire gives it.
func (h *tbAssignHarness) activityRaw(t *testing.T, id string) []map[string]any {
	t.Helper()
	rec := h.call("", http.MethodGet, "/v1/taskboard/tasks/"+id+"/activity", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Entries []map[string]any `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.Entries
}

// A worker asks on its own task and blocks it; the board counts the
// question as waiting on the operator; the operator answers; the answer
// lands under the question in the activity, the task is unblocked, and the
// worker gets the answer by mail.
func TestTaskboardQuestionOnTask(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Lane tint"})
	mine := h.task(rec, http.StatusCreated)

	code, body := h.askOn(h.worker, mine.ID, map[string]any{"title": "Lane colour?", "body": "The lane needs a tint.",
		"options": []map[string]any{{"label": "Amber"}, {"label": "Teal"}}, "recommend": "Amber", "block": true})
	require.Equal(t, http.StatusCreated, code, body)
	var asked struct {
		Task     tbTaskWithQuestions `json:"task"`
		Question tbAskedQuestion     `json:"question"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &asked))
	assert.Equal(t, 1, asked.Question.N)
	assert.Equal(t, "open", asked.Question.State)
	assert.Equal(t, "agent:"+tbAgentID(t, h.worker), asked.Question.AskedBy)
	assert.Equal(t, "blocked", asked.Task.Status)
	assert.Equal(t, "waiting on operator: Q1 Lane colour?", asked.Task.BlockReason)
	assert.Nil(t, asked.Task.Question, "a task asked on is no question task")

	// The board: the task stays the worker's; its question waits on the
	// operator.
	ctx := h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(t, http.StatusOK, ctx.Code)
	assert.Contains(t, ctx.Body.String(), `"waiting_on_you":1`)
	got := h.showQ(t, mine.ID)
	assert.Equal(t, 1, got.OpenQuestions)
	assert.Equal(t, tbAgentID(t, h.worker), got.AssigneeID())

	// The asker does not answer its own question.
	code, body = h.post(h.worker, mine.ID, "questions/1/answer", map[string]any{"picked": []string{"amber"}})
	assert.Equal(t, http.StatusForbidden, code, body)

	code, body = h.post("", mine.ID, "questions/Q1/answer", map[string]any{"picked": []string{"teal"}, "text": "but dimmer"})
	require.Equal(t, http.StatusOK, code, body)
	got = h.showQ(t, mine.ID)
	assert.Equal(t, "open", got.Status, "the answer unblocks the task it blocked")
	assert.Equal(t, 0, got.OpenQuestions)
	require.Len(t, got.Questions, 1)
	q := got.Questions[0]
	assert.Equal(t, "answered", q.State)
	require.NotNil(t, q.Answer)
	assert.Equal(t, []string{"teal"}, q.Answer.Picked)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.worker), "\n"), `[taskboard] answered Q1: crew · `+mine.ID)

	// The activity: the question entry, then the answer tied to it, then
	// the unblock that says why.
	var kinds []string
	for _, e := range h.activityRaw(t, mine.ID) {
		c, _ := e["comment"].(map[string]any)
		switch {
		case c != nil && c["question"] != nil:
			kinds = append(kinds, "question")
			assert.Equal(t, "The lane needs a tint.", c["body"])
		case c != nil && c["answers"] != nil:
			kinds = append(kinds, "answer")
			assert.Equal(t, "Answer: Teal; but dimmer", c["body"])
		case e["kind"] == "unblocked":
			kinds = append(kinds, "unblocked: "+e["detail"].(string))
		}
	}
	assert.Equal(t, []string{"question", "answer", "unblocked: Q1 answered"}, kinds)

	// Answered once; the answer route reads it.
	code, body = h.post("", mine.ID, "questions/1/answer", map[string]any{"text": "again"})
	assert.Equal(t, http.StatusConflict, code, body)
	rec = h.call(h.worker, http.MethodGet, "/v1/taskboard/tasks/"+mine.ID+"/questions/1", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"answered":true`)
}

// With several questions open, an answer names one; the asker withdraws
// one by deleting its entry; closing the task closes the rest.
func TestTaskboardQuestionsSeveralWithdrawClose(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Release"})
	mine := h.task(rec, http.StatusCreated)
	for _, title := range []string{"Which day?", "Who announces?", "Blog post?"} {
		code, body := h.askOn(h.worker, mine.ID, map[string]any{"title": title})
		require.Equal(t, http.StatusCreated, code, body)
	}
	// The operator does not ask its own questions.
	code, body := h.askOn("", mine.ID, map[string]any{"title": "Me?"})
	assert.Equal(t, http.StatusBadRequest, code, body)

	code, body = h.post("", mine.ID, "answer", map[string]any{"text": "Friday"})
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Contains(t, body, "which_question")
	code, body = h.post("", mine.ID, "questions/2/answer", map[string]any{"text": "the lead"})
	require.Equal(t, http.StatusOK, code, body)

	// Withdraw Q1: delete its entry.
	q1 := h.showQ(t, mine.ID).Questions[0]
	rec = h.call("", http.MethodDelete, "/v1/taskboard/tasks/"+mine.ID+"/comments/"+tbID(q1.EventID), nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, "only the asker withdraws it: %s", rec.Body.String())
	rec = h.call(h.worker, http.MethodDelete, "/v1/taskboard/tasks/"+mine.ID+"/comments/"+tbID(q1.EventID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// An answered question, and an answer, stay.
	q2 := h.showQ(t, mine.ID).Questions[1]
	rec = h.call(h.worker, http.MethodDelete, "/v1/taskboard/tasks/"+mine.ID+"/comments/"+tbID(q2.EventID), nil)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	rec = h.call(h.worker, http.MethodPatch, "/v1/taskboard/tasks/"+mine.ID+"/comments/"+tbID(q2.EventID), map[string]any{"body": "edited"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// Q3 is now the only open one: an answer without a number goes to it…
	// but first the task closes, and Q3 with it.
	code, body = h.post(h.worker, mine.ID, "report", map[string]any{"outcome": "done"})
	require.Equal(t, http.StatusOK, code, body)
	var states []string
	for _, q := range h.showQ(t, mine.ID).Questions {
		states = append(states, q.State)
	}
	assert.Equal(t, []string{"withdrawn", "answered", "closed"}, states)
	code, body = h.post("", mine.ID, "questions/3/answer", map[string]any{"text": "yes"})
	assert.Equal(t, http.StatusConflict, code, body)
}

// A question task (ask-operator <group>) is a task whose first question is
// the one it asks: stored as a question row, answered through its own
// route, and closed as done by the answer.
func TestTaskboardQuestionTaskIsFirstQuestion(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Ship it?",
		"question": map[string]any{"options": []map[string]any{{"label": "Yes"}, {"label": "No"}}}})
	asked := h.task(rec, http.StatusCreated)
	got := h.showQ(t, asked.ID)
	require.NotNil(t, got.Question)
	assert.Equal(t, 1, got.Question.N)
	assert.Equal(t, "Ship it?", got.Question.Title)
	require.Len(t, got.Questions, 1)

	q, err := db.GetGroupTaskQuestion(asked.ID, 1)
	require.NoError(t, err)
	assert.Equal(t, db.GroupTaskQuestionOpen, q.State)

	code, body := h.post("", asked.ID, "answer", map[string]any{"picked": []string{"Yes"}})
	require.Equal(t, http.StatusOK, code, body)
	got = h.showQ(t, asked.ID)
	assert.Equal(t, "done", got.Status)
	assert.Equal(t, "answered", got.Question.State)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.worker), "\n"), "[taskboard] answered: crew · "+asked.ID)
}

// A question task dropped and reopened can be answered again; withdrawing
// its only question drops it; a question's text is gated as a title is.
func TestTaskboardQuestionTaskReopenWithdrawText(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Ship it?", "question": map[string]any{}})
	asked := h.task(rec, http.StatusCreated)
	code, body := h.post("", asked.ID, "status", map[string]any{"status": "dropped"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "closed", h.showQ(t, asked.ID).Question.State)
	code, body = h.post("", asked.ID, "reopen", map[string]any{})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "open", h.showQ(t, asked.ID).Question.State, "the question closed with the task opens with it")
	code, body = h.post("", asked.ID, "answer", map[string]any{"text": "yes"})
	require.Equal(t, http.StatusOK, code, body)

	rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Never mind?", "question": map[string]any{}})
	other := h.task(rec, http.StatusCreated)
	q := h.showQ(t, other.ID).Question
	rec = h.call(h.worker, http.MethodDelete, "/v1/taskboard/tasks/"+other.ID+"/comments/"+tbID(q.EventID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := h.showQ(t, other.ID)
	assert.Equal(t, "dropped", got.Status, "withdrawing a question task's question withdraws the task")
	assert.Equal(t, "withdrawn", got.Question.State)

	rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Work"})
	work := h.task(rec, http.StatusCreated)
	for _, bad := range []map[string]any{{"title": "flip \u202e it"}, {"title": "ok", "body": "esc \x1b[31m red"}} {
		code, body = h.askOn(h.worker, work.ID, bad)
		assert.Equal(t, http.StatusBadRequest, code, body)
	}
}
