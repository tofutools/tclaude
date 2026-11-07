package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Moving and cloning a task to another group (tcl-0r0obl): POST
// /v1/taskboard/tasks/{id}/move-to-group, the CLI's move-to-group.

type tbTransfer struct {
	Task              tbTask   `json:"task"`
	Group             string   `json:"group"`
	From              string   `json:"from"`
	Clone             bool     `json:"clone"`
	DroppedDeps       []string `json:"dropped_deps"`
	DroppedDependents []string `json:"dropped_dependents"`
	Unassigned        string   `json:"unassigned"`
}

func (h *tbAssignHarness) transfer(conv, id string, body map[string]any) (int, tbTransfer, string) {
	code, raw := h.post(conv, id, "move-to-group", body)
	var out tbTransfer
	if code == http.StatusOK {
		require.NoError(h.t, json.Unmarshal([]byte(raw), &out))
	}
	return code, out, raw
}

func (h *tbAssignHarness) eventKinds(id string) []string {
	rec := h.call("", http.MethodGet, "/v1/taskboard/tasks/"+id+"/events", nil)
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		Events []struct{ Kind, Detail string } `json:"events"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	kinds := []string{}
	for _, e := range out.Events {
		kinds = append(kinds, e.Kind+": "+e.Detail)
	}
	return kinds
}

// The operator moves a task: it keeps its history, its dependencies across
// the old board are dropped, an assignee who is a member of the new group
// stays, and a block is lifted.
func TestTaskboardMoveToGroup(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	h.f.HaveGroup("ops")
	h.f.HaveMember("ops", h.worker)

	first := h.add("first", "next")
	moving := h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "moving", "stage": "next", "body": "the brief", "after": []string{first.ID}}), http.StatusCreated)
	after := h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "after it", "after": []string{moving.ID}}), http.StatusCreated)
	code, body := h.post("", moving.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)

	code, out, raw := h.transfer("", moving.ID, map[string]any{"group": "ops"})
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, moving.ID, out.Task.ID, "a move keeps the task")
	assert.Equal(t, "ops", out.Group)
	assert.Equal(t, "crew", out.From)
	assert.Equal(t, "next", out.Task.Stage, "an assignee who stays keeps it next")
	assert.Equal(t, h.assignee(moving.ID), tbAgentID(t, h.worker), "the worker is a member of ops: still assigned")
	assert.Empty(t, out.Unassigned)
	assert.Equal(t, []string{first.ID}, out.DroppedDeps)
	assert.Equal(t, []string{after.ID}, out.DroppedDependents)
	assert.Empty(t, out.Task.DependsOn)

	afterNow := h.task(h.call("", http.MethodGet, "/v1/taskboard/tasks/"+after.ID, nil), http.StatusOK)
	assert.Empty(t, afterNow.DependsOn, "the old board's dependency on it is gone")
	kinds := strings.Join(h.eventKinds(moving.ID), "\n")
	assert.Contains(t, kinds, "created: ", "its history stays")
	assert.Contains(t, kinds, "regrouped: from crew to ops")

	// An assignee who is not a member of the new group is unassigned, and told.
	held := h.add("held by the peer", "next")
	code, body = h.post("", held.ID, "assign", map[string]any{"to": h.peer})
	require.Equal(t, http.StatusOK, code, body)
	code, out, raw = h.transfer("", held.ID, map[string]any{"group": "ops", "stage": "idea"})
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, tbAgentID(t, h.peer), out.Unassigned)
	assert.Equal(t, "idea", out.Task.Stage)
	assert.Empty(t, h.assignee(held.ID))
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.peer), "\n"), "moved away from you")

	// A blocked task lands open.
	blocked := h.add("blocked", "next")
	code, body = h.post("", blocked.ID, "status", map[string]any{"status": "blocked", "reason": "waiting"})
	require.Equal(t, http.StatusOK, code, body)
	code, out, raw = h.transfer("", blocked.ID, map[string]any{"group": "ops"})
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, "open", out.Task.Status)
	assert.Empty(t, out.Task.BlockReason)

	// The moved tasks are on ops' board and off crew's.
	rec := h.call("", http.MethodGet, "/v1/groups/ops/taskboard", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), moving.ID)
	rec = h.call("", http.MethodGet, "/v1/groups/crew/taskboard", nil)
	assert.NotContains(t, rec.Body.String(), moving.ID)

	// Refusals.
	for _, c := range []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"group": "crew"}, http.StatusBadRequest},
		{map[string]any{"group": "nowhere"}, http.StatusNotFound},
		{map[string]any{"group": "ops@forge"}, http.StatusBadRequest},
		{map[string]any{"group": "ops", "stage": "done"}, http.StatusBadRequest},
		{map[string]any{}, http.StatusBadRequest},
	} {
		code, _, raw := h.transfer("", first.ID, c.body)
		assert.Equal(t, c.code, code, "%v: %s", c.body, raw)
	}
}

// A clone is a new task in the other group with the same title, brief and
// ref, no assignee and no dependencies; the original stays and notes it.
func TestTaskboardCloneToGroup(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	h.f.HaveGroup("ops")
	dep := h.add("dep", "next")
	src := h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "cloned one", "body": "the brief", "ref": "https://example.com/x", "stage": "next",
			"after": []string{dep.ID}}), http.StatusCreated)
	code, body := h.post("", src.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)

	code, out, raw := h.transfer("", src.ID, map[string]any{"group": "ops", "clone": true})
	require.Equal(t, http.StatusOK, code, raw)
	assert.True(t, out.Clone)
	assert.NotEqual(t, src.ID, out.Task.ID)
	assert.Equal(t, "ops", out.Task.Group)
	assert.Equal(t, "cloned one", out.Task.Title)
	assert.Equal(t, "idea", out.Task.Stage, "a clone starts in ideas unless a stage is given")
	require.NotNil(t, out.Task.Ref)
	assert.Equal(t, "https://example.com/x", out.Task.Ref.URL)
	assert.Empty(t, out.Task.DependsOn)
	assert.Empty(t, h.assignee(out.Task.ID))
	assert.Contains(t, strings.Join(h.eventKinds(out.Task.ID), "\n"), "cloned from "+src.ID+" in crew")

	still := h.task(h.call("", http.MethodGet, "/v1/taskboard/tasks/"+src.ID, nil), http.StatusOK)
	assert.Equal(t, "crew", still.Group, "the original stays")
	assert.Equal(t, tbAgentID(t, h.worker), h.assignee(src.ID))
	assert.Contains(t, strings.Join(h.eventKinds(src.ID), "\n"), "cloned: to "+out.Task.ID+" in ops")
}

// An agent needs taskboard.transfer on the target (ownership confers it,
// membership does not), and for a move taskboard.write on the source when
// it holds the task or taskboard.manage there otherwise.
func TestTaskboardTransferPermissions(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	ops := h.f.HaveGroup("ops")
	h.f.HaveMember("ops", h.worker)
	mine := h.add("the peer's", "next")
	code, body := h.post("", mine.ID, "assign", map[string]any{"to": h.peer})
	require.Equal(t, http.StatusOK, code, body)
	other := h.add("someone else's", "next")

	// Membership of ops gives the worker no transfer into it.
	code, _, raw := h.transfer(h.worker, other.ID, map[string]any{"group": "ops", "clone": true})
	assert.Equal(t, http.StatusForbidden, code, raw)
	// The lead owns crew, not ops.
	code, _, raw = h.transfer(h.lead, other.ID, map[string]any{"group": "ops"})
	assert.Equal(t, http.StatusForbidden, code, raw)

	// The peer owns ops: it may move the task it holds (it writes crew as a
	// member) but not someone else's (that needs taskboard.manage on crew).
	require.NoError(t, db.AddAgentGroupOwner(ops.ID, h.peer, "test"))
	code, _, raw = h.transfer(h.peer, other.ID, map[string]any{"group": "ops"})
	assert.Equal(t, http.StatusForbidden, code, raw)
	code, _, raw = h.transfer(h.peer, other.ID, map[string]any{"group": "ops", "clone": true})
	assert.Equal(t, http.StatusOK, code, "a clone needs only read on the source: %s", raw)
	code, out, raw := h.transfer(h.peer, mine.ID, map[string]any{"group": "ops"})
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, "ops", out.Group)

	// The lead, owning both, moves anyone's task.
	require.NoError(t, db.AddAgentGroupOwner(ops.ID, h.lead, "test"))
	code, _, raw = h.transfer(h.lead, other.ID, map[string]any{"group": "ops"})
	assert.Equal(t, http.StatusOK, code, raw)
}
