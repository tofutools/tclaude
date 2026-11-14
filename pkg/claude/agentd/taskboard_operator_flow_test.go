package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

type tbOperatorTask struct {
	tbTask
	BlockedOn string `json:"blocked_on"`
	Assignee  *struct {
		AgentID  string `json:"agent_id"`
		Operator bool   `json:"operator"`
	} `json:"assignee"`
	AssignedBy string `json:"assigned_by"`
}

func tbDecodeOperatorTask(t *testing.T, code int, body string, want int) tbOperatorTask {
	t.Helper()
	require.Equal(t, want, code, body)
	var out tbOperatorTask
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	return out
}

// A member asks the operator about its own work in one call (--blocks): its
// task waits, blocked on the operator task. The operator closes it as done
// with a note: the waiting task reopens and its assignee is mailed. A task
// blocked for another reason that also depends on it stays blocked.
func TestTaskboardOperatorAskBlocksAndUnblocks(t *testing.T) {
	h := newTaskboardAssignFlow(t)

	// The worker's own task (taskboard.own: assigned to it as created).
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "ship the lanes"})
	mine := tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusCreated)

	// A member may not block someone else's task on an operator task.
	rec = h.call(h.peer, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "should I?", "to": "operator", "blocks": mine.ID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// The worker asks the operator, and its task waits, in one write.
	rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "which colour for the lane?", "to": "operator", "blocks": mine.ID})
	op := tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusCreated)
	require.NotNil(t, op.Assignee)
	assert.True(t, op.Assignee.Operator)
	assert.Equal(t, "operator", op.Assignee.AgentID)
	assert.Equal(t, "next", op.Stage)
	assert.Equal(t, "agent:"+tbAgentID(t, h.worker), op.AssignedBy)

	rec = h.call("", http.MethodGet, "/v1/taskboard/tasks/"+mine.ID, nil)
	waiting := tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusOK)
	assert.Equal(t, "blocked", waiting.Status)
	assert.Equal(t, "waiting on operator: which colour for the lane?", waiting.BlockReason)
	assert.Equal(t, op.ID, waiting.BlockedOn)
	assert.Equal(t, []string{op.ID}, waiting.DependsOn)

	// A second task, blocked for its own reason, also depends on it.
	rec = h.call(h.peer, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "other work", "after": []string{op.ID}, "status": "blocked", "block_reason": "CI is red"})
	other := tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusCreated)

	// The context counts it as waiting on you, and needs you; the two
	// blocked tasks do not need the operator (tk_3cb51ce0).
	rec = h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var ctx struct {
		Groups []struct {
			Group  string `json:"group"`
			Counts struct {
				NeedsYou     int `json:"needs_you"`
				WaitingOnYou int `json:"waiting_on_you"`
				InProgress   int `json:"in_progress"`
			} `json:"counts"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ctx))
	require.Len(t, ctx.Groups, 1)
	assert.Equal(t, 1, ctx.Groups[0].Counts.WaitingOnYou)
	assert.Equal(t, 1, ctx.Groups[0].Counts.NeedsYou, "only the operator task")

	// The fleet brief raises the board for the operator task alone.
	rec = h.call("", http.MethodGet, "/v1/fleet", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var brief struct {
		Attention []struct {
			Kind   string `json:"kind"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"attention"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &brief))
	var line string
	for _, a := range brief.Attention {
		if a.Kind == "taskboard" {
			line = a.Title + " " + a.Detail
		}
	}
	assert.Equal(t, "crew board: 1 task waits on you", line)

	// An agent cannot close the operator's task; the worker who asked
	// cannot report on it either.
	code, body := h.post(h.peer, op.ID, "status", map[string]any{"status": "done"})
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = h.post(h.worker, op.ID, "report", map[string]any{"outcome": "done"})
	assert.Equal(t, http.StatusConflict, code, body)
	code, body = h.post(h.worker, op.ID, "status", map[string]any{"status": "done"})
	assert.Equal(t, http.StatusForbidden, code, "the agent that asked does not answer its own question: %s", body)

	// The operator closes it as done, with a note.
	code, body = h.post("", op.ID, "status", map[string]any{"status": "done", "reason": "use amber"})
	require.Equal(t, http.StatusOK, code, body)

	rec = h.call("", http.MethodGet, "/v1/taskboard/tasks/"+mine.ID, nil)
	reopened := tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusOK)
	assert.Equal(t, "open", reopened.Status, "the waiting task is unblocked")
	assert.Empty(t, reopened.BlockedOn)
	rec = h.call("", http.MethodGet, "/v1/taskboard/tasks/"+other.ID, nil)
	still := tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusOK)
	assert.Equal(t, "blocked", still.Status, "a task blocked for another reason stays blocked")
	assert.Equal(t, "CI is red", still.BlockReason)

	subjects := strings.Join(tbInboxSubjects(t, h.worker), "\n")
	assert.Contains(t, subjects, "[taskboard] unblocked: "+op.ID+" is done")
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.peer), "\n"), "[taskboard] still blocked: "+op.ID+" is done")
}

// The two-step path: ask the operator, then block your own task on it with
// block --on. Assigning the operator a task someone else holds needs
// taskboard.assign; the owner may.
func TestTaskboardOperatorTwoStepAndPermissions(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "mine"})
	mine := tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusCreated)
	rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "a question", "to": "operator"})
	op := tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusCreated)

	// block --on only waits on an operator task.
	code, body := h.post(h.worker, mine.ID, "status", map[string]any{"status": "blocked", "on": mine.ID})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, body = h.post(h.worker, mine.ID, "status", map[string]any{"status": "blocked", "on": op.ID})
	got := tbDecodeOperatorTask(t, code, body, http.StatusOK)
	assert.Equal(t, op.ID, got.BlockedOn)
	assert.Contains(t, got.DependsOn, op.ID)

	// The worker that asked may withdraw (drop) its question; its task
	// stays blocked, and it is told.
	code, body = h.post(h.worker, op.ID, "status", map[string]any{"status": "dropped", "reason": "figured it out"})
	require.Equal(t, http.StatusOK, code, body)
	rec = h.call("", http.MethodGet, "/v1/taskboard/tasks/"+mine.ID, nil)
	assert.Equal(t, "blocked", tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusOK).Status)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.worker), "\n"), "[taskboard] still blocked: "+op.ID+" is dropped")

	// An ordinary task closed as done mails nobody about the tasks queued
	// after it.
	first := h.add("first", "next")
	rec = h.call(h.peer, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "after first", "after": []string{first.ID}})
	tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusCreated)
	before := len(tbInboxSubjects(t, h.peer))
	code, body = h.post("", first.ID, "status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Len(t, tbInboxSubjects(t, h.peer), before)

	// Handing the operator a task someone else holds needs taskboard.assign.
	idea := h.add("someone else's", "")
	code, body = h.post(h.lead, idea.ID, "assign", map[string]any{"to": h.peer})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post(h.worker, idea.ID, "assign", map[string]any{"to": "operator"})
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, agentd.PermTaskboardAssign)
	code, body = h.post(h.lead, idea.ID, "assign", map[string]any{"to": "operator"})
	handed := tbDecodeOperatorTask(t, code, body, http.StatusOK)
	require.NotNil(t, handed.Assignee)
	assert.True(t, handed.Assignee.Operator)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.peer), "\n"), "reassigned by")

	// A member hands the operator a task it holds itself.
	code, body = h.post(h.worker, mine.ID, "status", map[string]any{"status": "open"})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post(h.worker, mine.ID, "assign", map[string]any{"to": "operator"})
	tbDecodeOperatorTask(t, code, body, http.StatusOK)
}
