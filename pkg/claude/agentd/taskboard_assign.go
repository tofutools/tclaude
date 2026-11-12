package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Task board assignment and reports (tcl-2eeakl, docs/taskboard.md).
//
// A task has at most one assignee, an active member of the task's group.
// Assigning another agent (and unassigning it, and closing its task as done)
// takes taskboard.assign, scoped by group and target_agent and conferred by
// ownership of the group; taking an open, unassigned task for yourself (an
// idea is promoted on the way) and releasing it takes taskboard.own
// (taskboard_own.go) and is narrowed by the writer lock; reporting done or
// failed on your own task takes taskboard.own and is never locked. The
// assignee gets an inbox brief from the assigner. An assignment never retires,
// releases or otherwise touches the agent.

const PermTaskboardAssign = "taskboard.assign"

func registerTaskboardAssignRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/assign", handleTaskboardTask(taskboardOwnGate, handleTaskboardAssign))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/unassign", handleTaskboardTask(taskboardOwnGate, handleTaskboardUnassign))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/start", handleTaskboardTask(taskboardOwnGate, handleTaskboardStart))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/report", handleTaskboardTask(taskboardOwnGate, handleTaskboardReport))
	mux.HandleFunc("GET /v1/taskboard/mine", handleTaskboardMine)
	mux.HandleFunc("GET /v1/groups/{name}/taskboard/writers", v1GroupRoute(handleTaskboardWritersGet))
	mux.HandleFunc("POST /v1/groups/{name}/taskboard/writers", v1GroupRoute(handleTaskboardWritersSet))
}

// callerAgentID is the confirmed calling agent's id and conv, "" for anyone
// else.
func callerAgentID(r *http.Request) (agentID, conv string) {
	conv = confirmedAgentConv(r)
	if conv == "" {
		return "", ""
	}
	id, err := db.AgentIDForConv(conv)
	if err != nil {
		return "", ""
	}
	return id, conv
}

// requireTaskboardAssigneeAction gates an action on assignee's behalf: the
// assignee itself needs taskboard.own (and, when lockApplies, a pass of the
// writer lock); anyone else needs taskboard.assign for the group and that
// target agent.
func requireTaskboardAssigneeAction(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, assignee string, lockApplies bool) bool {
	if self, _ := callerAgentID(r); self != "" && self == assignee {
		return requireTaskboardOwn(w, r, g, lockApplies)
	}
	// The operator is no agent: a grant narrowed to target agents does not
	// reach it, so only the group scope is described.
	if assignee == db.OperatorAssignee {
		_, ok := requireGroupPermission(w, r, PermTaskboardAssign, g, ActionContext{Group: g.Name})
		return ok
	}
	conv, _ := db.CurrentConvForAgent(assignee)
	_, ok := requireGroupPermission(w, r, PermTaskboardAssign, g,
		ActionContext{Group: g.Name, TargetAgent: assignee, targetConv: conv})
	return ok
}

// activeGroupMember returns the agent's live conv when agentID is an active,
// non-retired member of g, "" otherwise.
func activeGroupMember(g *db.AgentGroup, agentID string) string {
	if g == nil || g.IsArchived() || agentID == "" {
		return ""
	}
	members, err := db.ListAgentGroupMembers(g.ID)
	if err != nil {
		return ""
	}
	for _, m := range members {
		id, err := db.AgentIDForConv(m.ConvID)
		if err != nil || id != agentID {
			continue
		}
		if state, err := db.AgentState(m.ConvID); err != nil || state == db.AgentStateRetired {
			return ""
		}
		return m.ConvID
	}
	return ""
}

// resolveTaskboardMember resolves a selector (name, agent id, conv id or
// prefix) to an active member of g.
func resolveTaskboardMember(g *db.AgentGroup, selector string) (agentID, conv string, err error) {
	// One answer whether the agent is missing or only outside the group, so
	// a caller who assigns in one group (a linked node, say) cannot probe
	// for agents elsewhere on this node.
	notMember := func() error {
		return fmt.Errorf("%s is not an active member of %s; an assignee must be", guideText(selector, 60), g.Name)
	}
	res, _, rerr := agent.ResolveSelector(selector)
	if rerr != nil || res.ConvID == "" {
		return "", "", notMember()
	}
	agentID, _ = db.AgentIDForConv(res.ConvID)
	if agentID == "" {
		return "", "", notMember()
	}
	if conv = activeGroupMember(g, agentID); conv == "" {
		return "", "", notMember()
	}
	return agentID, conv, nil
}

// taskboardCallerName is how the caller is named in mail; a linked node's
// change is named as its own.
func taskboardCallerName(r *http.Request) string {
	if author := taskboardPeerAuthor(r); author != "" {
		return author
	}
	if id, conv := callerAgentID(r); id != "" {
		if t := agent.CachedTitle(conv); t != "" && t != agent.UnknownTitle {
			return t
		}
		return id
	}
	return "the operator"
}

// mailTaskboard queues an inbox message about a task from the caller to conv.
// Delivery is inbox mail, never send-keys. A failure is logged: the change has
// already been made.
func mailTaskboard(r *http.Request, g *db.AgentGroup, toConv, subject, body string) {
	from := confirmedAgentConv(r)
	m := &db.AgentMessage{GroupID: g.ID, FromConv: from, ToConv: toConv, Subject: subject, Body: body,
		ToRecipients: []string{toConv}, OperatorAuthored: from == "" && callerIsHuman(r) && taskboardPeerAuthor(r) == ""}
	if _, err := queueAgentMessage(m); err != nil {
		slog.Warn("taskboard: mail failed", "to", toConv, "error", err)
	}
}

// taskboardSubject leads with what happened, so a truncated inbox listing
// still tells the notices apart: `[taskboard] assigned to you by lead: crew ·
// tk_… "title"`.
func taskboardSubject(g *db.AgentGroup, t *db.GroupTask, what string) string {
	return fmt.Sprintf("[taskboard] %s: %s · %s %q", what, g.Name, t.ID, t.Title)
}

// taskboardBrief is the assignee's inbox brief: the task, its ref and
// dependencies, and the exact report commands.
func taskboardBrief(g *db.AgentGroup, t *db.GroupTask, by string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task %s in group %s: %q\nAssigned to you by %s.\n", t.ID, g.Name, t.Title, by)
	if t.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", t.Body)
	}
	for i, ref := range t.Refs() {
		if i == 0 {
			b.WriteString("\n")
		}
		fmt.Fprintf(&b, "Ref (%s): %s %s\n", ref.Kind, ref.Label, ref.URL)
	}
	if len(t.DependsOn) > 0 {
		fmt.Fprintf(&b, "After: %s\n", strings.Join(t.DependsOn, ", "))
	}
	if files, err := db.ListGroupTaskAttachments(t.ID); err == nil && len(files) > 0 {
		names := make([]string, 0, len(files))
		for _, f := range files {
			names = append(names, f.Name)
		}
		fmt.Fprintf(&b, "Files: %s\n", strings.Join(names, ", "))
		fmt.Fprintf(&b, "Fetch them:        tclaude agent taskboard attachments %s --save <dir>\n", t.ID)
	}
	fmt.Fprintf(&b, "\nWhen it is done:   tclaude agent taskboard report done --task %s --note \"what you did\"\n", t.ID)
	fmt.Fprintf(&b, "If you cannot:     tclaude agent taskboard report failed --task %s --note \"why\"\n", t.ID)
	fmt.Fprintf(&b, "Give it back:      tclaude agent taskboard release %s\n", t.ID)
	fmt.Fprintf(&b, "Show it:           tclaude agent taskboard show %s\n", t.ID)
	b.WriteString("\nReporting does not end your session.\n")
	return b.String()
}

// POST /v1/taskboard/tasks/{id}/assign {"to": "<agent>"} — assign the task to
// an active member of its group; {"to": ""} (or the caller itself) takes it.
// The task starts (in progress) unless "start": false: then its assignee
// holds it in "stage" (idea or next; its own stage when omitted).
func handleTaskboardAssign(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		To    string `json:"to"`
		Start *bool  `json:"start"`
		Stage string `json:"stage"`
		// OverLimit lets the operator start the task in a full In progress.
		OverLimit bool `json:"over_limit"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	hold := ""
	if body.Start != nil && !*body.Start {
		hold = firstNonEmpty(strings.TrimSpace(body.Stage), t.Stage)
	} else if strings.TrimSpace(body.Stage) != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a started task is in progress; stage names where a held one sits (start false)")
		return
	}
	self, selfConv := callerAgentID(r)
	to := strings.TrimSpace(body.To)
	var target, targetConv string
	if to == "" {
		if self == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "name the agent to assign (to)")
			return
		}
		target, targetConv = self, selfConv
	} else {
		var err error
		if target, targetConv, err = resolveTaskboardAssignee(g, to); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	take := self != "" && target == self
	if take {
		if activeGroupMember(g, self) == "" {
			writeError(w, http.StatusConflict, "not_member", "only an active member of "+g.Name+" takes its tasks")
			return
		}
		if !requireTaskboardAssigneeAction(w, r, g, self, true) {
			return
		}
	} else {
		if target == db.OperatorAssignee {
			if !requireTaskboardOperatorAssign(w, r, g, t) {
				return
			}
		} else if _, ok := requireGroupPermission(w, r, PermTaskboardAssign, g,
			ActionContext{Group: g.Name, TargetAgent: target, targetConv: targetConv}); !ok {
			return
		}
		// A reassign also takes the task away from its holder: the caller
		// must be allowed to do that too.
		if t.AssigneeID != "" && t.AssigneeID != target && !requireTaskboardAssigneeAction(w, r, g, t.AssigneeID, false) {
			return
		}
	}
	// Started, the task enters In progress and meets its limit; queued,
	// it is assigned held in next and waits in line.
	defer lockTaskboardLanes(g.ID)()
	want := *t
	want.AssigneeID, want.StartedAt = target, time.Time{}
	if hold == "" {
		want.StartedAt = time.Now()
	}
	hit, ok := checkTaskboardLimit(w, r, g, t, &want, body.OverLimit)
	if !ok {
		return
	}
	if hit.queued() {
		hold = db.GroupTaskStageNext
	}
	before := t.AssigneeID
	out, err := db.AssignGroupTaskHeld(t.ID, target, taskboardActor(r), take, hold, taskboardActor(r), before)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	out, note := queueTaskboardLimit(r, g, hit, out)
	told := map[string]bool{}
	setAuditTargetConv(r, targetConv)
	if take {
		setAuditDetail(r, "took "+t.ID)
	} else {
		setAuditDetail(r, "assigned "+t.ID+" to "+target)
	}
	// Re-assigning a task its assignee held starts it: brief them, as start does.
	if !take && before == target && hold == "" && !t.Started() && target != db.OperatorAssignee && out.Started() {
		by := taskboardCallerName(r)
		n := newTaskboardNotice(r, g, out, boardopts.EventStarted)
		n.told = told
		n.mail(boardopts.RecipientAssignee, target, targetConv,
			taskboardSubject(g, out, "started for you by "+by), taskboardLinked(g, out, taskboardBrief(g, out, by)))
		n.others(fmt.Sprintf("started for %s by %s", taskboardAgentLabel(target), by), "", boardopts.RecipientAssignee)
	}
	if before != target {
		by := taskboardCallerName(r)
		n := newTaskboardNotice(r, g, out, boardopts.EventAssigned)
		n.told = told
		// A reassign is also the unassigned event of the agent losing the
		// task. Both notices share who has heard, so nobody hears twice.
		var gone *taskboardNotice
		if !take && before != "" {
			gone = newTaskboardNotice(r, g, t, boardopts.EventUnassigned)
			gone.told = n.told
			if conv := activeGroupMember(g, before); conv != "" {
				gone.mail(boardopts.RecipientAssignee, before, conv, taskboardSubject(g, out, "reassigned by "+by),
					taskboardLinked(g, out, fmt.Sprintf("Task %s %q in %s was reassigned by %s. You no longer hold it.\n", out.ID, out.Title, g.Name, by)))
			}
		}
		switch {
		case take, target == db.OperatorAssignee:
		case note.queued():
			subject, text := taskboardQueuedNote(g, out, by, note)
			n.mail(boardopts.RecipientAssignee, target, targetConv, subject, text)
		case hold != "":
			subject, text := taskboardHeldNote(g, out, by)
			n.mail(boardopts.RecipientAssignee, target, targetConv, subject, text)
		default:
			n.mail(boardopts.RecipientAssignee, target, targetConv,
				taskboardSubject(g, out, "assigned to you by "+by), taskboardLinked(g, out, taskboardBrief(g, out, by)))
		}
		what := fmt.Sprintf("assigned to %s by %s", taskboardAgentLabel(target), by)
		if take {
			what = "taken by " + taskboardAgentLabel(target)
		}
		if before != "" {
			what += ", from " + taskboardAgentLabel(before)
		}
		n.others(what, "", boardopts.RecipientAssignee)
		if gone != nil {
			gone.others(fmt.Sprintf("reassigned from %s to %s by %s", taskboardAgentLabel(before), taskboardAgentLabel(target), by), "",
				boardopts.RecipientAssignee)
		}
	}
	announceTaskboardLimit(r, g, out, note, told)
	writeTaskboardTaskLimit(w, http.StatusOK, g, out, note)
}

// POST /v1/taskboard/tasks/{id}/unassign — the assignee releases its task, or
// an assigner takes it back.
func handleTaskboardUnassign(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	// {"self": true} is `release`: only the holder gives a task back, so a
	// caller who could also take it back from someone else does not by mistake.
	var body struct {
		Self bool `json:"self"`
	}
	if r.ContentLength != 0 && !decodeTaskboardBody(w, r, &body) {
		return
	}
	if t.AssigneeID == "" {
		writeError(w, http.StatusConflict, "not_assigned", "task "+t.ID+" is not assigned")
		return
	}
	self, _ := callerAgentID(r)
	release := self != "" && self == t.AssigneeID
	// A release names the caller's own task: refuse one it does not hold
	// before the gate, which would otherwise ask for taskboard.assign (and
	// could put that to the operator) for something the caller never meant.
	if body.Self && !release {
		writeError(w, http.StatusConflict, "not_assignee", "task "+t.ID+" is not assigned to you; to take it back from its assignee, use unassign")
		return
	}
	if !requireTaskboardAssigneeAction(w, r, g, t.AssigneeID, true) {
		return
	}
	detail := t.AssigneeID
	if !release {
		detail = "from " + t.AssigneeID
	}
	out, err := db.UnassignGroupTask(t.ID, release, detail, taskboardActor(r), t.AssigneeID)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	conv, _ := db.CurrentConvForAgent(t.AssigneeID)
	setAuditTargetConv(r, conv)
	setAuditDetail(r, "unassigned "+t.ID+" from "+t.AssigneeID)
	by := taskboardCallerName(r)
	n := newTaskboardNotice(r, g, t, boardopts.EventUnassigned)
	what := "released by " + taskboardAgentLabel(t.AssigneeID)
	if !release {
		what = fmt.Sprintf("taken back from %s by %s", taskboardAgentLabel(t.AssigneeID), by)
		if conv := activeGroupMember(g, t.AssigneeID); conv != "" {
			n.mail(boardopts.RecipientAssignee, t.AssigneeID, conv, taskboardSubject(g, out, "unassigned by "+by),
				taskboardLinked(g, out, fmt.Sprintf("Task %s %q in %s was taken back by %s. You no longer hold it.\n", out.ID, out.Title, g.Name, by)))
		}
	}
	n.others(what, "", boardopts.RecipientAssignee)
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// POST /v1/taskboard/tasks/{id}/report {"outcome":"done"|"failed","note":…} —
// the assignee's report. Only the current assignee reports; a report after an
// unassign or reassign is refused not_assignee (409); a repeat is
// acknowledged. done closes the task and leaves the agent where it is.
func handleTaskboardReport(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Outcome string `json:"outcome"`
		Note    string `json:"note"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	self, _ := callerAgentID(r)
	if self == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "only the assignee reports; close a task with done")
		return
	}
	if t.AssigneeID == self && activeGroupMember(g, self) == "" {
		setAuditDetail(r, "refused: not a member, "+t.ID)
		writeError(w, http.StatusConflict, "not_member", "you are no longer an active member of "+g.Name)
		return
	}
	if !requireTaskboardOwn(w, r, g, false) {
		return
	}
	outcome := strings.TrimSpace(body.Outcome)
	switch outcome {
	case "done", db.GroupTaskOutcomeSucceeded:
		outcome = db.GroupTaskOutcomeSucceeded
	case db.GroupTaskOutcomeFailed:
	default:
		writeError(w, http.StatusBadRequest, "invalid_arg", "outcome is done or failed")
		return
	}
	note, err := validateTaskText("the note", strings.TrimSpace(body.Note), db.GroupTaskOutcomeNoteMax)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	var waiters []string
	if outcome == db.GroupTaskOutcomeSucceeded {
		waiters = taskboardWaitersBefore(t)
	}
	rep, err := db.ReportGroupTask(t.ID, self, outcome, note, taskboardActor(r))
	if errors.Is(err, db.ErrGroupTaskNotAssignee) {
		setAuditDetail(r, "refused: not the assignee of "+t.ID)
		writeError(w, http.StatusConflict, "not_assignee", err.Error())
		return
	}
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, "reported "+outcome+" on "+t.ID)
	views, err := taskboardViews(g.Name, []*db.GroupTask{rep.Task})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if !rep.Repeated {
		taskboardNotifyAssigner(r, g, t, rep, outcome, note)
		if outcome == db.GroupTaskOutcomeSucceeded {
			mailTaskboardWaiters(r, g, rep.Task, db.GroupTaskDone, note, waiters, false)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": views[0], "repeated": rep.Repeated, "blocked": rep.Blocked})
}

// taskboardNotifyAssigner tells the agent that assigned the task about the
// report, when that was an agent other than the reporter, and anyone else
// the board sends report messages to.
func taskboardNotifyAssigner(r *http.Request, g *db.AgentGroup, before *db.GroupTask, rep *db.GroupTaskReport, outcome, note string) {
	event := map[string]string{db.GroupTaskOutcomeSucceeded: boardopts.EventReportDone, db.GroupTaskOutcomeFailed: boardopts.EventReportFailed}[outcome]
	n := newTaskboardNotice(r, g, before, event)
	who := taskboardCallerName(r)
	tail := who + " reported " + map[string]string{db.GroupTaskOutcomeSucceeded: "done", db.GroupTaskOutcomeFailed: "failed"}[outcome]
	text := fmt.Sprintf("Task %s %q in %s: %s.\n", before.ID, before.Title, g.Name, tail)
	if note != "" {
		text += "\nNote: " + note + "\n"
	}
	if rep.Blocked {
		text += "\nThe task is now blocked after " + fmt.Sprint(rep.Task.Failures) + " failed attempts.\n"
	}
	subject, body := taskboardSubject(g, before, tail), taskboardLinked(g, before, text)
	if assigner, ok := strings.CutPrefix(before.AssignedBy, "agent:"); ok && assigner != before.AssigneeID {
		n.mail(boardopts.RecipientAssigner, assigner, activeTaskboardAgentConv(assigner), subject, body)
	}
	n.others(tail, body, boardopts.RecipientAssigner)
}

// GET /v1/taskboard/mine — the live tasks assigned to the calling agent, in
// every group.
func handleTaskboardMine(w http.ResponseWriter, r *http.Request) {
	if !taskboardEnabled(w) {
		return
	}
	self, conv := callerAgentID(r)
	if self == "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "only an agent has tasks assigned to it")
		return
	}
	assigned, err := db.ListGroupTasksAssignedTo(self)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	views := []taskboardTaskView{}
	readable := map[string]bool{}
	for _, a := range assigned {
		// Only the groups the agent is still an active member of and reads:
		// a task left on it after it left the group stays out of view.
		ok, seen := readable[a.GroupName]
		if !seen {
			g, err := db.GetAgentGroupByName(a.GroupName)
			ok = err == nil && g != nil && activeGroupMember(g, self) != "" && agentReadsTaskboard(r, conv, g)
			readable[a.GroupName] = ok
		}
		if !ok {
			continue
		}
		v, err := taskboardViews(a.GroupName, []*db.GroupTask{a.Task})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		views = append(views, v...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"group": "", "tasks": views})
}

type taskboardWritersView struct {
	Group   string              `json:"group"`
	Locked  bool                `json:"locked"`
	Writers []taskboardAgentRef `json:"writers"`
	SetBy   string              `json:"set_by,omitempty"`
}

func writeTaskboardWriters(w http.ResponseWriter, g *db.AgentGroup) {
	lock, err := db.GetTaskboardLock(g.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	out := taskboardWritersView{Group: g.Name, Locked: lock.Locked, Writers: []taskboardAgentRef{}, SetBy: lock.SetBy}
	for _, id := range lock.Writers {
		out.Writers = append(out.Writers, taskboardAgentRef{AgentID: id, Name: taskboardAgentName(id)})
	}
	writeJSON(w, http.StatusOK, out)
}

// GET /v1/groups/{name}/taskboard/writers — the board's writer lock.
func handleTaskboardWritersGet(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !taskboardEnabled(w) || !requireTaskboard(w, r, g, false) {
		return
	}
	writeTaskboardWriters(w, g)
}

// POST /v1/groups/{name}/taskboard/writers {"agents":[…]} | {"members":true}
// — lock the board to the listed members (owners, explicit grant holders and
// the operator stay writers), or open it to every member again.
func handleTaskboardWritersSet(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !taskboardEnabled(w) {
		return
	}
	if _, ok := requireGroupPermission(w, r, PermTaskboardManage, g); !ok || !requireGroupActive(w, g) {
		return
	}
	var body struct {
		Agents  []string `json:"agents"`
		Members bool     `json:"members"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	if body.Members && len(body.Agents) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "give agents or members, not both")
		return
	}
	var writers []string
	if !body.Members {
		writers = []string{}
		for _, sel := range body.Agents {
			if sel = strings.TrimSpace(sel); sel == "" {
				continue
			}
			id, _, err := resolveTaskboardMember(g, sel)
			if err != nil {
				writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
				return
			}
			writers = append(writers, id)
		}
	}
	if err := db.SetTaskboardLock(g.ID, writers, taskboardActor(r)); err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if writers == nil {
		setAuditDetail(r, "unlocked")
	} else {
		setAuditDetail(r, fmt.Sprintf("locked to %d writers", len(writers)))
	}
	writeTaskboardWriters(w, g)
}
