package agentd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// "Ask an agent" (tcl-kp5ngc, docs/taskboard.md): the dashboard's "+ New task"
// can hand a prose request to a member of the group, who turns it into tasks
// with `tclaude agent taskboard add`. The request is inbox mail, never
// send-keys, and the CLI form is `tclaude agent taskboard ask <group> --to
// <agent> "…"`.

// maxTaskboardAskBytes bounds the request's prose, as a task's brief is.
const maxTaskboardAskBytes = maxTaskBodyBytes

// activeGroupOwner returns the live conv of agentID when it is an owner of g
// that is not retired, "" otherwise. An owner may change the board (it
// passes the writer lock), so it can be asked even when it is not a member.
func activeGroupOwner(g *db.AgentGroup, agentID string) string {
	if g == nil || g.IsArchived() || agentID == "" {
		return ""
	}
	owners, err := db.ListAgentGroupOwners(g.ID)
	if err != nil {
		return ""
	}
	for _, o := range owners {
		if o.AgentID != agentID {
			continue
		}
		conv, err := db.CurrentConvForAgent(agentID)
		if err != nil || conv == "" {
			return ""
		}
		if state, err := db.AgentState(conv); err != nil || state == db.AgentStateRetired {
			return ""
		}
		return conv
	}
	return ""
}

// resolveTaskboardAskee resolves who to ask: an active member of g, or an
// active owner of it.
func resolveTaskboardAskee(g *db.AgentGroup, selector string) (agentID, conv string, err error) {
	if agentID, conv, err = resolveTaskboardMember(g, selector); err == nil {
		return agentID, conv, nil
	}
	if res, _, rerr := agent.ResolveSelector(selector); rerr == nil && res.ConvID != "" {
		if id, _ := db.AgentIDForConv(res.ConvID); id != "" {
			if c := activeGroupOwner(g, id); c != "" {
				return id, c, nil
			}
		}
	}
	return "", "", fmt.Errorf("%s is not an active member or owner of %s; the agent asked must be", guideText(selector, 60), g.Name)
}

// taskboardAskBrief is the inbox message the asked agent gets: what to do
// with the request, then the request itself.
func taskboardAskBrief(g *db.AgentGroup, by, prompt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s asks you to put tasks on the %s task board.\n\n", by, g.Name)
	b.WriteString("Turn the request below into one or more well-formed tasks: a one-line title, a short brief, a ref where\n")
	b.WriteString("there is one (awb/Linear id, owner/repo#n or URL), and the stage (an idea, unless it is clearly next):\n\n")
	fmt.Fprintf(&b, "  tclaude agent taskboard add %s \"title\" --body \"brief\" [--ref <ref>] [--next] [--after tk_…]\n\n", g.Name)
	b.WriteString("Only create the tasks: do not start the work. Reply to this message when you are done, with the task ids.\n\n")
	b.WriteString("Request:\n")
	b.WriteString(prompt)
	b.WriteString("\n")
	return b.String()
}

// POST /v1/groups/{name}/taskboard/ask {"to": "<agent>", "prompt": "…"} —
// mail an active member (or owner) who may change the board a request to turn prose
// into tasks. The caller needs taskboard.write on the group, as an add does.
func handleTaskboardAsk(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !taskboardEnabled(w) || !requireTaskboard(w, r, g, true) || !requireGroupActive(w, g) {
		return
	}
	var body struct {
		To     string `json:"to"`
		Prompt string `json:"prompt"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	prompt, err := validateTaskText("the request", strings.TrimSpace(body.Prompt), maxTaskboardAskBytes)
	if err == nil && prompt == "" {
		err = fmt.Errorf("give the request: what tasks should the agent add?")
	}
	if err == nil && strings.TrimSpace(body.To) == "" {
		err = fmt.Errorf("give the agent to ask (to)")
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	agentID, conv, err := resolveTaskboardAskee(g, strings.TrimSpace(body.To))
	if err != nil {
		writeError(w, http.StatusBadRequest, "not_member", err.Error())
		return
	}
	if !taskboardMemberWrites(r, g, conv) {
		writeError(w, http.StatusConflict, "taskboard_locked",
			fmt.Sprintf("%s may not add tasks to the %s task board: that takes taskboard.write (an owner has it) and a pass of its writer lock; ask an owner", guideText(body.To, 60), g.Name))
		return
	}
	by := taskboardCallerName(r)
	from := confirmedAgentConv(r)
	m := &db.AgentMessage{GroupID: g.ID, FromConv: from, ToConv: conv, ToRecipients: []string{conv},
		Subject: fmt.Sprintf("[taskboard] task request from %s: %s", by, g.Name),
		Body:    taskboardAskBrief(g, by, prompt),
		// As every taskboard notice: a linked node's request is its own,
		// never this node's operator's.
		OperatorAuthored: from == "" && callerIsHuman(r) && taskboardPeerAuthor(r) == ""}
	id, err := queueAgentMessage(m)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mail", err.Error())
		return
	}
	setAuditDetail(r, fmt.Sprintf("asked %s for tasks on %s", agentID, g.Name))
	writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "to": agentID, "message_id": id})
}
