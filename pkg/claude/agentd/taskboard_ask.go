package agentd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// "Ask an agent" (tcl-kp5ngc, docs/taskboard.md): the dashboard's "+ New task"
// can hand a prose request to a member of the group, who turns it into tasks
// with `tclaude agent taskboard add`. The request is inbox mail, never
// send-keys, and the CLI form is `tclaude agent taskboard ask <group> --to
// <agent> "…"`.

// maxTaskboardAskBytes bounds the request's prose, as a task's brief is.
const maxTaskboardAskBytes = maxTaskBodyBytes

// taskboardMemberWrites reports whether the member (or owner) behind conv may
// change g's board under its writer lock; membership and ownership grant
// taskboard.write. (An explicit deny of taskboard.write on a member is not
// looked at: its own `add` would be refused.) It is evaluated for that member alone, so none of
// the caller's own grants (a sudo, say) count for it.
func taskboardMemberWrites(r *http.Request, g *db.AgentGroup, conv string) bool {
	ok, err := taskboardLockPasses(r.WithContext(context.Background()), g, conv)
	return err == nil && ok
}

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
// with the request, then the request itself. files names the files the
// request came with, attached to the message.
func taskboardAskBrief(g *db.AgentGroup, by, prompt string, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s asks you to put tasks on the %s task board.\n\n", by, g.Name)
	b.WriteString("Turn the request below into one or more well-formed tasks: a one-line title, a short brief, a ref where\n")
	b.WriteString("there is one (awb/Linear id, owner/repo#n or URL), and the stage (an idea, unless it is clearly next):\n\n")
	fmt.Fprintf(&b, "  tclaude agent taskboard add %s \"title\" --body \"brief\" [--ref <ref>] [--next] [--after tk_…]\n\n", g.Name)
	if len(files) > 0 {
		fmt.Fprintf(&b, "The request came with %d file(s), attached to this message: %s.\n", len(files), strings.Join(files, ", "))
		b.WriteString("Save them (tclaude agent inbox attachment <this message id>) and put each on the task it belongs to\n")
		b.WriteString("with --attach <file> on the add (repeatable), or later with: tclaude agent taskboard attach <task> <file>…\n\n")
	}
	b.WriteString("Only create the tasks: do not start the work. Reply to this message when you are done, with the task ids.\n\n")
	b.WriteString("Request:\n")
	b.WriteString(prompt)
	b.WriteString("\n")
	return b.String()
}

// POST /v1/groups/{name}/taskboard/ask {"to": "<agent>", "prompt": "…"} —
// mail an active member (or owner) who may change the board a request to turn prose
// into tasks. The caller needs taskboard.write on the group, as an add does.
// Multipart carries files with the request (the JSON as the "task" part,
// "file" parts), delivered as the mail's attachments under the task
// attachments' caps and allow-list.
func handleTaskboardAsk(w http.ResponseWriter, r *http.Request, g *db.AgentGroup) {
	if !taskboardEnabled(w) || !requireTaskboard(w, r, g, true) || !requireGroupActive(w, g) {
		return
	}
	var body struct {
		To     string `json:"to"`
		Prompt string `json:"prompt"`
	}
	files, ok := decodeTaskboardRequest(w, r, &body)
	if !ok {
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
			fmt.Sprintf("%s may not change the %s task board (it is locked to its listed writers); ask one of them", guideText(body.To, 60), g.Name))
		return
	}
	by := taskboardCallerName(r)
	from := confirmedAgentConv(r)
	names := make([]string, 0, len(files))
	for _, f := range files {
		names = append(names, f.Name)
	}
	m := &db.AgentMessage{GroupID: g.ID, FromConv: from, ToConv: conv, ToRecipients: []string{conv},
		Subject: fmt.Sprintf("[taskboard] task request from %s: %s", by, g.Name),
		Body:    taskboardAskBrief(g, by, prompt, names),
		// As every taskboard notice: a linked node's request is its own,
		// never this node's operator's.
		OperatorAuthored: from == "" && callerIsHuman(r) && taskboardPeerAuthor(r) == ""}
	var id int64
	if len(files) == 0 {
		id, err = queueAgentMessage(m)
	} else {
		id, err = queueTaskboardAskWithFiles(w, m, files)
		if id == 0 && err == nil {
			return // refused, and answered
		}
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mail", err.Error())
		return
	}
	setAuditDetail(r, fmt.Sprintf("asked %s for tasks on %s", agentID, g.Name))
	writeJSON(w, http.StatusOK, map[string]any{"group": g.Name, "to": agentID, "message_id": id})
}

// queueTaskboardAskWithFiles queues an ask's mail with its files as the
// message's attachments, in their own directory of the message store. A
// full inbox is refused with the queue's answer (0, nil).
func queueTaskboardAskWithFiles(w http.ResponseWriter, m *db.AgentMessage, files []taskboardUpload) (int64, error) {
	rows, dir, err := storeMessageAttachments(filepath.Join(operatorMessageAttachmentsBase, convops.GenerateUUID()), taskAttachmentPayloads(files))
	if err != nil {
		return 0, err
	}
	id, _, err := queueRegularAgentMessageWithAttachments(m, rows)
	if err != nil {
		_ = os.RemoveAll(dir)
		if full, ok := agentMessageQueueFull(err); ok {
			writeQueueFull(w, m.ToConv, full)
			return 0, nil
		}
		return 0, err
	}
	return id, nil
}
