package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// InsertLatestCronAgentMessage atomically replaces any older buffered tick
// from cronJobID for this recipient, then appends m to the inbox. A row already
// claimed by a delivery worker is left alone because its pane injection may be
// in flight; delivered/read rows remain as message history.
func InsertLatestCronAgentMessage(m *AgentMessage, cronJobID int64) (messageID int64, replaced int64, err error) {
	if m == nil || cronJobID <= 0 || m.ToConv == "" {
		return 0, 0, fmt.Errorf("invalid cron agent message")
	}
	d, err := Open()
	if err != nil {
		return 0, 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()

	targetAgent, err := agentIDForConvTx(tx, m.ToConv)
	if err != nil {
		return 0, 0, err
	}
	var res sql.Result
	if targetAgent != "" && !m.PinGen {
		res, err = tx.Exec(`DELETE FROM agent_messages
			WHERE id IN (
				SELECT am.id
				FROM agent_messages am
				JOIN agent_cron_messages cm ON cm.message_id = am.id
				WHERE cm.cron_job_id = ? AND am.to_agent = ? AND am.pin_gen = 0
				  AND am.delivered_at IS NULL AND am.read_at IS NULL
				  AND am.nudge_claimed_at IS NULL AND am.nudge_cancelled_at IS NULL
			)`, cronJobID, targetAgent)
	} else {
		res, err = tx.Exec(`DELETE FROM agent_messages
			WHERE id IN (
				SELECT am.id
				FROM agent_messages am
				JOIN agent_cron_messages cm ON cm.message_id = am.id
				WHERE cm.cron_job_id = ? AND am.to_conv = ?
				  AND (am.pin_gen = 1 OR am.to_agent = '')
				  AND am.delivered_at IS NULL AND am.read_at IS NULL
				  AND am.nudge_claimed_at IS NULL AND am.nudge_cancelled_at IS NULL
			)`, cronJobID, m.ToConv)
	}
	if err != nil {
		return 0, 0, err
	}
	replaced, err = res.RowsAffected()
	if err != nil {
		return 0, 0, err
	}

	messageID, err = insertAgentMessage(tx, m)
	if err != nil {
		return 0, 0, err
	}
	if m.OperatorAuthored {
		if _, err := tx.Exec(`INSERT INTO operator_agent_messages (message_id) VALUES (?)`, messageID); err != nil {
			return 0, 0, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO agent_cron_messages (message_id, cron_job_id) VALUES (?, ?)`,
		messageID, cronJobID); err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return messageID, replaced, nil
}

// AgentMessageCronJobID returns trusted scheduler provenance for tests and
// internal consumers. Zero means the message did not originate from cron.
func AgentMessageCronJobID(messageID int64) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var cronJobID int64
	err = d.QueryRow(`SELECT cron_job_id FROM agent_cron_messages WHERE message_id = ?`, messageID).Scan(&cronJobID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return cronJobID, nil
}
