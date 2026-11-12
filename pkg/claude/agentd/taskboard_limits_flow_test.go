package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// tbLimitTask is a task as the limits read it.
type tbLimitTask struct {
	ID             string `json:"id"`
	Stage          string `json:"stage"`
	Column         string `json:"column"`
	Status         string `json:"status"`
	Held           bool   `json:"held"`
	StartedAt      string `json:"started_at"`
	Queued         bool   `json:"queued"`
	QueuedFor      string `json:"queued_for"`
	QueuedReason   string `json:"queued_reason"`
	QueuedPosition int    `json:"queued_position"`
	Assignee       *struct {
		AgentID string `json:"agent_id"`
	} `json:"assignee"`
	Limit *struct {
		Lane     string `json:"lane"`
		Label    string `json:"label"`
		Max      int    `json:"max"`
		Count    int    `json:"count"`
		Per      string `json:"per"`
		Action   string `json:"action"`
		Position int    `json:"position"`
	} `json:"limit"`
}

func (v tbLimitTask) started() bool { return v.StartedAt != "" && !v.Held }

func tbDecodeLimit(t *testing.T, code int, body string, want int) tbLimitTask {
	t.Helper()
	require.Equal(t, want, code, body)
	var out tbLimitTask
	require.NoError(t, json.Unmarshal([]byte(body), &out), body)
	return out
}

func (h *tbAssignHarness) limitView(id string) tbLimitTask {
	h.t.Helper()
	rec := h.call("", http.MethodGet, "/v1/taskboard/tasks/"+id, nil)
	return tbDecodeLimit(h.t, rec.Code, rec.Body.String(), http.StatusOK)
}

// assign assigns task id to conv's agent (as the lead), answering the task.
func (h *tbAssignHarness) limitAssign(by, id, to string, extra map[string]any) (int, string) {
	body := map[string]any{"to": to}
	for k, v := range extra {
		body[k] = v
	}
	return h.post(by, id, "assign", body)
}

// At a full In progress, the default is to queue: the task is assigned held
// in Next, in line, and its assignee told it waits. When a slot frees, the
// first in line starts for its assignee, who gets its brief.
func TestTaskboardLimitQueue(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	h.setOptions(h.lead, map[string]string{"limits.in_progress.max": "1"})

	first := h.assigned("first")
	assert.True(t, h.limitView(first.ID).started())

	// The second waits: held, in line, its assignee told.
	second := h.add("second", "next")
	code, body := h.limitAssign(h.lead, second.ID, h.peer, nil)
	got := tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit)
	assert.Equal(t, "queued", got.Limit.Action)
	assert.Equal(t, "in_progress", got.Limit.Lane)
	assert.Equal(t, 1, got.Limit.Max)
	assert.Equal(t, 1, got.Limit.Count)
	assert.Equal(t, 1, got.Limit.Position)
	assert.False(t, got.started(), "a queued task is held")
	assert.Equal(t, "next", got.Stage)
	assert.True(t, got.Queued)
	assert.Equal(t, "in_progress", got.QueuedFor)
	assert.Equal(t, 1, got.QueuedPosition)
	assert.Equal(t, "In progress is full, 1st in line", got.QueuedReason)
	peerSubjects := tbInboxSubjects(t, h.peer)
	assert.Equal(t, 1, tbCountSubjects(peerSubjects, "queued for you, 1st in line for In progress"))
	assert.Zero(t, tbCountSubjects(peerSubjects, "assigned to you by"), "no brief yet")
	assert.Zero(t, tbCountSubjects(peerSubjects, "queued for In progress"), "nobody hears twice")
	assert.Contains(t, tbEventKinds(t, second.ID), db.GroupTaskEventQueued)

	// A third, for the worker, is second in line; starting it by hand only
	// keeps it in line.
	third := h.add("third", "next")
	code, body = h.limitAssign(h.lead, third.ID, h.worker, nil)
	got = tbDecodeLimit(t, code, body, http.StatusOK)
	assert.Equal(t, 2, got.Limit.Position)
	code, body = h.post(h.worker, third.ID, "start", nil)
	got = tbDecodeLimit(t, code, body, http.StatusOK)
	assert.Equal(t, "queued", got.Limit.Action)
	assert.False(t, got.started())
	assert.Equal(t, 2, h.limitView(third.ID).QueuedPosition, "it keeps its place")

	// The first is done: the second starts for the peer, with its brief,
	// and the third moves up.
	code, body = h.post(h.worker, first.ID, "report", map[string]any{"outcome": "done", "note": "ok"})
	require.Equal(t, http.StatusOK, code, body)
	v := h.limitView(second.ID)
	assert.True(t, v.started(), "first in line starts")
	assert.False(t, v.Queued)
	assert.Empty(t, v.QueuedFor)
	assert.Equal(t, 1, tbCountSubjects(tbInboxSubjects(t, h.peer), "started for you: a slot freed in In progress"))
	msgs := tbInboxBodies(t, h.peer, "started for you")
	require.Len(t, msgs, 1)
	assert.Contains(t, msgs[0], "report done --task "+second.ID, "the brief")
	v = h.limitView(third.ID)
	assert.False(t, v.started())
	assert.Equal(t, 1, v.QueuedPosition)
	assert.Equal(t, "In progress is full, 1st in line", v.QueuedReason)
}

// A task created for an agent meets the limit too; a task in line leaves it
// when it changes hands or closes; raising the limit lets the line in.
func TestTaskboardLimitQueueCreateAndRaise(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	h.setOptions(h.lead, map[string]string{"limits.in_progress.max": "1"})
	h.assigned("holds the slot")

	add := func(title string) tbLimitTask {
		rec := h.call(h.lead, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
			map[string]any{"title": title, "to": h.peer})
		return tbDecodeLimit(t, rec.Code, rec.Body.String(), http.StatusCreated)
	}
	a, b, c := add("a"), add("b"), add("c")
	for i, v := range []tbLimitTask{a, b, c} {
		require.NotNil(t, v.Limit, v.ID)
		assert.Equal(t, "queued", v.Limit.Action)
		assert.Equal(t, i+1, v.Limit.Position)
		assert.False(t, v.started())
	}
	assert.Equal(t, 3, tbCountSubjects(tbInboxSubjects(t, h.peer), "queued for you"))

	// Taken back, a task leaves the line; dropped, too.
	code, body := h.post("", a.ID, "unassign", nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, h.limitView(a.ID).QueuedFor)
	code, body = h.post("", b.ID, "status", map[string]any{"status": "dropped"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, h.limitView(b.ID).QueuedFor)
	assert.Equal(t, 1, h.limitView(c.ID).QueuedPosition)

	// Room for three: the last in line starts.
	h.setOptions(h.lead, map[string]string{"limits.in_progress.max": "3"})
	assert.True(t, h.limitView(c.ID).started())
	assert.False(t, h.limitView(a.ID).started(), "out of line, it is not started")
}

// refuse refuses a change into a full lane, 409 limit_reached; an agent may
// not go over the limit; the operator may, and warn lets anyone in, saying so.
func TestTaskboardLimitRefuseWarnAndOverride(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	h.setOptions(h.lead, map[string]string{"limits.in_progress.max": "1", "limits.at_limit": "refuse"})
	h.assigned("holds the slot")

	task := h.add("waits", "next")
	code, body := h.limitAssign(h.lead, task.ID, h.peer, nil)
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, body, "limit_reached")
	assert.Contains(t, body, "In progress is full (1 of 1)")
	assert.Nil(t, h.limitView(task.ID).Assignee, "refused, nothing changed")

	code, body = h.limitAssign(h.lead, task.ID, h.peer, map[string]any{"over_limit": true})
	assert.Equal(t, http.StatusForbidden, code, "agents follow the limit")
	assert.Contains(t, body, "only the operator")

	code, body = h.limitAssign("", task.ID, h.peer, map[string]any{"over_limit": true})
	got := tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit)
	assert.Equal(t, "over", got.Limit.Action)
	assert.True(t, got.started())
	assert.Equal(t, 2, h.contextCounts()["in_progress"])

	// warn: in, over the limit, and the answer says so; limit_reached to
	// the owners tells the lead.
	h.setOptions(h.lead, map[string]string{"limits.at_limit": "warn", "events.limit_reached.to": "owners"})
	warned := h.add("warned", "next")
	code, body = h.limitAssign("", warned.ID, h.worker, nil)
	got = tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit)
	assert.Equal(t, "warned", got.Limit.Action)
	assert.Equal(t, 2, got.Limit.Count)
	assert.True(t, got.started())
	assert.Equal(t, 1, tbCountSubjects(tbInboxSubjects(t, h.lead), "let in over the limit"))

	// limit_reached off: nobody hears.
	h.setOptions(h.lead, map[string]string{"events.limit_reached.enabled": "off"})
	quiet := h.add("quiet", "next")
	code, body = h.limitAssign("", quiet.ID, h.worker, nil)
	tbDecodeLimit(t, code, body, http.StatusOK)
	assert.Equal(t, 1, tbCountSubjects(tbInboxSubjects(t, h.lead), "let in over the limit"))
}

// Per assignee, each agent has its own In progress: another agent's task
// still starts, the same agent's waits, and a release frees its slot.
func TestTaskboardLimitPerAssignee(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	h.setOptions(h.lead, map[string]string{"limits.in_progress.max": "1", "limits.per": "assignee"})
	mine := h.assigned("worker's")

	other := h.add("peer's", "next")
	code, body := h.limitAssign(h.lead, other.ID, h.peer, nil)
	got := tbDecodeLimit(t, code, body, http.StatusOK)
	assert.Nil(t, got.Limit)
	assert.True(t, got.started())

	more := h.add("worker's second", "next")
	code, body = h.limitAssign(h.lead, more.ID, h.worker, nil)
	got = tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit)
	assert.Equal(t, "queued", got.Limit.Action)
	assert.Equal(t, "assignee", got.Limit.Per)

	// Reassigning the peer's task to the worker is a new slot of the
	// worker's: queued.
	code, body = h.limitAssign(h.lead, other.ID, h.worker, nil)
	got = tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit)
	assert.Equal(t, 2, got.Limit.Position)

	code, body = h.post(h.worker, mine.ID, "unassign", map[string]any{"self": true})
	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, h.limitView(more.ID).started(), "the worker's first in line")
	assert.False(t, h.limitView(other.ID).started())
}

// A column's limit: a move into a full column queues the task where it is
// (In progress), with the column's own at_limit over the board's; the idle
// check passes it over; when the column frees it moves in.
func TestTaskboardLimitColumn(t *testing.T) {
	h := newTaskboardIdleBoardFlow(t)
	now := time.Now()
	h.columns("", http.MethodPost, "", map[string]any{"label": "In review"}, http.StatusCreated)
	h.setOptions(h.lead, map[string]string{"limits.at_limit": "refuse", "limits.columns.in-review.max": "1",
		"limits.columns.in-review.at_limit": "queue", "idle.enabled": "on", "idle.grace": "1m"})
	reviewed, waiting, working := h.assigned("in review"), h.assigned("waits for review"), h.assigned("still working")

	code, body := h.post(h.worker, reviewed.ID, "stage", map[string]any{"stage": "in-review"})
	assert.Nil(t, tbDecodeLimit(t, code, body, http.StatusOK).Limit)
	code, body = h.post(h.worker, waiting.ID, "stage", map[string]any{"stage": "in-review"})
	got := tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit)
	assert.Equal(t, "queued", got.Limit.Action, "the column's own at_limit")
	assert.Equal(t, "In review", got.Limit.Label)
	assert.Empty(t, got.Column, "it stays in In progress")
	assert.True(t, got.started())
	assert.Equal(t, "in-review", got.QueuedFor)
	assert.Equal(t, "In review is full, 1st in line", got.QueuedReason)

	agentd.SweepTaskboardIdleForTest(now.Add(time.Hour))
	assert.Zero(t, tbStalledEvents(t, waiting.ID), "waiting for review is not idling")
	assert.Equal(t, 1, tbStalledEvents(t, working.ID))

	// The reviewed task goes back to work (In progress has no limit, so the
	// board's refuse does not apply): the column frees and the waiting one
	// moves in.
	code, body = h.post(h.worker, reviewed.ID, "stage", map[string]any{"stage": "next"})
	require.Equal(t, http.StatusOK, code, body)
	v := h.limitView(waiting.ID)
	assert.Equal(t, "in-review", v.Column)
	assert.Empty(t, v.QueuedFor)

	// Removing the column takes its limit and its line with it.
	code, body = h.post(h.worker, working.ID, "stage", map[string]any{"stage": "in-review"})
	assert.Equal(t, "in-review", tbDecodeLimit(t, code, body, http.StatusOK).QueuedFor)
	h.columns("", http.MethodDelete, "/in-review", nil, http.StatusOK)
	assert.Empty(t, h.limitView(working.ID).QueuedFor)
	rec := h.call("", http.MethodGet, tbOptionsPath, nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "in-review", "the column's limit is pruned")
}

// A reopen into a full In progress queues the task; the sweep lets the line
// in when a slot frees by something no route saw.
func TestTaskboardLimitReopenAndSweep(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	closed := h.assigned("closed early")
	code, body := h.post(h.worker, closed.ID, "report", map[string]any{"outcome": "done", "note": "ok"})
	require.Equal(t, http.StatusOK, code, body)
	h.setOptions(h.lead, map[string]string{"limits.in_progress.max": "1"})
	busy := h.assigned("busy")

	code, body = h.post("", closed.ID, "reopen", map[string]any{})
	got := tbDecodeLimit(t, code, body, http.StatusOK)
	require.NotNil(t, got.Limit)
	assert.Equal(t, "queued", got.Limit.Action)
	assert.Equal(t, "open", got.Status)
	assert.False(t, got.started(), "it comes back held, in line")
	assert.Equal(t, 1, tbCountSubjects(tbInboxSubjects(t, h.worker), "queued for you"))

	// Closed behind the daemon's routes: the sweep fills the slot.
	_, err := db.SetGroupTaskStatus(busy.ID, db.GroupTaskDone, "", "test")
	require.NoError(t, err)
	assert.False(t, h.limitView(closed.ID).started())
	agentd.SweepTaskboardQueuesForTest()
	assert.True(t, h.limitView(closed.ID).started())
}

// The CLI shows the limits, sets them, and says what a full lane did.
func TestTaskboardLimitsCLI(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	saveTaskboardConfig(t, nil)
	bridgeAgentClientToMux(t, h.f.Mux) // as the operator
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}
	rc, out, errOut := run("options", "show", "crew")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "limits:")
	assert.Regexp(t, `limits.in_progress\s+max none\s+queue\s+default`, out)

	rc, out, errOut = run("options", "set", "crew", "limits.in_progress.max=1", "limits.at_limit=refuse")
	require.Equal(t, 0, rc, errOut)
	assert.Regexp(t, `limits.at_limit\s+refuse\s+set on this board`, out)
	assert.Regexp(t, `limits.in_progress\s+max 1\s+refuse\s+set on this board`, out)

	h.assigned("holds the slot")
	task := h.add("waits", "next")
	rc, _, errOut = run("assign", task.ID, "--to", h.peer)
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, errOut, "In progress is full (1 of 1)")
	rc, out, errOut = run("assign", task.ID, "--to", h.peer, "--over-limit")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "over the limit: In progress is full (1 of 1)")

	rc, _, errOut = run("options", "set", "crew", "limits.at_limit=queue")
	require.Equal(t, 0, rc, errOut)
	queued := h.add("queued", "next")
	rc, out, errOut = run("assign", queued.ID, "--to", h.worker)
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "queued: In progress is full (2 of 1); 1st in line")
}
