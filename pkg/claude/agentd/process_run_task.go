package agentd

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/process/engine"
	"github.com/tofutools/tclaude/pkg/claude/process/executor"
	"github.com/tofutools/tclaude/pkg/claude/process/store"
)

// A process runs for a board task (tk_8aa88514): `tclaude process run X
// --task tk_…`, the task menu's "Run with a process…", `taskboard add
// --process X`, and a group's default way of working when a task is taken or
// started. The run carries the task's id (its scope's TaskID), the task card
// shows its progress, the run card links back, and the run's end settles the
// task: done when it completes, blocked with the reason when it fails.

// Process run params a task fills: its text (title, then body) and its id.
// A template uses them as {{params.task}} and {{params.task_ref}}. A start
// that passes either explicitly keeps its own value.
const (
	processRunTaskParam    = "task"
	processRunTaskRefParam = "task_ref"
	// processRunTaskTextMax bounds the task text a run carries in its params.
	processRunTaskTextMax = 16 << 10
)

func init() {
	executor.SetTerminalHook(func(runID string, status engine.RunStatus) {
		settleProcessRunTask(runID, status)
		if status == engine.RunFailed {
			notifyProcessRunFailed(runID)
		}
	})
}

// processRunTaskLocks serialise, per task, the check that a task has no
// live run with the create that gives it one, so two starts for one task
// make one run. Per task: a start waiting on a permission popup holds up
// only starts for the same task.
var processRunTaskLocks = struct {
	sync.Mutex
	held map[string]*processRunTaskLock
}{held: map[string]*processRunTaskLock{}}

type processRunTaskLock struct {
	sync.Mutex
	refs int
}

// lockProcessRunTask locks task id's starts and returns the unlock.
func lockProcessRunTask(id string) func() {
	processRunTaskLocks.Lock()
	l := processRunTaskLocks.held[id]
	if l == nil {
		l = &processRunTaskLock{}
		processRunTaskLocks.held[id] = l
	}
	l.refs++
	processRunTaskLocks.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		processRunTaskLocks.Lock()
		l.refs--
		if l.refs == 0 {
			delete(processRunTaskLocks.held, id)
		}
		processRunTaskLocks.Unlock()
	}
}

// resolveProcessRunTask reads the task a start names (request.Task) and
// fits the request to it: a run for a task is its group's unless the caller
// scopes it to an agent, and it gets the task's text and id as params. It
// answers the error itself and reports false when the start cannot go on. A
// request for no task passes through as (nil, true). With gate, the caller
// must be able to change the task's board: the run carries its text and its
// end closes it. (A board's way of working starting on take or start has had
// its caller's taskboard gates already.)
func resolveProcessRunTask(w http.ResponseWriter, r *http.Request, request *processRunCreateRequest, gate bool) (*db.GroupTask, bool) {
	id := strings.TrimSpace(request.Task)
	if id == "" {
		return nil, true
	}
	task, err := db.GetGroupTask(id)
	if errors.Is(err, db.ErrGroupTaskNotFound) {
		writeError(w, http.StatusNotFound, "process_run_task_not_found", fmt.Sprintf("no task %q", id))
		return nil, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "process_run_task", err.Error())
		return nil, false
	}
	if task.Kind == db.GroupTaskKindEpic {
		writeError(w, http.StatusBadRequest, "process_run_task_epic", fmt.Sprintf("%s is an epic: run a process for one of its tasks", id))
		return nil, false
	}
	if task.Status == db.GroupTaskDone || task.Status == db.GroupTaskDropped {
		writeError(w, http.StatusConflict, "process_run_task_closed", fmt.Sprintf("task %s is %s", id, task.Status))
		return nil, false
	}
	group, err := db.GetAgentGroupByID(task.GroupID)
	if err != nil || group == nil {
		writeError(w, http.StatusNotFound, "process_run_task_not_found", fmt.Sprintf("task %s has no group", id))
		return nil, false
	}
	if gate && !requireTaskboard(w, r, group, true) {
		return nil, false
	}
	groupRef := strings.TrimSpace(request.Group)
	if groupRef == "" && strings.TrimSpace(request.Agent) == "" {
		request.Group = group.Name
	} else if groupRef != "" {
		named, err := resolveGroupToken(groupRef)
		if err != nil || named == nil || named.ID != group.ID {
			writeError(w, http.StatusBadRequest, "process_run_task_group",
				fmt.Sprintf("task %s is on %s's board: run its process in %s", id, group.Name, group.Name))
			return nil, false
		}
	}
	if live, err := liveProcessRunForTask(id); err != nil {
		writeProcessRuntimeError(w, err)
		return nil, false
	} else if live != "" {
		writeError(w, http.StatusConflict, "process_run_task_busy", fmt.Sprintf("task %s already has a run in progress: %s", id, live))
		return nil, false
	}
	params := make(map[string]string, len(request.Params)+2)
	for k, v := range request.Params {
		params[k] = v
	}
	if _, ok := params[processRunTaskParam]; !ok {
		params[processRunTaskParam] = processRunTaskText(task)
	}
	if _, ok := params[processRunTaskRefParam]; !ok {
		params[processRunTaskRefParam] = task.ID
	}
	request.Params = params
	request.Task = task.ID
	return task, true
}

// processRunTaskScopeFits refuses a run for a task bound to an agent that is
// not an active member of the task's group: a task's work is its group's.
func processRunTaskScopeFits(w http.ResponseWriter, task *db.GroupTask, scope db.ProcessRunScope) bool {
	if scope.Kind != db.ProcessRunScopeAgent {
		return true
	}
	group, err := db.GetAgentGroupByID(task.GroupID)
	if err == nil && group != nil && activeGroupMember(group, scope.AgentID) != "" {
		return true
	}
	writeError(w, http.StatusBadRequest, "process_run_task_group",
		fmt.Sprintf("task %s is on another group's board: bind its run to a member of that group", task.ID))
	return false
}

// processRunTaskText is the task as a run's prompt reads it: its title, then
// its body, bounded.
func processRunTaskText(task *db.GroupTask) string {
	text := strings.TrimSpace(task.Title)
	if body := strings.TrimSpace(task.Body); body != "" {
		text += "\n\n" + body
	}
	if len(text) > processRunTaskTextMax {
		text = text[:processRunTaskTextMax]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return text
}

// liveProcessRunForTask is the id of the task's run still in progress, "" when
// it has none.
func liveProcessRunForTask(taskID string) (string, error) {
	runs, err := db.ProcessRunsForTask(taskID, 20)
	if err != nil {
		return "", err
	}
	for _, run := range runs {
		if engine.RunStatus(run.Status) == engine.RunRunning {
			return run.ID, nil
		}
	}
	return "", nil
}

// processRunTaskActor is who settles a task when its run ends.
func processRunTaskActor(runID string) string { return "system:process-run " + runID }

// settleProcessRunTask closes the task a run was for as the run ended:
// done when it completed; blocked, saying so, when it failed. A canceled run
// leaves the task as it is: someone stopped the process, not the work. A
// task closed meanwhile is left alone.
func settleProcessRunTask(runID string, status engine.RunStatus) {
	scope, err := db.GetProcessRunScope(runID)
	if err != nil || scope.TaskID == "" {
		return
	}
	task, err := db.GetGroupTask(scope.TaskID)
	if err != nil || task.Status == db.GroupTaskDone || task.Status == db.GroupTaskDropped {
		return
	}
	group, err := db.GetAgentGroupByID(task.GroupID)
	if err != nil || group == nil {
		return
	}
	name := processRunTemplateName(runID)
	var next, reason string
	switch status {
	case engine.RunCompleted:
		next, reason = db.GroupTaskDone, fmt.Sprintf("its process %s completed (run %s)", name, runID)
	case engine.RunFailed:
		next, reason = db.GroupTaskBlocked, fmt.Sprintf("its process %s failed (run %s)", name, runID)
		if failure := processRunFailure(runID); failure != nil {
			reason = fmt.Sprintf("its process %s failed at step %s (run %s): %s", name, failure.NodeID, runID,
				processConcernHeadline(sanitizeProcessText(failure.Reason)))
		}
	default:
		return
	}
	var waiters []string
	if next == db.GroupTaskDone {
		waiters = taskboardWaitersBefore(task)
	}
	out, err := db.SetGroupTaskStatus(task.ID, next, reason, processRunTaskActor(runID))
	if err != nil {
		slog.Warn("process run: settle its task", "run", runID, "task", task.ID, "error", err)
		return
	}
	// As a close by hand: the tasks waiting on it are told.
	if next == db.GroupTaskDone {
		mailTaskboardWaiters(nil, group, out, next, reason, waiters, task.AssigneeID == db.OperatorAssignee)
	}
}

// processRunTemplateName is the template id a run was started from, for a
// human reading the task's history; the run id when it cannot be read.
func processRunTemplateName(runID string) string {
	run, err := db.GetProcessRun(runID)
	if err != nil {
		return runID
	}
	if id, _, ok := strings.Cut(run.TemplateRef, "@"); ok && id != "" {
		return id
	}
	return run.TemplateRef
}

// taskCaptureWriter is an http.ResponseWriter that keeps what a handler wrote,
// so a taskboard change can run the process-run start path (its gates, its
// errors) and report the outcome in its own answer.
type taskCaptureWriter struct {
	header http.Header
	status int
	body   []byte
}

func (c *taskCaptureWriter) Header() http.Header {
	if c.header == nil {
		c.header = http.Header{}
	}
	return c.header
}
func (c *taskCaptureWriter) WriteHeader(status int) { c.status = status }
func (c *taskCaptureWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	c.body = append(c.body, b...)
	return len(b), nil
}

// taskProcessStart is how a taskboard change started a task's process: the
// run, or why none started.
type taskProcessStart struct {
	Process string `json:"process"`
	Run     string `json:"run,omitempty"`
	Error   string `json:"error,omitempty"`
}

// startTaskProcess starts template for task, scoped to the task's group.
// With asCaller the caller's own process.runs.start gate applies and the run
// is theirs; without, it is the board's way of working that starts it, set
// by whoever could (the board's options, or a choice made under
// process.runs.start), and the operator owns the run.
func startTaskProcess(r *http.Request, g *db.AgentGroup, task *db.GroupTask, template string, asCaller bool) taskProcessStart {
	out := taskProcessStart{Process: template}
	request := processRunCreateRequest{TemplateID: template, Task: task.ID, Group: g.Name}
	defer lockProcessRunTask(task.ID)()
	cw := &taskCaptureWriter{}
	if _, ok := resolveProcessRunTask(cw, r, &request, false); !ok {
		out.Error = capturedError(cw)
		return out
	}
	caller := ""
	scope := db.ProcessRunScope{Kind: db.ProcessRunScopeGroup, GroupID: g.ID}
	if asCaller {
		var ok bool
		caller, scope, ok = authorizeProcessRunCreate(cw, r, request, template)
		if !ok {
			out.Error = capturedError(cw)
			return out
		}
	}
	scope.TaskID = task.ID
	actor, err := processTemplateAuthor(caller)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	startProcessRunAs(cw, r, request, string(actor), scope)
	if cw.status != http.StatusCreated {
		out.Error = capturedError(cw)
		return out
	}
	var view struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(cw.body, &view)
	out.Run = view.ID
	return out
}

// capturedError is the message of an error a captured handler wrote.
func capturedError(cw *taskCaptureWriter) string {
	var answer struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(cw.body, &answer) == nil && answer.Error != "" {
		return answer.Error
	}
	if text := strings.TrimSpace(string(cw.body)); text != "" {
		return text
	}
	return fmt.Sprintf("HTTP %d", cw.status)
}

// taskProcessChoice is the process a task follows: its own choice, else its
// board's default way of working; "" for none.
func taskProcessChoice(task *db.GroupTask) string {
	own, err := db.GroupTaskProcess(task.ID)
	if err != nil {
		return ""
	}
	switch own {
	case db.GroupTaskProcessNone:
		return ""
	case "":
		_, opts, _, err := taskboardOptions(task.GroupID)
		if err != nil {
			return ""
		}
		return opts.Process
	default:
		return own
	}
}

// startTaskProcessOnStart starts the process a task follows once it is
// started (taken, started, or created started), if it has never had a run:
// a process runs for a task once, from its start. It reports what it did,
// nil when nothing was to start. asCaller makes the run the caller's (an
// edit that picks the process starts it as its editor); else it is the
// board's way of working, the operator's.
func startTaskProcessOnStart(r *http.Request, g *db.AgentGroup, task *db.GroupTask, asCaller ...bool) *taskProcessStart {
	if task == nil || !task.Started() || task.IsEpic() ||
		task.Status == db.GroupTaskDone || task.Status == db.GroupTaskDropped {
		return nil
	}
	template := taskProcessChoice(task)
	if template == "" {
		return nil
	}
	if runs, err := db.ProcessRunsForTask(task.ID, 1); err != nil || len(runs) > 0 {
		return nil
	}
	started := startTaskProcess(r, g, task, template, len(asCaller) > 0 && asCaller[0])
	if started.Error != "" {
		slog.Warn("taskboard: start the task's process", "task", task.ID, "process", template, "error", started.Error)
	}
	return &started
}

// validateTaskProcessChoice checks a task's choice of process as a write
// gives it: "" (follow the board), "none", or a saved template the caller
// may start in the group. It answers the error itself.
func validateTaskProcessChoice(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, choice string) bool {
	switch choice {
	case "", db.GroupTaskProcessNone:
		return true
	}
	if !boardopts.ValidProcess(choice) {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("process: %q is not a process template id", choice))
		return false
	}
	if _, ok := requireGroupPermission(w, r, PermProcessRunsStart, g, ActionContext{ProcessTemplate: choice}); !ok {
		return false
	}
	fs, err := store.NewFS(processStoreRoot())
	if err == nil {
		_, err = fs.GetTemplateHead(r.Context(), choice)
	}
	if err != nil {
		writeError(w, http.StatusNotFound, "process_template_not_found", fmt.Sprintf("no process template %q", choice))
		return false
	}
	return true
}
