package agentd_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

type tbAssignHarness struct {
	*taskboardHarness
	lead, worker, peer string
	crew               *db.AgentGroup
}

// newTaskboardAssignFlow: group crew with an owner (lead, not a member) and
// two members; self.taskboard is in the default baseline, as setup installs.
func newTaskboardAssignFlow(t *testing.T) *tbAssignHarness {
	h := &tbAssignHarness{taskboardHarness: newTaskboardFlow(t),
		lead: "tb-lead-1111", worker: "tb-worker-2222", peer: "tb-peer-3333"}
	require.NoError(t, config.Save(&config.Config{
		Features: &config.FeaturesConfig{Taskboard: true},
		Agent:    &config.AgentConfig{DefaultPermissions: []string{agentd.PermSelfTaskboard}},
	}))
	for _, c := range []string{h.lead, h.worker, h.peer} {
		h.agent(t, c)
	}
	h.crew = h.f.HaveGroup("crew")
	h.f.HaveMember("crew", h.worker)
	h.f.HaveMember("crew", h.peer)
	require.NoError(t, db.AddAgentGroupOwner(h.crew.ID, h.lead, "test"))
	return h
}

func (h *tbAssignHarness) add(title, stage string) tbTask {
	return h.task(h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": title, "stage": stage, "body": "the brief for " + title}), http.StatusCreated)
}

func (h *tbAssignHarness) post(conv, id, verb string, body any) (int, string) {
	rec := h.call(conv, http.MethodPost, "/v1/taskboard/tasks/"+id+"/"+verb, body)
	return rec.Code, rec.Body.String()
}

func tbAgentID(t *testing.T, conv string) string {
	id, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	return id
}

func tbInboxSubjects(t *testing.T, conv string) []string {
	msgs, err := db.ListInboxForActor(conv, tbAgentID(t, conv), 50)
	require.NoError(t, err)
	out := []string{}
	for _, m := range msgs {
		out = append(out, m.Subject)
	}
	return out
}

// The owner assigns a member, who gets the brief from the owner, reports done
// and stays a member; a reassigned agent's late report is refused.
func TestTaskboardAssignBriefAndReport(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	idea := h.add("ship the lanes", "")

	// A member may not assign another agent.
	code, body := h.post(h.peer, idea.ID, "assign", map[string]any{"to": h.worker})
	assert.Equal(t, http.StatusForbidden, code)
	assert.Contains(t, body, agentd.PermTaskboardAssign)

	// The owner assigns; the idea is promoted on assign.
	code, body = h.post(h.lead, idea.ID, "assign", map[string]any{"to": h.worker})
	require.Equal(t, http.StatusOK, code, body)
	var got tbAssignTask
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, "next", got.Stage)
	require.NotNil(t, got.Assignee)
	assert.Equal(t, tbAgentID(t, h.worker), got.Assignee.AgentID)
	assert.Equal(t, "agent:"+tbAgentID(t, h.lead), got.AssignedBy)

	msgs, err := db.ListInboxForActor(h.worker, tbAgentID(t, h.worker), 10)
	require.NoError(t, err)
	require.Len(t, msgs, 1)
	assert.Equal(t, h.lead, msgs[0].FromConv, "the brief is from the assigner")
	assert.Contains(t, msgs[0].Subject, "[taskboard] crew · "+idea.ID)
	assert.Contains(t, msgs[0].Subject, "assigned to you by")
	assert.Contains(t, msgs[0].Body, "the brief for ship the lanes")
	assert.Contains(t, msgs[0].Body, "tclaude agent taskboard report done --task "+idea.ID)

	row := auditRowByVerb(t, "taskboard.assign")
	assert.Equal(t, "crew", row.GroupName)
	assert.Equal(t, h.worker, row.TargetConv)
	assert.Contains(t, row.Detail, idea.ID)

	// The worker sees it as its own.
	rec := h.call(h.worker, http.MethodGet, "/v1/taskboard/mine", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), idea.ID)

	// Reassign to the peer: the worker is told, and its late report refused.
	code, body = h.post(h.lead, idea.ID, "assign", map[string]any{"to": h.peer})
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.worker), "\n"), "reassigned by")
	code, body = h.post(h.worker, idea.ID, "report", map[string]any{"outcome": "done", "note": "late"})
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, body, "not_assignee")
	assert.Equal(t, http.StatusConflict, auditRowByVerb(t, "taskboard.report").Status, "the refusal is audited")

	// The peer reports done; it stays a member, the task is closed, the lead told.
	code, body = h.post(h.peer, idea.ID, "report", map[string]any{"outcome": "done", "note": "merged #1"})
	require.Equal(t, http.StatusOK, code, body)
	var rep struct {
		Task     tbAssignTask `json:"task"`
		Repeated bool         `json:"repeated"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &rep))
	assert.Equal(t, "done", rep.Task.Status)
	assert.Equal(t, "succeeded", rep.Task.Outcome)
	assert.False(t, rep.Repeated)
	state, err := db.AgentState(h.peer)
	require.NoError(t, err)
	assert.NotEqual(t, db.AgentStateRetired, state, "done leaves the agent where it is")
	members, err := db.ListAgentGroupMembers(h.crew.ID)
	require.NoError(t, err)
	assert.Len(t, members, 2)
	assert.Contains(t, strings.Join(tbInboxSubjects(t, h.lead), "\n"), "reported done")

	// The same report again is acknowledged.
	code, body = h.post(h.peer, idea.ID, "report", map[string]any{"outcome": "done", "note": "merged #1"})
	require.Equal(t, http.StatusOK, code, body)
	assert.Contains(t, body, `"repeated":true`)
}

type tbAssignTask struct {
	tbTask
	Assignee *struct {
		AgentID string `json:"agent_id"`
	} `json:"assignee"`
	AssignedBy string `json:"assigned_by"`
	Outcome    string `json:"outcome"`
	Failures   int    `json:"failures"`
}

// Members take and release queued tasks; a take of an idea is refused; three
// failed reports block a task; retiring an assignee unassigns.
func TestTaskboardTakeFailAndRetire(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	idea := h.add("an idea", "")
	code, body := h.post(h.worker, idea.ID, "assign", map[string]any{"to": ""})
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Contains(t, body, "not_queued")

	next := h.add("queued", "next")
	for i := 1; i <= 3; i++ {
		code, body = h.post(h.worker, next.ID, "assign", map[string]any{"to": ""})
		require.Equal(t, http.StatusOK, code, "take %d: %s", i, body)
		code, body = h.post(h.worker, next.ID, "report", map[string]any{"outcome": "failed", "note": "try " + string(rune('0'+i))})
		require.Equal(t, http.StatusOK, code, body)
	}
	assert.Contains(t, body, `"blocked":true`)
	shown := h.task(h.call(h.worker, http.MethodGet, "/v1/taskboard/tasks/"+next.ID, nil), http.StatusOK)
	assert.Equal(t, "blocked", shown.Status)
	assert.Equal(t, "3 failed attempts", shown.BlockReason)

	// Take, then release.
	other := h.add("another", "next")
	code, _ = h.post(h.peer, other.ID, "assign", map[string]any{"to": ""})
	require.Equal(t, http.StatusOK, code)
	code, body = h.post(h.worker, other.ID, "assign", map[string]any{"to": ""})
	assert.Equal(t, http.StatusConflict, code, "someone else holds it: %s", body)
	code, body = h.post(h.worker, other.ID, "unassign", nil)
	assert.Equal(t, http.StatusForbidden, code, "a member does not take back a peer's task: %s", body)
	code, body = h.post(h.peer, other.ID, "unassign", nil)
	require.Equal(t, http.StatusOK, code, body)

	// Retiring the assignee unassigns its live task.
	code, _ = h.post(h.peer, other.ID, "assign", map[string]any{"to": ""})
	require.Equal(t, http.StatusOK, code)
	_, err := db.RetireAgent(h.peer, "human", "test")
	require.NoError(t, err)
	var after tbAssignTask
	rec := h.call("", http.MethodGet, "/v1/taskboard/tasks/"+other.ID, nil)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &after))
	assert.Nil(t, after.Assignee)
}

// A scoped taskboard.assign grant reaches only its group and target agents;
// the writer lock narrows members but not owners, writers or reports.
func TestTaskboardAssignScopeAndLock(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	const steward = "tb-steward-4444"
	h.agent(t, steward)
	task := h.add("scoped", "next")

	require.NoError(t, db.GrantAgentPermissionWithScope(steward, agentd.PermTaskboardRead, `{"group":["crew"]}`, "test"))
	require.NoError(t, db.GrantAgentPermissionWithScope(steward, agentd.PermTaskboardAssign,
		`{"group":["crew"],"target_agent":["`+tbAgentID(t, h.peer)+`"]}`, "test"))
	code, body := h.post(steward, task.ID, "assign", map[string]any{"to": h.worker})
	assert.Equal(t, http.StatusForbidden, code, "the scope names only the peer: %s", body)
	code, body = h.post(steward, task.ID, "assign", map[string]any{"to": h.peer})
	require.Equal(t, http.StatusOK, code, body)
	// Assignees must be members.
	code, body = h.post(h.lead, task.ID, "assign", map[string]any{"to": steward})
	assert.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, "not an active member")

	// A member may not lock; the owner locks to the worker.
	rec := h.call(h.peer, http.MethodPost, "/v1/groups/crew/taskboard/writers", map[string]any{"agents": []string{h.peer}})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = h.call(h.lead, http.MethodPost, "/v1/groups/crew/taskboard/writers", map[string]any{"agents": []string{h.worker}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"locked":true`)

	// The locked-out peer cannot add or take, but still reports its own task.
	rec = h.call(h.peer, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "x"})
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "taskboard_locked")
	free := h.add("free", "next")
	code, body = h.post(h.peer, free.ID, "assign", map[string]any{"to": ""})
	assert.Equal(t, http.StatusForbidden, code, body)
	code, body = h.post(h.peer, task.ID, "report", map[string]any{"outcome": "done"})
	require.Equal(t, http.StatusOK, code, "reports are never locked: %s", body)

	// The listed writer and the owner still change the board; the list says it is locked.
	rec = h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "by writer"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = h.call(h.lead, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "by owner"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = h.call(h.worker, http.MethodGet, "/v1/groups/crew/taskboard", nil)
	assert.Contains(t, rec.Body.String(), `"locked":true`)

	// Unlock: every member writes again.
	rec = h.call(h.lead, http.MethodPost, "/v1/groups/crew/taskboard/writers", map[string]any{"members": true})
	require.Equal(t, http.StatusOK, rec.Code)
	rec = h.call(h.peer, http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "y"})
	assert.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	// done on someone else's task needs assign; on your own, self.taskboard.
	mine := h.add("mine", "next")
	code, _ = h.post(h.worker, mine.ID, "assign", map[string]any{"to": ""})
	require.Equal(t, http.StatusOK, code)
	code, body = h.post(h.peer, mine.ID, "status", map[string]any{"status": "done"})
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Contains(t, body, agentd.PermTaskboardAssign)
	code, body = h.post(h.worker, mine.ID, "status", map[string]any{"status": "done"})
	assert.Equal(t, http.StatusOK, code, body)
}

// Review regressions: a reassign needs the right over the current holder too;
// another member cannot drop a held task; release never takes a task from
// someone else; /mine hides groups the agent has left.
func TestTaskboardHolderProtections(t *testing.T) {
	h := newTaskboardAssignFlow(t)
	const steward = "tb-steward-5555"
	h.agent(t, steward)
	task := h.add("held", "next")
	code, _ := h.post(h.worker, task.ID, "assign", map[string]any{"to": ""})
	require.Equal(t, http.StatusOK, code)

	require.NoError(t, db.GrantAgentPermissionWithScope(steward, agentd.PermTaskboardRead, `{"group":["crew"]}`, "test"))
	require.NoError(t, db.GrantAgentPermissionWithScope(steward, agentd.PermTaskboardAssign,
		`{"group":["crew"],"target_agent":["`+tbAgentID(t, h.peer)+`"]}`, "test"))
	code, body := h.post(steward, task.ID, "assign", map[string]any{"to": h.peer})
	assert.Equal(t, http.StatusForbidden, code, "the worker is outside the steward's scope: %s", body)

	code, body = h.post(h.peer, task.ID, "status", map[string]any{"status": "dropped"})
	assert.Equal(t, http.StatusForbidden, code, "dropping someone's task needs assign: %s", body)

	code, body = h.post(h.lead, task.ID, "unassign", map[string]any{"self": true})
	assert.Equal(t, http.StatusConflict, code, body)
	assert.Contains(t, body, "not_assignee")
	code, body = h.post(h.lead, task.ID, "unassign", nil)
	require.Equal(t, http.StatusOK, code, "the owner takes it back: %s", body)

	code, _ = h.post(h.peer, task.ID, "assign", map[string]any{"to": ""})
	require.Equal(t, http.StatusOK, code)
	require.NoError(t, db.RemoveAgentGroupMember(h.crew.ID, h.peer))
	rec := h.call(h.peer, http.MethodGet, "/v1/taskboard/mine", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), task.ID)
}
