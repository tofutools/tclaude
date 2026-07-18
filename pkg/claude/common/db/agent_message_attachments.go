package db

import "fmt"

// AgentMessageAttachment is daemon-owned metadata for a file accompanying an
// inbox message. StoragePath is an absolute, agent-readable path.
type AgentMessageAttachment struct {
	ID          int64  `json:"-"`
	MessageID   int64  `json:"-"`
	Ordinal     int    `json:"-"`
	Filename    string `json:"name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size"`
	StoragePath string `json:"path"`
}

// InsertAgentMessageWithAttachments atomically persists the inbox row and all
// attachment metadata. Callers own cleanup of copied bytes if this fails.
func InsertAgentMessageWithAttachments(m *AgentMessage, attachments []AgentMessageAttachment) (int64, error) {
	id, _, err := insertAgentMessageWithAttachmentsBounded(m, attachments, 0)
	return id, err
}

// InsertAgentMessageWithAttachmentsBounded is the attachment-aware twin of
// InsertAgentMessageBounded. The queue check, message row, operator marker,
// and attachment rows commit as one transaction.
func InsertAgentMessageWithAttachmentsBounded(m *AgentMessage, attachments []AgentMessageAttachment, limit int) (id int64, pending int, err error) {
	if limit <= 0 {
		return 0, 0, fmt.Errorf("agent message queue limit must be positive")
	}
	m.RegularSend = true
	return insertAgentMessageWithAttachmentsBounded(m, attachments, limit)
}

func insertAgentMessageWithAttachmentsBounded(m *AgentMessage, attachments []AgentMessageAttachment, limit int) (id int64, pending int, err error) {
	d, err := Open()
	if err != nil {
		return 0, 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if limit > 0 {
		pending, err = countUnprocessedRegularMessageBacklog(tx, m)
		if err != nil {
			return 0, 0, err
		}
		if pending >= limit {
			return 0, pending, &AgentMessageQueueFullError{Pending: pending, Limit: limit}
		}
	}
	id, err = insertAgentMessage(tx, m)
	if err != nil {
		return 0, 0, err
	}
	if m.OperatorAuthored {
		if _, err := tx.Exec(`INSERT INTO operator_agent_messages (message_id) VALUES (?)`, id); err != nil {
			return 0, 0, err
		}
	}
	for i := range attachments {
		a := &attachments[i]
		a.MessageID = id
		a.Ordinal = i
		if a.ContentType == "" {
			a.ContentType = "application/octet-stream"
		}
		res, err := tx.Exec(`INSERT INTO agent_message_attachments
			(message_id, ordinal, filename, content_type, size_bytes, storage_path)
			VALUES (?, ?, ?, ?, ?, ?)`, id, i, a.Filename, a.ContentType, a.SizeBytes, a.StoragePath)
		if err != nil {
			return 0, 0, err
		}
		a.ID, _ = res.LastInsertId()
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	if limit > 0 {
		pending++
	}
	return id, pending, nil
}

func countUnprocessedRegularMessageBacklog(q dbExecQuerier, m *AgentMessage) (int, error) {
	if m.ToConv == "" {
		return 0, fmt.Errorf("message target is required")
	}
	if !m.PinGen {
		agentID, err := agentIDForConvTx(q, m.ToConv)
		if err != nil {
			return 0, err
		}
		if agentID != "" {
			var n int
			err = q.QueryRow(`SELECT COUNT(*) FROM agent_messages
				WHERE regular_send = 1 AND processed_at = '' AND (
					(to_agent = ? AND pin_gen = 0) OR
					(to_agent = '' AND to_conv IN (
						SELECT conv_id FROM agent_conversations WHERE agent_id = ?)))`,
				agentID, agentID).Scan(&n)
			return n, err
		}
	}
	var n int
	err := q.QueryRow(`SELECT COUNT(*) FROM agent_messages
		WHERE to_conv = ? AND (pin_gen = 1 OR to_agent = '') AND regular_send = 1 AND processed_at = ''`,
		m.ToConv).Scan(&n)
	return n, err
}

func IsOperatorAgentMessage(messageID int64) bool {
	d, err := Open()
	if err != nil {
		return false
	}
	var n int
	return d.QueryRow(`SELECT COUNT(*) FROM operator_agent_messages WHERE message_id = ?`, messageID).Scan(&n) == nil && n == 1
}

// OperatorAuthoredMessages reports which of ids are operator-authored, as
// a set (absent id == not operator-authored). The mailbox read path
// decorates a whole page at once and runs on every 2s dashboard refresh,
// so this batches what IsOperatorAgentMessage answers one row at a time —
// a 50-row page would otherwise cost 50 round trips. An empty ids slice
// short-circuits rather than building a zero-placeholder IN ().
func OperatorAuthoredMessages(ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := d.Query(`SELECT message_id FROM operator_agent_messages
		WHERE message_id IN (`+sqlPlaceholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

func ListAgentMessageAttachments(messageID int64) ([]AgentMessageAttachment, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, message_id, ordinal, filename, content_type, size_bytes, storage_path
		FROM agent_message_attachments WHERE message_id = ? ORDER BY ordinal`, messageID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AgentMessageAttachment{}
	for rows.Next() {
		var a AgentMessageAttachment
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Ordinal, &a.Filename, &a.ContentType, &a.SizeBytes, &a.StoragePath); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func ListAllAgentMessageAttachments() ([]AgentMessageAttachment, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, message_id, ordinal, filename, content_type, size_bytes, storage_path FROM agent_message_attachments`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []AgentMessageAttachment{}
	for rows.Next() {
		var a AgentMessageAttachment
		if err := rows.Scan(&a.ID, &a.MessageID, &a.Ordinal, &a.Filename, &a.ContentType, &a.SizeBytes, &a.StoragePath); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func DeleteAgentMessageAttachment(id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM agent_message_attachments WHERE id = ?`, id)
	return err
}
