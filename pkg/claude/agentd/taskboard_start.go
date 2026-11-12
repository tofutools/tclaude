package agentd

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
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
		// OverLimit lets the operator start the task in a full In progress.
		OverLimit bool `json:"over_limit"`
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
	// A start enters In progress and meets its limit; queued, the task
	// stays held and waits in line.
	defer lockTaskboardLanes(g.ID)()
	want := *t
	if start {
		want.StartedAt = time.Now()
	}
	hit, ok := checkTaskboardLimit(w, r, g, t, &want, body.OverLimit)
	if !ok {
		return
	}
	out := t
	if !hit.queued() {
		var err error
		if out, err = db.StartGroupTask(t.ID, start, stage, taskboardActor(r), t.AssigneeID); err != nil {
			writeTaskboardError(w, err)
			return
		}
	}
	out, note := finishTaskboardLimit(r, g, hit, out)
	verb := "started "
	if !start {
		verb = "held "
	}
	setAuditDetail(r, verb+t.ID)
	// Started by someone else: the assignee gets its brief, as on an assign.
	if start && !t.Started() && out.Started() && t.AssigneeID != db.OperatorAssignee {
		by := taskboardCallerName(r)
		n := newTaskboardNotice(r, g, out, boardopts.EventStarted)
		if conv := activeGroupMember(g, t.AssigneeID); conv != "" {
			n.mail(boardopts.RecipientAssignee, t.AssigneeID, conv,
				taskboardSubject(g, out, "started for you by "+by), taskboardLinked(g, out, taskboardBrief(g, out, by)))
		}
		what := "started by " + by
		if self, _ := callerAgentID(r); self != t.AssigneeID {
			what = fmt.Sprintf("started for %s by %s", taskboardAgentLabel(t.AssigneeID), by)
		}
		n.others(what, "", boardopts.RecipientAssignee)
	}
	writeTaskboardTaskLimit(w, http.StatusOK, g, out, note)
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
