package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Questions for the operator (tk_aa049a67, tk_8b2c86f7, docs/taskboard.md):
// an agent asks the operator a question on any task of the board
// (`taskboard ask-operator <task>`), or on a new task made for it
// (`ask-operator <group>`), with options to pick and free text. Each
// question is an entry of the task's activity; the operator answers it in
// the dashboard or with `taskboard answer`, the answer is an entry right
// under it, it comes back to the asking agent by mail, and it is on GET
// …/questions/{n}, which a waiting agent long-polls (`ask-operator --wait`).

func registerTaskboardQuestionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/questions", handleTaskboardTask(taskboardOwnGate, handleTaskboardAskOperator))
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/questions/{n}/answer", handleTaskboardTask(taskboardOwnGate, handleTaskboardAnswer))
	mux.HandleFunc("GET /v1/taskboard/tasks/{id}/questions/{n}", handleTaskboardTask(taskboardReadAccess, handleTaskboardAnswerGet))
	// A question task's routes (phase 2): its only open question.
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/answer", handleTaskboardTask(taskboardOwnGate, handleTaskboardAnswer))
	mux.HandleFunc("GET /v1/taskboard/tasks/{id}/answer", handleTaskboardTask(taskboardReadAccess, handleTaskboardAnswerGet))
}

// taskboardAnswerWaitMax caps one long poll of GET …/questions/{n}; a
// longer wait polls again.
const taskboardAnswerWaitMax = 30 * time.Second

// taskboardWaitFresh is how long after its last poll an asker still counts
// as waiting.
const taskboardWaitFresh = 90 * time.Second

// taskboardWaiting remembers, in memory only, when the asker last polled a
// question's answer: the card says the asker is waiting.
var taskboardWaiting sync.Map // "<task id>#<n>" → time.Time

func taskboardWaitKey(id string, n int) string { return id + "#" + strconv.Itoa(n) }

func taskboardQuestionWaiting(id string, n int, now time.Time) bool {
	v, ok := taskboardWaiting.Load(taskboardWaitKey(id, n))
	return ok && now.Sub(v.(time.Time)) < taskboardWaitFresh
}

// taskboardQuestionView is a question on the wire: Waiting marks one whose
// asker is polling for the answer (ask-operator --wait) right now.
type taskboardQuestionView struct {
	*db.GroupTaskQuestion
	Waiting bool `json:"waiting,omitempty"`
}

func taskboardQuestionViewOf(taskID string, q *db.GroupTaskQuestion, now time.Time) *taskboardQuestionView {
	return &taskboardQuestionView{GroupTaskQuestion: q, Waiting: q.Open() && taskboardQuestionWaiting(taskID, q.N, now)}
}

// taskboardQuestionN reads {n} ("2" or "Q2"); 0 when the route has none.
func taskboardQuestionN(w http.ResponseWriter, r *http.Request) (int, bool) {
	s := strings.TrimPrefix(strings.TrimPrefix(r.PathValue("n"), "Q"), "q")
	if s == "" {
		return 0, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		writeError(w, http.StatusBadRequest, "invalid_arg", "a question is named by its number (Q1, Q2…)")
		return 0, false
	}
	return n, true
}

// POST /v1/taskboard/tasks/{id}/questions {"title", "body", "options",
// "multi", "recommend", "block"} — `ask-operator <task>`: asks the
// operator on the task, as an entry of its activity. Asking is gated as
// commenting is; block (the task waits on the answer) as a change of the
// task.
func handleTaskboardAskOperator(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		db.GroupTaskQuestion
		Block bool `json:"block"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	if body.Block {
		if !requireTaskboardTaskChange(w, r, g, t) {
			return
		}
	} else if !requireTaskboardCommenter(w, r, g) {
		return
	}
	if !requireGroupActive(w, g) {
		return
	}
	actor := taskboardActor(r)
	if actor == "human:operator" {
		writeError(w, http.StatusBadRequest, "invalid_arg", "questions are for the operator to answer; ask an agent with `taskboard ask` or a comment")
		return
	}
	q := body.GroupTaskQuestion
	// The question and its context are gated as a task's title and text.
	var err error
	if q.Title, err = validateTaskTitle(q.Title); err == nil {
		q.Body, err = validateTaskText("a question's context", strings.TrimSpace(q.Body), db.GroupTaskQuestionBodyMax)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_arg", err.Error())
		return
	}
	// Who asked is the daemon's to say, never the body's.
	q.AskedBy, q.AskedVia, q.Blocks = actor, taskboardPeerAskerPrincipal(r), body.Block
	q.Answer, q.State = nil, ""
	out, asked, err := db.AskGroupTaskQuestion(t.ID, &q)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	setAuditDetail(r, fmt.Sprintf("asked Q%d on %s", asked.N, t.ID))
	views, err := taskboardViews(g.Name, []*db.GroupTask{out})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"task": views[0], "question": taskboardQuestionViewOf(t.ID, asked, time.Now())})
}

// POST /v1/taskboard/tasks/{id}/questions/{n}/answer (or …/answer for the
// task's only open question) {"picked": ["key"|"label"…], "text": "…"} —
// `taskboard answer`. The operator answers, or a taskboard.assign holder;
// never the agent that asked (it may withdraw its question instead).
func handleTaskboardAnswer(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	n, ok := taskboardQuestionN(w, r)
	if !ok {
		return
	}
	var body struct {
		Picked []string `json:"picked"`
		Text   string   `json:"text"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	if _, ok := requireGroupPermission(w, r, PermTaskboardAssign, g, ActionContext{Group: g.Name}); !ok {
		return
	}
	pending, err := taskboardPendingQuestion(t.ID, n)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	if taskboardActor(r) == pending.AskedBy {
		writeError(w, http.StatusForbidden, "own_question", "the agent that asked does not answer its own question; withdraw it instead")
		return
	}
	waiters := taskboardWaitersBefore(t)
	out, q, err := db.AnswerGroupTaskQuestion(t.ID, pending.N, body.Picked, body.Text, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	taskboardWaiting.Delete(taskboardWaitKey(t.ID, q.N))
	setAuditDetail(r, fmt.Sprintf("answered Q%d on %s", q.N, t.ID))
	mailTaskboardAnswer(r, g, out, q)
	if !out.Live() {
		mailTaskboardWaiters(r, g, out, db.GroupTaskDone, q.AnswerText(), waiters, true)
	}
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// taskboardPendingQuestion is the question an answer names: n, or with n 0
// the task's only open question. The store checks it again as it answers.
func taskboardPendingQuestion(taskID string, n int) (*db.GroupTaskQuestion, error) {
	if n > 0 {
		return db.GetGroupTaskQuestion(taskID, n)
	}
	qs, err := db.ListGroupTaskQuestions([]string{taskID})
	if err != nil {
		return nil, err
	}
	var open []*db.GroupTaskQuestion
	for _, q := range qs[taskID] {
		if q.Open() {
			open = append(open, q)
		}
	}
	switch {
	case len(qs[taskID]) == 0:
		return nil, &db.GroupTaskConflict{Code: "not_question", Msg: "task " + taskID + " has no question"}
	case len(open) == 1:
		return open[0], nil
	case len(open) == 0:
		// The task's last question, so the store says why it is not
		// answerable.
		return qs[taskID][len(qs[taskID])-1], nil
	}
	return nil, &db.GroupTaskConflict{Code: "which_question",
		Msg: fmt.Sprintf("task %s has %d open questions; name one (Q%d…)", taskID, len(open), open[0].N)}
}

// taskboardQuestionMailKey tags an answer's federation mail: the task id
// for Q1 (as a phase 2 question task's answer was tagged), "<id>#<n>"
// after it. The asking node records the same key.
func taskboardQuestionMailKey(taskID string, n int) string {
	if n <= 1 {
		return taskID
	}
	return taskboardWaitKey(taskID, n)
}

// mailTaskboardAnswer sends the answer to the agent that asked: a readable
// line, then the answer as JSON. A linked node's agent gets it as federation
// mail.
func mailTaskboardAnswer(r *http.Request, g *db.AgentGroup, t *db.GroupTask, q *db.GroupTaskQuestion) {
	if q == nil || q.Answer == nil {
		return
	}
	asker, local := strings.CutPrefix(q.AskedBy, "agent:")
	if !local && q.AskedVia == "" {
		return
	}
	raw, _ := json.Marshal(q.Answer)
	var b strings.Builder
	// A question task's own question reads as the task.
	own := t.IsQuestionTask() && q.N == 1
	if own {
		fmt.Fprintf(&b, "%s answered your question %s %q in %s.\n\n", taskboardCallerName(r), t.ID, t.Title, g.Name)
	} else {
		fmt.Fprintf(&b, "%s answered your question Q%d %q on %s %q in %s.\n\n", taskboardCallerName(r), q.N, q.Title, t.ID, t.Title, g.Name)
	}
	for _, k := range q.Answer.Picked {
		for _, o := range q.Options {
			if o.Key == k {
				fmt.Fprintf(&b, "Picked: %s\n", o.Label)
			}
		}
	}
	if q.Answer.Text != "" {
		fmt.Fprintf(&b, "Said: %s\n", q.Answer.Text)
	}
	fmt.Fprintf(&b, "\n%s\n", raw)
	subject := taskboardSubject(g, t, "answered")
	if !own {
		subject = taskboardSubject(g, t, fmt.Sprintf("answered Q%d", q.N))
	}
	if !local {
		// A linked node's agent asked: the answer goes back over the
		// federation (taskboard_question_peer.go).
		mailTaskboardAnswerToPeer(t, q, subject, b.String())
		return
	}
	conv, err := db.CurrentConvForAgent(asker)
	if err != nil || conv == "" {
		return
	}
	if state, err := db.AgentState(conv); err != nil || state == db.AgentStateRetired {
		return
	}
	mailTaskboard(r, g, conv, subject, taskboardLinked(g, t, b.String()))
}

// GET /v1/taskboard/tasks/{id}/questions/{n}[?wait=30s] (or …/answer for
// the task's only open question, else its last) — the question's state:
// {"task", "n", "status", "state", "answered", "question"}. With wait it
// holds the request until the question is answered or closed, or the wait
// (at most 30s) runs out; `ask-operator --wait` polls it again for a longer
// wait.
func handleTaskboardAnswerGet(w http.ResponseWriter, r *http.Request, t *db.GroupTask, _ *db.AgentGroup) {
	n, ok := taskboardQuestionN(w, r)
	if !ok {
		return
	}
	q, err := taskboardPendingQuestion(t.ID, n)
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	n = q.N
	var wait time.Duration
	if s := r.URL.Query().Get("wait"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			writeError(w, http.StatusBadRequest, "invalid_arg", "wait is a duration such as 30s")
			return
		}
		wait = min(d, taskboardAnswerWaitMax)
	}
	cur := t
	key := taskboardWaitKey(t.ID, n)
	// Only the asker's own polls mark the card "waiting".
	mark := func() {
		if taskboardActor(r) == q.AskedBy {
			taskboardWaiting.Store(key, time.Now())
		}
	}
	if wait > 0 {
		deadline := time.Now().Add(wait)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for q.Open() && cur.Live() && time.Now().Before(deadline) {
			mark()
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
			}
			if cur, err = db.GetGroupTask(t.ID); err != nil {
				writeTaskboardError(w, err)
				return
			}
			if q, err = db.GetGroupTaskQuestion(t.ID, n); err != nil {
				writeTaskboardError(w, err)
				return
			}
		}
		mark()
	}
	if !q.Open() {
		taskboardWaiting.Delete(key)
	}
	writeJSON(w, http.StatusOK, map[string]any{"task": cur.ID, "n": q.N, "status": cur.Status, "state": q.State,
		"answered": q.Answer != nil, "question": q})
}
