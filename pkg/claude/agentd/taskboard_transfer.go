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
	// A linked node learns of no group it may not move tasks into: a missing
	// group and one it holds no taskboard.transfer on answer alike.
	if a := peerGrantFromRequest(r); a != nil && target != nil && !fedPeerAllows(a.peer, target.ID, PermTaskboardTransfer) {
		target = nil
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
		// The clone shares the original's files: the rows are copied, the
		// blobs are content-addressed.
		files, err := db.ListGroupTaskAttachments(t.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "db", err.Error())
			return
		}
		out, err := db.CreateGroupTask(db.NewGroupTask{GroupID: target.ID, Title: t.Title, Body: t.Body, Stage: stage,
			RefKind: t.RefKind, RefURL: t.RefURL, RefLabel: t.RefLabel, Actor: actor,
			Origin: fmt.Sprintf("cloned from %s in %s", t.ID, g.Name), Attachments: files})
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
	// A done task's former assignee has nothing to stop: no mail.
	if res.Unassigned != "" && res.Task.Status != db.GroupTaskDone {
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
	return ok && requireTaskboardUnlocked(w, r, g)
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

// Across nodes (tcl-0r0obl, D2): a move or clone between a group here and a
// linked node's, or between two linked nodes in the fleet view, is driven by
// the client (the dashboard, or the CLI) in two steps, each on its own node
// under that node's gates and grants:
//
//  1. POST /v1/groups/{name}/taskboard/transfer-in on the target node creates
//     the task there (taskboard.transfer), naming where it came from.
//  2. Only after that succeeded, a move closes the original: POST
//     /v1/taskboard/tasks/{id}/moved-away on the source node drops it with a
//     "moved to node/group" note (taskboard.write for the task's holder,
//     taskboard.manage otherwise). The client retries this step and warns
//     rather than leaving a silent duplicate.
//
// A done task stays where it is: only a live task moves across nodes.

// taskboardTransferOrigin names a task on some node.
type taskboardTransferOrigin struct {
	Node  string `json:"node"`
	Group string `json:"group"`
	Task  string `json:"task"`
	// via is the linked node that sent it, as this node knows it, when that
	// is not the node the caller names (a move between two linked nodes,
	// driven from a third). Set by this node only.
	via string
}

func (o taskboardTransferOrigin) String() string {
	where := guideText(o.Group, 60)
	if o.Node != "" {
		where = guideText(o.Node, 60) + "/" + where
	}
	if o.Task != "" {
		where += " " + guideText(o.Task, 40)
	}
	if o.via != "" {
		where += " (via " + o.via + ")"
	}
	return where
}

// POST /v1/groups/{name}/taskboard/transfer-in {"title", "body", "ref",
// "ref_label", "stage", "clone", "origin": {node, group, task}}. A task
// with files comes as multipart: that JSON as the "task" part, and its
// files as "file" parts, all landing in one transaction.
func handleTaskboardTransferIn(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !taskboardEnabled(w) || !requireGroupActive(w, g) {
		return
	}
	if _, ok := requireGroupPermission(w, r, PermTaskboardTransfer, g); !ok || !requireTaskboardUnlocked(w, r, g) {
		return
	}
	var body struct {
		Title    string                  `json:"title"`
		Body     string                  `json:"body"`
		Ref      string                  `json:"ref"`
		RefLabel string                  `json:"ref_label"`
		Stage    string                  `json:"stage"`
		Clone    bool                    `json:"clone"`
		Origin   taskboardTransferOrigin `json:"origin"`
	}
	files, ok := decodeTaskboardRequest(w, r, &body)
	if !ok {
		return
	}
	title, err := validateTaskTitle(body.Title)
	if err == nil {
		body.Body, err = validateTaskText("the body", body.Body, maxTaskBodyBytes)
	}
	var kind, refURL, refLabel string
	if err == nil {
		kind, refURL, refLabel, err = parseTaskRef(body.Ref, body.RefLabel)
	}
	stage := strings.TrimSpace(body.Stage)
	if err == nil && stage != "" && stage != db.GroupTaskStageIdea && stage != db.GroupTaskStageNext {
		err = fmt.Errorf("stage is idea or next")
	}
	if err == nil && strings.TrimSpace(body.Origin.Group) == "" {
		err = fmt.Errorf("give the origin: the group (and node and task) the task comes from")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// A task coming in from a linked node names that node as this node
	// knows it. A different node the caller names (the source of a move
	// between two linked nodes) is kept as its claim, marked with who sent it.
	if node := taskboardPeerNode(r); node != "" {
		claimed := strings.TrimSpace(body.Origin.Node)
		if claimed == "" || claimed == node {
			body.Origin.Node = node
		} else {
			body.Origin.via = node
		}
	}
	verb := "moved from "
	if body.Clone {
		verb = "cloned from "
	}
	actor := taskboardActor(r)
	atts, ok := storeTaskUploadsOrFail(w, files, actor)
	if !ok {
		return
	}
	out, err := db.CreateGroupTask(db.NewGroupTask{GroupID: g.ID, Title: title, Body: body.Body, Stage: stage,
		RefKind: kind, RefURL: refURL, RefLabel: refLabel, Actor: actor, Origin: verb + body.Origin.String(), Attachments: atts})
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditGroup(r, g.Name)
	setAuditDetail(r, fmt.Sprintf("%s%s as %s", verb, body.Origin.String(), out.ID))
	writeTaskboardTask(w, http.StatusCreated, g, out)
}

// POST /v1/taskboard/tasks/{id}/moved-away {"to": {node, group, task}} — the
// second step of a move to another node: drop the original, noting where it
// went. Gated as a move's source side is.
func handleTaskboardMovedAway(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		To taskboardTransferOrigin `json:"to"`
		// Check runs the gates and answers without dropping: the client
		// asks it before creating the task on the target, so a refusal it
		// could foresee never leaves two copies.
		Check bool `json:"check"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	if strings.TrimSpace(body.To.Group) == "" && !body.Check {
		writeError(w, http.StatusBadRequest, "invalid_arg", "give where the task went: to {node, group, task}")
		return
	}
	if !t.Live() {
		writeError(w, http.StatusConflict, "not_live", fmt.Sprintf("task %s is %s; only a live task moves to another node", t.ID, t.Status))
		return
	}
	if self, _ := callerAgentID(r); self != "" && self == t.AssigneeID {
		if !requireTaskboard(w, r, g, true) {
			return
		}
	} else if _, ok := requireGroupPermission(w, r, PermTaskboardManage, g); !ok || !requireTaskboardUnlocked(w, r, g) {
		return
	}
	if body.Check {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "task": t.ID})
		return
	}
	note := "moved to " + body.To.String()
	out, err := db.SetGroupTaskStatusFor(t.ID, db.GroupTaskDropped, note, taskboardActor(r), db.AnyAssignee)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	if t.AssigneeID != "" {
		if conv, _ := db.CurrentConvForAgent(t.AssigneeID); conv != "" {
			mailTaskboard(r, g, conv, taskboardSubject(g, t, "moved away from you"),
				fmt.Sprintf("%s moved task %s %q from %s to %s. It is closed here and no longer assigned to you; stop work on it.\n",
					taskboardCallerName(r), t.ID, t.Title, g.Name, body.To.String()))
		}
	}
	setAuditDetail(r, fmt.Sprintf("%s %s", t.ID, note))
	writeTaskboardTask(w, http.StatusOK, g, out)
}
