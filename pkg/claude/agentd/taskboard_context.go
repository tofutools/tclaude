package agentd

import (
	"net/http"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// taskboard_context.go is the dashboard's read of every task board the caller
// can see (tcl-o1tzq6, docs/taskboard.md): per group, the counts its folded
// section and summary line show and the rows its open section lists. The
// Groups tab polls GET /v1/taskboard/context alongside the snapshot, as it
// does /v1/process/context.
//
// Visibility is the board's own read gate, group by group: the operator sees
// every active group; an agent sees the groups it reads through membership,
// ownership or a taskboard.read / taskboard.write grant (scoped or not), and
// an explicit deny hides a group. A group the caller cannot read is absent,
// never listed empty, so its existence does not leak.
//
// It is bounded per group: at most taskboardContextLiveRows live rows (the
// board is meant to hold about ten) and taskboardContextClosedRows of the
// most recently closed ones. The counts always cover every live task.

const (
	taskboardContextLiveRows   = 50
	taskboardContextClosedRows = 8
	taskboardDoneWindow        = 7 * 24 * time.Hour
)

// taskboardContextTask is a task row without its body. The task view
// carries the assignment and the last report.
type taskboardContextTask = taskboardTaskView

// taskboardCounts are a board's counts. Live is every open or blocked task;
// the stage buckets split the open ones: InProgress is assigned, Next and
// Ideas are unassigned in that stage. Queued (open with an unmet dependency)
// overlaps Next. NeedsYou is the blocked tasks for now; stalled assignments
// join it with the idle-without-report slice.
type taskboardCounts struct {
	Live         int `json:"live"`
	NeedsYou     int `json:"needs_you"`
	InProgress   int `json:"in_progress"`
	Next         int `json:"next"`
	Ideas        int `json:"ideas"`
	Blocked      int `json:"blocked"`
	Queued       int `json:"queued"`
	DoneThisWeek int `json:"done_this_week"`
	Dropped      int `json:"dropped"`
}

type taskboardGroupContext struct {
	GroupID int64                  `json:"group_id"`
	Group   string                 `json:"group"`
	Counts  taskboardCounts        `json:"counts"`
	Tasks   []taskboardContextTask `json:"tasks"`
	Closed  []taskboardContextTask `json:"closed"`
	// Truncated says the live rows stop short of Counts.Live.
	Truncated bool `json:"truncated,omitempty"`
}

type taskboardContextView struct {
	Groups []taskboardGroupContext `json:"groups"`
}

// GET /v1/taskboard/context — every readable board's counts and rows.
func handleTaskboardContext(w http.ResponseWriter, r *http.Request) {
	if !taskboardEnabled(w) {
		return
	}
	groups, ok := taskboardContextGroups(w, r)
	if !ok {
		return
	}
	now := time.Now()
	view := taskboardContextView{Groups: make([]taskboardGroupContext, 0, len(groups))}
	for _, g := range groups {
		entry, err := buildTaskboardGroupContext(g, now)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		view.Groups = append(view.Groups, entry)
	}
	writeJSON(w, http.StatusOK, view)
}

// taskboardContextGroups lists the active groups whose board the caller
// reads. Anyone neither the operator nor a confirmed agent (a peer grant,
// say) goes through the board's own gate group by group, so a grant scoped to
// some groups never reads as all of them.
func taskboardContextGroups(w http.ResponseWriter, r *http.Request) ([]*db.AgentGroup, bool) {
	conv := confirmedAgentConv(r)
	human := callerIsHuman(r)
	all, err := db.ListAgentGroups()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return nil, false
	}
	out := make([]*db.AgentGroup, 0, len(all))
	for _, g := range all {
		if g.IsArchived() {
			continue
		}
		switch {
		case human:
		case conv != "":
			if !agentReadsTaskboard(r, conv, g) {
				continue
			}
		default:
			if !requireTaskboard(&taskboardGateWriter{header: http.Header{}}, r, g, false) {
				continue
			}
		}
		out = append(out, g)
	}
	if !human && conv == "" && len(out) == 0 {
		// Nothing readable: answer with the gate's refusal, not an empty list.
		_, _ = requirePermission(w, r, PermTaskboardRead)
		return nil, false
	}
	return out, true
}

func buildTaskboardGroupContext(g *db.AgentGroup, now time.Time) (taskboardGroupContext, error) {
	entry := taskboardGroupContext{GroupID: g.ID, Group: g.Name, Tasks: []taskboardContextTask{}, Closed: []taskboardContextTask{}}
	tasks, err := db.ListGroupTasks(g.ID, db.GroupTaskListOpts{Closed: true})
	if err != nil {
		return entry, err
	}
	// ListGroupTasks orders the live tasks (next, then ideas, by rank) ahead
	// of the closed ones (most recent first).
	var live, closed []*db.GroupTask
	for _, t := range tasks {
		if t.Live() {
			live = append(live, t)
			continue
		}
		if t.Status == db.GroupTaskDropped {
			entry.Counts.Dropped++
		} else if t.Status == db.GroupTaskDone && !t.ClosedAt.IsZero() && now.Sub(t.ClosedAt) <= taskboardDoneWindow {
			entry.Counts.DoneThisWeek++
		}
		if len(closed) < taskboardContextClosedRows {
			closed = append(closed, t)
		}
	}
	liveViews, err := taskboardContextTasks(g.Name, live)
	if err != nil {
		return entry, err
	}
	for _, v := range liveViews {
		entry.Counts.Live++
		switch {
		case v.Status == db.GroupTaskBlocked:
			entry.Counts.Blocked++
			entry.Counts.NeedsYou++
		case v.Assignee != nil:
			entry.Counts.InProgress++
		case v.Stage == db.GroupTaskStageIdea:
			entry.Counts.Ideas++
		default:
			entry.Counts.Next++
		}
		if v.Queued {
			entry.Counts.Queued++
		}
	}
	if len(liveViews) > taskboardContextLiveRows {
		liveViews = liveViews[:taskboardContextLiveRows]
		entry.Truncated = true
	}
	entry.Tasks = liveViews
	if entry.Closed, err = taskboardContextTasks(g.Name, closed); err != nil {
		return entry, err
	}
	return entry, nil
}

// taskboardContextTasks is taskboardViews without the bodies (a row shows
// none).
func taskboardContextTasks(group string, tasks []*db.GroupTask) ([]taskboardContextTask, error) {
	views, err := taskboardViews(group, tasks)
	if err != nil {
		return nil, err
	}
	for i := range views {
		views[i].Body = ""
	}
	return views, nil
}

// dashboardTaskboardRoutes are the board's write routes the Groups tab's
// Task board section acts through (groups-taskboard.js). Each is the CLI's
// own route, so every dashboard action has a CLI verb.
var dashboardTaskboardRoutes = []struct {
	pattern string
	handler http.HandlerFunc
}{
	{"POST /v1/groups/{name}/taskboard/tasks", v1GroupRoute(handleTaskboardCreate)},
	{"GET /v1/taskboard/tasks/{id}/events", handleTaskboardTask(taskboardReadAccess, handleTaskboardEvents)},
	{"PATCH /v1/taskboard/tasks/{id}", handleTaskboardTask(taskboardWriteAccess, handleTaskboardEdit)},
	{"POST /v1/taskboard/tasks/{id}/move", handleTaskboardTask(taskboardWriteAccess, handleTaskboardMove)},
	{"POST /v1/taskboard/tasks/{id}/stage", handleTaskboardTask(taskboardWriteAccess, handleTaskboardStage)},
	{"POST /v1/taskboard/tasks/{id}/status", handleTaskboardTask(taskboardOwnGate, handleTaskboardStatus)},
	{"POST /v1/taskboard/tasks/{id}/assign", handleTaskboardTask(taskboardOwnGate, handleTaskboardAssign)},
	{"POST /v1/taskboard/tasks/{id}/unassign", handleTaskboardTask(taskboardOwnGate, handleTaskboardUnassign)},
}

func registerDashboardTaskboardRoutes(mux *http.ServeMux) {
	for _, route := range dashboardTaskboardRoutes {
		mux.HandleFunc(route.pattern, dashboardProcessRoute(route.handler))
	}
}
