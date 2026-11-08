package db

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// Question tasks (tk_aa049a67, phase 2 of tcl-p4t78f): an operator task that
// asks a question, modelled on AskUserQuestion. Its options (none, or 2 to
// 6) are picked one or, with Multi, several; free text is always allowed and
// required when there are no options. The answer is stored with the
// question, the task is closed as done with it, and the asking agent gets it
// by mail and on the answer route.

// GroupTaskQuestionKind is the one operator_request kind so far.
const GroupTaskQuestionKind = "question"

// Bounds of a question.
const (
	GroupTaskQuestionOptionsMax = 6
	GroupTaskQuestionLabelMax   = 80
	GroupTaskQuestionDescMax    = 300
	GroupTaskQuestionPreviewMax = 2 << 10
	GroupTaskAnswerTextMax      = 4000 // characters
)

// GroupTaskEventAnswered is a question answered.
const GroupTaskEventAnswered = "answered"

// GroupTaskQuestion is a question task's operator_request.
type GroupTaskQuestion struct {
	Kind      string                    `json:"kind"`
	Multi     bool                      `json:"multi,omitempty"`
	Options   []GroupTaskQuestionOption `json:"options,omitempty"`
	Recommend string                    `json:"recommend,omitempty"`
	// AskedBy is the actor that asked ("agent:<id>", "peer: <author>",
	// "operator"), fixed at create: assign and unassign rewrite the task's
	// assigned_by, but the answer still goes back to the asker.
	AskedBy string `json:"asked_by,omitempty"`
	// AskedVia is a linked node's asking agent by stable ids
	// ("peer:<instance>/agent:<id>"), set by the daemon when a peer's agent
	// asked: the answer goes back to it as federation mail.
	AskedVia string           `json:"asked_via,omitempty"`
	Answer   *GroupTaskAnswer `json:"answer,omitempty"`
}

// GroupTaskQuestionOption is one choice.
type GroupTaskQuestionOption struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Preview     string `json:"preview,omitempty"`
}

// GroupTaskAnswer is the operator's answer: the picked option keys and/or
// free text.
type GroupTaskAnswer struct {
	Picked     []string  `json:"picked"`
	Text       string    `json:"text,omitempty"`
	AnsweredBy string    `json:"answered_by"`
	AnsweredAt time.Time `json:"answered_at"`
}

// Question reads the task's question; nil for any other task.
func (t *GroupTask) Question() *GroupTaskQuestion {
	if t.OperatorRequest == "" {
		return nil
	}
	var q GroupTaskQuestion
	if json.Unmarshal([]byte(t.OperatorRequest), &q) != nil || q.Kind != GroupTaskQuestionKind {
		return nil
	}
	return &q
}

// NormalizeGroupTaskQuestion checks a new question and fills in what may be
// left out: the kind, and each option's key (from its label). Recommend may
// name an option by key or label.
func NormalizeGroupTaskQuestion(q *GroupTaskQuestion) error {
	if q.Kind == "" {
		q.Kind = GroupTaskQuestionKind
	}
	if q.Kind != GroupTaskQuestionKind {
		return taskConflict("invalid_arg", "operator_request kind is %q", GroupTaskQuestionKind)
	}
	if q.Answer != nil {
		return taskConflict("invalid_arg", "a new question has no answer")
	}
	if n := len(q.Options); n == 1 || n > GroupTaskQuestionOptionsMax {
		return taskConflict("invalid_arg", "a question has no options, or 2 to %d", GroupTaskQuestionOptionsMax)
	}
	if q.Multi && len(q.Options) == 0 {
		return taskConflict("invalid_arg", "multi is for a question with options")
	}
	seen := map[string]bool{}
	for i := range q.Options {
		o := &q.Options[i]
		o.Label = strings.TrimSpace(o.Label)
		o.Description = strings.TrimSpace(o.Description)
		switch {
		case o.Label == "" || len(o.Label) > GroupTaskQuestionLabelMax || strings.ContainsAny(o.Label, "\r\n"):
			return taskConflict("invalid_arg", "option %d needs a one-line label of up to %d bytes", i+1, GroupTaskQuestionLabelMax)
		case len(o.Description) > GroupTaskQuestionDescMax:
			return taskConflict("invalid_arg", "option %q: the description is at most %d bytes", o.Label, GroupTaskQuestionDescMax)
		case len(o.Preview) > GroupTaskQuestionPreviewMax:
			return taskConflict("invalid_arg", "option %q: the preview is at most %d bytes", o.Label, GroupTaskQuestionPreviewMax)
		case !utf8.ValidString(o.Label + o.Description + o.Preview):
			return taskConflict("invalid_arg", "option %d is not valid UTF-8", i+1)
		}
		key := strings.TrimSpace(o.Key)
		if key == "" {
			key = questionKey(o.Label, i)
		}
		if !questionKeyOK(key) {
			return taskConflict("invalid_arg", "option key %q: letters, digits, - and _ only, up to 40", key)
		}
		if seen[key] {
			return taskConflict("invalid_arg", "two options share the key %q", key)
		}
		seen[key] = true
		o.Key = key
	}
	if r := strings.TrimSpace(q.Recommend); r != "" {
		q.Recommend = ""
		for _, o := range q.Options {
			if o.Key == r || strings.EqualFold(o.Label, r) {
				q.Recommend = o.Key
			}
		}
		if q.Recommend == "" {
			return taskConflict("invalid_arg", "recommend names none of the options: %q", r)
		}
	}
	return nil
}

func (q *GroupTaskQuestion) encode() (string, error) {
	if err := NormalizeGroupTaskQuestion(q); err != nil {
		return "", err
	}
	b, err := json.Marshal(q)
	return string(b), err
}

// questionKey derives an option's key from its label: lower case, runs of
// anything else than letters and digits as one "-".
func questionKey(label string, i int) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(label) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if b.Len() > 0 && !dash {
			b.WriteByte('-')
			dash = true
		}
		if b.Len() >= 40 {
			break
		}
	}
	key := strings.Trim(b.String(), "-")
	if key == "" {
		key = "option-" + string(rune('1'+i))
	}
	return key
}

func questionKeyOK(key string) bool {
	if key == "" || len(key) > 40 {
		return false
	}
	for _, r := range key {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// AnswerText is the answer as one line: the picked labels, then the text.
func (q *GroupTaskQuestion) AnswerText() string {
	if q == nil || q.Answer == nil {
		return ""
	}
	var labels []string
	for _, k := range q.Answer.Picked {
		for _, o := range q.Options {
			if o.Key == k {
				labels = append(labels, o.Label)
			}
		}
	}
	parts := []string{}
	if len(labels) > 0 {
		parts = append(parts, "Answer: "+strings.Join(labels, ", "))
	}
	if q.Answer.Text != "" {
		parts = append(parts, q.Answer.Text)
	}
	return strings.Join(parts, "; ")
}

// Asker is the actor that asked the question: AskedBy, or for a question
// stored before AskedBy, the task's creator.
func (t *GroupTask) Asker() string {
	if q := t.Question(); q != nil && q.AskedBy != "" {
		return q.AskedBy
	}
	return t.CreatedBy
}

// AnswerGroupTask answers a live question task: picked are option keys (or
// labels), text the free answer. The answer is stored with the question and
// the task is closed as done with it as the reason, releasing the tasks
// waiting on it. A question is answered once.
func AnswerGroupTask(id string, picked []string, text, actor string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		q := t.Question()
		if q == nil {
			return taskConflict("not_question", "task %s is not a question", id)
		}
		if q.Answer != nil {
			return taskConflict("answered", "task %s is answered already", id)
		}
		if !t.Live() {
			return taskConflict("not_live", "task %s is %s; reopen it to answer it", id, t.Status)
		}
		text = strings.TrimSpace(text)
		if !utf8.ValidString(text) || utf8.RuneCountInString(text) > GroupTaskAnswerTextMax {
			return taskConflict("invalid_arg", "the answer text is at most %d characters of UTF-8", GroupTaskAnswerTextMax)
		}
		keys := []string{}
		seen := map[string]bool{}
		for _, p := range picked {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			key := ""
			for _, o := range q.Options {
				if o.Key == p || strings.EqualFold(o.Label, p) {
					key = o.Key
				}
			}
			if key == "" {
				return taskConflict("invalid_arg", "%q is none of the question's options", p)
			}
			if !seen[key] {
				seen[key] = true
				keys = append(keys, key)
			}
		}
		switch {
		case len(keys) == 0 && text == "":
			if len(q.Options) == 0 {
				return taskConflict("invalid_arg", "answer it with text")
			}
			return taskConflict("invalid_arg", "pick an option, or answer with text")
		case len(keys) > 1 && !q.Multi:
			return taskConflict("invalid_arg", "pick one option")
		}
		q.Answer = &GroupTaskAnswer{Picked: keys, Text: text, AnsweredBy: actor, AnsweredAt: now.UTC().Truncate(time.Second)}
		raw, err := json.Marshal(q)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET operator_request = ? WHERE id = ?`, string(raw), id); err != nil {
			return err
		}
		reason := truncateUTF8(q.AnswerText(), 400)
		if err := addGroupTaskEventTx(tx, id, actor, GroupTaskEventAnswered, reason, now); err != nil {
			return err
		}
		return setGroupTaskStatusTx(tx, t, GroupTaskDone, reason, actor, AnyAssignee, now)
	})
}
