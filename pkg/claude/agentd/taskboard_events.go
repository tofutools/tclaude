package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// The board's automatic messages (docs/taskboard.md "Board options"): each
// event's message is switched on or off per board and sent to the board's
// recipients for it (boardopts.EventNames). By default every event is on and
// goes where it always has, so a board that sets nothing behaves as before.
//
// Each sending site keeps its own message for the recipient it was written
// for (the assignee's brief, the assigner's report) and asks the notice
// whether that recipient is still on the board's list; any other recipient
// the board adds gets a short note naming the event. Nobody hears twice, and
// whoever caused the event gets no note about it.

// taskboardNotice is one event's message about one task.
type taskboardNotice struct {
	r     *http.Request // nil for a sweep
	g     *db.AgentGroup
	t     *db.GroupTask // the task the event is about, as its roles read
	event boardopts.ResolvedEvent
	told  map[string]bool // agents told, or not to tell
	// actor is the agent that caused the event: it gets no note, and the
	// site's own message only with actorToo.
	actor    string
	actorToo bool
	// operatorTold is set once the operator has heard, or caused the event.
	operatorTold bool
}

// newTaskboardNotice resolves event on g's board. t names the task's roles
// (assignee, assigner, creator); pass the task as it was before the change
// when the change clears a role (an unassign).
func newTaskboardNotice(r *http.Request, g *db.AgentGroup, t *db.GroupTask, event string) *taskboardNotice {
	_, o, cols, err := taskboardOptions(g.ID)
	if err != nil {
		slog.Warn("taskboard: board options unreadable; sending the default messages", "group", g.Name, "error", err)
	}
	n := &taskboardNotice{r: r, g: g, t: t, told: map[string]bool{},
		event: boardopts.Resolve(o, taskboardOptionsGlobals(), cols).Event(event)}
	if r != nil {
		if self, _ := callerAgentID(r); self != "" {
			n.actor = self
		} else if callerIsHuman(r) && taskboardPeerAuthor(r) == "" {
			n.operatorTold = true
		}
	}
	return n
}

// withActor lets the site's own message reach the agent that caused the
// event: a close that leaves the closer's own waiting task blocked still
// tells it so, as it always has.
func (n *taskboardNotice) withActor() *taskboardNotice {
	n.actorToo = true
	return n
}

// sends reports whether the board sends this event's message to recipient.
func (n *taskboardNotice) sends(recipient string) bool { return n.event.Sends(recipient) }

// mail sends the site's own message to agentID (at conv) when recipient is
// on the board's list and the agent has not heard yet. It reports whether it
// sent.
func (n *taskboardNotice) mail(recipient, agentID, conv, subject, body string) bool {
	if !n.sends(recipient) || agentID == "" || conv == "" || n.told[agentID] || (agentID == n.actor && !n.actorToo) {
		return false
	}
	n.told[agentID] = true
	n.send(conv, subject, body)
	return true
}

func (n *taskboardNotice) send(conv, subject, body string) {
	if n.r == nil {
		systemMailTaskboard(n.g, conv, subject, body)
		return
	}
	mailTaskboard(n.r, n.g, conv, subject, body)
}

// others tells the board's recipients the site does not write its own
// message for: what happened, as "what" ("assigned to builder by lead"),
// with body, or a short note when body is "". handled are the recipients the
// site has already dealt with.
func (n *taskboardNotice) others(what, body string, handled ...string) {
	if !n.event.Enabled.Value {
		return
	}
	subject := taskboardSubject(n.g, n.t, what)
	if body == "" {
		body = taskboardLinked(n.g, n.t, fmt.Sprintf("Task %s %q in %s: %s.\nRead it: tclaude agent taskboard show %s\n",
			n.t.ID, n.t.Title, n.g.Name, what, n.t.ID))
	}
	agent := func(id string) {
		if id == "" || id == db.OperatorAssignee || n.told[id] || id == n.actor {
			return
		}
		n.told[id] = true
		if conv := activeTaskboardAgentConv(id); conv != "" {
			n.send(conv, subject, body)
		}
	}
	for _, recipient := range n.event.To.Value {
		if slices.Contains(handled, recipient) {
			continue
		}
		switch recipient {
		case boardopts.RecipientAssignee:
			agent(n.t.AssigneeID)
		case boardopts.RecipientAssigner:
			if id, ok := strings.CutPrefix(n.t.AssignedBy, "agent:"); ok {
				agent(id)
			}
		case boardopts.RecipientCreator:
			if id, ok := strings.CutPrefix(n.t.CreatedBy, "agent:"); ok {
				agent(id)
			}
		case boardopts.RecipientOwners:
			owners, err := db.ListAgentGroupOwners(n.g.ID)
			if err != nil {
				slog.Warn("taskboard: owners unreadable", "group", n.g.Name, "error", err)
			}
			for _, o := range owners {
				agent(o.AgentID)
			}
		case boardopts.RecipientOperator:
			if n.operatorTold {
				continue
			}
			n.operatorTold = true
			if _, err := db.InsertHumanMessage(&db.HumanMessage{FromTitle: "Task board", GroupName: n.g.Name,
				Subject: subject, Body: body}); err != nil {
				slog.Warn("taskboard: operator message failed", "task", n.t.ID, "error", err)
			}
		}
	}
}

// taskboardAgentLabel names an agent in a note: its title, else its id.
func taskboardAgentLabel(agentID string) string {
	if agentID == db.OperatorAssignee {
		return "the operator"
	}
	if name := taskboardAgentName(agentID); name != "" {
		return name
	}
	return agentID
}
