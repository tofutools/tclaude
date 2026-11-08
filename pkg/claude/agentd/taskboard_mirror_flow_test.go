package agentd_test

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Mirrors (tk_8b2c86f7): a notify-human message that asks something, and an
// agent's --ask-human prompt, are also questions on the board, answered in
// either place.

// mirrored finds the question mirrored from source on the board, by the
// store, and the task it is on.
func mirrored(t *testing.T, source string) (string, *db.GroupTaskQuestion) {
	t.Helper()
	id, q, err := db.GetGroupTaskQuestionBySource(source)
	require.NoError(t, err)
	return id, q
}

func (h *tbAssignHarness) notifyHuman(t *testing.T, conv string, body map[string]any) int64 {
	t.Helper()
	rec := testharness.Serve(h.f.Mux, agentd.AsAgentPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/notify-human", body), conv))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		ID int64 `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out.ID
}

func notifySource(id int64) string { return "notify:" + strconv.FormatInt(id, 10) }

// A worker's decision put to the operator lands on the task it works on;
// picking on the board decides the message, the pick reaches the worker as
// the Messages decision mail does (no taskboard mail of its own), and the
// answer is kept under the question.
func TestTaskboardMirrorNotifyDecisionAnsweredOnBoard(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	require.NoError(t, db.GrantAgentPermission(h.worker, agentd.PermHumanNotify, "test"))
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "Lane tint"})
	mine := h.task(rec, http.StatusCreated)
	current, err := db.CurrentGroupTaskOf(h.crew.ID, tbAgentID(t, h.worker))
	require.NoError(t, err)
	require.Equal(t, mine.ID, current, "the worker's own new task is the one it works on")

	// A plain notification is not mirrored.
	fyi := h.notifyHuman(t, h.worker, map[string]any{"subject": "Shipped", "body": "The lanes are out."})
	_, q := mirrored(t, notifySource(fyi))
	assert.Nil(t, q)

	id := h.notifyHuman(t, h.worker, map[string]any{"subject": "Lane colour", "body": "The lane needs a tint.",
		"options": []string{"Amber", "Teal"}, "recommended": 2})
	taskID, q := mirrored(t, notifySource(id))
	require.NotNil(t, q)
	assert.Equal(t, mine.ID, taskID)
	assert.Equal(t, "Lane colour", q.Title)
	assert.Equal(t, "2", q.Recommend)
	assert.Equal(t, "agent:"+tbAgentID(t, h.worker), q.AskedBy)

	got := h.showQ(t, mine.ID)
	assert.Equal(t, 1, got.OpenQuestions)

	// Its asker does not withdraw it on the board; an agent does not answer it.
	code, body := h.post(h.worker, mine.ID, "questions/1/answer", map[string]any{"picked": []string{"Teal"}})
	assert.NotEqual(t, http.StatusOK, code, body)
	code, body = h.post(h.lead, mine.ID, "questions/1/answer", map[string]any{"picked": []string{"Teal"}})
	assert.Equal(t, http.StatusForbidden, code, body)
	// A decision is answered with a pick.
	code, body = h.post("", mine.ID, "questions/1/answer", map[string]any{"text": "either"})
	assert.Equal(t, http.StatusBadRequest, code, body)

	code, body = h.post("", mine.ID, "questions/1/answer", map[string]any{"picked": []string{"Teal"}, "text": "dimmer"})
	require.Equal(t, http.StatusOK, code, body)
	m, err := db.GetHumanMessage(id)
	require.NoError(t, err)
	assert.Equal(t, "Teal", m.Decision)
	_, q = mirrored(t, notifySource(id))
	assert.Equal(t, db.GroupTaskQuestionAnswered, q.State)
	require.NotNil(t, q.Answer)
	assert.Equal(t, []string{"2"}, q.Answer.Picked)
	assert.Equal(t, "dimmer", q.Answer.Text)

	subjects := tbInboxSubjects(t, h.worker)
	assert.Contains(t, strings.Join(subjects, "\n"), "Re: Lane colour")
	for _, s := range subjects {
		assert.NotContains(t, s, "[taskboard]", "the board sends no mail of its own for a mirrored question")
	}

	// Answered already: in Messages it is decided.
	reply := testharness.Serve(dashMessageMux(t), testharness.JSONRequest(t, http.MethodPost, "/api/human-messages/reply", map[string]any{"id": id, "choice": 1}))
	assert.Equal(t, http.StatusConflict, reply.Code, reply.Body.String())
}

// Answered in Messages, the board's question is answered; dismissed or
// deleted there, it closes. An agent with no started task asks on a new
// question task for the operator.
func TestTaskboardMirrorNotifyAnsweredInMessages(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	require.NoError(t, db.GrantAgentPermission(h.worker, agentd.PermHumanNotify, "test"))
	h.f.HaveAliveSession(h.worker, "worker", "tclaude-worker", h.f.TestCwd("work"))
	mux := dashMessageMux(t)

	// A question in words: its subject ends in "?".
	id := h.notifyHuman(t, h.worker, map[string]any{"subject": "Which branch?", "body": "main or next."})
	taskID, q := mirrored(t, notifySource(id))
	require.NotNil(t, q)
	wrapper := h.showQ(t, taskID)
	assert.Equal(t, "Which branch?", wrapper.Title)
	assert.Equal(t, "operator", wrapper.Assignee.AgentID)
	require.NotNil(t, wrapper.Question)

	reply := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/human-messages/reply", map[string]any{"id": id, "body": "main"}))
	require.Equal(t, http.StatusOK, reply.Code, reply.Body.String())
	_, q = mirrored(t, notifySource(id))
	assert.Equal(t, db.GroupTaskQuestionAnswered, q.State)
	assert.Equal(t, "main", q.Answer.Text)
	assert.Equal(t, "done", h.showQ(t, taskID).Status, "answering its only question closes the question task")

	// A decision dismissed in Messages closes its question.
	dec := h.notifyHuman(t, h.worker, map[string]any{"body": "Ship it?\nThe build is green.", "options": []string{"Yes", "No"}})
	taskID, q = mirrored(t, notifySource(dec))
	require.NotNil(t, q)
	assert.Equal(t, "Ship it?", q.Title)
	wrapped, err := db.GetGroupTask(taskID)
	require.NoError(t, err)
	assert.Equal(t, "The build is green.", wrapped.Body)
	dismiss := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/human-messages/dismiss", map[string]any{"ids": []int64{dec}}))
	require.Equal(t, http.StatusOK, dismiss.Code, dismiss.Body.String())
	_, q = mirrored(t, notifySource(dec))
	assert.Equal(t, db.GroupTaskQuestionClosed, q.State)
	assert.Equal(t, "Dismissed in Messages", q.Answer.Text)
	assert.Equal(t, "dropped", h.showQ(t, taskID).Status)

	// Deleted in Messages, it closes too.
	gone := h.notifyHuman(t, h.worker, map[string]any{"subject": "Rename it?", "body": "To lanes.", "options": []string{"Yes", "No"}})
	del := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodPost, "/api/human-messages/delete", map[string]any{"id": gone}))
	require.Equal(t, http.StatusOK, del.Code, del.Body.String())
	_, q = mirrored(t, notifySource(gone))
	assert.Equal(t, db.GroupTaskQuestionClosed, q.State)
}

// An agent in no group is not mirrored.
func TestTaskboardMirrorUngroupedNotMirrored(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	require.NoError(t, db.GrantAgentPermission(h.lead, agentd.PermHumanNotify, "test"))
	id := h.notifyHuman(t, h.lead, map[string]any{"subject": "Any news?", "body": "?", "options": []string{"Yes", "No"}})
	_, q := mirrored(t, notifySource(id))
	assert.Nil(t, q)
}

// An agent's --ask-human prompt is a 🔐 question on the board while it is
// pending; deciding it on the board decides the prompt, and the question
// task made for it is archived. A prompt that times out closes, and the
// sweep at daemon start closes what a previous run left open.
func TestTaskboardMirrorApproval(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	worker := tbAgentID(t, h.worker)

	done := agentd.RaiseApprovalForTest("apr-board-1", agentd.PermTaskboardAssign, worker, h.worker, "worker", "crew", time.Minute)
	var taskID string
	var q *db.GroupTaskQuestion
	require.Eventually(t, func() bool {
		taskID, q = mirrored(t, "approval:apr-board-1")
		return q != nil
	}, 5*time.Second, 10*time.Millisecond, "the pending prompt is mirrored")
	assert.Equal(t, "worker asks for "+agentd.PermTaskboardAssign, q.Title)
	keys := []string{}
	for _, o := range q.Options {
		keys = append(keys, o.Key)
	}
	assert.Equal(t, []string{"approve", "deny"}, keys)

	// Words are not a decision.
	code, body := h.post("", taskID, "questions/1/answer", map[string]any{"picked": []string{"approve"}, "text": "ok"})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, body = h.post("", taskID, "questions/1/answer", map[string]any{"picked": []string{"Allow"}})
	require.Equal(t, http.StatusOK, code, body)
	select {
	case approved := <-done:
		assert.True(t, approved)
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt was not decided")
	}
	require.Eventually(t, func() bool {
		_, q = mirrored(t, "approval:apr-board-1")
		return q.State == db.GroupTaskQuestionAnswered
	}, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, []string{"approve"}, q.Answer.Picked)
	task, err := db.GetGroupTask(taskID)
	require.NoError(t, err)
	assert.Equal(t, db.GroupTaskDone, task.Status)
	assert.False(t, task.ArchivedAt.IsZero(), "a question task made for a prompt is archived when it closes")

	// Timed out: closed.
	done = agentd.RaiseApprovalForTest("apr-board-2", agentd.PermTaskboardAssign, worker, h.worker, "worker", "crew", 50*time.Millisecond)
	select {
	case approved := <-done:
		assert.False(t, approved)
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt did not time out")
	}
	_, q = mirrored(t, "approval:apr-board-2")
	require.NotNil(t, q)
	assert.Equal(t, db.GroupTaskQuestionClosed, q.State)
	assert.Equal(t, "Timed out: denied", q.Answer.Text)

	// A question a previous run left open closes at start.
	stale := &db.GroupTaskQuestion{Title: "worker asks for x", Source: "approval:apr-gone",
		Options: []db.GroupTaskQuestionOption{{Key: "approve", Label: "Allow"}, {Key: "deny", Label: "Deny"}}}
	taskID, _, err = db.MirrorGroupTaskQuestion(h.crew.ID, worker, stale)
	require.NoError(t, err)
	agentd.SweepMirroredApprovalsForTest()
	_, q = mirrored(t, "approval:apr-gone")
	assert.Equal(t, db.GroupTaskQuestionClosed, q.State)
	task, err = db.GetGroupTask(taskID)
	require.NoError(t, err)
	assert.Equal(t, db.GroupTaskDropped, task.Status)
	assert.False(t, task.ArchivedAt.IsZero())
}

// A linked node reading the board under its grants sees that the operator
// has a question there, not what it asks.
func TestTaskboardMirrorRedactedForPeers(t *testing.T) {
	var doc any
	require.NoError(t, json.Unmarshal([]byte(`{
		"tasks": [{"id": "tk_1", "status": "open", "title": "Ship it?", "body": "green",
			"question": {"n": 1, "source": "notify:7", "title": "Ship it?", "options": [{"key": "1", "label": "Yes"}], "answer": {"text": "yes"}},
			"questions": [{"n": 1, "source": "notify:7", "title": "Ship it?"}, {"n": 2, "title": "Plain?", "body": "kept"}]}],
		"entries": [
			{"kind": "comment", "comment": {"body": "secret context", "question": {"n": 1, "source": "approval:a", "title": "x asks for y"}}},
			{"kind": "comment", "comment": {"body": "the answer", "answers": 1}},
			{"kind": "comment", "comment": {"body": "a comment"}}
		]}`), &doc))
	agentd.RedactMirroredForTest(doc)
	out, err := json.Marshal(doc)
	require.NoError(t, err)
	s := string(out)
	for _, leak := range []string{"Ship it?", "green", "Yes", "secret context", "the answer", "x asks for y"} {
		assert.NotContains(t, s, leak)
	}
	for _, kept := range []string{"Plain?", "kept", "a comment"} {
		assert.Contains(t, s, kept)
	}
}
