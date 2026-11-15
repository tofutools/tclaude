package agentd_test

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
)

// A process runs for a board task (tk_8aa88514): started for it by id, by
// `taskboard add --process`, or by the board's default way of working when
// the task is taken. The run carries the task, and its end settles the task.

type taskRunWire struct {
	ID     string            `json:"id"`
	Status string            `json:"status"`
	Params map[string]string `json:"params"`
	Scope  struct {
		Kind      string `json:"kind"`
		Group     string `json:"group"`
		Owner     string `json:"owner"`
		Task      string `json:"task"`
		TaskTitle string `json:"taskTitle"`
	} `json:"scope"`
}

type taskProcessWire struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	BlockReason  string `json:"block_reason"`
	Process      string `json:"process"`
	ProcessStart *struct {
		Process string `json:"process"`
		Run     string `json:"run"`
		Error   string `json:"error"`
	} `json:"process_start"`
}

// processTaskFlow is the crew board of newTaskboardAssignFlow with two saved
// processes: gate, which ends well once the operator approves, and
// gate-fail, which ends failed when the operator rejects.
func processTaskFlow(t *testing.T) *tbAssignHarness {
	h := newTaskboardAssignFlow(t)
	root := filepath.Join(h.f.World.HomeDir, ".tclaude", "process-task-runs")
	t.Cleanup(agentd.SetProcessStoreRootForTest(root))
	t.Cleanup(agentd.ResetProcessRunRuntimeForTest())
	putProcessRuntimeTemplate(t, root, processHumanGateTemplate("gate"))
	failing := processHumanGateTemplate("gate-fail")
	failing.Nodes["choose"] = model.Node{
		Type: model.NodeTypeDecision, Description: "Approve, or reject and fail.",
		Performer: &model.Performer{Kind: model.PerformerHuman, Ask: "Ship it?"},
		Next:      model.Next{"approve": "end", "reject": "failed"},
	}
	failing.Nodes["failed"] = model.Node{Type: model.NodeTypeEnd, Result: "failed"}
	putProcessRuntimeTemplate(t, root, failing)
	return h
}

func (h *tbAssignHarness) taskProcess(conv, method, path string, body any, want int) taskProcessWire {
	h.t.Helper()
	rec := h.call(conv, method, path, body)
	require.Equal(h.t, want, rec.Code, "body=%s", rec.Body.String())
	var out taskProcessWire
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

func (h *tbAssignHarness) runsForTask(taskID string) []taskRunWire {
	h.t.Helper()
	rec := h.call("", http.MethodGet, "/v1/process/runs?task="+taskID, nil)
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var page struct {
		Runs []taskRunWire `json:"runs"`
	}
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &page))
	return page.Runs
}

// waitTaskStatus polls the task until it reads status: a run's end settles
// its task after the run's commit.
func (h *tbAssignHarness) waitTaskStatus(taskID, status string) taskProcessWire {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last taskProcessWire
	for time.Now().Before(deadline) {
		last = h.taskProcess("", http.MethodGet, "/v1/taskboard/tasks/"+taskID, nil, http.StatusOK)
		if last.Status == status {
			return last
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("task %s is %s, not %s, 10s after its run ended", taskID, last.Status, status)
	return last
}

func TestProcessRunForATaskCarriesItAndItsEndClosesTheTask(t *testing.T) {
	h := processTaskFlow(t)
	task := h.add("Checkout redesign", "next")

	rec := h.call("", http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "gate", "task": task.ID, "authorizeProgramProfiles": []string{}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var run taskRunWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &run))
	assert.Equal(t, "group", run.Scope.Kind, "a task's run is its group's")
	assert.Equal(t, "crew", run.Scope.Group)
	assert.Equal(t, task.ID, run.Scope.Task)
	assert.Equal(t, "Checkout redesign", run.Scope.TaskTitle, "the run card says which task")
	assert.Equal(t, task.ID, run.Params["task_ref"])
	assert.Equal(t, "Checkout redesign\n\nthe brief for Checkout redesign", run.Params["task"], "the task's text is an input")

	again := h.call("", http.MethodPost, "/v1/process/runs", map[string]any{"templateId": "gate", "task": task.ID})
	assert.Equal(t, http.StatusConflict, again.Code, "one live run per task: %s", again.Body.String())
	assert.Contains(t, again.Body.String(), run.ID)

	elsewhere := h.f.HaveGroup("other")
	wrong := h.call("", http.MethodPost, "/v1/process/runs", map[string]any{"templateId": "gate", "task": task.ID, "group": elsewhere.Name})
	assert.Equal(t, http.StatusBadRequest, wrong.Code, "a task's run is not another group's: %s", wrong.Body.String())

	runs := h.runsForTask(task.ID)
	require.Len(t, runs, 1)
	assert.Equal(t, run.ID, runs[0].ID)

	decide := h.call("", http.MethodPost, "/v1/process/runs/"+run.ID+"/decide", map[string]any{"nodeId": "choose", "verdict": "approve"})
	require.Equal(t, http.StatusAccepted, decide.Code, decide.Body.String())
	h.waitTaskStatus(task.ID, "done")
}

func TestProcessRunThatFailsBlocksItsTask(t *testing.T) {
	h := processTaskFlow(t)
	task := h.add("Saved search filters", "next")
	rec := h.call("", http.MethodPost, "/v1/process/runs", map[string]any{"templateId": "gate-fail", "task": task.ID})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var run taskRunWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &run))
	decide := h.call("", http.MethodPost, "/v1/process/runs/"+run.ID+"/decide", map[string]any{"nodeId": "choose", "verdict": "reject"})
	require.Equal(t, http.StatusAccepted, decide.Code, decide.Body.String())
	blocked := h.waitTaskStatus(task.ID, "blocked")
	assert.Contains(t, blocked.BlockReason, "gate-fail failed")
	assert.Contains(t, blocked.BlockReason, run.ID)
}

func TestTaskboardAddWithAProcessStartsItForTheNewTask(t *testing.T) {
	h := processTaskFlow(t)
	added := h.taskProcess("", http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "Saved search filters", "process": "gate"}, http.StatusCreated)
	require.NotNil(t, added.ProcessStart, "add --process says what it started")
	assert.Empty(t, added.ProcessStart.Error)
	assert.Equal(t, "gate", added.Process)
	runs := h.runsForTask(added.ID)
	require.Len(t, runs, 1)
	assert.Equal(t, added.ProcessStart.Run, runs[0].ID)
	assert.Equal(t, "human:operator", runs[0].Scope.Owner, "the caller's run")

	missing := h.call("", http.MethodPost, "/v1/groups/crew/taskboard/tasks", map[string]any{"title": "x", "process": "nope"})
	assert.Equal(t, http.StatusNotFound, missing.Code, "an unknown process adds no task: %s", missing.Body.String())

	// A member may add its own task, but not start a process it may not
	// start: the task's choice is gated like the run.
	denied := h.call(h.worker, http.MethodPost, "/v1/groups/crew/taskboard/tasks",
		map[string]any{"title": "mine", "mine": true, "process": "gate"})
	assert.Equal(t, http.StatusForbidden, denied.Code, denied.Body.String())
}

func TestBoardDefaultWayOfWorkingStartsWhenATaskIsTaken(t *testing.T) {
	h := processTaskFlow(t)
	h.call("", http.MethodPatch, "/v1/groups/crew/taskboard/options", map[string]any{"set": map[string]string{"process": "gate"}})
	ctx := h.call("", http.MethodGet, "/v1/taskboard/context", nil)
	require.Equal(t, http.StatusOK, ctx.Code, ctx.Body.String())
	var boards struct {
		Groups []struct {
			Group   string `json:"group"`
			Process string `json:"process"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(ctx.Body.Bytes(), &boards))
	shown := ""
	for _, b := range boards.Groups {
		if b.Group == "crew" {
			shown = b.Process
		}
	}
	assert.Equal(t, "gate", shown, "the board shows its default way of working on every card")
	follows := h.add("Follows the board", "next")
	opted := h.add("No process for this one", "next")
	assert.Empty(t, h.runsForTask(follows.ID), "nothing runs before the task is started")
	h.taskProcess("", http.MethodPatch, "/v1/taskboard/tasks/"+opted.ID, map[string]any{"process": "none"}, http.StatusOK)

	took := h.taskProcess(h.worker, http.MethodPost, "/v1/taskboard/tasks/"+follows.ID+"/assign", map[string]any{"to": ""}, http.StatusOK)
	require.NotNil(t, took.ProcessStart, "taking it starts the board's way of working")
	assert.Empty(t, took.ProcessStart.Error)
	runs := h.runsForTask(follows.ID)
	require.Len(t, runs, 1)
	assert.Equal(t, "crew", runs[0].Scope.Group)
	assert.Equal(t, "human:operator", runs[0].Scope.Owner, "the board's way of working, not the member, starts it")

	none := h.taskProcess(h.worker, http.MethodPost, "/v1/taskboard/tasks/"+opted.ID+"/assign", map[string]any{"to": ""}, http.StatusOK)
	assert.Nil(t, none.ProcessStart, "a task that opted out runs nothing")
	assert.Empty(t, h.runsForTask(opted.ID))

	// Off by default: a board that sets none runs nothing.
	h.call("", http.MethodPatch, "/v1/groups/crew/taskboard/options", map[string]any{"reset": []string{"process"}})
	plain := h.add("Plain", "next")
	h.taskProcess(h.peer, http.MethodPost, "/v1/taskboard/tasks/"+plain.ID+"/assign", map[string]any{"to": ""}, http.StatusOK)
	assert.Empty(t, h.runsForTask(plain.ID))
}

// A run for a task carries its brief and its end closes it: starting one
// takes what changing the task's board does, whatever the run's scope.
func TestProcessRunForATaskNeedsItsBoard(t *testing.T) {
	h := processTaskFlow(t)
	h.f.HaveGroup("other")
	rec := h.call("", http.MethodPost, "/v1/groups/other/taskboard/tasks", map[string]any{"title": "Not yours", "body": "a private brief"})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var foreign taskProcessWire
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &foreign))

	bound := h.call(h.worker, http.MethodPost, "/v1/process/runs",
		map[string]any{"templateId": "gate", "agent": tbAgentID(t, h.worker), "task": foreign.ID})
	assert.NotEqual(t, http.StatusCreated, bound.Code, "a run bound to oneself is not a way onto another board: %s", bound.Body.String())
	assert.Empty(t, h.runsForTask(foreign.ID))
}

// The board's way of working runs as the operator's: setting it takes a
// process that exists, under the setter's own process.runs.start.
func TestBoardWayOfWorkingIsAProcessTheSetterMayStart(t *testing.T) {
	h := processTaskFlow(t)
	missing := h.call("", http.MethodPatch, "/v1/groups/crew/taskboard/options", map[string]any{"set": map[string]string{"process": "nope"}})
	assert.Equal(t, http.StatusNotFound, missing.Code, missing.Body.String())
	cleared := h.call("", http.MethodPatch, "/v1/groups/crew/taskboard/options", map[string]any{"set": map[string]string{"process": "none"}})
	assert.Equal(t, http.StatusOK, cleared.Code, cleared.Body.String())
	set := h.call("", http.MethodPatch, "/v1/groups/crew/taskboard/options", map[string]any{"set": map[string]string{"process": "gate"}})
	assert.Equal(t, http.StatusOK, set.Code, set.Body.String())
}
