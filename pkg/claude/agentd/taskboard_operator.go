package agentd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The operator as an assignee (tcl-p4t78f, docs/taskboard.md). "operator"
// names the operator of the node that holds the board: on a linked node's
// board it is that node's operator. Giving the operator a task takes
// taskboard.assign for the group (a grant narrowed to target agents does not
// cover the operator), or taskboard.own for an agent asking about its own
// work: a new task it creates, or a task it holds itself. The agent that
// asked keeps its own-rights (edit, block, drop) on that operator task. Only
// the operator, or a taskboard.assign holder, closes it as done, and closing
// it reopens the tasks blocked waiting on it and mails their assignees.

// isTaskboardOperator reports whether a "to" names the operator.
func isTaskboardOperator(to string) bool {
	return strings.EqualFold(strings.TrimSpace(to), db.OperatorAssignee)
}

// resolveTaskboardAssignee resolves a "to": the operator, or an active
// member of g (resolveTaskboardMember).
func resolveTaskboardAssignee(g *db.AgentGroup, to string) (assignee, conv string, err error) {
	if isTaskboardOperator(to) {
		return db.OperatorAssignee, "", nil
	}
	return resolveTaskboardMember(g, to)
}

// taskboardAskedByCaller reports whether t is an operator task the calling
// agent asked (assigned to the operator).
func taskboardAskedByCaller(r *http.Request, t *db.GroupTask) bool {
	if t == nil || t.AssigneeID != db.OperatorAssignee {
		return false
	}
	self, _ := callerAgentID(r)
	return self != "" && t.AssignedBy == taskboardActor(r)
}

// requireTaskboardOperatorAssign gates giving the operator a task: t is nil
// for a new task the caller creates. An agent asking about its own work (a
// new task, or one it holds) needs taskboard.own and a pass of the writer
// lock; anyone else needs taskboard.assign for the group.
func requireTaskboardOperatorAssign(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, t *db.GroupTask) bool {
	if self, conv := callerAgentID(r); self != "" && (t == nil || t.AssigneeID == self) && agentOwnsTaskboard(r, conv, g) {
		return requireTaskboardOwn(w, r, g, true)
	}
	_, ok := requireGroupPermission(w, r, PermTaskboardAssign, g, ActionContext{Group: g.Name})
	return ok
}

// requireTaskboardHolderAction is requireTaskboardAssigneeAction for task t,
// which may be held by the operator: then the agent that asked acts under
// taskboard.own (a withdraw, say), anyone else needs taskboard.assign for
// the group.
func requireTaskboardHolderAction(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, t *db.GroupTask, lockApplies bool) bool {
	if t.AssigneeID != db.OperatorAssignee {
		return requireTaskboardAssigneeAction(w, r, g, t.AssigneeID, lockApplies)
	}
	if _, conv := callerAgentID(r); taskboardAskedByCaller(r, t) && agentOwnsTaskboard(r, conv, g) {
		return requireTaskboardOwn(w, r, g, lockApplies)
	}
	_, ok := requireGroupPermission(w, r, PermTaskboardAssign, g, ActionContext{Group: g.Name})
	return ok
}

// requireTaskboardWaitOn checks that on can be waited on from g's board: a
// live task of the same group. A refusal writes the error.
func requireTaskboardWaitOn(w http.ResponseWriter, g *db.AgentGroup, on string) (*db.GroupTask, bool) {
	t, err := db.GetGroupTask(on)
	if err != nil || t.GroupID != g.ID {
		writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("%s is not a task of %s's board", guideText(on, 40), g.Name))
		return nil, false
	}
	return t, true
}

// taskboardWaitersBefore lists, before t is closed, the tasks blocked
// waiting on it; nil when there are none or the read fails.
func taskboardWaitersBefore(t *db.GroupTask) []string {
	if !t.Live() {
		return nil
	}
	waiters, _ := db.GroupTaskWaiters(t.ID)
	return waiters
}

// mailTaskboardWaiters tells the agents whose tasks wait on t that t was
// closed (status done or dropped). waiters are the tasks that were blocked
// waiting on t before the close. On done they were reopened ("unblocked");
// an operator task's other blocked dependents stay blocked and are told
// too. On a drop the waiters stay blocked. note is the closing reason.
func mailTaskboardWaiters(r *http.Request, g *db.AgentGroup, t *db.GroupTask, status, note string, waiters []string, wasOperator bool) {
	if len(waiters) == 0 && (!wasOperator || status != db.GroupTaskDone) {
		return
	}
	released := map[string]bool{}
	for _, id := range waiters {
		released[id] = true
	}
	dependents, err := db.GroupTaskDependents(t.ID)
	if err != nil {
		return
	}
	by := taskboardCallerName(r)
	how := "closed as done"
	if status == db.GroupTaskDropped {
		how = "dropped"
	}
	for _, d := range dependents {
		if !d.Live() || d.AssigneeID == "" || d.AssigneeID == db.OperatorAssignee {
			continue
		}
		var what string
		switch {
		case status == db.GroupTaskDone && released[d.ID] && d.Status == db.GroupTaskOpen:
			what = "unblocked"
		case d.Status == db.GroupTaskBlocked && (released[d.ID] || wasOperator):
			what = "still blocked"
		default:
			continue
		}
		conv := activeGroupMember(g, d.AssigneeID)
		if conv == "" {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "Task %s %q, which your task %s %q waits on, was %s by %s.\n", t.ID, t.Title, d.ID, d.Title, how, by)
		if note != "" {
			fmt.Fprintf(&b, "\n%s\n", note)
		}
		if what == "unblocked" {
			b.WriteString("\nYour task is open again.\n")
		} else {
			fmt.Fprintf(&b, "\nYour task stays blocked: %s\nUnblock it: tclaude agent taskboard unblock %s\n", d.BlockReason, d.ID)
		}
		fmt.Fprintf(&b, "Read the activity: tclaude agent taskboard activity %s\n", t.ID)
		mailTaskboard(r, g, conv, taskboardSubject(g, d, what+": "+t.ID+" is "+map[bool]string{true: "done", false: "dropped"}[status == db.GroupTaskDone]), taskboardLinked(g, d, b.String()))
	}
}

// handleTaskboardBlockOn is `block <task> --on tk_…`: the task waits on
// another task of its board, typically one the operator holds.
func handleTaskboardBlockOn(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup, on, status, reason string) {
	if status != db.GroupTaskBlocked || strings.TrimSpace(reason) != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "on is for status blocked, without a reason: the reason names the task waited on")
		return
	}
	target, ok := requireTaskboardWaitOn(w, g, on)
	if !ok || !requireTaskboardTaskChange(w, r, g, t) {
		return
	}
	out, err := db.BlockGroupTaskOn(t.ID, target.ID, taskboardActor(r), t.AssigneeID)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, t.ID+" blocked on "+target.ID)
	writeTaskboardTask(w, http.StatusOK, g, out)
}
