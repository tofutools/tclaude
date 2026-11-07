package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

type tbStarted struct {
	tbTask
	Assignee *struct {
		AgentID string `json:"agent_id"`
	} `json:"assignee"`
}

// A new task starts in any column but Done-or-not is one create call
// (`taskboard add --assignee / --blocked / --done`): in progress (assigned,
// so next), blocked with its reason, or done (logged after the fact, closed
// now); the assignee is gated and briefed as an assign is, and work logged
// as done tells nobody.
func TestTaskboardCreateStartState(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	worker := tbAgentID(t, h.worker)
	create := func(conv string, body map[string]any) (int, tbStarted, string) {
		t.Helper()
		rec := h.call(conv, http.MethodPost, "/v1/groups/crew/taskboard/tasks", body)
		var out tbStarted
		if rec.Code == http.StatusCreated {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		}
		return rec.Code, out, rec.Body.String()
	}

	// In progress: assigned in the create, so next, and briefed.
	code, got, body := create("", map[string]any{"title": "in flight", "to": h.worker})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "next", got.Stage)
	assert.Equal(t, "open", got.Status)
	require.NotNil(t, got.Assignee)
	assert.Equal(t, worker, got.Assignee.AgentID)
	assert.Contains(t, tbInboxSubjects(t, h.worker), `[taskboard] assigned to you by the operator: crew · `+got.ID+` "in flight"`)

	// Blocked: needs its reason; assigned and blocked in one call.
	code, _, body = create("", map[string]any{"title": "stuck", "status": "blocked"})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, got, body = create("", map[string]any{"title": "stuck", "status": "blocked", "block_reason": "waiting on CI", "to": h.worker})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "blocked", got.Status)
	assert.Equal(t, "waiting on CI", got.BlockReason)
	require.NotNil(t, got.Assignee)

	// Done: logged after the fact, with who did it, and no mail.
	before := len(tbInboxSubjects(t, h.worker))
	code, got, body = create("", map[string]any{"title": "fixed the flake", "status": "done", "to": h.worker})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "done", got.Status)
	require.NotNil(t, got.Assignee)
	assert.Equal(t, worker, got.Assignee.AgentID)
	assert.Len(t, tbInboxSubjects(t, h.worker), before, "work logged as done tells nobody")
	assert.Equal(t, "next", got.Stage, "an assigned task is queued, done or not, so a reopen keeps it queued")

	// An anchor in the ideas does not put an assigned task there.
	code, idea, body := create("", map[string]any{"title": "an idea"})
	require.Equal(t, http.StatusCreated, code, body)
	code, got, body = create("", map[string]any{"title": "next to the idea", "to": h.worker, "position": map[string]any{"before": idea.ID}})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "next", got.Stage)
	rec := h.call("", http.MethodGet, "/v1/groups/crew/taskboard?all=1", nil)
	assert.Contains(t, rec.Body.String(), "fixed the flake")
	rec = h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	assert.Contains(t, rec.Body.String(), `"done_this_week":1`, "it counts in done this week")

	// The assignee is gated as an assign is: a member may not put a task on
	// another member, and an assignee must be an active member.
	code, _, body = create(h.peer, map[string]any{"title": "for you", "to": h.worker})
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, agentd.PermTaskboardAssign)
	code, _, body = create("", map[string]any{"title": "for the owner", "to": h.lead})
	assert.Equal(t, http.StatusBadRequest, code, body)

	// A member takes its own new task on self.taskboard.
	code, got, body = create(h.peer, map[string]any{"title": "mine now", "to": h.peer})
	require.Equal(t, http.StatusCreated, code, body)
	require.NotNil(t, got.Assignee)

	// An import takes none of it.
	code, _, body = create("", map[string]any{"refs": []string{"tcl-a1b2c3"}, "status": "done"})
	assert.Equal(t, http.StatusBadRequest, code, body)
	code, _, body = create("", map[string]any{"title": "x", "status": "dropped"})
	assert.Equal(t, http.StatusBadRequest, code, body)
}
