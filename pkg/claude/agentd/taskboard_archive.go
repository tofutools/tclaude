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
// taskboard.write.

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

// POST /v1/groups/{g}/taskboard/archive-done {"older_than"?: "7d"} —
// `taskboard archive <group> --done [--older-than 7d]`: every done task of
// the board not yet archived, or those closed longer ago than older_than.
func handleTaskboardArchiveDone(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !taskboardEnabled(w) || !requireTaskboard(w, r, g, true) || !requireGroupActive(w, g) {
		return
	}
	var body struct {
		OlderThan string `json:"older_than"`
	}
	if r.ContentLength != 0 && !decodeTaskboardBody(w, r, &body) {
		return
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
	ids, err := db.ArchiveDoneGroupTasks(g.ID, before, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, fmt.Sprintf("archived %d done tasks in %s", len(ids), g.Name))
	writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "archived": ids, "count": len(ids)})
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
