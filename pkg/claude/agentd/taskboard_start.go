package agentd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Assigned ideas (tk_3826822a). An assignee, agent or operator, may hold a
// task without starting it: an idea to triage, or queued in next. It is in
// progress only once started.

// POST /v1/taskboard/tasks/{id}/start {"start": true|false, "stage": "idea"|"next"}
// — start a task its assignee holds (the default), or put a started one back
// to be held in stage (its own stage when omitted). The holder's action: the
// assignee itself under taskboard.own, anyone else under taskboard.assign.
func handleTaskboardStart(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Start *bool  `json:"start"`
		Stage string `json:"stage"`
	}
	if r.ContentLength != 0 && !decodeTaskboardBody(w, r, &body) {
		return
	}
	start := body.Start == nil || *body.Start
	stage := strings.TrimSpace(body.Stage)
	if start && stage != "" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a started task is in progress; stage names where a held one sits")
		return
	}
	if t.AssigneeID == "" {
		writeError(w, http.StatusConflict, "not_assigned", "task "+t.ID+" is not assigned; take it or assign it")
		return
	}
	if !requireTaskboardHolderAction(w, r, g, t, true) {
		return
	}
	out, err := db.StartGroupTask(t.ID, start, stage, taskboardActor(r), t.AssigneeID)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	verb := "started "
	if !start {
		verb = "held "
	}
	setAuditDetail(r, verb+t.ID)
	// Started by someone else: the assignee gets its brief, as on an assign.
	if start && !t.Started() && t.AssigneeID != db.OperatorAssignee {
		if self, _ := callerAgentID(r); self != t.AssigneeID {
			if conv := activeGroupMember(g, t.AssigneeID); conv != "" {
				by := taskboardCallerName(r)
				mailTaskboard(r, g, conv, taskboardSubject(g, out, "started for you by "+by), taskboardLinked(g, out, taskboardBrief(g, out, by)))
			}
		}
	}
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// taskboardHeldNote is the light mail an agent gets for a task assigned to
// it without being started: an idea to triage, or one queued for it.
func taskboardHeldNote(g *db.AgentGroup, t *db.GroupTask, by string) (subject, text string) {
	what, next := "queued for you", "Start it when you get to it"
	if t.Stage == db.GroupTaskStageIdea {
		what, next = "an idea for you to triage", "Start it, drop it, or give it back"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s put task %s %q in %s in your lane, %s. It is not started.\n", by, t.ID, t.Title, g.Name, what)
	fmt.Fprintf(&b, "\n%s:\n", next)
	fmt.Fprintf(&b, "Show it:     tclaude agent taskboard show %s\n", t.ID)
	fmt.Fprintf(&b, "Start it:    tclaude agent taskboard start %s\n", t.ID)
	fmt.Fprintf(&b, "Give back:   tclaude agent taskboard release %s\n", t.ID)
	return taskboardSubject(g, t, what+", from "+by), taskboardLinked(g, t, b.String())
}
