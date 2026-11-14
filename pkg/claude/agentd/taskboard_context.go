package agentd

import (
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// taskboard_context.go is the dashboard's read of every task board the caller
// can see (tcl-o1tzq6, docs/taskboard.md): per group, the counts its folded
// section shows and the rows its open section lists. The
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
// the stage buckets split the open ones: Stalled is assigned to an agent idle
// past taskboard.idle_grace without reporting, InProgress is the other
// started ones (WaitingOnYou the operator's, started or queued for it), Next
// and Ideas are the rest in that stage, unassigned or held by an agent
// (IdeasForYou the operator's ideas). Queued (open with an unmet dependency)
// overlaps the others. NeedsYou is WaitingOnYou: what needs the operator is
// what is assigned to it (tk_3cb51ce0). A blocked or stalled task is only
// that, not the operator's to act on.
type taskboardCounts struct {
	Live       int `json:"live"`
	NeedsYou   int `json:"needs_you"`
	InProgress int `json:"in_progress"`
	Next       int `json:"next"`
	Ideas      int `json:"ideas"`
	Blocked    int `json:"blocked"`
	Stalled    int `json:"stalled"`
	// WaitingOnYou counts the live tasks assigned to the operator (a blocked
	// one counts in Blocked too), and the open questions on the other live
	// tasks; NeedsYou is the same count.
	WaitingOnYou int `json:"waiting_on_you"`
	// IdeasForYou counts the ideas the operator holds to triage
	// (tk_3826822a); they are not waiting on it.
	IdeasForYou  int `json:"ideas_for_you,omitempty"`
	Queued       int `json:"queued"`
	DoneThisWeek int `json:"done_this_week"`
	Dropped      int `json:"dropped"`
	// InColumns counts the open tasks in the board's custom columns
	// (per column in the group's Columns), so the buckets add up to Live.
	InColumns int `json:"in_columns,omitempty"`
}

// taskboardColumnCount is one custom column and how many open tasks stand
// in it.
type taskboardColumnCount struct {
	Slug  string `json:"slug"`
	Label string `json:"label"`
	Count int    `json:"count"`
	// State is hidden or cordoned; absent for shown (tk_bf5d2b02).
	State string `json:"state,omitempty"`
}

type taskboardGroupContext struct {
	GroupID int64           `json:"group_id"`
	Group   string          `json:"group"`
	Counts  taskboardCounts `json:"counts"`
	// Columns are the board's custom columns in its order, each with how
	// many open tasks stand in it (tcl-plby5h); those are not in the
	// counts' InProgress or Next. A blocked task in one counts as Blocked.
	// Kept apart from Counts, whose values are all numbers.
	Columns []taskboardColumnCount `json:"columns,omitempty"`
	// BuiltinColumns is the state of the built-in columns the board does
	// not show (ideas, done: hidden), by slug (tk_bf5d2b02).
	BuiltinColumns map[string]string      `json:"builtin_columns,omitempty"`
	Tasks          []taskboardContextTask `json:"tasks"`
	Closed         []taskboardContextTask `json:"closed"`
	// Truncated says the live rows stop short of Counts.Live.
	Truncated bool `json:"truncated,omitempty"`
	// Locked says the writer lock narrows the board; Writers then lists the
	// active members (agent ids) who may still change it, which "+ New task ›
	// Ask an agent" picks from. Without the lock every member may.
	Locked  bool     `json:"locked,omitempty"`
	Writers []string `json:"writers,omitempty"`
	// Askable lists the active members and owners (agent ids) "Ask an
	// agent" may mail: those who write the board (taskboard.write, which a
	// member holds only through a grant) and pass its lock. A member that
	// only tracks its own work there (taskboard.own) is not one.
	Askable []string `json:"askable"`
	// Options summarises the board's options (taskboard_context_options.go);
	// absent from the node summary.
	Options *taskboardContextOptions `json:"options,omitempty"`
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
	globals := taskboardOptionsGlobals()
	view := taskboardContextView{Groups: make([]taskboardGroupContext, 0, len(groups))}
	for _, g := range groups {
		// The options summary is the board's settings: only for a caller
		// that reads them (taskboard_options_access.go). A task's own queue
		// position is a task fact and reads with the board.
		var opts *boardopts.Globals
		if callerReadsTaskboardOptions(r, g) {
			opts = &globals
		}
		entry, err := buildTaskboardGroupContext(r, g, now, opts)
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

// globals are the global board options the options summary resolves
// against, read once per request; nil leaves the summary out.
func buildTaskboardGroupContext(r *http.Request, g *db.AgentGroup, now time.Time, globals *boardopts.Globals) (taskboardGroupContext, error) {
	entry := taskboardGroupContext{GroupID: g.ID, Group: g.Name, Tasks: []taskboardContextTask{}, Closed: []taskboardContextTask{}}
	// A nil request wants only the counts (the node summary).
	if r != nil {
		if err := taskboardContextWriters(r, g, &entry); err != nil {
			return entry, err
		}
		if err := taskboardContextAskable(r, g, &entry); err != nil {
			return entry, err
		}
	}
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
	if globals != nil {
		// The summary is a convenience: a board whose options cannot be
		// read still lists its tasks.
		if opts, err := taskboardContextOptionsOf(g, *globals, live); err != nil {
			slog.Warn("taskboard: reading board options for the context failed", "group", g.Name, "error", err)
		} else {
			entry.Options = opts
		}
	}
	liveViews, err := taskboardContextTasks(g.Name, live)
	if err != nil {
		return entry, err
	}
	cols, err := db.ListGroupTaskColumns(g.ID)
	if err != nil {
		return entry, err
	}
	states, err := db.ListGroupTaskColumnStates(g.ID)
	if err != nil {
		return entry, err
	}
	colAt := map[string]int{}
	for i, c := range cols {
		colAt[c.Slug] = i
		entry.Columns = append(entry.Columns, taskboardColumnCount{Slug: c.Slug, Label: c.Label, State: states[c.Slug]})
	}
	entry.BuiltinColumns = taskboardBuiltinColumnStates(g)
	for _, v := range liveViews {
		entry.Counts.Live++
		i, inColumn := colAt[v.Column]
		// A task the operator holds waits on it in any column, blocked
		// too (tk_3cb51ce0); its ideas to triage do not.
		operatorIdea := v.Held && v.Stage == db.GroupTaskStageIdea
		operatorWaits := v.Assignee != nil && v.Assignee.Operator && !operatorIdea
		switch {
		case v.Status == db.GroupTaskBlocked:
			entry.Counts.Blocked++
			if operatorWaits {
				entry.Counts.WaitingOnYou++
				entry.Counts.NeedsYou++
			}
		case v.Stalled:
			entry.Counts.Stalled++
		case v.Assignee != nil && v.Assignee.Operator && operatorIdea:
			entry.Counts.IdeasForYou++
		case v.Assignee != nil && v.Assignee.Operator:
			entry.Counts.WaitingOnYou++
			entry.Counts.NeedsYou++
		case inColumn && v.Column != "":
			entry.Columns[i].Count++
			entry.Counts.InColumns++
		case v.Assignee != nil && !v.Held:
			entry.Counts.InProgress++
		case v.Stage == db.GroupTaskStageIdea:
			entry.Counts.Ideas++
		default:
			entry.Counts.Next++
		}
		// An open question on a task the operator does not already wait
		// on as a whole waits on it too (tk_8b2c86f7).
		if !operatorWaits {
			entry.Counts.WaitingOnYou += v.OpenQuestions
			entry.Counts.NeedsYou += v.OpenQuestions
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
	{"GET /v1/groups/{name}/taskboard", v1GroupRoute(handleTaskboardList)},
	{"POST /v1/groups/{name}/taskboard/tasks", v1GroupRoute(handleTaskboardCreate)},
	{"POST /v1/groups/{name}/taskboard/ask", v1GroupRoute(handleTaskboardAsk)},
	{"POST /v1/groups/{name}/taskboard/ref-lookup", v1GroupRoute(handleTaskboardRefLookup)},
	{"GET /v1/taskboard/tasks/{id}", handleTaskboardTask(taskboardReadAccess, handleTaskboardShow)},
	{"GET /v1/taskboard/tasks/{id}/events", handleTaskboardTask(taskboardReadAccess, handleTaskboardEvents)},
	{"PATCH /v1/taskboard/tasks/{id}", handleTaskboardTask(taskboardOwnGate, handleTaskboardEdit)},
	{"POST /v1/taskboard/tasks/{id}/move", handleTaskboardTask(taskboardOwnGate, handleTaskboardMove)},
	{"POST /v1/taskboard/tasks/{id}/stage", handleTaskboardTask(taskboardOwnGate, handleTaskboardStage)},
	{"POST /v1/taskboard/tasks/{id}/status", handleTaskboardTask(taskboardOwnGate, handleTaskboardStatus)},
	{"POST /v1/taskboard/tasks/{id}/reopen", handleTaskboardTask(taskboardOwnGate, handleTaskboardReopen)},
	{"POST /v1/taskboard/tasks/{id}/archive", handleTaskboardTask(taskboardOwnGate, handleTaskboardArchive)},
	{"POST /v1/taskboard/tasks/{id}/unarchive", handleTaskboardTask(taskboardOwnGate, handleTaskboardUnarchive)},
	{"POST /v1/groups/{name}/taskboard/archive-done", v1GroupRoute(handleTaskboardArchiveDone)},
	{"POST /v1/groups/{name}/taskboard/archive-closed", v1GroupRoute(handleTaskboardArchiveClosed)},
	{"POST /v1/taskboard/tasks/{id}/move-to-group", handleTaskboardTask(taskboardOwnGate, handleTaskboardMoveToGroup)},
	{"POST /v1/taskboard/tasks/{id}/moved-away", handleTaskboardTask(taskboardOwnGate, handleTaskboardMovedAway)},
	{"POST /v1/groups/{name}/taskboard/transfer-in", v1GroupRoute(handleTaskboardTransferIn)},
	{"POST /v1/taskboard/tasks/{id}/assign", handleTaskboardTask(taskboardOwnGate, handleTaskboardAssign)},
	{"POST /v1/taskboard/tasks/{id}/unassign", handleTaskboardTask(taskboardOwnGate, handleTaskboardUnassign)},
	{"POST /v1/taskboard/tasks/{id}/start", handleTaskboardTask(taskboardOwnGate, handleTaskboardStart)},
	{"POST /v1/taskboard/tasks/{id}/answer", handleTaskboardTask(taskboardOwnGate, handleTaskboardAnswer)},
	{"GET /v1/taskboard/tasks/{id}/answer", handleTaskboardTask(taskboardReadAccess, handleTaskboardAnswerGet)},
	// A linked node's agent asks this node's operator on a task (the peer
	// view's mux is this one); the operator's own ask is refused there.
	{"POST /v1/taskboard/tasks/{id}/questions", handleTaskboardTask(taskboardOwnGate, handleTaskboardAskOperator)},
	{"POST /v1/taskboard/tasks/{id}/questions/{n}/answer", handleTaskboardTask(taskboardOwnGate, handleTaskboardAnswer)},
	{"GET /v1/taskboard/tasks/{id}/questions/{n}", handleTaskboardTask(taskboardReadAccess, handleTaskboardAnswerGet)},
	{"GET /v1/groups/{name}/taskboard/writers", v1GroupRoute(handleTaskboardWritersGet)},
	{"POST /v1/groups/{name}/taskboard/writers", v1GroupRoute(handleTaskboardWritersSet)},
	{"POST /v1/taskboard/tasks/{id}/attachments", handleTaskboardTask(taskboardOwnGate, handleTaskboardAttach)},
	{"GET /v1/taskboard/tasks/{id}/attachments", handleTaskboardTask(taskboardReadAccess, handleTaskboardAttachments)},
	{"GET /v1/taskboard/tasks/{id}/attachments/{aid}", handleTaskboardTask(taskboardReadAccess, handleTaskboardAttachmentGet)},
	{"DELETE /v1/taskboard/tasks/{id}/attachments/{aid}", handleTaskboardTask(taskboardOwnGate, handleTaskboardDetach)},
}

func registerDashboardTaskboardRoutes(mux *http.ServeMux) {
	for _, route := range dashboardTaskboardRoutes {
		mux.HandleFunc(route.pattern, dashboardProcessRoute(route.handler))
	}
	for _, route := range taskboardCommentRoutes {
		mux.HandleFunc(route.pattern, dashboardProcessRoute(route.handler))
	}
	for _, route := range taskboardColumnRoutes {
		mux.HandleFunc(route.pattern, dashboardProcessRoute(route.handler))
	}
	for _, route := range taskboardOptionRoutes {
		mux.HandleFunc(route.pattern, dashboardProcessRoute(route.handler))
	}
	// A drawing board's File › Export › Attach to a task… (tk_413bdf5d).
	// Registered here, not in the table: rendering reaches the peer-view
	// proxy, whose mux is this one, so a package-level entry would be an
	// initialization cycle.
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/drawings", dashboardProcessRoute(handleTaskboardTask(taskboardOwnGate, handleTaskboardDrawing)))
}

// taskboardContextWriters fills Locked and Writers: with the lock set, the
// active members and owners who pass it. Listed writers and owners pass
// without a permission lookup; only the other members are looked up.
func taskboardContextWriters(r *http.Request, g *db.AgentGroup, entry *taskboardGroupContext) error {
	lock, err := db.GetTaskboardLock(g.ID)
	if err != nil || !lock.Locked {
		return err
	}
	entry.Locked = true
	entry.Writers = []string{}
	seen := map[string]bool{}
	add := func(id string) {
		if !seen[id] {
			seen[id] = true
			entry.Writers = append(entry.Writers, id)
		}
	}
	owners, err := db.ListAgentGroupOwners(g.ID)
	if err != nil {
		return err
	}
	for _, o := range owners {
		if activeGroupOwner(g, o.AgentID) != "" {
			add(o.AgentID)
		}
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return err
	}
	for _, m := range members {
		id, err := db.AgentIDForConv(m.ConvID)
		if err != nil || id == "" || seen[id] {
			continue
		}
		if state, err := db.AgentState(m.ConvID); err != nil || state == db.AgentStateRetired {
			continue
		}
		if slices.Contains(lock.Writers, id) || taskboardMemberWrites(r, g, m.ConvID) {
			add(id)
		}
	}
	return nil
}

// taskboardContextAskable fills Askable: the active owners and members who
// may be asked for tasks, as handleTaskboardAsk decides it.
func taskboardContextAskable(r *http.Request, g *db.AgentGroup, entry *taskboardGroupContext) error {
	entry.Askable = []string{}
	seen := map[string]bool{}
	owners, err := db.ListAgentGroupOwners(g.ID)
	if err != nil {
		return err
	}
	for _, o := range owners {
		if conv := activeGroupOwner(g, o.AgentID); !seen[o.AgentID] && conv != "" && taskboardMemberWrites(r, g, conv) {
			seen[o.AgentID] = true
			entry.Askable = append(entry.Askable, o.AgentID)
		}
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return err
	}
	for _, m := range members {
		id, err := db.AgentIDForConv(m.ConvID)
		if err != nil || id == "" || seen[id] {
			continue
		}
		if state, err := db.AgentState(m.ConvID); err != nil || state == db.AgentStateRetired {
			continue
		}
		if taskboardMemberWrites(r, g, m.ConvID) {
			seen[id] = true
			entry.Askable = append(entry.Askable, id)
		}
	}
	return nil
}
