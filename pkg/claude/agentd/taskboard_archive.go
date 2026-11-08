package agentd

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Archiving tasks by hand (tk_2f3e574f, tk_4f591970, docs/taskboard.md): an
// archived task leaves every list and the history, but is kept: `ls
// --archived` lists it, `unarchive` brings it back into the history and
// `reopen` back onto the board. Archiving is gated as closing is: a live task
// is dropped first, with the gate a drop has; a closed one takes the holder's
// taskboard.own or taskboard.write. "Archive done" archives a board's done
// tasks, optionally only those closed longer ago than an age, and takes
// taskboard.write. "Archive closed" does the same for the done tasks, the
// dropped ones, or both.

// POST /v1/taskboard/tasks/{id}/archive {"reason"?} — `taskboard archive`.
func handleTaskboardArchive(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Reason string `json:"reason"`
	}
	if r.ContentLength != 0 && !decodeTaskboardBody(w, r, &body) {
		return
	}
	reason, err := validateTaskText("the reason", strings.TrimSpace(body.Reason), maxTaskReasonLen)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	actor := taskboardActor(r)
	if t.Live() {
		// A live task is dropped on the way, gated as a drop is.
		if t.AssigneeID != "" {
			if !requireTaskboardHolderAction(w, r, g, t, false) {
				return
			}
		} else if !requireTaskboardTaskChange(w, r, g, t) {
			return
		}
		waiters := taskboardWaitersBefore(t)
		dropped, err := db.SetGroupTaskStatusFor(t.ID, db.GroupTaskDropped, firstNonEmpty(reason, "archived"), actor, t.AssigneeID)
		if err != nil {
			writeTaskboardError(w, err)
			return
		}
		mailTaskboardWaiters(r, g, dropped, db.GroupTaskDropped, firstNonEmpty(reason, "archived"), waiters, t.AssigneeID == db.OperatorAssignee)
	} else if !requireTaskboardTaskChange(w, r, g, t) {
		return
	}
	out, err := db.ArchiveGroupTask(t.ID, actor)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, "archived "+t.ID)
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// POST /v1/taskboard/tasks/{id}/unarchive — `taskboard unarchive`.
func handleTaskboardUnarchive(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	if !requireTaskboardTaskChange(w, r, g, t) {
		return
	}
	out, err := db.UnarchiveGroupTask(t.ID, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, "unarchived "+t.ID)
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// POST /v1/groups/{g}/taskboard/archive-done {"older_than"?: "7d"} — the
// done tasks only: the route `archive-closed` came after, kept for a client
// or a linked node that only knows this one.
func handleTaskboardArchiveDone(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	archiveClosedTasks(w, r, g, false)
}

// POST /v1/groups/{g}/taskboard/archive-closed {"statuses": ["done",
// "dropped"], "older_than"?: "7d"} — `taskboard archive <group> --done
// --dropped [--older-than 7d]`: every task of the board in those statuses
// not yet archived, or those closed longer ago than older_than. A new route,
// not a field on archive-done: a node that predates it answers 404 rather
// than archiving its done tasks in place of the dropped ones asked for.
func handleTaskboardArchiveClosed(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	archiveClosedTasks(w, r, g, true)
}

func archiveClosedTasks(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, byStatus bool) {
	if !taskboardEnabled(w) || !requireTaskboard(w, r, g, true) || !requireGroupActive(w, g) {
		return
	}
	var body struct {
		Statuses  []string `json:"statuses"`
		OlderThan string   `json:"older_than"`
	}
	if r.ContentLength != 0 && !decodeTaskboardBody(w, r, &body) {
		return
	}
	statuses := []string{db.GroupTaskDone}
	if byStatus {
		var err error
		if statuses, err = closedStatuses(body.Statuses); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
	}
	var before time.Time
	if s := strings.TrimSpace(body.OlderThan); s != "" {
		age, err := parseTaskboardAge(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
			return
		}
		before = time.Now().Add(-age)
	}
	ids, err := db.ArchiveClosedGroupTasks(g.ID, statuses, before, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, fmt.Sprintf("archived %d %s tasks in %s", len(ids), strings.Join(statuses, " and "), g.Name))
	writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "statuses": statuses, "archived": ids, "count": len(ids)})
}

// closedStatuses reads archive-closed's statuses: done, dropped or both,
// each once, in that order.
func closedStatuses(in []string) ([]string, error) {
	var done, dropped bool
	for _, s := range in {
		switch strings.TrimSpace(s) {
		case db.GroupTaskDone:
			done = true
		case db.GroupTaskDropped:
			dropped = true
		default:
			return nil, fmt.Errorf("statuses are done and dropped, not %q", guideText(s, 20))
		}
	}
	out := []string{}
	if done {
		out = append(out, db.GroupTaskDone)
	}
	if dropped {
		out = append(out, db.GroupTaskDropped)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("give statuses: done, dropped or both")
	}
	return out, nil
}

// parseTaskboardAge reads an age: a Go duration ("36h", "90m") or a whole
// number of days ("7d").
func parseTaskboardAge(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err == nil && n >= 0 && n <= 3650 {
			return time.Duration(n) * 24 * time.Hour, nil
		}
	} else if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return d, nil
	}
	return 0, errors.New("older_than is an age such as 1d, 7d or 36h")
}
