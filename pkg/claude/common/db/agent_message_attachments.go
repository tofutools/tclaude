package db

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
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	id, err := insertAgentMessage(tx, m)
	if err != nil {
		return 0, err
	}
	if m.OperatorAuthored {
		if _, err := tx.Exec(`INSERT INTO operator_agent_messages (message_id) VALUES (?)`, id); err != nil {
			return 0, err
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
			return 0, err
		}
		a.ID, _ = res.LastInsertId()
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func IsOperatorAgentMessage(messageID int64) bool {
	d, err := Open()
	if err != nil {
		return false
	}
	var n int
	return d.QueryRow(`SELECT COUNT(*) FROM operator_agent_messages WHERE message_id = ?`, messageID).Scan(&n) == nil && n == 1
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
