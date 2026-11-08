package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Assigned ideas (tk_3826822a): an assignee holds a task without starting
// it, in Ideas or Next, until it is started.

type tbHeldTask struct {
	tbTask
	Held     bool `json:"held"`
	Assignee *struct {
		AgentID  string `json:"agent_id"`
		Operator bool   `json:"operator"`
	} `json:"assignee"`
}

func tbDecodeHeld(t *testing.T, code int, body string, want int) tbHeldTask {
	t.Helper()
	require.Equal(t, want, code, body)
	var out tbHeldTask
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	return out
}

func (h *tbAssignHarness) heldView(id string) tbHeldTask {
	rec := h.call("", http.MethodGet, "/v1/taskboard/tasks/"+id, nil)
	return tbDecodeHeld(h.t, rec.Code, rec.Body.String(), http.StatusOK)
}

// A task given to an agent as an idea stays an idea in its lane: a short
// note, not the brief, no stall clock. Starting it promotes it and starts
// the clock; holding it again puts it back.
func TestTaskboardHeldIdeaStartsAndHolds(t *testing.T) {
	h := newTaskboardStallFlow(t, "1ms")
	before := len(tbInboxSubjects(t, h.worker))
	rec := h.call(h.lead, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "look into caching", "to": h.worker, "start": false, "stage": "idea"})
	idea := tbDecodeHeld(t, rec.Code, rec.Body.String(), http.StatusCreated)
	assert.True(t, idea.Held)
	assert.Equal(t, "idea", idea.Stage)
	require.NotNil(t, idea.Assignee)
	assert.Equal(t, tbAgentID(t, h.worker), idea.Assignee.AgentID)
	subjects := tbInboxSubjects(t, h.worker)
	require.Len(t, subjects, before+1)
	assert.Contains(t, subjects[len(subjects)-1], "an idea for you to triage")

	// A held task never stalls, however long its assignee idles.
	h.f.SetSessionStatus(h.worker, session.StatusIdle)
	agentd.SweepTaskboardIdleForTest(time.Now().Add(time.Hour))
	assert.Zero(t, tbStalledEvents(t, idea.ID))

	// The context counts it as an idea, not in progress.
	counts := h.contextCounts()
	assert.Equal(t, 1, counts["ideas"])
	assert.Zero(t, counts["in_progress"])

	// Someone else may not start it; its assignee may.
	code, body := h.post(h.peer, idea.ID, "start", nil)
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = h.post(h.worker, idea.ID, "start", nil)
	started := tbDecodeHeld(t, code, body, http.StatusOK)
	assert.False(t, started.Held)
	assert.Equal(t, "next", started.Stage, "an idea started is promoted")
	counts = h.contextCounts()
	assert.Zero(t, counts["ideas"])
	assert.Equal(t, 1, counts["in_progress"]+counts["stalled"], "started: in progress (stalled, its agent idles)")
	agentd.SweepTaskboardIdleForTest(time.Now().Add(time.Hour))
	assert.Equal(t, 1, tbStalledEvents(t, idea.ID), "a started task stalls")

	// Held again, back in the ideas.
	code, body = h.post(h.worker, idea.ID, "start", map[string]any{"start": false, "stage": "idea"})
	back := tbDecodeHeld(t, code, body, http.StatusOK)
	assert.True(t, back.Held)
	assert.Equal(t, "idea", back.Stage)
	require.NotNil(t, back.Assignee, "holding keeps the assignee")

	// start names no stage; an unassigned task is not started.
	code, body = h.post(h.worker, idea.ID, "start", map[string]any{"stage": "next"})
	assert.Equal(t, http.StatusBadRequest, code, body)
	free := h.add("nobody's", "next")
	code, body = h.post(h.lead, free.ID, "start", nil)
	assert.Equal(t, http.StatusConflict, code, body)
}

// assign --hold queues a task for its assignee without starting it; a
// plain assign of a task it holds starts it with the brief.
func TestTaskboardAssignHoldThenAssignStarts(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	task := h.add("queued work", "idea")
	code, body := h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker, "start": false, "stage": "next"})
	held := tbDecodeHeld(t, code, body, http.StatusOK)
	assert.True(t, held.Held)
	assert.Equal(t, "next", held.Stage)
	subjects := strings.Join(tbInboxSubjects(t, h.worker), "\n")
	assert.Contains(t, subjects, "queued for you")
	assert.NotContains(t, subjects, "assigned to you by")

	// A stage without start false is refused.
	code, body = h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker, "stage": "next"})
	assert.Equal(t, http.StatusBadRequest, code, body)

	code, body = h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker})
	started := tbDecodeHeld(t, code, body, http.StatusOK)
	assert.False(t, started.Held)
	assert.True(t, h.heldView(task.ID).Assignee != nil)
}

// The operator's ideas are not waiting on it: counted apart, and nothing
// waits on one. Queued for the operator, a task waits on it as before.
func TestTaskboardOperatorIdeaIsNotWaiting(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "mine"})
	mine := tbDecodeHeld(t, rec.Code, rec.Body.String(), http.StatusCreated)
	rec = h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "an idea for me", "to": "operator", "start": false, "stage": "idea"})
	idea := tbDecodeHeld(t, rec.Code, rec.Body.String(), http.StatusCreated)
	assert.True(t, idea.Held)

	counts := h.contextCounts()
	assert.Equal(t, 1, counts["ideas_for_you"])
	assert.Zero(t, counts["waiting_on_you"])

	// Nothing waits on an idea.
	code, body := h.post(h.worker, mine.ID, "status", map[string]any{"status": "blocked", "on": idea.ID})
	assert.Equal(t, http.StatusBadRequest, code, body)

	// --blocks lands a held operator task in Next, where it is waited on.
	rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "which one?", "to": "operator", "start": false, "stage": "idea", "blocks": mine.ID})
	asked := tbDecodeHeld(t, rec.Code, rec.Body.String(), http.StatusCreated)
	assert.Equal(t, "next", asked.Stage)
	counts = h.contextCounts()
	assert.Equal(t, 1, counts["waiting_on_you"])
	assert.Equal(t, 1, counts["ideas_for_you"])
}

func (h *tbAssignHarness) contextCounts() map[string]int {
	rec := h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var ctx struct {
		Groups []struct {
			Counts map[string]int `json:"counts"`
		} `json:"groups"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &ctx))
	require.Len(h.t, ctx.Groups, 1)
	return ctx.Groups[0].Counts
}
