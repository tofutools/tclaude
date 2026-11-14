package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Questions for the operator (tk_aa049a67, tk_8b2c86f7): any task may carry
// questions, each an entry of its activity stream (a comment of kind
// "question") with its own row in group_task_questions, numbered per task
// (Q1, Q2…). A question is modelled on AskUserQuestion: its options (none,
// or 2 to 6) are picked one or, with Multi, several; free text is always
// allowed and required when there are no options. The answer is an entry of
// its own (kind "answer", tied to the question's entry), the question's row
// records it, and the asking agent gets it by mail and on the question's
// answer route.
//
// A question task (`taskboard ask-operator <group>`) is a task assigned to
// the operator whose first question is the one it asks; its
// operator_request is the marker {"kind":"question"}. Answering its last
// open question closes it as done.

// GroupTaskQuestionKind is the one operator_request kind so far.
const GroupTaskQuestionKind = "question"

// groupTaskQuestionMarker is a question task's operator_request.
const groupTaskQuestionMarker = `{"kind":"question"}`

// Bounds of a question.
const (
	GroupTaskQuestionOptionsMax = 6
	GroupTaskQuestionLabelMax   = 80
	GroupTaskQuestionDescMax    = 300
	GroupTaskQuestionPreviewMax = 2 << 10
	GroupTaskQuestionTitleMax   = 200
	GroupTaskQuestionBodyMax    = 8 << 10
	GroupTaskAnswerTextMax      = 4000 // characters
)

// What a comment is, for questions: the question and its answer.
const (
	GroupTaskCommentQuestion = "question"
	GroupTaskCommentAnswer   = "answer"
)

// groupTaskEventAnsweredV2 is the change entry a phase 2 answer wrote; the
// v327 migration turns it into an answer entry.
const groupTaskEventAnsweredV2 = "answered"

// A question's state.
const (
	GroupTaskQuestionOpen      = "open"
	GroupTaskQuestionAnswered  = "answered"
	GroupTaskQuestionWithdrawn = "withdrawn" // its asker deleted it
	GroupTaskQuestionClosed    = "closed"    // its task closed first
)

// groupTaskQuestionSpec is what a question offers, as stored in its spec.
type groupTaskQuestionSpec struct {
	Kind      string                    `json:"kind"`
	Multi     bool                      `json:"multi,omitempty"`
	Options   []GroupTaskQuestionOption `json:"options,omitempty"`
	Recommend string                    `json:"recommend,omitempty"`
	// Attachments are the ids of the task's own files the question shows
	// (tk_72dff9f8).
	Attachments []int64 `json:"attachments,omitempty"`
}

// GroupTaskQuestion is one question on a task: what it asks, who asked, and
// its state and answer.
type GroupTaskQuestion struct {
	Kind string `json:"kind"`
	// N numbers the question on its task, from 1.
	N int `json:"n,omitempty"`
	// Title is the question, one line; Body its context (Markdown), the
	// text of its activity entry.
	Title     string                    `json:"title,omitempty"`
	Body      string                    `json:"body,omitempty"`
	Multi     bool                      `json:"multi,omitempty"`
	Options   []GroupTaskQuestionOption `json:"options,omitempty"`
	Recommend string                    `json:"recommend,omitempty"`
	// AskedBy is the actor that asked ("agent:<id>", "peer: <author>"),
	// fixed when it is asked: the answer goes back to it whoever holds the
	// task by then.
	AskedBy string `json:"asked_by,omitempty"`
	// AskedVia is a linked node's asking agent by stable ids
	// ("peer:<instance>/agent:<id>"), set by the daemon when a peer's agent
	// asked: the answer goes back to it as federation mail.
	AskedVia string    `json:"asked_via,omitempty"`
	AskedAt  time.Time `json:"asked_at,omitzero"`
	State    string    `json:"state,omitempty"`
	// Blocks says asking blocked the task until the question is answered
	// or withdrawn.
	Blocks        bool             `json:"blocks,omitempty"`
	Answer        *GroupTaskAnswer `json:"answer,omitempty"`
	EventID       int64            `json:"event_id,omitempty"`
	AnswerEventID int64            `json:"answer_event_id,omitempty"`
	ClosedAt      time.Time        `json:"closed_at,omitzero"`
	// AttachmentIDs are the task's own files the question shows the
	// operator, in order: files sent with it (`ask-operator --attach`) and
	// files already on the task it names (--attach-existing). A file
	// detached later drops out of the question's view.
	AttachmentIDs []int64 `json:"attachment_ids,omitempty"`
	// Source names where a mirrored question is answered too
	// ("notify:<message id>", "approval:<request id>"); empty for one asked
	// on the board. A mirrored question is answered through its source and
	// closes with it.
	Source string `json:"source,omitempty"`
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

// Open reports whether the question still waits on its answer.
func (q *GroupTaskQuestion) Open() bool { return q != nil && q.State == GroupTaskQuestionOpen }

// IsQuestionTask reports whether the task was made to ask a question
// (`ask-operator <group>`): answering its last open question closes it.
func (t *GroupTask) IsQuestionTask() bool {
	if t.OperatorRequest == "" {
		return false
	}
	var m struct {
		Kind string `json:"kind"`
	}
	return json.Unmarshal([]byte(t.OperatorRequest), &m) == nil && m.Kind == GroupTaskQuestionKind
}

// NormalizeGroupTaskQuestion checks a new question and fills in what may be
// left out: the kind, and each option's key (from its label). Recommend may
// name an option by key or label. The title and body are checked when it is
// asked (askGroupTaskQuestionTx), since a question task's title is the
// task's.
func NormalizeGroupTaskQuestion(q *GroupTaskQuestion) error {
	if q.Kind == "" {
		q.Kind = GroupTaskQuestionKind
	}
	if q.Kind != GroupTaskQuestionKind {
		return taskConflict("invalid_arg", "a question's kind is %q", GroupTaskQuestionKind)
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

// blockReason is the reason a task asked on with Blocks is blocked for.
func (q *GroupTaskQuestion) blockReason() string {
	return truncateUTF8(fmt.Sprintf("waiting on operator: Q%d %s", q.N, q.Title), 400)
}

// checkQuestionTitle: one line of valid UTF-8 with no control characters,
// 1 to GroupTaskQuestionTitleMax bytes.
func checkQuestionTitle(s string) (string, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return "", taskConflict("invalid_arg", "a question needs its question (title)")
	case len(s) > GroupTaskQuestionTitleMax:
		return "", taskConflict("invalid_arg", "a question's title is at most %d bytes", GroupTaskQuestionTitleMax)
	case !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl):
		return "", taskConflict("invalid_arg", "a question's title is one line of text")
	}
	return s, nil
}

// askGroupTaskQuestionTx records question q on task t: its activity entry
// and its row, numbered after the task's last question. q.AskedBy is the
// actor.
func askGroupTaskQuestionTx(tx *sql.Tx, t *GroupTask, q *GroupTaskQuestion, now time.Time) error {
	if err := NormalizeGroupTaskQuestion(q); err != nil {
		return err
	}
	title, err := checkQuestionTitle(q.Title)
	if err != nil {
		return err
	}
	q.Title = title
	q.Body = strings.TrimSpace(q.Body)
	if len(q.Body) > GroupTaskQuestionBodyMax || !utf8.ValidString(q.Body) {
		return taskConflict("invalid_arg", "a question's context is at most %d bytes of UTF-8", GroupTaskQuestionBodyMax)
	}
	if q.AskedBy == "" {
		return taskConflict("invalid_arg", "a question needs its asker")
	}
	if err := tx.QueryRow(`SELECT COALESCE(MAX(n), 0) + 1 FROM group_task_questions WHERE task_id = ?`, t.ID).Scan(&q.N); err != nil {
		return err
	}
	res, err := tx.Exec(`INSERT INTO group_task_events (task_id, at, actor, kind, detail, body, comment_kind)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, t.ID, dbTime(now), q.AskedBy, GroupTaskEventComment,
		truncateUTF8(q.Title, GroupTaskEventDetailMax), q.Body, GroupTaskCommentQuestion)
	if err != nil {
		return err
	}
	q.EventID, _ = res.LastInsertId()
	spec, err := q.specJSON()
	if err != nil {
		return err
	}
	q.State, q.AskedAt = GroupTaskQuestionOpen, now.UTC().Truncate(time.Second)
	_, err = tx.Exec(`INSERT INTO group_task_questions (task_id, n, event_id, title, spec, asked_by, asked_via, asked_at, state, blocks, source)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, t.ID, q.N, q.EventID, q.Title, string(spec), q.AskedBy, q.AskedVia,
		dbTime(now), q.State, q.Blocks, q.Source)
	return err
}

func (q *GroupTaskQuestion) specJSON() (string, error) {
	spec, err := json.Marshal(groupTaskQuestionSpec{Kind: GroupTaskQuestionKind, Multi: q.Multi, Options: q.Options,
		Recommend: q.Recommend, Attachments: q.AttachmentIDs})
	return string(spec), err
}

// GroupTaskQuestionAttachmentsMax caps the files one question shows.
const GroupTaskQuestionAttachmentsMax = GroupTaskAttachmentsCap

// AskGroupTaskQuestion asks the operator question q on live task id, as
// q.AskedBy. With q.Blocks the task, which must be open, is blocked until
// the question is answered or withdrawn.
func AskGroupTaskQuestion(id string, q *GroupTaskQuestion) (*GroupTask, *GroupTaskQuestion, error) {
	return AskGroupTaskQuestionWithFiles(id, q, nil, nil)
}

// AskGroupTaskQuestionWithFiles is AskGroupTaskQuestion that, in the same
// transaction, attaches files to the task (as `taskboard attach` does) and
// links them to the question, after the task's files that existing names
// ("#13", "13" or a file name).
func AskGroupTaskQuestionWithFiles(id string, q *GroupTaskQuestion, files []GroupTaskAttachment, existing []string) (*GroupTask, *GroupTaskQuestion, error) {
	out, err := withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if !t.Live() {
			return taskConflict("not_live", "task %s is %s; reopen it to ask on it", id, t.Status)
		}
		if q.Blocks && t.Status != GroupTaskOpen {
			return taskConflict("blocked", "task %s is blocked already; ask without blocking it", id)
		}
		ids, err := resolveTaskAttachmentNamesTx(tx, id, existing)
		if err != nil {
			return err
		}
		rows := append([]GroupTaskAttachment(nil), files...)
		if err := insertGroupTaskAttachmentsTx(tx, id, 0, rows, q.AskedBy, now); err != nil {
			return err
		}
		for _, a := range rows {
			ids = append(ids, a.ID)
		}
		if q.AttachmentIDs, err = linkQuestionAttachments(nil, ids); err != nil {
			return err
		}
		if err := askGroupTaskQuestionTx(tx, t, q, now); err != nil {
			return err
		}
		if q.Blocks {
			return setGroupTaskStatusTx(tx, t, GroupTaskBlocked, q.blockReason(), q.AskedBy, AnyAssignee, now)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return out, q, nil
}

const groupTaskQuestionCols = `q.task_id, q.n, q.event_id, q.title, COALESCE(e.body, ''), q.spec, q.asked_by, q.asked_via,
	q.asked_at, q.state, q.blocks, q.answer, COALESCE(q.answer_event_id, 0), q.closed_at, q.source`

const groupTaskQuestionFrom = ` FROM group_task_questions q LEFT JOIN group_task_events e ON e.id = q.event_id`

func scanGroupTaskQuestion(s rowScanner) (string, *GroupTaskQuestion, error) {
	var q GroupTaskQuestion
	var taskID, spec, answer string
	var asked, closed dbTimestamp
	if err := s.Scan(&taskID, &q.N, &q.EventID, &q.Title, &q.Body, &spec, &q.AskedBy, &q.AskedVia, &asked, &q.State,
		&q.Blocks, &answer, &q.AnswerEventID, &closed, &q.Source); err != nil {
		return "", nil, err
	}
	var sp groupTaskQuestionSpec
	_ = json.Unmarshal([]byte(spec), &sp)
	q.Kind, q.Multi, q.Options, q.Recommend, q.AttachmentIDs = GroupTaskQuestionKind, sp.Multi, sp.Options, sp.Recommend, sp.Attachments
	q.AskedAt, q.ClosedAt = asked.Time(), closed.Time()
	if answer != "" {
		var a GroupTaskAnswer
		if json.Unmarshal([]byte(answer), &a) == nil {
			q.Answer = &a
		}
	}
	return taskID, &q, nil
}

// resolveTaskAttachmentNamesTx reads names of task id's own files ("#13",
// "13" or a file name, case-insensitive) as their ids.
func resolveTaskAttachmentNamesTx(tx taskTx, id string, names []string) ([]int64, error) {
	if len(names) == 0 {
		return nil, nil
	}
	files, err := listGroupTaskAttachmentsTx(tx, id)
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(names))
	for _, raw := range names {
		name := strings.TrimSpace(raw)
		var found []int64
		if n, err := strconv.ParseInt(strings.TrimPrefix(name, "#"), 10, 64); err == nil {
			for _, f := range files {
				if f.ID == n {
					found = append(found, f.ID)
				}
			}
		}
		if len(found) == 0 {
			for _, f := range files {
				if strings.EqualFold(f.Name, name) {
					found = append(found, f.ID)
				}
			}
		}
		if len(found) != 1 {
			return nil, taskConflict("no_attachment", "task %s has no file %q of its own (see `taskboard attachments %s`)", id, truncateUTF8(name, 80), id)
		}
		out = append(out, found[0])
	}
	return out, nil
}

// linkQuestionAttachments appends ids to have, each once, under the cap.
func linkQuestionAttachments(have, ids []int64) ([]int64, error) {
	out := append([]int64(nil), have...)
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	if len(out) > GroupTaskQuestionAttachmentsMax {
		return nil, taskConflict("attachment_cap", "a question shows at most %d files", GroupTaskQuestionAttachmentsMax)
	}
	return out, nil
}

// linkQuestionTaskFilesTx links a new question task's files to its first
// question, so the answer dialog shows them as the question's.
func linkQuestionTaskFilesTx(tx *sql.Tx, taskID string, q *GroupTaskQuestion, files []GroupTaskAttachment) error {
	if len(files) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(files))
	for _, f := range files {
		ids = append(ids, f.ID)
	}
	var err error
	if q.AttachmentIDs, err = linkQuestionAttachments(q.AttachmentIDs, ids); err != nil {
		return err
	}
	spec, err := q.specJSON()
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE group_task_questions SET spec = ? WHERE task_id = ? AND n = ?`, spec, taskID, q.N)
	return err
}

func getGroupTaskQuestionTx(q taskTx, taskID string, n int) (*GroupTaskQuestion, error) {
	_, out, err := scanGroupTaskQuestion(q.QueryRow(`SELECT `+groupTaskQuestionCols+groupTaskQuestionFrom+`
		WHERE q.task_id = ? AND q.n = ?`, taskID, n))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, taskConflict("no_question", "task %s has no question Q%d", taskID, n)
	}
	return out, err
}

// GetGroupTaskQuestion returns question n of a task.
func GetGroupTaskQuestion(taskID string, n int) (*GroupTaskQuestion, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return getGroupTaskQuestionTx(d, taskID, n)
}

// ListGroupTaskQuestions maps each of the given tasks to its questions, in
// order; a task with none is absent.
func ListGroupTaskQuestions(ids []string) (map[string][]*GroupTaskQuestion, error) {
	out := map[string][]*GroupTaskQuestion{}
	if len(ids) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	for start := 0; start < len(ids); start += 500 {
		chunk := ids[start:min(start+500, len(ids))]
		args := make([]any, 0, len(chunk))
		for _, id := range chunk {
			args = append(args, id)
		}
		rows, err := d.Query(`SELECT `+groupTaskQuestionCols+groupTaskQuestionFrom+`
			WHERE q.task_id IN (?`+strings.Repeat(",?", len(chunk)-1)+`) ORDER BY q.task_id, q.n`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			id, q, err := scanGroupTaskQuestion(rows)
			if err != nil {
				_ = rows.Close()
				return nil, err
			}
			out[id] = append(out[id], q)
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// openQuestionTx picks the question to answer: n, or with n 0 the task's
// only open question.
func openQuestionTx(tx *sql.Tx, taskID string, n int) (*GroupTaskQuestion, error) {
	if n > 0 {
		return getGroupTaskQuestionTx(tx, taskID, n)
	}
	var ns []int
	rows, err := tx.Query(`SELECT n FROM group_task_questions WHERE task_id = ? AND state = ? ORDER BY n`, taskID, GroupTaskQuestionOpen)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var k int
		if err := rows.Scan(&k); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ns = append(ns, k)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch len(ns) {
	case 0:
		var any int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM group_task_questions WHERE task_id = ?`, taskID).Scan(&any); err != nil {
			return nil, err
		}
		if any == 0 {
			return nil, taskConflict("not_question", "task %s has no question", taskID)
		}
		return nil, taskConflict("answered", "task %s has no open question", taskID)
	case 1:
		return getGroupTaskQuestionTx(tx, taskID, ns[0])
	}
	return nil, taskConflict("which_question", "task %s has %d open questions; name one (Q%d…)", taskID, len(ns), ns[0])
}

// endQuestionBlockTx unblocks a task that asking q blocked, if it is still
// blocked for it.
func endQuestionBlockTx(tx *sql.Tx, t *GroupTask, q *GroupTaskQuestion, why, actor string, now time.Time) error {
	if !q.Blocks || t.Status != GroupTaskBlocked || t.BlockReason != q.blockReason() {
		return nil
	}
	return setGroupTaskStatusTx(tx, t, GroupTaskOpen, why, actor, AnyAssignee, now)
}

// AnswerGroupTaskQuestion answers question n of live task id (n 0: its
// only open question): picked are option keys (or labels), text the free
// answer. The answer is an entry under the question, the question's row
// records it, a task blocked on it is unblocked, and a question task with
// no open question left is closed as done with it as the reason. A
// question is answered once. (A task that closes closes its open
// questions: the group_task_questions_close trigger.)
func AnswerGroupTaskQuestion(id string, n int, picked []string, text, actor string) (*GroupTask, *GroupTaskQuestion, error) {
	var answered *GroupTaskQuestion
	out, err := withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		q, err := openQuestionTx(tx, id, n)
		if err != nil {
			return err
		}
		switch q.State {
		case GroupTaskQuestionOpen:
		case GroupTaskQuestionAnswered:
			return taskConflict("answered", "Q%d of task %s is answered already", q.N, id)
		default:
			return taskConflict("not_live", "Q%d of task %s is %s", q.N, id, q.State)
		}
		if !t.Live() {
			return taskConflict("not_live", "task %s is %s; reopen it to answer it", id, t.Status)
		}
		a, err := checkGroupTaskAnswer(q, picked, text)
		if err != nil {
			return err
		}
		a.AnsweredBy, a.AnsweredAt = actor, now.UTC().Truncate(time.Second)
		q.Answer, q.State, q.ClosedAt = a, GroupTaskQuestionAnswered, a.AnsweredAt
		raw, err := json.Marshal(a)
		if err != nil {
			return err
		}
		reason := truncateUTF8(q.AnswerText(), 400)
		if q.Source != "" {
			// A mirrored answer is its original's to show (a linked node
			// reads the board's history, not the human inbox).
			reason = fmt.Sprintf("Q%d answered", q.N)
		}
		res, err := tx.Exec(`INSERT INTO group_task_events (task_id, at, actor, kind, detail, body, comment_kind, comment_id)
			VALUES (?, ?, ?, ?, '', ?, ?, ?)`, id, dbTime(now), actor, GroupTaskEventComment,
			truncateUTF8(q.AnswerText(), GroupTaskCommentMaxBytes), GroupTaskCommentAnswer, q.EventID)
		if err != nil {
			return err
		}
		q.AnswerEventID, _ = res.LastInsertId()
		if _, err := tx.Exec(`UPDATE group_task_questions SET state = ?, answer = ?, answer_event_id = ?, closed_at = ?
			WHERE task_id = ? AND n = ?`, q.State, string(raw), q.AnswerEventID, dbTime(now), id, q.N); err != nil {
			return err
		}
		answered = q
		if err := endQuestionBlockTx(tx, t, q, fmt.Sprintf("Q%d answered", q.N), actor, now); err != nil {
			return err
		}
		if !t.IsQuestionTask() {
			return nil
		}
		var open int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM group_task_questions WHERE task_id = ? AND state = ?`, id, GroupTaskQuestionOpen).Scan(&open); err != nil {
			return err
		}
		if open > 0 {
			return nil
		}
		cur, err := getGroupTaskTx(tx, id)
		if err != nil {
			return err
		}
		return setGroupTaskStatusTx(tx, cur, GroupTaskDone, reason, actor, AnyAssignee, now)
	})
	if err != nil {
		return nil, nil, err
	}
	return out, answered, nil
}

// checkGroupTaskAnswer checks an answer to q: picked names its options (by
// key or label), text is free.
func checkGroupTaskAnswer(q *GroupTaskQuestion, picked []string, text string) (*GroupTaskAnswer, error) {
	text = strings.TrimSpace(text)
	if !utf8.ValidString(text) || utf8.RuneCountInString(text) > GroupTaskAnswerTextMax {
		return nil, taskConflict("invalid_arg", "the answer text is at most %d characters of UTF-8", GroupTaskAnswerTextMax)
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
			return nil, taskConflict("invalid_arg", "%q is none of the question's options", p)
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	switch {
	case len(keys) == 0 && text == "":
		if len(q.Options) == 0 {
			return nil, taskConflict("invalid_arg", "answer it with text")
		}
		return nil, taskConflict("invalid_arg", "pick an option, or answer with text")
	case len(keys) > 1 && !q.Multi:
		return nil, taskConflict("invalid_arg", "pick one option")
	}
	return &GroupTaskAnswer{Picked: keys, Text: text}, nil
}

// withdrawGroupTaskQuestionTx withdraws the open question whose entry is
// eventID: its asker deleted it. A task blocked on it is unblocked.
func withdrawGroupTaskQuestionTx(tx *sql.Tx, t *GroupTask, eventID int64, actor string, now time.Time) error {
	var n int
	if err := tx.QueryRow(`SELECT n FROM group_task_questions WHERE task_id = ? AND event_id = ?`, t.ID, eventID).Scan(&n); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	q, err := getGroupTaskQuestionTx(tx, t.ID, n)
	if err != nil {
		return err
	}
	if q.Source != "" {
		return taskConflict("mirrored", "Q%d is answered where it was asked (%s); it closes with it", q.N, q.Source)
	}
	switch q.State {
	case GroupTaskQuestionOpen:
	case GroupTaskQuestionAnswered:
		return taskConflict("answered", "Q%d is answered; its question stays in the activity", q.N)
	default:
		return nil
	}
	if _, err := tx.Exec(`UPDATE group_task_questions SET state = ?, closed_at = ? WHERE task_id = ? AND n = ?`,
		GroupTaskQuestionWithdrawn, dbTime(now), t.ID, n); err != nil {
		return err
	}
	why := fmt.Sprintf("Q%d withdrawn", q.N)
	if err := endQuestionBlockTx(tx, t, q, why, actor, now); err != nil {
		return err
	}
	// A question task left with nothing to answer, and no answer, is
	// dropped: withdrawing its question withdraws it.
	if !t.IsQuestionTask() {
		return nil
	}
	var open, answered int
	if err := tx.QueryRow(`SELECT COALESCE(SUM(state = 'open'), 0), COALESCE(SUM(state = 'answered'), 0)
		FROM group_task_questions WHERE task_id = ?`, t.ID).Scan(&open, &answered); err != nil {
		return err
	}
	if open > 0 || answered > 0 {
		return nil
	}
	cur, err := getGroupTaskTx(tx, t.ID)
	if err != nil {
		return err
	}
	return setGroupTaskStatusTx(tx, cur, GroupTaskDropped, why, actor, AnyAssignee, now)
}

// reopenGroupTaskQuestionsTx opens again the questions of a reopened task
// that closed with it (the group_task_questions_close trigger).
func reopenGroupTaskQuestionsTx(tx *sql.Tx, taskID string) error {
	// A mirrored question closed with its source, or with the task while its
	// source went on alone: it stays closed.
	_, err := tx.Exec(`UPDATE group_task_questions SET state = ?, closed_at = NULL WHERE task_id = ? AND state = ? AND source = ''`,
		GroupTaskQuestionOpen, taskID, GroupTaskQuestionClosed)
	return err
}

// questionTaskAnswered reports whether question task t has an answered
// question: such a task is not reopened (ask again instead).
func questionTaskAnsweredTx(tx taskTx, t *GroupTask) (bool, error) {
	if !t.IsQuestionTask() {
		return false, nil
	}
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM group_task_questions WHERE task_id = ? AND state = ?`, t.ID, GroupTaskQuestionAnswered).Scan(&n)
	return n > 0, err
}

// CountOpenGroupTaskQuestions counts, per group, the open questions on live
// tasks that are not already waiting on the operator themselves (an open
// task the operator holds, outside its ideas), so nothing is counted twice.
func CountOpenGroupTaskQuestions() (map[int64]int, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT t.group_id, COUNT(*) FROM group_task_questions q JOIN group_tasks t ON t.id = q.task_id
		WHERE q.state = 'open' AND t.status IN ('open','blocked')
			AND NOT (t.status = 'open' AND t.assignee_kind = 'operator' AND NOT (t.stage IN ('idea','backlog') AND t.started_at IS NULL))
		GROUP BY t.group_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]int{}
	for rows.Next() {
		var gid int64
		var n int
		if err := rows.Scan(&gid, &n); err != nil {
			return nil, err
		}
		out[gid] = n
	}
	return out, rows.Err()
}
