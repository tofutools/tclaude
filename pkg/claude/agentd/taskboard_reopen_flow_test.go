package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// reopen puts a done task back where it was with its assignee, or in another
// stage and with another assignee, in one write, gated as closing is.
func TestTaskboardReopen(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	reopen := func(conv, id string, body map[string]any) (int, string) { return h.post(conv, id, "reopen", body) }

	// The worker's own task, done.
	rec := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "mine"})
	mine := tbDecodeOperatorTask(t, rec.Code, rec.Body.String(), http.StatusCreated)
	code, body := h.post(h.worker, mine.ID, "report", map[string]any{"outcome": "done", "note": "shipped"})
	require.Equal(t, http.StatusOK, code, body)

	// A live task is not reopened.
	open := h.add("still open", "next")
	code, body = reopen("", open.ID, nil)
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Contains(t, body, "not_closed")

	// Another member may not reopen the worker's task; the worker may, and
	// it comes back assigned, in next.
	code, body = reopen(h.peer, mine.ID, map[string]any{})
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = reopen(h.worker, mine.ID, map[string]any{})
	got := tbDecodeOperatorTask(t, code, body, http.StatusOK)
	assert.Equal(t, "open", got.Status)
	assert.Equal(t, "next", got.Stage)
	require.NotNil(t, got.Assignee)
	assert.Equal(t, tbAgentID(t, h.worker), got.Assignee.AgentID)

	// Done again; the owner reopens it as an idea with no assignee. As an
	// idea with the assignee kept, it is refused.
	code, body = h.post(h.worker, mine.ID, "report", map[string]any{"outcome": "done", "note": "again"})
	require.Equal(t, http.StatusOK, code, body)
	code, body = reopen(h.lead, mine.ID, map[string]any{"stage": "idea"})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, body = reopen(h.lead, mine.ID, map[string]any{"stage": "idea", "to": ""})
	got = tbDecodeOperatorTask(t, code, body, http.StatusOK)
	assert.Equal(t, "idea", got.Stage)
	assert.Nil(t, got.Assignee)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.worker), "\n"), "reopened by")

	// Done unassigned; the owner reopens it for the peer, which gets the brief.
	code, body = h.post(h.lead, mine.ID, "status", map[string]any{"status": "done"})
	require.Equal(t, http.StatusOK, code, body)
	code, body = reopen(h.lead, mine.ID, map[string]any{"to": h.peer})
	got = tbDecodeOperatorTask(t, code, body, http.StatusOK)
	assert.Equal(t, "next", got.Stage, "an assigned task is in next")
	require.NotNil(t, got.Assignee)
	assert.Equal(t, tbAgentID(t, h.peer), got.Assignee.AgentID)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.peer), "\n"), "reopened and assigned to you by")

	// The history has it, in order.
	events, err := db.ListGroupTaskEvents(mine.ID)
	require.NoError(t, err)
	var kinds []string
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	assert.Contains(t, strings.Join(kinds, " "), "reopened assigned")

	// A member reopening someone else's done task for itself needs
	// taskboard.assign to take it from its holder.
	code, body = h.post(h.peer, mine.ID, "report", map[string]any{"outcome": "done", "note": "x"})
	require.Equal(t, http.StatusOK, code, body)
	code, body = reopen(h.worker, mine.ID, map[string]any{"to": h.worker})
	assert.Equal(t, http.StatusForbidden, code, body)
}
