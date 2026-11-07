package agentd

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Task board assignment and reports (tcl-2eeakl, docs/taskboard.md).
//
// A task has at most one assignee, an active member of the task's group.
// Assigning another agent (and unassigning it, and closing its task as done)
// takes taskboard.assign, scoped by group and target_agent and conferred by
// ownership of the group; taking an open task for yourself and releasing it
// takes self.taskboard and is narrowed by the writer lock; reporting done or
// failed on your own task takes self.taskboard and is never locked. The
// assignee gets an inbox brief from the assigner. An assignment never retires,
// releases or otherwise touches the agent.

const PermTaskboardAssign = "taskboard.assign"

func registerTaskboardAssignRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/assign", handleTaskboardTask(taskboardOwnGate, handleTaskboardAssign))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/unassign", handleTaskboardTask(taskboardOwnGate, handleTaskboardUnassign))
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
// assignee itself needs self.taskboard (and, when lockApplies, a pass of the
// writer lock); anyone else needs taskboard.assign for the group and that
// target agent.
func requireTaskboardAssigneeAction(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, assignee string, lockApplies bool) bool {
	if self, _ := callerAgentID(r); self != "" && self == assignee {
		if _, ok := requirePermission(w, r, PermSelfTaskboard); !ok {
			return false
		}
		return !lockApplies || requireTaskboardUnlocked(w, r, g)
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
	res, _, rerr := agent.ResolveSelector(selector)
	if rerr != nil || res.ConvID == "" {
		return "", "", fmt.Errorf("no agent %q", guideText(selector, 60))
	}
	agentID, _ = db.AgentIDForConv(res.ConvID)
	if agentID == "" {
		return "", "", fmt.Errorf("%q is not an agent", guideText(selector, 60))
	}
	if conv = activeGroupMember(g, agentID); conv == "" {
		return "", "", fmt.Errorf("%s is not an active member of %s; an assignee must be", guideText(selector, 60), g.Name)
	}
	return agentID, conv, nil
}

// taskboardCallerName is how the caller is named in mail.
func taskboardCallerName(r *http.Request) string {
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
		ToRecipients: []string{toConv}, OperatorAuthored: from == "" && callerIsHuman(r)}
	if _, err := queueAgentMessage(m); err != nil {
		slog.Warn("taskboard: mail failed", "to", toConv, "error", err)
	}
}

func taskboardSubject(g *db.AgentGroup, t *db.GroupTask, tail string) string {
	return fmt.Sprintf("[taskboard] %s · %s %q — %s", g.Name, t.ID, t.Title, tail)
}

// taskboardBrief is the assignee's inbox brief: the task, its ref and
// dependencies, and the exact report commands.
func taskboardBrief(g *db.AgentGroup, t *db.GroupTask, by string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Task %s in group %s: %q\nAssigned to you by %s.\n", t.ID, g.Name, t.Title, by)
	if t.Body != "" {
		fmt.Fprintf(&b, "\n%s\n", t.Body)
	}
	if t.RefKind != "" {
		fmt.Fprintf(&b, "\nRef (%s): %s %s\n", t.RefKind, t.RefLabel, t.RefURL)
	}
	if len(t.DependsOn) > 0 {
		fmt.Fprintf(&b, "After: %s\n", strings.Join(t.DependsOn, ", "))
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
func handleTaskboardAssign(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		To string `json:"to"`
	}
	if !decodeTaskboardBody(w, r, &body) {
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
		if target, targetConv, err = resolveTaskboardMember(g, to); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	take := self != "" && target == self
	if take {
		if !requireTaskboardAssigneeAction(w, r, g, self, true) {
			return
		}
		if activeGroupMember(g, self) == "" {
			writeError(w, http.StatusConflict, "not_member", "only an active member of "+g.Name+" takes its tasks")
			return
		}
	} else if _, ok := requireGroupPermission(w, r, PermTaskboardAssign, g,
		ActionContext{Group: g.Name, TargetAgent: target, targetConv: targetConv}); !ok {
		return
	}
	before := t.AssigneeID
	out, err := db.AssignGroupTask(t.ID, target, taskboardActor(r), take, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditTargetConv(r, targetConv)
	if take {
		setAuditDetail(r, "took "+t.ID)
	} else {
		setAuditDetail(r, "assigned "+t.ID+" to "+target)
	}
	if !take && before != target {
		by := taskboardCallerName(r)
		mailTaskboard(r, g, targetConv, taskboardSubject(g, out, "assigned to you by "+by), taskboardBrief(g, out, by))
		if before != "" {
			if conv := activeGroupMember(g, before); conv != "" {
				mailTaskboard(r, g, conv, taskboardSubject(g, out, "reassigned by "+by),
					fmt.Sprintf("Task %s %q in %s was reassigned by %s. You no longer hold it.\n", out.ID, out.Title, g.Name, by))
			}
		}
	}
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// POST /v1/taskboard/tasks/{id}/unassign — the assignee releases its task, or
// an assigner takes it back.
func handleTaskboardUnassign(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	if t.AssigneeID == "" {
		writeError(w, http.StatusConflict, "not_assigned", "task "+t.ID+" is not assigned")
		return
	}
	if !requireTaskboardAssigneeAction(w, r, g, t.AssigneeID, true) {
		return
	}
	self, _ := callerAgentID(r)
	release := self != "" && self == t.AssigneeID
	detail := t.AssigneeID
	if !release {
		detail = "from " + t.AssigneeID
	}
	out, err := db.UnassignGroupTask(t.ID, release, detail, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	conv, _ := db.CurrentConvForAgent(t.AssigneeID)
	setAuditTargetConv(r, conv)
	setAuditDetail(r, "unassigned "+t.ID+" from "+t.AssigneeID)
	if !release {
		if conv := activeGroupMember(g, t.AssigneeID); conv != "" {
			by := taskboardCallerName(r)
			mailTaskboard(r, g, conv, taskboardSubject(g, out, "unassigned by "+by),
				fmt.Sprintf("Task %s %q in %s was taken back by %s. You no longer hold it.\n", out.ID, out.Title, g.Name, by))
		}
	}
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
	if _, ok := requirePermission(w, r, PermSelfTaskboard); !ok {
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
	if t.AssigneeID == self && activeGroupMember(g, self) == "" {
		setAuditDetail(r, "refused: not a member, "+t.ID)
		writeError(w, http.StatusConflict, "not_member", "you are no longer an active member of "+g.Name)
		return
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
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": views[0], "repeated": rep.Repeated, "blocked": rep.Blocked})
}

// taskboardNotifyAssigner tells the agent that assigned the task about the
// report, when that was an agent other than the reporter.
func taskboardNotifyAssigner(r *http.Request, g *db.AgentGroup, before *db.GroupTask, rep *db.GroupTaskReport, outcome, note string) {
	assigner, ok := strings.CutPrefix(before.AssignedBy, "agent:")
	if !ok || assigner == before.AssigneeID {
		return
	}
	conv, err := db.CurrentConvForAgent(assigner)
	if err != nil || conv == "" {
		return
	}
	if state, err := db.AgentState(conv); err != nil || state == db.AgentStateRetired {
		return
	}
	who := taskboardCallerName(r)
	tail := who + " reported " + map[string]string{db.GroupTaskOutcomeSucceeded: "done", db.GroupTaskOutcomeFailed: "failed"}[outcome]
	text := fmt.Sprintf("Task %s %q in %s: %s.\n", before.ID, before.Title, g.Name, tail)
	if note != "" {
		text += "\nNote: " + note + "\n"
	}
	if rep.Blocked {
		text += "\nThe task is now blocked after " + fmt.Sprint(rep.Task.Failures) + " failed attempts.\n"
	}
	mailTaskboard(r, g, conv, taskboardSubject(g, before, tail), text)
}

// GET /v1/taskboard/mine — the live tasks assigned to the calling agent, in
// every group.
func handleTaskboardMine(w http.ResponseWriter, r *http.Request) {
	if !taskboardEnabled(w) {
		return
	}
	self, _ := callerAgentID(r)
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
	for _, a := range assigned {
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
