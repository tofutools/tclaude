package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// The backlog (tk_5d2547b0): parked work is added, moved in and out with
// the CLI's own verbs, hidden from a plain ls behind a "+N in backlog" line,
// counted apart in the board's context, and a member parks only its own
// task.
func TestTaskboardBacklogFlow(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	bridgeAgentClientToMux(t, h.f.Mux) // as the operator
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}
	get := func(id string) tbTask {
		t.Helper()
		return h.task(h.call("", http.MethodGet, "/v1/taskboard/tasks/"+id, nil), http.StatusOK)
	}

	rc, out, errOut := run("add", "crew", "Parked from the start", "--backlog")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "[backlog, open]")
	idea := h.add("an idea", "idea")
	queued := h.add("queued", "next")

	// A plain ls leaves the backlog out and says so; --backlog lists it last.
	rc, out, errOut = run("ls", "crew")
	require.Equal(t, 0, rc, errOut)
	assert.NotContains(t, out, "Parked from the start")
	assert.Contains(t, out, "+1 in backlog")
	rc, out, errOut = run("ls", "crew", "--backlog")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "── backlog (parked) ──")
	assert.Contains(t, out, "Parked from the start")
	assert.NotContains(t, out, "in backlog:")
	rc, out, errOut = run("ls", "crew", "--json")
	require.Equal(t, 0, rc, errOut)
	var listed struct {
		Tasks         []tbTask `json:"tasks"`
		BacklogHidden int      `json:"backlog_hidden"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &listed))
	assert.Len(t, listed.Tasks, 2)
	assert.Equal(t, 1, listed.BacklogHidden)

	// backlog parks; promote steps back to the ideas, demote parks an idea.
	rc, out, errOut = run("backlog", queued.ID)
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "parked")
	assert.Equal(t, "backlog", get(queued.ID).Stage)
	rc, _, errOut = run("promote", queued.ID)
	require.Equal(t, 0, rc, errOut)
	assert.Equal(t, "idea", get(queued.ID).Stage)
	rc, _, errOut = run("promote", queued.ID)
	require.Equal(t, 0, rc, errOut)
	assert.Equal(t, "next", get(queued.ID).Stage)
	rc, _, errOut = run("demote", idea.ID)
	require.Equal(t, 0, rc, errOut)
	assert.Equal(t, "backlog", get(idea.ID).Stage)
	rc, _, errOut = run("move", idea.ID, "--stage", "idea")
	require.Equal(t, 0, rc, errOut)
	assert.Equal(t, "idea", get(idea.ID).Stage)
	rc, _, errOut = run("move", idea.ID, "--stage", "backlog")
	require.Equal(t, 0, rc, errOut)

	// The context counts the backlog apart, outside the live tasks.
	var ctx struct {
		Groups []struct {
			Group  string `json:"group"`
			Counts struct {
				Live    int `json:"live"`
				Ideas   int `json:"ideas"`
				Next    int `json:"next"`
				Backlog int `json:"backlog"`
			} `json:"counts"`
		} `json:"groups"`
	}
	rec := h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ctx))
	require.Len(t, ctx.Groups, 1)
	assert.Equal(t, 2, ctx.Groups[0].Counts.Backlog)
	assert.Equal(t, 1, ctx.Groups[0].Counts.Live)
	assert.Equal(t, 1, ctx.Groups[0].Counts.Next)

	// A member under taskboard.own parks its own started task, held, and
	// brings it back; another's task it may not park.
	own := h.task(h.call(h.peer, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "the peer's own"}), http.StatusCreated)
	code, body := h.post(h.peer, own.ID, "stage", map[string]any{"stage": "backlog"})
	require.Equal(t, http.StatusOK, code, body)
	var parked struct {
		Stage    string `json:"stage"`
		Held     bool   `json:"held"`
		Assignee *struct {
			AgentID string `json:"agent_id"`
		} `json:"assignee"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &parked))
	assert.Equal(t, "backlog", parked.Stage)
	assert.True(t, parked.Held, "a parked task is not started")
	require.NotNil(t, parked.Assignee)
	code, body = h.post(h.peer, own.ID, "stage", map[string]any{"stage": "idea"})
	require.Equal(t, http.StatusOK, code, body)
	code, _ = h.post(h.peer, queued.ID, "stage", map[string]any{"stage": "backlog"})
	assert.Equal(t, http.StatusForbidden, code)

	// An assigned add into the backlog is held there, not started.
	rc, out, errOut = run("add", "crew", "Later, for the worker", "--backlog", "--assignee", tbAgentID(t, h.worker))
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "[backlog, open]")
}
