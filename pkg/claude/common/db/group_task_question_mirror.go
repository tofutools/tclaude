package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Mirrored questions (tk_8b2c86f7): a notify-human question or an
// --ask-human prompt is also a question on the board, so everything waiting
// on the operator is in one place. Its Source names the original, which stays
// where it is and is answered there or through it: the board's question is
// answered (AnswerGroupTaskQuestionBySource) or closed
// (CloseGroupTaskQuestionBySource) when the original is.

// Mirrored question sources.
const (
	GroupTaskQuestionSourceNotify   = "notify:"
	GroupTaskQuestionSourceApproval = "approval:"
)

// GetGroupTaskQuestionBySource returns the question mirrored from source and
// its task's id; nil when there is none.
func GetGroupTaskQuestionBySource(source string) (string, *GroupTaskQuestion, error) {
	if source == "" {
		return "", nil, nil
	}
	d, err := Open()
	if err != nil {
		return "", nil, err
	}
	id, q, err := scanGroupTaskQuestion(d.QueryRow(`SELECT `+groupTaskQuestionCols+groupTaskQuestionFrom+`
		WHERE q.source = ?`, source))
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	return id, q, err
}

// CurrentGroupTaskOf is the live task agentID holds in group groupID that it
// started most recently: what it works on now. "" when it holds none started.
func CurrentGroupTaskOf(groupID int64, agentID string) (string, error) {
	d, err := Open()
	if err != nil {
		return "", err
	}
	var id string
	err = d.QueryRow(`SELECT id FROM group_tasks WHERE group_id = ? AND assignee_id = ? AND assignee_kind = ''
		AND status IN ('open','blocked') AND started_at IS NOT NULL ORDER BY started_at DESC, id DESC LIMIT 1`,
		groupID, agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CurrentGroupTaskGroupOf is the group of the live task agentID started most
// recently, in any group; 0 when it holds none started.
func CurrentGroupTaskGroupOf(agentID string) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var id int64
	err = d.QueryRow(`SELECT group_id FROM group_tasks WHERE assignee_id = ? AND assignee_kind = ''
		AND status IN ('open','blocked') AND started_at IS NOT NULL ORDER BY started_at DESC, id DESC LIMIT 1`,
		agentID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// MirrorGroupTaskQuestion asks q, whose Source names the original, as agent
// agentID (q.AskedBy is "agent:<agentID>") on its current task in group
// groupID, or, when it works on none, on a new question task for the
// operator titled as q. Mirroring a source again returns the question it made.
func MirrorGroupTaskQuestion(groupID int64, agentID string, q *GroupTaskQuestion) (string, *GroupTaskQuestion, error) {
	if q.Source == "" {
		return "", nil, taskConflict("invalid_arg", "a mirrored question names its source")
	}
	if id, got, err := GetGroupTaskQuestionBySource(q.Source); err != nil || got != nil {
		return id, got, err
	}
	q.AskedBy = "agent:" + agentID
	q.Answer, q.State, q.Blocks = nil, "", false
	title, err := checkQuestionTitle(truncateUTF8(strings.TrimSpace(q.Title), GroupTaskQuestionTitleMax))
	if err != nil {
		return "", nil, err
	}
	q.Title = title
	q.Body = truncateUTF8(strings.TrimSpace(q.Body), GroupTaskQuestionBodyMax)
	current, err := CurrentGroupTaskOf(groupID, agentID)
	if err != nil {
		return "", nil, err
	}
	var taskID string
	if current != "" {
		_, _, err = AskGroupTaskQuestion(current, q)
		taskID = current
	} else {
		body := q.Body
		var t *GroupTask
		t, err = CreateGroupTask(NewGroupTask{GroupID: groupID, Title: q.Title, Body: body, Actor: q.AskedBy,
			Status: GroupTaskOpen, AssigneeID: OperatorAssignee, AssignedBy: q.AskedBy, Question: q})
		if t != nil {
			taskID = t.ID
		}
	}
	if err != nil {
		// Mirrored at the same time by another path: that one stands.
		if id, got, gerr := GetGroupTaskQuestionBySource(q.Source); gerr == nil && got != nil {
			return id, got, nil
		}
		return "", nil, err
	}
	return taskID, q, nil
}

// AnswerGroupTaskQuestionBySource records the answer the original of a
// mirrored question got, if the question is still open: picked are option
// keys or labels, text the free answer. A question task the answer closes is
// archived with archive (it leaves "done this week"). It returns the task
// and question it answered, nil when there was nothing to answer.
func AnswerGroupTaskQuestionBySource(source string, picked []string, text, actor string, archive bool) (*GroupTask, *GroupTaskQuestion, error) {
	taskID, q, err := GetGroupTaskQuestionBySource(source)
	if err != nil || q == nil || !q.Open() {
		return nil, nil, err
	}
	t, answered, err := AnswerGroupTaskQuestion(taskID, q.N, picked, text, actor)
	if err != nil {
		return nil, nil, err
	}
	if archive && !t.Live() && t.IsQuestionTask() {
		if err := archiveGroupTask(t); err != nil {
			return nil, nil, err
		}
	}
	return t, answered, nil
}

// CloseGroupTaskQuestionBySource closes the open question mirrored from
// source when its original ends unanswered (dismissed, deleted, timed out):
// why is an entry under the question, and the closed question's answer text. A question task left with nothing to
// answer is dropped, and archived with archive.
func CloseGroupTaskQuestionBySource(source, why, actor string, archive bool) error {
	taskID, q, err := GetGroupTaskQuestionBySource(source)
	if err != nil || q == nil || !q.Open() {
		return err
	}
	t, err := withGroupTask(taskID, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		res, err := tx.Exec(`UPDATE group_task_questions SET state = ?, closed_at = ? WHERE task_id = ? AND n = ? AND state = ?`,
			GroupTaskQuestionClosed, dbTime(now), taskID, q.N, GroupTaskQuestionOpen)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		ev, err := tx.Exec(`INSERT INTO group_task_events (task_id, at, actor, kind, detail, body, comment_kind, comment_id)
			VALUES (?, ?, ?, ?, '', ?, ?, ?)`, taskID, dbTime(now), actor, GroupTaskEventComment,
			truncateUTF8(why, GroupTaskCommentMaxBytes), GroupTaskCommentAnswer, q.EventID)
		if err != nil {
			return err
		}
		evID, _ := ev.LastInsertId()
		// The note is kept as the answer's text; the state says it is none.
		note, err := json.Marshal(GroupTaskAnswer{Picked: []string{}, Text: why, AnsweredBy: actor, AnsweredAt: now.UTC().Truncate(time.Second)})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE group_task_questions SET answer = ?, answer_event_id = ? WHERE task_id = ? AND n = ?`,
			string(note), evID, taskID, q.N); err != nil {
			return err
		}
		if !t.IsQuestionTask() {
			return nil
		}
		var open, answered int
		if err := tx.QueryRow(`SELECT COALESCE(SUM(state = 'open'), 0), COALESCE(SUM(state = 'answered'), 0)
			FROM group_task_questions WHERE task_id = ?`, taskID).Scan(&open, &answered); err != nil {
			return err
		}
		if open > 0 {
			return nil
		}
		status := GroupTaskDropped
		if answered > 0 {
			status = GroupTaskDone
		}
		return setGroupTaskStatusTx(tx, t, status, why, actor, AnyAssignee, now)
	})
	if err != nil {
		return err
	}
	if archive && !t.Live() && t.IsQuestionTask() {
		return archiveGroupTask(t)
	}
	return nil
}

// CloseGroupTaskQuestionsBySourcePrefix closes every open question mirrored
// from a source starting with prefix, except those keep holds: at daemon
// start the --ask-human prompts of the last run are gone, unanswered.
func CloseGroupTaskQuestionsBySourcePrefix(prefix, why, actor string, archive bool, keep func(source string) bool) error {
	d, err := Open()
	if err != nil {
		return err
	}
	rows, err := d.Query(`SELECT source FROM group_task_questions WHERE state = ? AND source LIKE ? ESCAPE '\'`,
		GroupTaskQuestionOpen, likeEscape(prefix)+"%")
	if err != nil {
		return err
	}
	var sources []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			_ = rows.Close()
			return err
		}
		sources = append(sources, s)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, s := range sources {
		if keep != nil && keep(s) {
			continue
		}
		if err := CloseGroupTaskQuestionBySource(s, why, actor, archive); err != nil {
			return err
		}
	}
	return nil
}

// archiveGroupTask archives closed task t at once.
func archiveGroupTask(t *GroupTask) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE group_tasks SET archived_at = ? WHERE id = ? AND status IN ('done','dropped') AND archived_at IS NULL`,
		dbTime(time.Now()), t.ID)
	return err
}
