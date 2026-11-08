package agentd

import (
	"cmp"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Mirrors onto the board (tk_8b2c86f7, docs/taskboard.md): a notify-human
// message that asks something, and an agent's --ask-human prompt, are also
// questions on the board, so everything waiting on the operator is in one
// place. The original stays exactly where it is and keeps its own flow; the
// board's question names it as its source (db.GroupTaskQuestion.Source).
//
//   - Answering on the board answers the original through its own path (the
//     Messages reply or decision, the Access-requests decision), and the
//     answer reaches the agent only that way: the board sends no mail of its
//     own.
//   - The original answered, dismissed, deleted or timed out anywhere writes
//     the board's answer or closes its question, from the one place each
//     original ends (serveHumanMessagesReply, dismissDecisions, the approval
//     waiter's recordResolved).
//
// The question lands on the board of the group taskboardMirrorGroup picks, on
// the asker's current task there, or on a new question task for the
// operator. Mirroring fails soft: the original is never held up by it.

const (
	mirrorActorOperator = "human:operator"
	mirrorActorSystem   = "system"
)

// taskboardMirrorGroup is the group whose board a mirrored question of
// agentID lands on: hint (the group the original names) if the agent is a
// member there; else the group of the task it started most recently; else
// its first group. Only an active group counts. nil when the board is off or
// the agent is in no active group: the original is then not mirrored.
func taskboardMirrorGroup(agentID, hint string) *db.AgentGroup {
	if agentID == "" {
		return nil
	}
	if cfg, _ := config.Load(); !cfg.TaskboardEnabled() {
		return nil
	}
	groups, err := db.ListGroupsForAgent(agentID)
	if err != nil {
		slog.Warn("taskboard mirror: list the asker's groups", "agent", agentID, "err", err)
		return nil
	}
	var active []*db.AgentGroup
	for _, g := range groups {
		if !g.IsArchived() {
			active = append(active, g)
		}
	}
	if len(active) == 0 {
		return nil
	}
	if hint != "" {
		for _, g := range active {
			if g.Name == hint {
				return g
			}
		}
	}
	if gid, err := db.CurrentGroupTaskGroupOf(agentID); err == nil && gid != 0 {
		for _, g := range active {
			if g.ID == gid {
				return g
			}
		}
	}
	return active[0]
}

func notifyQuestionSource(id int64) string {
	return db.GroupTaskQuestionSourceNotify + strconv.FormatInt(id, 10)
}

func approvalQuestionSource(id string) string { return db.GroupTaskQuestionSourceApproval + id }

// humanMessageQuestion is what a notify-human message asks the operator, or
// nil for one that asks nothing: a decision (options), or a message whose
// subject, or with no subject its first line, ends in "?". A process
// obligation and a senderless message are not mirrored.
func humanMessageQuestion(m *db.HumanMessage) *db.GroupTaskQuestion {
	if m == nil || m.FromAgent == "" || m.ProcessCommandID != "" {
		return nil
	}
	title, body := strings.TrimSpace(m.Subject), strings.TrimSpace(m.Body)
	if title == "" {
		first, rest, _ := strings.Cut(body, "\n")
		title, body = strings.TrimSpace(first), strings.TrimSpace(rest)
	}
	if len(m.Options) == 0 && !strings.HasSuffix(title, "?") {
		return nil
	}
	q := &db.GroupTaskQuestion{Title: title, Body: body, Source: notifyQuestionSource(m.ID)}
	for i, o := range m.Options {
		opt := db.GroupTaskQuestionOption{Key: strconv.Itoa(i + 1), Label: o}
		if len(o) > db.GroupTaskQuestionLabelMax {
			opt.Label, opt.Description = truncateBytes(o, db.GroupTaskQuestionLabelMax-len("…"))+"…", o
		}
		q.Options = append(q.Options, opt)
	}
	if m.Recommended > 0 && m.Recommended <= len(m.Options) {
		q.Recommend = strconv.Itoa(m.Recommended)
	}
	return q
}

// truncateBytes cuts s to at most n bytes on a rune boundary.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// mirrorHumanMessage puts notify-human message id on the board when it asks
// something.
func mirrorHumanMessage(id int64) {
	m, err := db.GetHumanMessage(id)
	if err != nil {
		slog.Warn("taskboard mirror: load a notify-human message", "message", id, "err", err)
		return
	}
	q := humanMessageQuestion(m)
	if q == nil {
		return
	}
	g := taskboardMirrorGroup(m.FromAgent, m.GroupName)
	if g == nil {
		return
	}
	if _, _, err := db.MirrorGroupTaskQuestion(g.ID, m.FromAgent, q); err != nil {
		slog.Warn("taskboard mirror: notify-human question", "message", m.ID, "group", g.Name, "err", err)
	}
}

// humanMessageAnswered records on the board the operator's answer to
// notify-human message orig: its decision (choice, 1-based, with note), or a
// reply. A reply without a pick does not answer a decision.
func humanMessageAnswered(orig *db.HumanMessage, choice int, text string) {
	var picked []string
	switch {
	case choice > 0:
		picked = []string{strconv.Itoa(choice)}
	case len(orig.Options) > 0:
		return
	case text == "":
		text = "(replied with attachments)"
	}
	if _, _, err := db.AnswerGroupTaskQuestionBySource(notifyQuestionSource(orig.ID), picked, text, mirrorActorOperator, false); err != nil {
		slog.Warn("taskboard mirror: record a notify-human answer", "message", orig.ID, "err", err)
	}
}

// humanMessagesEnded closes the board questions of notify-human messages
// that ended unanswered (dismissed, deleted).
func humanMessagesEnded(ids []int64, why, actor string) {
	for _, id := range ids {
		if err := db.CloseGroupTaskQuestionBySource(notifyQuestionSource(id), why, actor, false); err != nil {
			slog.Warn("taskboard mirror: close a notify-human question", "message", id, "err", err)
		}
	}
}

// Option keys of a mirrored --ask-human prompt: the decisions of
// POST /api/access-requests/{id}/decision.
const (
	approvalPickApprove      = "approve"
	approvalPickAlways       = "always"
	approvalPickAlwaysScoped = "always_scoped"
	approvalPickDeny         = "deny"
)

// approvalQuestion is the board's question for an agent's pending
// --ask-human prompt: Allow, Always allow (where the prompt offers it,
// scoped or not), Deny.
func approvalQuestion(req *approvalRequest) *db.GroupTaskQuestion {
	who := cmp.Or(req.convTitle, req.agentID)
	title := fmt.Sprintf("%s asks for %s", who, req.perm)
	var ctx []string
	if req.targetGroup != "" {
		ctx = append(ctx, "Group: "+req.targetGroup)
	}
	if req.targetConvID != "" {
		ctx = append(ctx, "On: "+cmp.Or(req.targetConvTitle, req.targetConvID))
	}
	if req.scopeDisplay != "" {
		ctx = append(ctx, "Scope: "+req.scopeDisplay)
	}
	if req.method != "" || req.path != "" {
		ctx = append(ctx, "Request: `"+strings.TrimSpace(req.method+" "+req.path)+"`")
	}
	ctx = append(ctx, "Unanswered, it is denied when it times out. It is also in Messages → Access requests.")
	opts := []db.GroupTaskQuestionOption{{Key: approvalPickApprove, Label: "Allow", Description: "this request only"}}
	if req.autoGrantable {
		opts = append(opts, db.GroupTaskQuestionOption{Key: approvalPickAlways, Label: "Always allow",
			Description: "grant " + req.perm + " to this agent"})
		if req.scopeJSON != "" && isScopedAutoGrantableSlug(req.perm) {
			opts = append(opts, db.GroupTaskQuestionOption{Key: approvalPickAlwaysScoped, Label: "Always allow, scoped",
				Description: truncateBytes("grant "+req.perm+" for "+req.scopeDisplay, db.GroupTaskQuestionDescMax)})
		}
	}
	opts = append(opts, db.GroupTaskQuestionOption{Key: approvalPickDeny, Label: "Deny"})
	return &db.GroupTaskQuestion{Title: title, Body: strings.Join(ctx, "\n\n"), Options: opts,
		Source: approvalQuestionSource(req.id)}
}

// mirrorApproval puts an agent's pending --ask-human prompt on the board. A
// linked node's access request is this node's operator's alone: not mirrored.
func mirrorApproval(req *approvalRequest) {
	if req == nil || req.peerAccess != nil || req.agentID == "" {
		return
	}
	g := taskboardMirrorGroup(req.agentID, req.targetGroup)
	if g == nil {
		return
	}
	if _, _, err := db.MirrorGroupTaskQuestion(g.ID, req.agentID, approvalQuestion(req)); err != nil {
		slog.Warn("taskboard mirror: --ask-human prompt", "request", req.id, "group", g.Name, "err", err)
	}
}

// approvalResolved records on the board how an agent's --ask-human prompt
// ended, whoever decided it. A question task made for it is archived.
func approvalResolved(req *approvalRequest, outcome string) {
	if req == nil || req.peerAccess != nil {
		return
	}
	source := approvalQuestionSource(req.id)
	pick := ""
	switch outcome {
	case "approved":
		pick = approvalPickApprove
	case "always":
		pick = approvalPickAlways
	case "always (scoped)":
		pick = approvalPickAlwaysScoped
	case "declined":
		pick = approvalPickDeny
	}
	var err error
	if pick == "" {
		err = db.CloseGroupTaskQuestionBySource(source, "Timed out: denied", mirrorActorSystem, true)
	} else {
		_, _, err = db.AnswerGroupTaskQuestionBySource(source, []string{pick}, "", mirrorActorOperator, true)
	}
	if err != nil {
		slog.Warn("taskboard mirror: record an --ask-human outcome", "request", req.id, "outcome", outcome, "err", err)
	}
}

// sweepMirroredApprovals closes the board questions of --ask-human prompts a
// previous daemon run left pending: they died with it. A prompt of this run,
// raised while the daemon was still starting, stays.
func sweepMirroredApprovals() {
	live := func(source string) bool {
		id, ok := strings.CutPrefix(source, db.GroupTaskQuestionSourceApproval)
		return ok && pendingApproval(id) != nil
	}
	if err := db.CloseGroupTaskQuestionsBySourcePrefix(db.GroupTaskQuestionSourceApproval,
		"Expired: the daemon restarted", mirrorActorSystem, true, live); err != nil {
		slog.Warn("taskboard mirror: close the last run's --ask-human prompts", "err", err)
	}
}

// answerMirroredQuestion answers on the board a question mirrored from
// another place: through its source, which records the answer on the board
// as it takes it. Only the operator answers it here, as there; a linked node
// answers the original in this node's human inbox, under its rules.
func answerMirroredQuestion(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, t *db.GroupTask, q *db.GroupTaskQuestion, picked []string, text string) {
	if taskboardActor(r) != mirrorActorOperator || taskboardPeerAuthor(r) != "" {
		writeError(w, http.StatusForbidden, "operator_only",
			fmt.Sprintf("Q%d mirrors %s: only the operator answers it, here or where it was asked", q.N, q.Source))
		return
	}
	if !q.Open() {
		writeError(w, http.StatusConflict, "answered", fmt.Sprintf("Q%d of task %s is %s", q.N, t.ID, q.State))
		return
	}
	key := ""
	for _, p := range picked {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if key != "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "pick one option")
			return
		}
		for _, o := range q.Options {
			if o.Key == p || strings.EqualFold(o.Label, p) {
				key = o.Key
			}
		}
		if key == "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", fmt.Sprintf("%q is none of the question's options", p))
			return
		}
	}
	if key == "" && len(q.Options) > 0 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "pick one of the options (a note can go with it)")
		return
	}
	switch {
	case strings.HasPrefix(q.Source, db.GroupTaskQuestionSourceNotify):
		id, err := strconv.ParseInt(strings.TrimPrefix(q.Source, db.GroupTaskQuestionSourceNotify), 10, 64)
		if err != nil {
			writeError(w, http.StatusConflict, "invalid_source", "unknown source "+q.Source)
			return
		}
		choice, _ := strconv.Atoi(key)
		if _, fail := replyHumanMessage(r.Context(), humanMessageReply{ID: id, Body: text, Choice: choice}); fail != nil {
			if fail.code == "not_found" {
				humanMessagesEnded([]int64{id}, "Deleted in Messages", mirrorActorSystem)
			}
			fail.write(w)
			return
		}
	case strings.HasPrefix(q.Source, db.GroupTaskQuestionSourceApproval):
		id := strings.TrimPrefix(q.Source, db.GroupTaskQuestionSourceApproval)
		if strings.TrimSpace(text) != "" {
			writeError(w, http.StatusBadRequest, "invalid_arg", "an access request takes a pick, no text")
			return
		}
		req := pendingApproval(id)
		if req == nil {
			writeError(w, http.StatusConflict, "not_pending", "the access request is decided or expired already")
			return
		}
		if fail := decideApproval(req, key); fail != nil {
			fail.write(w)
			return
		}
	default:
		writeError(w, http.StatusConflict, "invalid_source", "unknown source "+q.Source)
		return
	}
	setAuditDetail(r, fmt.Sprintf("answered Q%d on %s via %s", q.N, t.ID, q.Source))
	out, err := db.GetGroupTask(t.ID)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	writeTaskboardTask(w, http.StatusOK, g, out)
}
