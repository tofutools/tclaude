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
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Custom stage columns (tcl-plby5h): a group's own columns, such as In
// review, between In progress and Blocked.

type tbColumns struct {
	Columns []struct {
		Slug  string `json:"slug"`
		Label string `json:"label"`
	} `json:"columns"`
	Moved []string `json:"moved"`
}

func (c tbColumns) slugs() []string {
	out := []string{}
	for _, col := range c.Columns {
		out = append(out, col.Slug)
	}
	return out
}

func (h *tbAssignHarness) columns(conv, method, path string, body any, want int) tbColumns {
	h.t.Helper()
	rec := h.call(conv, method, "/v1/groups/crew/taskboard/columns"+path, body)
	require.Equal(h.t, want, rec.Code, rec.Body.String())
	var out tbColumns
	if want < 300 {
		require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	}
	return out
}

type tbColumnTask struct {
	tbTask
	Column  string `json:"column"`
	Stalled bool   `json:"stalled"`
}

func tbDecodeColumnTask(t *testing.T, code int, body string, want int) tbColumnTask {
	t.Helper()
	require.Equal(t, want, code, body)
	var out tbColumnTask
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	return out
}

// Owners manage the columns; members cannot. Labels make slugs, built-in
// names are refused, the cap holds, and reorder and rename keep the slug.
func TestTaskboardColumnsManage(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	assert.Empty(t, h.columns(h.worker, http.MethodGet, "", nil, http.StatusOK).Columns)

	h.columns(h.worker, http.MethodPost, "", map[string]any{"label": "In review"}, http.StatusForbidden)
	got := h.columns(h.lead, http.MethodPost, "", map[string]any{"label": "In review"}, http.StatusCreated)
	assert.Equal(t, []string{"in-review"}, got.slugs())
	got = h.columns("", http.MethodPost, "", map[string]any{"label": "In CI"}, http.StatusCreated)
	assert.Equal(t, []string{"in-review", "in-ci"}, got.slugs())

	h.columns("", http.MethodPost, "", map[string]any{"label": "Blocked"}, http.StatusBadRequest)
	h.columns("", http.MethodPost, "", map[string]any{"label": "in review!"}, http.StatusConflict)
	got = h.columns("", http.MethodPost, "", map[string]any{"label": "Design", "before": "in-review"}, http.StatusCreated)
	assert.Equal(t, []string{"design", "in-review", "in-ci"}, got.slugs())
	h.columns("", http.MethodPost, "", map[string]any{"label": "QA"}, http.StatusCreated)
	h.columns("", http.MethodPost, "", map[string]any{"label": "Fifth"}, http.StatusConflict)

	got = h.columns("", http.MethodPatch, "/design", map[string]any{"to_end": true}, http.StatusOK)
	assert.Equal(t, []string{"in-review", "in-ci", "qa", "design"}, got.slugs())
	got = h.columns("", http.MethodPatch, "/in-ci", map[string]any{"label": "In CI (macOS)"}, http.StatusOK)
	assert.Equal(t, "In CI (macOS)", got.Columns[1].Label)
	assert.Equal(t, "in-ci", got.Columns[1].Slug, "a rename keeps the slug")
	h.columns(h.worker, http.MethodDelete, "/qa", nil, http.StatusForbidden)
	got = h.columns(h.lead, http.MethodDelete, "/qa", nil, http.StatusOK)
	assert.Equal(t, []string{"in-review", "in-ci", "design"}, got.slugs())
	h.columns("", http.MethodDelete, "/qa", nil, http.StatusNotFound)

	// The board lists its columns.
	rec := h.call(h.worker, http.MethodGet, "/v1/groups/crew/taskboard", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var board tbColumns
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &board))
	assert.Equal(t, []string{"in-review", "in-ci", "design"}, board.slugs())
}

// A worker moves its own task into In review: it keeps its assignee, never
// stalls there, counts in the column, and leaves it on next, done or when the
// column is removed.
func TestTaskboardColumnsTaskLifecycle(t *testing.T) {
	h := newTaskboardStallFlow(t, "1ms")
	h.columns("", http.MethodPost, "", map[string]any{"label": "In review"}, http.StatusCreated)
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "my PR"})
	task := tbDecodeColumnTask(t, rec.Code, rec.Body.String(), http.StatusCreated)

	// Someone else's task is not the peer's to move; its own worker moves it,
	// by slug or by label.
	code, body := h.post(h.peer, task.ID, "stage", map[string]any{"stage": "in-review"})
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = h.post(h.worker, task.ID, "stage", map[string]any{"stage": "no-such"})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, body = h.post(h.worker, task.ID, "stage", map[string]any{"stage": "In review"})
	moved := tbDecodeColumnTask(t, code, body, http.StatusOK)
	assert.Equal(t, "in-review", moved.Column)
	assert.Equal(t, "next", moved.Stage)

	// Its agent idles with the PR open: no stall, no alarm.
	h.f.SetSessionStatus(h.worker, session.StatusIdle)
	agentd.SweepTaskboardIdleForTest(time.Now().Add(time.Hour))
	assert.Zero(t, tbStalledEvents(t, task.ID))
	assert.False(t, h.columnTask(task.ID).Stalled)
	counts := h.contextCounts()
	assert.Zero(t, counts["in_progress"]+counts["stalled"])
	assert.Equal(t, 1, h.contextColumns()["in-review"])

	// Back to In progress: the stall clock runs again.
	code, body = h.post(h.worker, task.ID, "stage", map[string]any{"stage": "next"})
	assert.Empty(t, tbDecodeColumnTask(t, code, body, http.StatusOK).Column)
	agentd.SweepTaskboardIdleForTest(time.Now().Add(time.Hour))
	assert.Equal(t, 1, tbStalledEvents(t, task.ID))

	// Closing clears it; a reopen comes back in progress.
	code, body = h.post(h.worker, task.ID, "stage", map[string]any{"stage": "in-review"})
	require.Equal(t, http.StatusOK, code, body)
	code, body = h.post("", task.ID, "status", map[string]any{"status": "done"})
	assert.Empty(t, tbDecodeColumnTask(t, code, body, http.StatusOK).Column)
	code, body = h.post("", task.ID, "reopen", map[string]any{})
	assert.Empty(t, tbDecodeColumnTask(t, code, body, http.StatusOK).Column)

	// An idea moved into a column is promoted; a demote takes it out.
	idea := h.add("an idea", "idea")
	code, body = h.post("", idea.ID, "stage", map[string]any{"stage": "in-review"})
	got := tbDecodeColumnTask(t, code, body, http.StatusOK)
	assert.Equal(t, "next", got.Stage)
	assert.Equal(t, "in-review", got.Column)
	code, body = h.post("", idea.ID, "stage", map[string]any{"stage": "idea"})
	assert.Empty(t, tbDecodeColumnTask(t, code, body, http.StatusOK).Column)

	// Removing the column sends its tasks back to next, with history.
	code, body = h.post(h.worker, task.ID, "stage", map[string]any{"stage": "in-review"})
	require.Equal(t, http.StatusOK, code, body)
	removed := h.columns("", http.MethodDelete, "/in-review", nil, http.StatusOK)
	assert.Equal(t, []string{task.ID}, removed.Moved)
	assert.Empty(t, h.columnTask(task.ID).Column)
	assert.Contains(t, tbEventKinds(t, task.ID), "column")
}

// The CLI: columns add/ls/rm, move --stage, and the column in ls and the
// summary.
func TestTaskboardColumnsCLI(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	bridgeAgentClientToMux(t, h.f.Mux) // as the operator
	run := func(args ...string) (int, string, string) {
		t.Helper()
		stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
		rc := agent.RunTaskboardCLI(args, stdout, stderr)
		return rc, stdout.String(), stderr.String()
	}
	rc, out, errOut := run("columns", "add", "crew", "In review")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "In review [in-review]")
	task := h.add("ship it", "next")
	rc, out, errOut = run("move", task.ID, "--stage", "in-review")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "[in-review, open]")
	rc, out, errOut = run("ls", "crew")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "(custom columns: In review [in-review])")
	assert.Contains(t, out, task.ID+"  in-review")
	rc, out, errOut = run("ls", "--summary")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "1 In review")
	rc, _, _ = run("move", task.ID, "--stage", "in-review", "--top")
	assert.NotEqual(t, 0, rc, "--stage takes no position")
	rc, out, errOut = run("columns", "rm", "crew", "in-review")
	require.Equal(t, 0, rc, errOut)
	assert.Contains(t, out, "back to next: "+task.ID)
	assert.Contains(t, out, "no custom columns")
}

func (h *tbAssignHarness) columnTask(id string) tbColumnTask {
	rec := h.call("", http.MethodGet, "/v1/taskboard/tasks/"+id, nil)
	return tbDecodeColumnTask(h.t, rec.Code, rec.Body.String(), http.StatusOK)
}

func (h *tbAssignHarness) contextColumns() map[string]int {
	rec := h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var ctx struct {
		Groups []struct {
			Columns []struct {
				Slug  string `json:"slug"`
				Count int    `json:"count"`
			} `json:"columns"`
		} `json:"groups"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &ctx))
	require.Len(h.t, ctx.Groups, 1)
	out := map[string]int{}
	for _, c := range ctx.Groups[0].Columns {
		out[c.Slug] = c.Count
	}
	return out
}

func tbEventKinds(t *testing.T, id string) []string {
	t.Helper()
	events, err := db.ListGroupTaskEvents(id)
	require.NoError(t, err)
	out := []string{}
	for _, e := range events {
		out = append(out, e.Kind)
	}
	return out
}
