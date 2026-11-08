package agentd

import (
	"net/url"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A task's own dashboard link (docs/taskboard.md "A task's link"):
// /groups?task=<task-id>:<group>, which opens the board at the task with
// its edit form open. The dashboard's title links and ⋯ Copy link make the
// same link (dashboard/js/task-route.js); a linked node's task adds
// @<node> after the id there.
//
// The link names this daemon's dashboard listener, whose port may be
// chosen at start, so a printed or mailed link lasts as long as the daemon
// does; the id beside it always works.

// taskboardTaskPath is a task's dashboard path on this node.
func taskboardTaskPath(group, taskID string) string {
	return "/groups?" + url.Values{"task": {taskID + ":" + group}}.Encode()
}

// taskboardTaskURL is a task's whole dashboard link, or "" when the
// dashboard listener is off.
func taskboardTaskURL(group, taskID string) string {
	base := strings.TrimRight(popupBaseURL, "/")
	if base == "" || group == "" || taskID == "" {
		return ""
	}
	return base + taskboardTaskPath(group, taskID)
}

// taskboardLinked is a [taskboard] mail's body with the task's link last.
func taskboardLinked(g *db.AgentGroup, t *db.GroupTask, body string) string {
	if g == nil || t == nil {
		return body
	}
	link := taskboardTaskURL(g.Name, t.ID)
	if link == "" {
		return body
	}
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	return body + "\nOpen it in the dashboard: " + link + "\n"
}
