package agentd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Question tasks (tk_aa049a67, docs/taskboard.md): an agent asks the
// operator a question on the board (`taskboard ask-operator`), with options
// to pick and free text; the operator answers it in the dashboard or with
// `taskboard answer`. The answer closes the task as done, comes back to the
// asking agent by mail, and is on GET …/answer, which a waiting agent
// long-polls (`ask-operator --wait`).

func registerTaskboardQuestionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/taskboard/tasks/{id}/answer", handleTaskboardTask(taskboardOwnGate, handleTaskboardAnswer))
	mux.HandleFunc("GET /v1/taskboard/tasks/{id}/answer", handleTaskboardTask(taskboardReadAccess, handleTaskboardAnswerGet))
}

// taskboardAnswerWaitMax caps one long poll of GET …/answer; a longer wait
// polls again.
const taskboardAnswerWaitMax = 30 * time.Second

// taskboardWaitFresh is how long after its last poll an asker still counts
// as waiting.
const taskboardWaitFresh = 90 * time.Second

// taskboardWaiting remembers, in memory only, when someone last polled a
// question's answer: the card says the asker is waiting.
var taskboardWaiting sync.Map // task id → time.Time

func taskboardQuestionWaiting(id string, now time.Time) bool {
	v, ok := taskboardWaiting.Load(id)
	return ok && now.Sub(v.(time.Time)) < taskboardWaitFresh
}

// POST /v1/taskboard/tasks/{id}/answer {"picked": ["key"|"label"…], "text": "…"}
// — `taskboard answer`. The operator answers, or a taskboard.assign holder;
// never the agent that asked (it may drop its question instead).
func handleTaskboardAnswer(w http.ResponseWriter, r *http.Request, t *db.GroupTask, g *db.AgentGroup) {
	var body struct {
		Picked []string `json:"picked"`
		Text   string   `json:"text"`
	}
	if !decodeTaskboardBody(w, r, &body) {
		return
	}
	if t.Question() == nil {
		writeError(w, http.StatusConflict, "not_question", "task "+t.ID+" is not a question; close it with done")
		return
	}
	if _, ok := requireGroupPermission(w, r, PermTaskboardAssign, g, ActionContext{Group: g.Name}); !ok {
		return
	}
	if taskboardActor(r) == t.Asker() {
		writeError(w, http.StatusForbidden, "own_question", "the agent that asked does not answer its own question; drop it to withdraw it")
		return
	}
	waiters := taskboardWaitersBefore(t)
	out, err := db.AnswerGroupTask(t.ID, body.Picked, body.Text, taskboardActor(r))
	if err != nil {
		writeTaskboardError(w, err)
		return
	}
	taskboardWaiting.Delete(t.ID)
	q := out.Question()
	setAuditDetail(r, "answered "+t.ID)
	mailTaskboardAnswer(r, g, out, q)
	mailTaskboardWaiters(r, g, out, db.GroupTaskDone, q.AnswerText(), waiters, true)
	writeTaskboardTask(w, http.StatusOK, g, out)
}

// mailTaskboardAnswer sends the answer to the agent that asked: a readable
// line, then the answer as JSON. A linked node's agent gets it as federation
// mail.
func mailTaskboardAnswer(r *http.Request, g *db.AgentGroup, t *db.GroupTask, q *db.GroupTaskQuestion) {
	if q == nil || q.Answer == nil {
		return
	}
	asker, local := strings.CutPrefix(t.Asker(), "agent:")
	if !local && q.AskedVia == "" {
		return
	}
	raw, _ := json.Marshal(q.Answer)
	var b strings.Builder
	fmt.Fprintf(&b, "%s answered your question %s %q in %s.\n\n", taskboardCallerName(r), t.ID, t.Title, g.Name)
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
	if !local {
		// A linked node's agent asked: the answer goes back over the
		// federation (taskboard_question_peer.go).
		mailTaskboardAnswerToPeer(t, q, taskboardSubject(g, t, "answered"), b.String())
		return
	}
	conv, err := db.CurrentConvForAgent(asker)
	if err != nil || conv == "" {
		return
	}
	if state, err := db.AgentState(conv); err != nil || state == db.AgentStateRetired {
		return
	}
	mailTaskboard(r, g, conv, taskboardSubject(g, t, "answered"), taskboardLinked(g, t, b.String()))
}

// GET /v1/taskboard/tasks/{id}/answer[?wait=30s] — the question's state:
// {"answered": bool, "status", "question"}. With wait it holds the request
// until the question is answered or closed, or the wait (at most 30s) runs
// out; `ask-operator --wait` polls it again for a longer wait.
func handleTaskboardAnswerGet(w http.ResponseWriter, r *http.Request, t *db.GroupTask, _ *db.AgentGroup) {
	if t.Question() == nil {
		writeError(w, http.StatusConflict, "not_question", "task "+t.ID+" is not a question")
		return
	}
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
	// Only the asker's own polls mark the card "waiting".
	mark := func() {
		if taskboardActor(r) == t.Asker() {
			taskboardWaiting.Store(t.ID, time.Now())
		}
	}
	if wait > 0 {
		deadline := time.Now().Add(wait)
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for cur.Live() && time.Now().Before(deadline) {
			mark()
			select {
			case <-r.Context().Done():
				return
			case <-tick.C:
			}
			next, err := db.GetGroupTask(t.ID)
			if err != nil {
				writeTaskboardError(w, err)
				return
			}
			cur = next
		}
		mark()
	}
	if !cur.Live() {
		taskboardWaiting.Delete(t.ID)
	}
	q := cur.Question()
	writeJSON(w, http.StatusOK, map[string]any{"task": cur.ID, "status": cur.Status,
		"answered": q != nil && q.Answer != nil, "question": q})
}
