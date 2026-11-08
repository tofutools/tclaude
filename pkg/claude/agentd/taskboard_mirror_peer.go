package agentd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// A mirrored question (taskboard_mirror.go) keeps its original's authority
// over federation. A linked node acting as this node's operator (unrestricted
// trust) reads the human inbox anyway and sees it as the operator does. A
// node reading the board under its taskboard grants sees that the operator
// has a question there, never what it asks or was answered: the notify-human
// message or the access request is the human inbox's to show, under its own
// rules (peer_human_inbox.go). Answering one is refused to every linked node
// (answerMirroredQuestion).

// mirroredRedactedTitle stands in for a mirrored question's title, and for
// the title of the question task made for it.
const mirroredRedactedTitle = "A question for this node's operator"

func mirroredSource(v any) bool {
	s, _ := v.(string)
	return strings.HasPrefix(s, db.GroupTaskQuestionSourceNotify) || strings.HasPrefix(s, db.GroupTaskQuestionSourceApproval)
}

// redactMirroredWriter buffers a taskboard JSON response to a linked node
// with restricted trust and redacts the mirrored questions in it as it ends.
type redactMirroredWriter struct {
	w      http.ResponseWriter
	header http.Header
	status int
	buf    bytes.Buffer
}

func newRedactMirroredWriter(w http.ResponseWriter) *redactMirroredWriter {
	return &redactMirroredWriter{w: w, header: http.Header{}, status: http.StatusOK}
}

func (rw *redactMirroredWriter) Header() http.Header         { return rw.header }
func (rw *redactMirroredWriter) WriteHeader(status int)      { rw.status = status }
func (rw *redactMirroredWriter) Write(b []byte) (int, error) { return rw.buf.Write(b) }

// finish writes the response, redacted when it is JSON that carries a
// mirrored question.
func (rw *redactMirroredWriter) finish() {
	body := rw.buf.Bytes()
	if strings.HasPrefix(rw.header.Get("Content-Type"), "application/json") &&
		(bytes.Contains(body, []byte(`"`+db.GroupTaskQuestionSourceNotify)) || bytes.Contains(body, []byte(`"`+db.GroupTaskQuestionSourceApproval))) {
		var doc any
		if json.Unmarshal(body, &doc) == nil {
			redactMirrored(doc)
			if out, err := json.Marshal(doc); err == nil {
				body = append(out, '\n')
			}
		}
	}
	for k, v := range rw.header {
		rw.w.Header()[k] = v
	}
	rw.w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	rw.w.WriteHeader(rw.status)
	_, _ = rw.w.Write(body)
}

// redactMirrored redacts, in a decoded taskboard response, each mirrored
// question (title, context, options and answer), the title and text of a
// question task made for one, and the text of its activity entries.
func redactMirrored(doc any) {
	// The activity entries answering a mirrored question name it by number;
	// a response with activity is one task's.
	answers := map[float64]bool{}
	var walk func(v any, collect bool)
	walk = func(v any, collect bool) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				walk(e, collect)
			}
		case map[string]any:
			if x["mirrored"] == true {
				x["title"] = mirroredRedactedTitle
			}
			if mirroredSource(x["source"]) {
				x["title"] = mirroredRedactedTitle
				for _, k := range []string{"body", "options", "recommend", "multi", "answer"} {
					delete(x, k)
				}
				if n, ok := x["n"].(float64); ok && collect {
					answers[n] = true
				}
			}
			if q, ok := x["question"].(map[string]any); ok && mirroredSource(q["source"]) {
				// A question task made for it (its title and text are the
				// question's), or the activity entry of the question.
				if _, task := x["status"]; task {
					x["title"] = mirroredRedactedTitle
				}
				delete(x, "body")
			}
			if n, ok := x["answers"].(float64); ok && !collect && answers[n] {
				delete(x, "body")
			}
			for _, e := range x {
				walk(e, collect)
			}
		}
	}
	walk(doc, true)
	walk(doc, false)
}
