package agentd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Moving or cloning a task to another group of this node (tcl-0r0obl,
// docs/taskboard.md). The CLI is `tclaude agent taskboard move-to-group
// <task> <group> [--clone] [--stage idea|next]`; the dashboard drags a task
// onto another group's board (Ctrl/Cmd clones) behind a confirmation.
//
// Permissions (an agent's; the operator holds them all):
//   - move:  taskboard.transfer on the target; on the source taskboard.write
//     when the caller holds the task, taskboard.manage otherwise.
//   - clone: taskboard.read on the source (the route's 404 rule) and
//     taskboard.transfer on the target.
//
// The target board's writer lock applies to a confirmed agent as for any
// change to that board.

// taskboardTransferResult is the route's answer.
type taskboardTransferResult struct {
	Task              taskboardTaskView `json:"task"`
	Group             string            `json:"group"`
	From              string            `json:"from"`
	Clone             bool              `json:"clone"`
	DroppedDeps       []string          `json:"dropped_deps,omitempty"`
	DroppedDependents []string          `json:"dropped_dependents,omitempty"`
	Unassigned        string            `json:"unassigned,omitempty"`
}

// POST /v1/taskboard/tasks/{id}/move-to-group {"group": "<name>", "clone":
// bool, "stage": "idea"|"next"}.
func handleTaskboardMoveToGroup(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Group string `json:"group"`
		Clone bool   `json:"clone"`
		Stage string `json:"stage"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Group)
	stage := strings.TrimSpace(body.Stage)
	var err error
	switch {
	case name == "":
		err = fmt.Errorf("give the group to move the task to")
	case stage != "" && stage != db.GroupTaskStageIdea && stage != db.GroupTaskStageNext:
		err = fmt.Errorf("stage is idea or next")
	case strings.Contains(name, "@"):
		err = fmt.Errorf("%s is on a linked node; moving a task to another node is not supported yet", guideText(name, 60))
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	target, err := db.GetAgentGroupByName(name)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	if target == nil {
		writeError(w, http.StatusNotFound, "not_found", "no such group "+guideText(name, 60))
		return
	}
	if target.ID == g.ID {
		writeError(w, http.StatusBadRequest, "same_group", fmt.Sprintf("task %s is already in %s", t.ID, g.Name))
		return
	}
	if !requireGroupActive(w, target) || !requireTaskboardTransfer(w, r, t, g, target, body.Clone) {
		return
	}
	setAuditGroup(r, target.Name)
	actor := taskboardActor(r)
	if body.Clone {
		out, err := db.CreateGroupTask(db.NewGroupTask{GroupID: target.ID, Title: t.Title, Body: t.Body, Stage: stage,
			RefKind: t.RefKind, RefURL: t.RefURL, RefLabel: t.RefLabel, Actor: actor,
			Origin: fmt.Sprintf("cloned from %s in %s", t.ID, g.Name)})
		if err != nil {
			writeTaskboardError(w, err)
			return
		}
		_ = db.NoteGroupTaskCloned(t.ID, actor, fmt.Sprintf("to %s in %s", out.ID, target.Name))
		setAuditDetail(r, fmt.Sprintf("cloned %s to %s as %s", t.ID, target.Name, out.ID))
		writeTaskboardTransfer(w, target, out, taskboardTransferResult{Group: target.Name, From: g.Name, Clone: true})
		return
	}
	keep := t.AssigneeID != "" && activeGroupMember(target, t.AssigneeID) != ""
	res, err := db.RegroupGroupTask(t.ID, db.GroupTaskRegroup{ToGroupID: target.ID, FromGroup: g.Name, ToGroup: target.Name,
		Stage: stage, KeepAssignee: keep, Actor: actor})
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	if res.Unassigned != "" {
		if conv, _ := db.CurrentConvForAgent(res.Unassigned); conv != "" {
			mailTaskboard(r, g, conv, taskboardSubject(target, res.Task, "moved away from you"),
				fmt.Sprintf("%s moved task %s %q from %s to %s. You are not a member of %s, so it is no longer assigned to you; stop work on it.\n",
					taskboardCallerName(r), res.Task.ID, res.Task.Title, g.Name, target.Name, target.Name))
		}
	}
	setAuditDetail(r, fmt.Sprintf("moved %s from %s to %s", t.ID, g.Name, target.Name))
	writeTaskboardTransfer(w, target, res.Task, taskboardTransferResult{Group: target.Name, From: g.Name,
		DroppedDeps: res.DroppedDeps, DroppedDependents: res.DroppedDependents, Unassigned: res.Unassigned})
}

// requireTaskboardTransfer gates a move or clone of t (in g) to target.
func requireTaskboardTransfer(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g, target *db.AgentGroup, clone bool) bool {
	if a := peerGrantFromRequest(r); a != nil {
		// A linked node's authority covers the task's own group; the target
		// is another group, so its transfer grant there is asked for here.
		if !fedPeerAllows(a.peer, target.ID, PermTaskboardTransfer) {
			writeError(w, http.StatusForbidden, "permission",
				fmt.Sprintf("this node does not grant you %s for group:%s", PermTaskboardTransfer, target.Name))
			return false
		}
	} else if _, ok := requireGroupPermission(w, r, PermTaskboardTransfer, target); !ok {
		return false
	}
	if !requireTaskboardUnlocked(w, r, target) {
		return false
	}
	if clone {
		// Reading the source is the route's own gate.
		return true
	}
	if self, _ := callerAgentID(r); self != "" && self == t.AssigneeID {
		return requireTaskboard(w, r, g, true)
	}
	_, ok := requireGroupPermission(w, r, PermTaskboardManage, g)
	return ok
}

func writeTaskboardTransfer(w http.ResponseWriter, g *db.AgentGroup, t *db.GroupTask, out taskboardTransferResult) {
	views, err := taskboardViews(g.Name, []*db.GroupTask{t})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	out.Task = views[0]
	writeJSON(w, http.StatusOK, out)
}
