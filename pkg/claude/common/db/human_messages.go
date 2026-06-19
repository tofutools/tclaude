package db

import (
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// HumanMessage is one notification a coordinating agent sent the human
// via `tclaude agent notify-human` — a row of the human_messages table,
// surfaced in the dashboard Messages tab (see migrateV43toV44).
//
// FromTitle and GroupName are snapshots taken at insert time, not live
// lookups: a later rename or deletion of the sending agent must not
// blank an old message. FromConv is kept raw so the tab's focus button
// can raise that conversation's terminal window.
//
// ReadAt is the zero time while the message is unread, and the time the
// human marked it read otherwise.
type HumanMessage struct {
	ID        int64
	FromConv  string
	FromTitle string
	GroupName string
	Subject   string
	Body      string
	CreatedAt time.Time
	ReadAt    time.Time
}

// IsRead reports whether the message has been marked read.
func (m *HumanMessage) IsRead() bool { return !m.ReadAt.IsZero() }

// InsertHumanMessage records one human-facing message and returns its
// new id. CreatedAt defaults to now when the caller leaves it zero.
func InsertHumanMessage(m *HumanMessage) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	created := m.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	readAt := ""
	if !m.ReadAt.IsZero() {
		readAt = m.ReadAt.Format(time.RFC3339Nano)
	}
	res, err := d.Exec(`
		INSERT INTO human_messages
			(from_conv, from_title, group_name, subject, body, created_at, read_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		m.FromConv, m.FromTitle, m.GroupName, m.Subject, m.Body,
		created.Format(time.RFC3339Nano), readAt)
	if err != nil {
		return 0, fmt.Errorf("insert human message: %w", err)
	}
	return res.LastInsertId()
}

// ListHumanMessages returns every human message, newest first.
//
// Ordering is by id DESC (autoincrement = insertion order), NOT created_at.
// created_at is an RFC3339Nano string compared lexically by SQLite: a time on
// a whole second serialises with no fractional part ("…:00Z") and sorts AFTER
// a later same-second value ("…:00.004Z") because '.' < 'Z'. ORDER BY
// created_at could therefore render a newer message below an older one near a
// second boundary. A `, id DESC` tiebreak does NOT fix it — the misordered
// rows have *different* created_at strings, so the id tiebreak never engages.
// This is the same RFC3339Nano flake fixed for the agent inbox/outbox in #411
// (see listAgentMessagesByCol) and the undelivered queue in #242; id is
// monotonic with insertion, giving a correct, total newest-first order
// independent of the timestamp format.
func ListHumanMessages() ([]*HumanMessage, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`
		SELECT id, from_conv, from_title, group_name, subject, body, created_at, read_at
		FROM human_messages
		ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []*HumanMessage
	for rows.Next() {
		var m HumanMessage
		var created, readAt string
		if err := rows.Scan(&m.ID, &m.FromConv, &m.FromTitle, &m.GroupName,
			&m.Subject, &m.Body, &created, &readAt); err != nil {
			return nil, err
		}
		if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
			m.CreatedAt = t
		} else {
			// A corrupt timestamp leaves the field zero rather than
			// failing the whole list — but log it so the corruption is
			// diagnosable instead of silently swallowed.
			slog.Warn("human_messages: unparseable created_at, leaving zero",
				"id", m.ID, "value", created, "error", err)
		}
		if readAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, readAt); err == nil {
				m.ReadAt = t
			} else {
				slog.Warn("human_messages: unparseable read_at, leaving zero",
					"id", m.ID, "value", readAt, "error", err)
			}
		}
		out = append(out, &m)
	}
	return out, rows.Err()
}

// CountUnreadHumanMessages returns how many human messages are unread —
// what the dashboard renders as the Messages tab badge.
func CountUnreadHumanMessages() (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var n int
	if err := d.QueryRow(`SELECT COUNT(*) FROM human_messages WHERE read_at = ''`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MarkHumanMessageRead stamps read_at on one message if it is currently
// unread. Idempotent: re-marking an already-read message is a no-op and
// leaves the original read timestamp intact. A non-existent id is also
// a no-op. Returns whether a row was actually transitioned.
func MarkHumanMessageRead(id int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(
		`UPDATE human_messages SET read_at = ? WHERE id = ? AND read_at = ''`,
		time.Now().Format(time.RFC3339Nano), id)
	if err != nil {
		return false, fmt.Errorf("mark human message read: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkAllHumanMessagesRead stamps read_at on every currently-unread
// message and returns how many were transitioned.
func MarkAllHumanMessagesRead() (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(
		`UPDATE human_messages SET read_at = ? WHERE read_at = ''`,
		time.Now().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("mark all human messages read: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteReadHumanMessages hard-deletes every message that has been
// marked read — the manual "clear read messages" control on the tab.
// Unread messages are left untouched. Returns how many rows were removed.
func DeleteReadHumanMessages() (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`DELETE FROM human_messages WHERE read_at != ''`)
	if err != nil {
		return 0, fmt.Errorf("delete read human messages: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// DeleteHumanMessage hard-deletes a single message by id, regardless of
// its read state — the per-message delete control on the tab, distinct
// from the bulk "clear read" sweep. A non-existent id is a no-op, not an
// error. Returns whether a row was actually removed.
func DeleteHumanMessage(id int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`DELETE FROM human_messages WHERE id = ?`, id)
	if err != nil {
		return false, fmt.Errorf("delete human message: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DeleteHumanMessages hard-deletes the listed messages by id, regardless
// of read state — the multi-select complement to DeleteHumanMessage on
// the Messages tab. Non-existent ids are silently skipped. Returns how
// many rows were actually removed.
func DeleteHumanMessages(ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	res, err := d.Exec(
		`DELETE FROM human_messages WHERE id IN (`+strings.Join(placeholders, ",")+`)`,
		args...)
	if err != nil {
		return 0, fmt.Errorf("delete human messages: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
