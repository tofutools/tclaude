package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Column states (tk_bf5d2b02): a board hides a column (custom, ideas or
// done) or cordons a custom one, which then takes no new task.

type tbColumnStates struct {
	Columns []struct {
		Slug  string `json:"slug"`
		State string `json:"state"`
	} `json:"columns"`
	BuiltinColumns map[string]string `json:"builtin_columns"`
}

func (h *tbAssignHarness) columnState(conv, slug, state string, want int) tbColumnStates {
	h.t.Helper()
	rec := h.call(conv, http.MethodPost, "/v1/groups/crew/taskboard/columns/"+url.PathEscape(slug)+"/state", map[string]any{"state": state})
	require.Equal(h.t, want, rec.Code, rec.Body.String())
	var out tbColumnStates
	if want < 300 {
		require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return out
}

func (c tbColumnStates) stateOf(slug string) string {
	for _, col := range c.Columns {
		if col.Slug == slug {
			return col.State
		}
	}
	return c.BuiltinColumns[slug]
}

// Hiding and cordoning: which columns take which state, who may set them,
// and that the board, its columns and its context all report them.
func TestTaskboardColumnStates(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	h.columns("", http.MethodPost, "", map[string]any{"label": "In review"}, http.StatusCreated)

	got := h.columnState(h.lead, "In review", "cordoned", http.StatusOK)
	assert.Equal(t, "cordoned", got.stateOf("in-review"), "found by label")
	got = h.columnState(h.lead, "idea", "hidden", http.StatusOK)
	assert.Equal(t, "hidden", got.stateOf("ideas"))
	got = h.columnState("", "done", "hidden", http.StatusOK)
	assert.Equal(t, "hidden", got.stateOf("done"))

	h.columnState("", "ideas", "cordoned", http.StatusBadRequest)
	h.columnState("", "next", "hidden", http.StatusBadRequest)
	h.columnState("", "blocked", "hidden", http.StatusBadRequest)
	h.columnState("", "no-such", "hidden", http.StatusNotFound)
	h.columnState("", "in-review", "bogus", http.StatusBadRequest)
	h.columnState(h.worker, "in-review", "hidden", http.StatusForbidden)

	rec := h.call("", http.MethodGet, "/v1/groups/crew/taskboard", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var board tbColumnStates
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &board))
	assert.Equal(t, "cordoned", board.stateOf("in-review"))
	assert.Equal(t, map[string]string{"ideas": "hidden", "done": "hidden"}, board.BuiltinColumns)

	rec = h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var ctx struct{ Groups []tbColumnStates }
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ctx))
	require.Len(t, ctx.Groups, 1)
	assert.Equal(t, "cordoned", ctx.Groups[0].stateOf("in-review"))
	assert.Equal(t, "hidden", ctx.Groups[0].stateOf("ideas"))

	got = h.columnState("", "in-review", "shown", http.StatusOK)
	assert.Empty(t, got.stateOf("in-review"))
	got = h.columnState("", "ideas", "shown", http.StatusOK)
	assert.Equal(t, map[string]string{"done": "hidden"}, got.BuiltinColumns)

	// Removing a column removes its state: a new column of that slug starts shown.
	h.columnState("", "in-review", "hidden", http.StatusOK)
	h.columns("", http.MethodDelete, "/in-review", nil, http.StatusOK)
	h.columns("", http.MethodPost, "", map[string]any{"label": "In review"}, http.StatusCreated)
	assert.Empty(t, h.columnState("", "done", "hidden", http.StatusOK).stateOf("in-review"))
}

// taskboard.columns alone manages the columns; taskboard.manage covers it.
func TestTaskboardColumnStatesPermission(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	h.columns(h.worker, http.MethodPost, "", map[string]any{"label": "In review"}, http.StatusForbidden)
	require.NoError(t, db.GrantAgentPermissionWithScope(h.worker, agentd.PermTaskboardColumns, `{"group":["crew"]}`, "human"))
	h.columns(h.worker, http.MethodPost, "", map[string]any{"label": "In review"}, http.StatusCreated)
	h.columnState(h.worker, "in-review", "cordoned", http.StatusOK)

	h.columnState(h.peer, "in-review", "shown", http.StatusForbidden)
	require.NoError(t, db.GrantAgentPermissionWithScope(h.peer, agentd.PermTaskboardManage, `{"group":["crew"]}`, "human"))
	h.columnState(h.peer, "in-review", "shown", http.StatusOK)
}

// A cordoned column takes no new task, whichever way it comes in: a move,
// a create, a reopen, or a transfer from another group. Its own tasks stay
// and may leave.
func TestTaskboardColumnCordonRefusesNewTasks(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	h.columns("", http.MethodPost, "", map[string]any{"label": "In review"}, http.StatusCreated)
	inside := h.add("already in review", "next")
	code, body := h.post("", inside.ID, "stage", map[string]any{"stage": "in-review"})
	require.Equal(t, http.StatusOK, code, body)
	h.columnState("", "in-review", "cordoned", http.StatusOK)
	assert.Equal(t, "in-review", h.columnTask(inside.ID).Column, "a cordon keeps the tasks in it")

	outside := h.add("wants in", "next")
	code, body = h.post("", outside.ID, "stage", map[string]any{"stage": "in-review"})
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Contains(t, body, "cordoned")

	rec := h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "new", "stage": "in-review"})
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	code, body = h.post("", outside.ID, "status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post("", outside.ID, "reopen", map[string]any{"stage": "in-review"})
	assert.Equal(t, http.StatusConflict, code, body)

	// From another group: it lands outside the cordoned column.
	ops := h.f.HaveGroup("ops")
	_, err := db.AddGroupTaskColumn(ops.ID, "in-review", "In review", "", "test")
	require.NoError(t, err)
	rec = h.call("", http.MethodPost, "/v1/groups/ops/taskboard/tasks", map[string]any{"title": "from ops", "stage": "in-review"})
	moving := tbDecodeColumnTask(t, rec.Code, rec.Body.String(), http.StatusCreated)
	require.Equal(t, "in-review", moving.Column)
	code, _, raw := h.transfer("", moving.ID, map[string]any{"group": "crew"})
	require.Equal(t, http.StatusOK, code, raw)
	assert.Empty(t, h.columnTask(moving.ID).Column, "a cordoned column takes no task from another group")

	// The task already in it leaves freely.
	code, body = h.post("", inside.ID, "stage", map[string]any{"stage": "next"})
	assert.Equal(t, http.StatusOK, code, body)
	h.columnState("", "in-review", "shown", http.StatusOK)
	code, body = h.post("", h.add("later", "next").ID, "stage", map[string]any{"stage": "in-review"})
	assert.Equal(t, http.StatusOK, code, body, "uncordoned, it takes tasks again")
}

// With Ideas hidden, a new or imported task without a stage starts in next;
// a named stage still holds.
func TestTaskboardIdeasHiddenStartsInNext(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	rec := h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "before"})
	assert.Equal(t, "idea", h.task(rec, http.StatusCreated).Stage)
	h.columnState("", "ideas", "hidden", http.StatusOK)
	rec = h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "after"})
	assert.Equal(t, "next", h.task(rec, http.StatusCreated).Stage)
	rec = h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "named", "stage": "idea"})
	assert.Equal(t, "idea", h.task(rec, http.StatusCreated).Stage)

	rec = h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"lines": []string{"imported"}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var imp struct {
		Results []struct{ Task *tbTask } `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &imp))
	require.Len(t, imp.Results, 1)
	require.NotNil(t, imp.Results[0].Task)
	assert.Equal(t, "next", imp.Results[0].Task.Stage)
}

// The CLI: hide, show, cordon (disable) and uncordon (enable), and the
// states in columns ls, ls and the summary.
func TestTaskboardColumnStatesCLI(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	bridgeAgentClientToMux(t, h.f.Mux)
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}
	rc, _, errOut := run("columns", "add", "crew", "In review")
	require.Equal(t, 0, rc, errOut)
	task := h.add("ship it", "next")
	rc, _, errOut = run("move", task.ID, "--stage", "in-review")
	require.Equal(t, 0, rc, errOut)

	rc, out, errOut := run("columns", "disable", "crew", "in-review")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "In review [in-review] (cordoned)")
	rc, out, errOut = run("columns", "hide", "crew", "ideas")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "crew columns: Ideas (hidden), Next")

	rc, out, errOut = run("ls", "crew")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "In review [in-review] (cordoned)")
	assert.Contains(t, out, "(hidden on the board: ideas;")
	rc, out, errOut = run("ls", "--summary")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "1 In review (cordoned)")

	rc, out, errOut = run("columns", "enable", "crew", "in-review")
	require.Equal(t, 0, rc, errOut)
	assert.NotContains(t, out, "(cordoned)")
	rc, out, errOut = run("columns", "show", "crew", "ideas")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "crew columns: Ideas, Next")
}
