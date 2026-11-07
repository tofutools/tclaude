package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func (h *tbAssignHarness) assignee(id string) string {
	rec := h.call("", http.MethodGet, "/v1/taskboard/tasks/"+id, nil)
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var v tbAssignTask
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &v))
	if v.Assignee == nil {
		return ""
	}
	return v.Assignee.AgentID
}

func tbLastUnassign(t *testing.T, id string) (actor, detail string) {
	events, err := db.ListGroupTaskEvents(id)
	require.NoError(t, err)
	for _, e := range events {
		if e.Kind == db.GroupTaskEventUnassigned {
			actor, detail = e.Actor, e.Detail
		}
	}
	return actor, detail
}

// Leaving a group unassigns the agent's live tasks on that group's board; a
// reincarnation, a re-add of the same member and the other groups' boards keep
// theirs; a retirement records itself as the reason.
func TestTaskboardUnassignOnLeavingGroup(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	worker := tbAgentID(t, h.worker)
	task := h.add("stays with the crew", "next")
	code, body := h.post(h.lead, task.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)

	// Re-adding the same member (a role change) keeps the assignment.
	h.f.HaveMemberWithRole("crew", h.worker, "dev")
	assert.Equal(t, worker, h.assignee(task.ID))

	// Reincarnation keeps it: the agent id is stable.
	const successor = "tb-worker-next-5555"
	h.f.HaveConvWithTitle(successor, successor)
	_, err := db.RotateAgentConv(h.worker, successor, "reincarnate")
	require.NoError(t, err)
	assert.Equal(t, worker, h.assignee(task.ID))
	rec := h.call(successor, http.MethodGet, "/v1/taskboard/mine", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), task.ID, "the successor still holds it")

	// A task on another group's board is untouched by leaving crew.
	h.f.HaveGroup("ops")
	h.f.HaveMember("ops", successor)
	opsTask := h.task(h.call("", http.MethodPost, "/v1/groups/ops/taskboard/tasks",
		map[string]any{"title": "ops work", "stage": "next"}), http.StatusCreated)
	code, body = h.post("", opsTask.ID, "assign", map[string]any{"to": successor})
	require.Equal(t, http.StatusOK, code, body)

	// Removing the member from crew unassigns crew's task.
	rec = h.call("", http.MethodDelete, "/v1/groups/crew/members/"+successor, nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.Empty(t, h.assignee(task.ID))
	actor, detail := tbLastUnassign(t, task.ID)
	assert.Equal(t, "system:membership", actor)
	assert.Equal(t, "assignee left the group", detail)
	assert.Equal(t, worker, h.assignee(opsTask.ID), "ops keeps its assignment")

	// Retiring the agent unassigns the rest, and says so.
	_, err = db.RetireAgent(successor, "human", "test")
	require.NoError(t, err)
	assert.Empty(t, h.assignee(opsTask.ID))
	actor, detail = tbLastUnassign(t, opsTask.ID)
	assert.Equal(t, "system:retire", actor)
	assert.Equal(t, "assignee retired", detail)
}
