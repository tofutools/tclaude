package agentd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// POST /v1/taskboard/tasks/{id}/reopen {"stage"?: "next"|"idea", "to"?:
// "<agent>"|"operator"|""} — `taskboard reopen` (tk_1223214b): a done or
// dropped task back on the board, where it was or in stage, with its
// assignee kept (to absent), changed, or taken off (to ""), in one write.
//
// Reopening is gated as closing is: the task's holder under taskboard.own,
// anyone else on taskboard.write. A change of assignee is gated as assign
// and unassign are.
func handleTaskboardReopen(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Stage string  `json:"stage"`
		To    *string `json:"to"`
		// Before or After places it beside a live task of the board, in that
		// task's stage, in the same write (a List drop). Placing is a
		// reorder, so it takes taskboard.write, as move does.
		Before string `json:"before"`
		After  string `json:"after"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	stage := strings.TrimSpace(body.Stage)
	if stage != "" && stage != db.GroupTaskStageIdea && stage != db.GroupTaskStageNext {
		writeError(w, http.StatusBadRequest, "invalid_arg", "stage is idea or next")
		return
	}
	pos := db.GroupTaskPosition{Before: strings.TrimSpace(body.Before), After: strings.TrimSpace(body.After)}
	if pos.Before != "" && pos.After != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "give before or after, not both")
		return
	}
	if t.Live() {
		writeError(w, http.StatusConflict, "not_closed", "task "+t.ID+" is "+t.Status+"; only a done or dropped task is reopened")
		return
	}
	if !requireTaskboardTaskChange(w, r, g, t) {
		return
	}
	if (pos.Before != "" || pos.After != "") && !requireTaskboard(w, r, g, true) {
		return
	}
	self, selfConv := callerAgentID(r)
	assignee, targetConv := db.KeepAssignee, ""
	// A kept assignee who has left the group does not get the task back.
	if body.To == nil && t.AssigneeID != "" && t.AssigneeID != db.OperatorAssignee && activeGroupMember(g, t.AssigneeID) == "" {
		assignee = ""
	}
	if body.To != nil {
		to := strings.TrimSpace(*body.To)
		if to == "" {
			assignee = ""
			// Taking it off someone else is an unassign.
			if t.AssigneeID != "" && t.AssigneeID != self && !requireTaskboardHolderAction(w, r, g, t, true) {
				return
			}
		} else {
			var err error
			if assignee, targetConv, err = resolveTaskboardAssignee(g, to); err != nil {
				writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
				return
			}
			if assignee == self {
				targetConv = selfConv
			}
			if assignee != t.AssigneeID {
				var ok bool
				switch assignee {
				case db.OperatorAssignee:
					ok = requireTaskboardOperatorAssign(w, r, g, t)
				case self:
					ok = requireTaskboardAssigneeAction(w, r, g, self, true)
				default:
					_, ok = requireGroupPermission(w, r, PermTaskboardAssign, g,
						ActionContext{Group: g.Name, TargetAgent: assignee, targetConv: targetConv})
				}
				// A change of hands also takes it back from whoever held it.
				if !ok || (t.AssigneeID != "" && t.AssigneeID != self && !requireTaskboardHolderAction(w, r, g, t, false)) {
					return
				}
			}
		}
	}
	actor := taskboardActor(r)
	out, err := db.ReopenGroupTask(t.ID, db.GroupTaskReopen{Stage: stage, Assignee: assignee, AssignedBy: actor, Actor: actor,
		ExpectAssignee: t.AssigneeID, Position: pos})
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, fmt.Sprintf("reopened %s in %s", t.ID, out.Stage))
	by := taskboardCallerName(r)
	if out.AssigneeID != t.AssigneeID {
		if out.AssigneeID != "" && out.AssigneeID != self && out.AssigneeID != db.OperatorAssignee && targetConv != "" {
			setAuditTargetConv(r, targetConv)
			mailTaskboard(r, g, targetConv, taskboardSubject(g, out, "reopened and assigned to you by "+by), taskboardLinked(g, out, taskboardBrief(g, out, by)))
		}
		if t.AssigneeID != "" && t.AssigneeID != self {
			if conv := activeGroupMember(g, t.AssigneeID); conv != "" {
				mailTaskboard(r, g, conv, taskboardSubject(g, out, "reopened by "+by),
					taskboardLinked(g, out, fmt.Sprintf("Task %s %q in %s was reopened by %s and no longer assigned to you.\n", out.ID, out.Title, g.Name, by)))
			}
		}
	} else if out.AssigneeID != "" && out.AssigneeID != self && out.AssigneeID != db.OperatorAssignee {
		if conv := activeGroupMember(g, out.AssigneeID); conv != "" {
			mailTaskboard(r, g, conv, taskboardSubject(g, out, "reopened by "+by),
				taskboardLinked(g, out, fmt.Sprintf("Task %s %q in %s was reopened by %s and is yours again.\n\n%s", out.ID, out.Title, g.Name, by, taskboardBrief(g, out, by))))
		}
	}
	writeTaskboardTask(w, http.StatusOK, g, out)
}
