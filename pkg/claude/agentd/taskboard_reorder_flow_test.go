package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// A member with only taskboard.own orders its own tasks among themselves
// (its lane), and nothing past them: not to the top of the stage, not
// beside someone else's task, not someone else's task.
func TestTaskboardOwnReordersOwnLane(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	mine := func(title string) tbTask {
		return h.task(h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
			map[string]any{"title": title}), http.StatusCreated)
	}
	first, second := mine("first"), mine("second")
	others := h.add("the operator's", "next")

	code, body := h.post(h.worker, second.ID, "move", map[string]any{"before": first.ID, "stage": "next"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"second", "first", "the operator's"}, h.list("", "/v1/groups/crew/taskboard"))

	for name, req := range map[string]map[string]any{
		"top of the stage":           {"top": true},
		"bottom of the stage":        {"bottom": true},
		"beside someone else's task": {"after": others.ID},
	} {
		code, body = h.post(h.worker, first.ID, "move", req)
		assert.Equal(t, http.StatusForbidden, code, "%s: %s", name, body)
		assert.Contains(t, body, agentd.PermTaskboardWrite, name)
	}
	code, body = h.post(h.worker, others.ID, "move", map[string]any{"before": first.ID})
	assert.Equal(t, http.StatusForbidden, code, "someone else's task: %s", body)

	// The writer still moves anything anywhere.
	code, body = h.post(h.lead, others.ID, "move", map[string]any{"top": true})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"the operator's", "second", "first"}, h.list("", "/v1/groups/crew/taskboard"))
}

// A move that names the stage it expects is refused as stale, and changes
// nothing, when its anchor has crossed to the other stage meanwhile; a
// stage that is not idea or next is a bad request.
func TestTaskboardMoveStageGuard(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	a := h.add("a", "next")
	b := h.add("b", "next")
	code, body := h.post("", b.ID, "stage", map[string]any{"stage": "idea"})
	require.Equal(t, http.StatusOK, code, body)

	code, body = h.post("", a.ID, "move", map[string]any{"after": b.ID, "stage": "next"})
	assert.Equal(t, http.StatusConflict, code, body)
	var refusal struct {
		Code string `json:"code"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &refusal), body)
	assert.Equal(t, "stale", refusal.Code, body)
	assert.Equal(t, []string{"a"}, h.list("", "/v1/groups/crew/taskboard?stage=next"), "a stays in the queue")

	code, body = h.post("", a.ID, "move", map[string]any{"top": true, "stage": "done"})
	assert.Equal(t, http.StatusBadRequest, code, body)
}
