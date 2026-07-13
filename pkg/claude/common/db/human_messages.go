package db

import (
	"database/sql"
	"errors"
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
// FromAgent is the stable agent_id companion of FromConv (JOH-321 F2),
// DERIVED from it at insert via agent_conversations — the pruning-immune
// sender attribution the Messages tab leads with, falling back to the
// FromConv resolution for old rows / a non-actor sender (empty FromAgent).
//
// ReadAt is the zero time while the message is unread, and the time the
// human marked it read otherwise.
type HumanMessage struct {
	ID               int64
	FromConv         string
	FromAgent        string
	FromTitle        string
	GroupName        string
	Subject          string
	Body             string
	CreatedAt        time.Time
	ReadAt           time.Time
	ProcessRunID     string
	ProcessNodeID    string
	ProcessCommandID string
	Attachment       *HumanMessageAttachment
}

// HumanMessageAttachment is one daemon-owned downloadable artifact published
// with a human notification. Multiple input paths are packaged as one zip by
// the CLI, keeping the message surface compact while still delivering a set.
type HumanMessageAttachment struct {
	MessageID   int64
	Filename    string
	ContentType string
	SizeBytes   int64
	StoragePath string
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
	return insertHumanMessage(d, m)
}

type humanMessageExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func insertHumanMessage(exec humanMessageExecer, m *HumanMessage) (int64, error) {
	created := m.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	readAt := ""
	if !m.ReadAt.IsZero() {
		readAt = m.ReadAt.Format(time.RFC3339Nano)
	}
	// Dual-write the stable sender ref (JOH-321 F2): from_agent is DERIVED from
	// from_conv via agent_conversations (agentForConvExpr), the same boundary the
	// v81 backfill used, so existing and freshly-inserted rows agree. A non-actor
	// / empty sender (the human-initiated path) resolves to ''. Any FromAgent
	// preset on the struct is intentionally ignored — from_conv is the source of
	// truth, so the denormalised ref can never drift from it.
	res, err := exec.Exec(`
		INSERT INTO human_messages
			(from_conv, from_agent, from_title, group_name, subject, body, created_at, read_at,
			 process_run_id, process_node_id, process_command_id)
		VALUES (?, `+agentForConvExpr+`, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.FromConv, m.FromConv, m.FromTitle, m.GroupName, m.Subject, m.Body,
		created.Format(time.RFC3339Nano), readAt, m.ProcessRunID, m.ProcessNodeID, m.ProcessCommandID)
	if err != nil {
		return 0, fmt.Errorf("insert human message: %w", err)
	}
	return res.LastInsertId()
}

// InsertHumanMessageWithAttachment commits the message and attachment metadata
// together. The caller must have already written StoragePath and removes it if
// this transaction fails; a committed message can therefore never be visible
// without its attachment row.
func InsertHumanMessageWithAttachment(m *HumanMessage, a *HumanMessageAttachment) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := insertHumanMessage(tx, m)
	if err != nil {
		return 0, err
	}
	a.MessageID = id
	if err := insertHumanMessageAttachment(tx, a); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit human message attachment: %w", err)
	}
	return id, nil
}

// InsertHumanMessageAttachment records the artifact metadata for a message.
// The caller writes the daemon-owned file first and removes it if this fails.
func InsertHumanMessageAttachment(a *HumanMessageAttachment) error {
	d, err := Open()
	if err != nil {
		return err
	}
	return insertHumanMessageAttachment(d, a)
}

func insertHumanMessageAttachment(exec humanMessageExecer, a *HumanMessageAttachment) error {
	_, err := exec.Exec(`INSERT INTO human_message_attachments
		(message_id, filename, content_type, size_bytes, storage_path)
		VALUES (?, ?, ?, ?, ?)`, a.MessageID, a.Filename, a.ContentType, a.SizeBytes, a.StoragePath)
	if err != nil {
		return fmt.Errorf("insert human message attachment: %w", err)
	}
	return nil
}

// ListHumanMessageAttachments returns every persisted artifact reference for
// filesystem reconciliation and quota accounting.
func ListHumanMessageAttachments() ([]*HumanMessageAttachment, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT message_id, filename, content_type, size_bytes, storage_path
		FROM human_message_attachments`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var attachments []*HumanMessageAttachment
	for rows.Next() {
		var a HumanMessageAttachment
		if err := rows.Scan(&a.MessageID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.StoragePath); err != nil {
			return nil, err
		}
		attachments = append(attachments, &a)
	}
	return attachments, rows.Err()
}

// HumanMessageAttachmentUsage reports stored bytes and row counts globally and
// for one stable agent (falling back to a conversation id for legacy/non-agent
// senders). Count quotas prevent zero/tiny files from exhausting DB rows/inodes.
func HumanMessageAttachmentUsage(agentID, convID string) (totalBytes, senderBytes int64, totalCount, senderCount int, err error) {
	d, err := Open()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if err := d.QueryRow(`SELECT COALESCE(SUM(size_bytes), 0), COUNT(*) FROM human_message_attachments`).
		Scan(&totalBytes, &totalCount); err != nil {
		return 0, 0, 0, 0, err
	}
	if agentID != "" {
		err = d.QueryRow(`SELECT COALESCE(SUM(a.size_bytes), 0), COUNT(*)
			FROM human_message_attachments a JOIN human_messages m ON m.id = a.message_id
			WHERE m.from_agent = ?`, agentID).Scan(&senderBytes, &senderCount)
	} else {
		err = d.QueryRow(`SELECT COALESCE(SUM(a.size_bytes), 0), COUNT(*)
			FROM human_message_attachments a JOIN human_messages m ON m.id = a.message_id
			WHERE m.from_conv = ?`, convID).Scan(&senderBytes, &senderCount)
	}
	return totalBytes, senderBytes, totalCount, senderCount, err
}

// DeleteHumanMessageAttachment removes stale metadata while preserving the
// human message itself (which remains readable without a download card).
func DeleteHumanMessageAttachment(messageID int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM human_message_attachments WHERE message_id = ?`, messageID)
	return err
}

// GetHumanMessageAttachment returns a message's attachment, if any.
func GetHumanMessageAttachment(messageID int64) (*HumanMessageAttachment, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var a HumanMessageAttachment
	err = d.QueryRow(`SELECT message_id, filename, content_type, size_bytes, storage_path
		FROM human_message_attachments WHERE message_id = ?`, messageID).
		Scan(&a.MessageID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.StoragePath)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func attachHumanMessageArtifacts(d *sql.DB, messages []*HumanMessage) error {
	if len(messages) == 0 {
		return nil
	}
	byID := make(map[int64]*HumanMessage, len(messages))
	for _, m := range messages {
		byID[m.ID] = m
	}
	rows, err := d.Query(`SELECT message_id, filename, content_type, size_bytes, storage_path
		FROM human_message_attachments`)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var a HumanMessageAttachment
		if err := rows.Scan(&a.MessageID, &a.Filename, &a.ContentType, &a.SizeBytes, &a.StoragePath); err != nil {
			return err
		}
		if m := byID[a.MessageID]; m != nil {
			m.Attachment = &a
		}
	}
	return rows.Err()
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
		SELECT id, from_conv, from_agent, from_title, group_name, subject, body, created_at, read_at,
		       process_run_id, process_node_id, process_command_id
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
		if err := rows.Scan(&m.ID, &m.FromConv, &m.FromAgent, &m.FromTitle, &m.GroupName,
			&m.Subject, &m.Body, &created, &readAt, &m.ProcessRunID, &m.ProcessNodeID, &m.ProcessCommandID); err != nil {
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := attachHumanMessageArtifacts(d, out); err != nil {
		return nil, err
	}
	return out, nil
}

// GetHumanMessage loads one human message by id, or (nil, nil) when no
// row matches — the caller distinguishes "not found" from an error. Used
// by the dashboard reply path to resolve, authoritatively, WHICH agent a
// notification came from (rather than trusting a target the browser
// passes), so a reply can only ever route back to the real sender.
func GetHumanMessage(id int64) (*HumanMessage, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`
		SELECT id, from_conv, from_agent, from_title, group_name, subject, body, created_at, read_at,
		       process_run_id, process_node_id, process_command_id
		FROM human_messages
		WHERE id = ?`, id)
	var m HumanMessage
	var created, readAt string
	switch err := row.Scan(&m.ID, &m.FromConv, &m.FromAgent, &m.FromTitle, &m.GroupName,
		&m.Subject, &m.Body, &created, &readAt, &m.ProcessRunID, &m.ProcessNodeID, &m.ProcessCommandID); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		return nil, err
	}
	if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
		m.CreatedAt = t
	} else {
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
	m.Attachment, err = GetHumanMessageAttachment(m.ID)
	if err != nil {
		return nil, err
	}
	return &m, nil
}

// FindHumanMessageForProcessCommand returns the first matching process
// notification. Deferred human dispatch uses it as an idempotency lookup after
// a restart between notification persistence and process-state append.
func FindHumanMessageForProcessCommand(commandID, subject string) (*HumanMessage, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`
		SELECT id, from_conv, from_agent, from_title, group_name, subject, body, created_at, read_at,
		       process_run_id, process_node_id, process_command_id
		FROM human_messages WHERE process_command_id = ? AND subject = ? ORDER BY id ASC LIMIT 1`, commandID, subject)
	var m HumanMessage
	var created, readAt string
	if err := row.Scan(&m.ID, &m.FromConv, &m.FromAgent, &m.FromTitle, &m.GroupName,
		&m.Subject, &m.Body, &created, &readAt, &m.ProcessRunID, &m.ProcessNodeID, &m.ProcessCommandID); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if readAt != "" {
		m.ReadAt, _ = time.Parse(time.RFC3339Nano, readAt)
	}
	return &m, nil
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

// MarkHumanMessageUnread clears read_at on one message if it is currently
// read — the reader's "mark unread" toggle, the opt-out complement to the
// auto-mark-on-open. Idempotent: re-marking an already-unread message is a
// no-op, and a non-existent id is also a no-op. Returns whether a row was
// actually transitioned.
func MarkHumanMessageUnread(id int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(
		`UPDATE human_messages SET read_at = '' WHERE id = ? AND read_at != ''`, id)
	if err != nil {
		return false, fmt.Errorf("mark human message unread: %w", err)
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

// DeleteReadHumanMessagesWithAttachments atomically captures the exact files
// owned by the rows it deletes. This closes the list/delete race for the
// dashboard clear-read action; filesystem removal happens after commit and an
// agentd reconciler retries any failed removal.
func DeleteReadHumanMessagesWithAttachments() (int, []string, error) {
	return deleteHumanMessagesWithAttachments("read_at != ''", nil)
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

// DeleteHumanMessagesWithAttachments is the attachment-aware dashboard delete.
// It returns paths captured in the same transaction as the message deletion.
func DeleteHumanMessagesWithAttachments(ids []int64) (int, []string, error) {
	if len(ids) == 0 {
		return 0, nil, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	return deleteHumanMessagesWithAttachments("id IN ("+strings.Join(placeholders, ",")+")", args)
}

func deleteHumanMessagesWithAttachments(where string, args []any) (int, []string, error) {
	d, err := Open()
	if err != nil {
		return 0, nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT a.storage_path
		FROM human_message_attachments a
		JOIN human_messages m ON m.id = a.message_id
		WHERE `+where, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("list deleted human message attachments: %w", err)
	}
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			_ = rows.Close()
			return 0, nil, err
		}
		paths = append(paths, path)
	}
	if err := rows.Close(); err != nil {
		return 0, nil, err
	}
	res, err := tx.Exec(`DELETE FROM human_messages WHERE `+where, args...)
	if err != nil {
		return 0, nil, fmt.Errorf("delete human messages with attachments: %w", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("commit delete human messages with attachments: %w", err)
	}
	return int(n), paths, nil
}
