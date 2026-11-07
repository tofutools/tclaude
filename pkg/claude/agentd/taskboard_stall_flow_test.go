package agentd_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// newTaskboardStallFlow is the assign flow with the worker live in a pane and
// taskboard.idle_grace set to grace.
func newTaskboardStallFlow(t *testing.T, grace string) *tbAssignHarness {
	h := newTaskboardAssignFlow(t)
	require.NoError(t, config.Save(&config.Config{
		Features:  &config.FeaturesConfig{Taskboard: true},
		Agent:     &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfTaskboard}},
		Taskboard: &config.TaskboardConfig{IdleGrace: grace},
	}))
	h.f.HaveAliveSession(h.worker, "tb-worker-session", "tb-worker-pane", h.f.TestCwd("worker"))
	return h
}

type tbStallView struct {
	ID        string `json:"id"`
	Stalled   bool   `json:"stalled"`
	IdleSince string `json:"idle_since"`
	Status    string `json:"status"`
	Assignee  *struct {
		AgentID string `json:"agent_id"`
	} `json:"assignee"`
}

func (h *tbAssignHarness) stallView(id string) tbStallView {
	rec := h.call("", http.MethodGet, "/v1/taskboard/tasks/"+id, nil)
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var v tbStallView
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &v))
	return v
}

func tbStalledEvents(t *testing.T, id string) int {
	events, err := db.ListGroupTaskEvents(id)
	require.NoError(t, err)
	n := 0
	for _, e := range events {
		if e.Kind == db.GroupTaskEventStalled {
			n++
		}
	}
	return n
}

// An assignee idle past the grace stalls its task: the assigner is told and
// the assignee nudged once per assignment, the board, the context route and
// the fleet brief count it as needing the operator, and a new turn clears it
// without failing or unassigning anything.
func TestTaskboardIdleWithoutReport(t *testing.T) {
	h := newTaskboardStallFlow(t, "1ms")
	task := h.add("port the sweep", "next")
	code, body := h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)

	// Working: not stalled, nothing sent.
	h.f.SetSessionStatus(h.worker, session.StatusWorking)
	time.Sleep(5 * time.Millisecond)
	agentd.SweepTaskboardIdleForTest(time.Now())
	assert.False(t, h.stallView(task.ID).Stalled)
	assert.Zero(t, tbStalledEvents(t, task.ID))

	// Idle past the grace: stalled.
	h.f.SetSessionStatus(h.worker, session.StatusIdle)
	time.Sleep(5 * time.Millisecond)
	v := h.stallView(task.ID)
	assert.True(t, v.Stalled)
	assert.NotEmpty(t, v.IdleSince)

	leadBefore := len(tbInboxSubjects(t, h.lead))
	workerBefore := len(tbInboxSubjects(t, h.worker))
	agentd.SweepTaskboardIdleForTest(time.Now())
	agentd.SweepTaskboardIdleForTest(time.Now()) // once per assignment
	assert.Equal(t, 1, tbStalledEvents(t, task.ID))

	lead := tbInboxSubjects(t, h.lead)
	require.Len(t, lead, leadBefore+1)
	assert.Contains(t, lead[len(lead)-1], "went idle on "+task.ID+" without reporting")
	msgs, err := db.ListInboxForActor(h.worker, tbAgentID(t, h.worker), 50)
	require.NoError(t, err)
	require.Len(t, msgs, workerBefore+1)
	nudge := msgs[len(msgs)-1]
	for _, m := range msgs {
		if strings.Contains(m.Subject, "you went idle") {
			nudge = m
		}
	}
	assert.Contains(t, nudge.Subject, "you went idle without reporting")
	assert.Contains(t, nudge.Body, "tclaude agent taskboard report done --task "+task.ID)
	assert.Empty(t, nudge.FromConv, "the nudge is from the board, not a peer")
	rec := h.call(h.worker, http.MethodGet, fmt.Sprintf("/v1/messages/%d?keep-unread=1", nudge.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var read struct {
		FromTitle string `json:"from_title"`
		Replyable bool   `json:"replyable"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &read))
	assert.Equal(t, "tclaude (system)", read.FromTitle, "tclaude's own mail is not shown as the operator's")
	assert.False(t, read.Replyable)

	// The context route counts it as needing the operator.
	rec = h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var ctx struct {
		Groups []struct {
			Group  string         `json:"group"`
			Counts map[string]int `json:"counts"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ctx))
	require.Len(t, ctx.Groups, 1)
	assert.Equal(t, 1, ctx.Groups[0].Counts["stalled"])
	assert.Equal(t, 1, ctx.Groups[0].Counts["needs_you"])
	assert.Equal(t, 0, ctx.Groups[0].Counts["in_progress"])

	// So does the fleet brief, with blocked tasks alongside.
	blocked := h.add("needs a decision", "next")
	code, body = h.post("", blocked.ID, "status", map[string]any{"status": "blocked", "reason": "which API?"})
	require.Equal(t, http.StatusOK, code, body)
	rec = h.call("", http.MethodGet, "/v1/fleet", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var brief struct {
		Attention []struct {
			Kind   string `json:"kind"`
			Title  string `json:"title"`
			Detail string `json:"detail"`
		} `json:"attention"`
		Taskboards []struct {
			Group    string `json:"group"`
			NeedsYou int    `json:"needs_you"`
			Blocked  int    `json:"blocked"`
			Stalled  int    `json:"stalled"`
		} `json:"taskboards"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &brief))
	require.Len(t, brief.Taskboards, 1)
	assert.Equal(t, "crew", brief.Taskboards[0].Group)
	assert.Equal(t, 2, brief.Taskboards[0].NeedsYou)
	assert.Equal(t, 1, brief.Taskboards[0].Blocked)
	assert.Equal(t, 1, brief.Taskboards[0].Stalled)
	var line string
	for _, a := range brief.Attention {
		if a.Kind == "taskboard" {
			line = a.Title + " " + a.Detail
		}
	}
	assert.Equal(t, "crew board: 2 tasks need you (1 stalled, 1 blocked)", line)

	// A new turn clears it; the task stays open and assigned.
	h.f.SetSessionStatus(h.worker, session.StatusWorking)
	v = h.stallView(task.ID)
	assert.False(t, v.Stalled)
	assert.Equal(t, "open", v.Status)
	require.NotNil(t, v.Assignee)
	assert.Equal(t, tbAgentID(t, h.worker), v.Assignee.AgentID)

	// Idle again within the same assignment: no second announcement.
	h.f.SetSessionStatus(h.worker, session.StatusIdle)
	time.Sleep(5 * time.Millisecond)
	agentd.SweepTaskboardIdleForTest(time.Now())
	assert.Equal(t, 1, tbStalledEvents(t, task.ID))
	assert.Len(t, tbInboxSubjects(t, h.lead), leadBefore+1)

	// A report ends the assignment and the stall.
	code, body = h.post(h.worker, task.ID, "report", map[string]any{"outcome": "done", "note": "ported"})
	require.Equal(t, http.StatusOK, code, body)
	assert.False(t, h.stallView(task.ID).Stalled)
}

// An operator-assigned stall goes to Messages; idle_grace 0 turns the check
// off.
func TestTaskboardIdleOperatorAndOff(t *testing.T) {
	h := newTaskboardStallFlow(t, "1ms")
	task := h.add("operator's task", "next")
	code, body := h.post("", task.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	h.f.SetSessionStatus(h.worker, session.StatusIdle)
	time.Sleep(5 * time.Millisecond)
	agentd.SweepTaskboardIdleForTest(time.Now())
	msgs, err := db.ListHumanMessagesPage(db.HumanMessageFilter{}, 0, 20)
	require.NoError(t, err)
	var found bool
	for _, m := range msgs {
		found = found || strings.Contains(m.Subject, "went idle on "+task.ID+" without reporting")
	}
	assert.True(t, found, "the operator is told in Messages")

	other := h.add("second task", "next")
	code, body = h.post("", other.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	require.NoError(t, config.Save(&config.Config{
		Features:  &config.FeaturesConfig{Taskboard: true},
		Agent:     &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfTaskboard}},
		Taskboard: &config.TaskboardConfig{IdleGrace: "0"},
	}))
	time.Sleep(5 * time.Millisecond)
	agentd.SweepTaskboardIdleForTest(time.Now())
	assert.Zero(t, tbStalledEvents(t, other.ID))
	assert.False(t, h.stallView(other.ID).Stalled)
}

// Time spent blocked is not idling on the task: an unblock restarts the clock.
func TestTaskboardIdleClockRestartsOnUnblock(t *testing.T) {
	h := newTaskboardStallFlow(t, "1h")
	task := h.add("waits on a decision", "next")
	code, body := h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)

	// The worker was assigned three hours ago and has been idle since then.
	long := time.Now().Add(-3 * time.Hour)
	d, err := db.Open()
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE group_tasks SET assigned_at = ? WHERE id = ?`, long.UnixNano(), task.ID)
	require.NoError(t, err)
	_, err = d.Exec(`UPDATE group_task_events SET at = ? WHERE task_id = ?`, long.UnixNano(), task.ID)
	require.NoError(t, err)
	h.f.SetSessionStatus(h.worker, session.StatusIdle)
	rows, err := db.FindSessionsByConvID(h.worker)
	require.NoError(t, err)
	for _, r := range rows {
		r.LastHook = long
		require.NoError(t, db.SaveSession(r))
	}

	// It was blocked all that time and has just been unblocked.
	code, body = h.post("", task.ID, "status", map[string]any{"status": "blocked", "reason": "which API?"})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post("", task.ID, "status", map[string]any{"status": "open"})
	require.Equal(t, http.StatusOK, code, body)

	agentd.SweepTaskboardIdleForTest(time.Now())
	assert.Zero(t, tbStalledEvents(t, task.ID), "not stalled right after the unblock")
	assert.False(t, h.stallView(task.ID).Stalled)

	agentd.SweepTaskboardIdleForTest(time.Now().Add(2 * time.Hour))
	assert.Equal(t, 1, tbStalledEvents(t, task.ID), "stalled once idle past the grace since the unblock")
}

// A new assignment is announced again; a task the agent took for itself only
// nudges the agent.
func TestTaskboardIdleReassignAndSelfTaken(t *testing.T) {
	h := newTaskboardStallFlow(t, "1ms")
	h.f.SetSessionStatus(h.worker, session.StatusIdle)
	task := h.add("handed around", "next")
	code, body := h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	time.Sleep(5 * time.Millisecond)
	agentd.SweepTaskboardIdleForTest(time.Now())
	require.Equal(t, 1, tbStalledEvents(t, task.ID))

	// Away to the peer and back: a fresh assignment, announced again.
	code, body = h.post(h.lead, task.ID, "assign", map[string]any{"to": h.peer})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	time.Sleep(5 * time.Millisecond)
	agentd.SweepTaskboardIdleForTest(time.Now())
	assert.Equal(t, 2, tbStalledEvents(t, task.ID))

	// Taken by the worker itself: no one else to tell, the worker is nudged.
	own := h.add("my own", "next")
	code, body = h.post(h.worker, own.ID, "assign", map[string]any{"to": ""})
	require.Equal(t, http.StatusOK, code, body)
	leadBefore := len(tbInboxSubjects(t, h.lead))
	humanBefore, err := db.CountHumanMessages(db.HumanMessageFilter{})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	agentd.SweepTaskboardIdleForTest(time.Now())
	assert.Equal(t, 1, tbStalledEvents(t, own.ID))
	assert.Len(t, tbInboxSubjects(t, h.lead), leadBefore)
	humanAfter, err := db.CountHumanMessages(db.HumanMessageFilter{})
	require.NoError(t, err)
	assert.Equal(t, humanBefore, humanAfter)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.worker), "\n"), own.ID+" \"my own\" — you went idle without reporting")
}
