package db

import (
	"database/sql"
	"encoding/json"
	"strings"
)

// migrateV326toV327 moves questions for the operator into a task's activity
// (tk_8b2c86f7): any task may carry questions, each an entry of its stream
// with its own row in group_task_questions, and the answer an entry right
// under it. A phase 2 question task (tk_aa049a67) becomes a task whose first
// question is the one it asked: its operator_request keeps only the marker
// {"kind":"question"}, its question moves into a row and a question entry
// dated when the task was created, and its "answered" change entry becomes
// the answer entry.
func migrateV326toV327(d *sql.DB) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS group_task_questions (
			id              INTEGER PRIMARY KEY,
			task_id         TEXT NOT NULL REFERENCES group_tasks(id) ON DELETE CASCADE,
			n               INTEGER NOT NULL,
			event_id        INTEGER NOT NULL,
			title           TEXT NOT NULL,
			spec            TEXT NOT NULL,
			asked_by        TEXT NOT NULL,
			asked_via       TEXT NOT NULL DEFAULT '',
			asked_at        INTEGER NOT NULL,
			state           TEXT NOT NULL DEFAULT 'open',
			blocks          INTEGER NOT NULL DEFAULT 0,
			answer          TEXT NOT NULL DEFAULT '',
			answer_event_id INTEGER,
			closed_at       INTEGER,
			UNIQUE (task_id, n)
		) STRICT`,
		`CREATE INDEX IF NOT EXISTS idx_group_task_questions_open ON group_task_questions(task_id) WHERE state = 'open'`,
		// A task that closes, by whatever path (a status change, a report, a
		// move away), closes its open questions with it.
		`CREATE TRIGGER IF NOT EXISTS group_task_questions_close
			AFTER UPDATE OF status ON group_tasks
			WHEN NEW.status IN ('done','dropped') AND OLD.status IN ('open','blocked')
			BEGIN
				UPDATE group_task_questions SET state = 'closed', closed_at = COALESCE(NEW.closed_at, NEW.updated_at)
					WHERE task_id = NEW.id AND state = 'open';
			END`,
	} {
		if _, err := tx.Exec(stmt); err != nil {
			return err
		}
	}
	if err := migrateV327PhaseTwoQuestions(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE schema_version SET version = 327`); err != nil {
		return err
	}
	return tx.Commit()
}

// v327PhaseTwoQuestion is a phase 2 operator_request.
type v327PhaseTwoQuestion struct {
	Kind      string                    `json:"kind"`
	Multi     bool                      `json:"multi,omitempty"`
	Options   []GroupTaskQuestionOption `json:"options,omitempty"`
	Recommend string                    `json:"recommend,omitempty"`
	AskedBy   string                    `json:"asked_by,omitempty"`
	AskedVia  string                    `json:"asked_via,omitempty"`
	Answer    *GroupTaskAnswer          `json:"answer,omitempty"`
}

func migrateV327PhaseTwoQuestions(tx *sql.Tx) error {
	type row struct {
		id, title, status, createdBy, req string
		created                           int64
		closed                            sql.NullInt64
	}
	rows, err := tx.Query(`SELECT id, title, status, created_by, created_at, closed_at, operator_request
		FROM group_tasks WHERE operator_request <> '' AND id NOT IN (SELECT task_id FROM group_task_questions)`)
	if err != nil {
		return err
	}
	var todo []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.title, &r.status, &r.createdBy, &r.created, &r.closed, &r.req); err != nil {
			_ = rows.Close()
			return err
		}
		todo = append(todo, r)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, r := range todo {
		var q v327PhaseTwoQuestion
		if json.Unmarshal([]byte(r.req), &q) != nil || q.Kind != GroupTaskQuestionKind {
			continue
		}
		askedBy := firstNonEmptyString(q.AskedBy, r.createdBy)
		spec, _ := json.Marshal(groupTaskQuestionSpec{Kind: GroupTaskQuestionKind, Multi: q.Multi, Options: q.Options, Recommend: q.Recommend})
		res, err := tx.Exec(`INSERT INTO group_task_events (task_id, at, actor, kind, detail, body, comment_kind)
			VALUES (?, ?, ?, ?, ?, '', ?)`, r.id, r.created, askedBy, GroupTaskEventComment,
			truncateUTF8(r.title, GroupTaskEventDetailMax), GroupTaskCommentQuestion)
		if err != nil {
			return err
		}
		eventID, _ := res.LastInsertId()
		state := GroupTaskQuestionOpen
		var answer string
		var answerEvent, closedAt any
		switch {
		case q.Answer != nil:
			state = GroupTaskQuestionAnswered
			raw, _ := json.Marshal(q.Answer)
			answer = string(raw)
			closedAt = dbTime(q.Answer.AnsweredAt)
			// The "answered" change entry becomes the answer entry.
			var id int64
			err := tx.QueryRow(`SELECT id FROM group_task_events WHERE task_id = ? AND kind = ? ORDER BY id DESC LIMIT 1`,
				r.id, groupTaskEventAnsweredV2).Scan(&id)
			switch {
			case err == nil:
				if _, err := tx.Exec(`UPDATE group_task_events SET kind = ?, comment_kind = ?, body = detail, detail = '', comment_id = ?
					WHERE id = ?`, GroupTaskEventComment, GroupTaskCommentAnswer, eventID, id); err != nil {
					return err
				}
				answerEvent = id
			case err != sql.ErrNoRows:
				return err
			}
		case r.status == GroupTaskDone || r.status == GroupTaskDropped:
			state = GroupTaskQuestionClosed
			if r.closed.Valid {
				closedAt = r.closed.Int64
			}
		}
		if _, err := tx.Exec(`INSERT INTO group_task_questions (task_id, n, event_id, title, spec, asked_by, asked_via,
				asked_at, state, answer, answer_event_id, closed_at)
			VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, r.id, eventID, strings.TrimSpace(r.title), string(spec), askedBy,
			q.AskedVia, r.created, state, answer, answerEvent, closedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET operator_request = ? WHERE id = ?`, groupTaskQuestionMarker, r.id); err != nil {
			return err
		}
	}
	return nil
}
