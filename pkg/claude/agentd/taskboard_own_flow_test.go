package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// taskboard.own (tcl-h4zc3i): a member with no write grant tracks its own
// work on its group's board. What it adds is assigned to it; it changes its
// own tasks, not others'; it takes any unassigned task but never steals one.
func TestTaskboardOwnTracksOwnWork(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	add := func(conv string, body map[string]any) (int, string) {
		rec := h.call(conv, http.MethodPost, "/v1/groups/crew/taskboard/tasks", body)
		return rec.Code, rec.Body.String()
	}
	decode := func(body string) tbAssignTask {
		var out tbAssignTask
		require.NoError(t, json.Unmarshal([]byte(body), &out), body)
		return out
	}
	worker := tbAgentID(t, h.worker)

	// What a member adds is assigned to it, in progress; the history says it
	// took it.
	code, body := add(h.worker, map[string]any{"title": "fix the flaky test"})
	require.Equal(t, http.StatusCreated, code, body)
	mine := decode(body)
	assert.Equal(t, "next", mine.Stage)
	require.NotNil(t, mine.Assignee)
	assert.Equal(t, worker, mine.Assignee.AgentID)
	assert.Empty(t, tbInboxSubjects(t, h.worker), "its own task briefs nobody")

	// An unassigned task (in next, or an idea) and an import take write.
	for _, req := range []map[string]any{
		{"title": "queued for anyone", "stage": "next"},
		{"title": "an idea", "stage": "idea"},
		{"lines": []string{"one", "two"}},
	} {
		code, body = add(h.worker, req)
		assert.Equal(t, http.StatusForbidden, code, "%v: %s", req, body)
		assert.Contains(t, body, agentd.PermTaskboardWrite, "%v", req)
	}
	// Blocked and done start states stay its own.
	code, body = add(h.worker, map[string]any{"title": "waits on CI", "status": "blocked", "block_reason": "CI is down"})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, worker, decode(body).Assignee.AgentID)
	// Naming someone else still takes taskboard.assign.
	code, body = add(h.worker, map[string]any{"title": "for the peer", "to": h.peer})
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, agentd.PermTaskboardAssign)

	// Its own task: edit, demote/promote, block, unblock, close.
	rec := h.call(h.worker, http.MethodPatch, "/v1/taskboard/tasks/"+mine.ID, map[string]any{"body": "root cause: a fixed sleep"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	code, body = h.post(h.worker, mine.ID, "status", map[string]any{"status": "blocked", "reason": "needs a fake clock"})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post(h.worker, mine.ID, "status", map[string]any{"status": "open"})
	require.Equal(t, http.StatusOK, code, body)
	// Reordering is a write, even of its own task.
	code, body = h.post(h.worker, mine.ID, "move", map[string]any{"top": true})
	assert.Equal(t, http.StatusForbidden, code, body)

	// Someone else's task, and an unassigned one, it may not change.
	others := h.add("the operator's", "next")
	rec = h.call(h.worker, http.MethodPatch, "/v1/taskboard/tasks/"+others.ID, map[string]any{"title": "renamed"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, body = h.post(h.worker, others.ID, "stage", map[string]any{"stage": "idea"})
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = h.post(h.worker, others.ID, "status", map[string]any{"status": "done"})
	assert.Equal(t, http.StatusForbidden, code, body)

	// It takes any unassigned task, an idea included, and never steals one.
	idea := h.add("an operator idea", "")
	code, body = h.post(h.peer, idea.ID, "assign", map[string]any{"to": ""})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, "next", decode(body).Stage, "the idea is promoted on the take")
	code, body = h.post(h.worker, idea.ID, "assign", map[string]any{"to": ""})
	assert.Equal(t, http.StatusConflict, code, "no stealing: %s", body)
	// Now the peer's, it changes it; then releases it.
	rec = h.call(h.peer, http.MethodPatch, "/v1/taskboard/tasks/"+idea.ID, map[string]any{"title": "the peer's now"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	code, body = h.post(h.peer, idea.ID, "unassign", map[string]any{"self": true})
	require.Equal(t, http.StatusOK, code, body)
	rec = h.call(h.peer, http.MethodPatch, "/v1/taskboard/tasks/"+idea.ID, map[string]any{"title": "not mine any more"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// done closes its own task.
	code, body = h.post(h.worker, mine.ID, "status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)

	// The owner keeps the broad rights: an unassigned idea, or --mine.
	code, body = add(h.lead, map[string]any{"title": "plan the sprint"})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Nil(t, decode(body).Assignee, "write adds unassigned")
}

// add --mine assigns a write holder's task to itself; without it, the task
// is unassigned. A member holding only taskboard.own is assigned regardless.
func TestTaskboardAddMine(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	h.groupWrites("crew")
	bridgeAgentClientToMuxAsAgent(t, h.f.Mux, h.worker)
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}
	rc, out, errOut := run("add", "crew", "an idea of mine", "--json")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	var idea tbAssignTask
	require.NoError(t, json.Unmarshal([]byte(out), &idea), out)
	assert.Equal(t, "idea", idea.Stage)
	assert.Nil(t, idea.Assignee)

	rc, out, errOut = run("add", "crew", "on it now", "--mine", "--json")
	require.Equal(t, 0, rc, "out=%s err=%s", out, errOut)
	var mine tbAssignTask
	require.NoError(t, json.Unmarshal([]byte(out), &mine), out)
	require.NotNil(t, mine.Assignee)
	assert.Equal(t, tbAgentID(t, h.worker), mine.Assignee.AgentID)
	assert.Equal(t, "next", mine.Stage)

	rc, _, errOut = run("add", "crew", "both", "--mine", "--assignee", h.peer)
	assert.NotEqual(t, 0, rc)
	assert.Contains(t, errOut, "--mine or --assignee")
}

// taskboard.own never reaches a board of a group the agent is not an active
// member of, whatever a grant's scope says; an explicit deny of the
// deprecated self.taskboard still denies it.
func TestTaskboardOwnStaysInOwnGroup(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	const outsider = "tb-own-outsider-4444"
	h.agent(t, outsider)
	require.NoError(t, db.SetAgentPermissionOverride(outsider, agentd.PermTaskboardRead, db.PermEffectGrant, "test"))
	require.NoError(t, db.SetAgentPermissionOverride(outsider, agentd.PermTaskboardOwn, db.PermEffectGrant, "test"))
	rec := h.call(outsider, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "from outside"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	task := h.add("open to members", "next")
	code, body := h.post(outsider, task.ID, "assign", map[string]any{"to": ""})
	assert.NotEqual(t, http.StatusOK, code, body)

	require.NoError(t, db.SetAgentPermissionOverride(h.worker, agentd.PermSelfTaskboard, db.PermEffectDeny, "test"))
	rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "denied"})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	code, body = h.post(h.worker, task.ID, "assign", map[string]any{"to": ""})
	assert.Equal(t, http.StatusForbidden, code, body)
	// The other member is untouched.
	code, body = h.post(h.peer, task.ID, "assign", map[string]any{"to": ""})
	assert.Equal(t, http.StatusOK, code, body)
}
