package db

import (
	"fmt"
	"strings"
	"time"
)

const (
	standingOrderTurnOriginPending = "pending"
	standingOrderTurnOriginActive  = "active"
)

// InsertStandingOrderAgentMessage atomically records a durable inbox message,
// its operator-authorship marker when applicable, and the trusted
// standing-order provenance used by OpenCode origin suppression. The nudge
// worker must never be able to observe the message without that provenance.
func InsertStandingOrderAgentMessage(
	m *AgentMessage,
	orderID, orderRevision int64,
) (int64, error) {
	if m == nil || orderID <= 0 || orderRevision <= 0 {
		return 0, fmt.Errorf("invalid standing-order agent message")
	}
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	messageID, err := insertAgentMessage(tx, m)
	if err != nil {
		return 0, err
	}
	if m.OperatorAuthored {
		if _, err := tx.Exec(
			`INSERT INTO operator_agent_messages (message_id) VALUES (?)`,
			messageID); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`
		INSERT INTO agent_standing_order_messages
			(message_id, order_id, order_revision)
		VALUES (?, ?, ?)`,
		messageID, orderID, orderRevision); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return messageID, nil
}

// AgentMessageIsStandingOrder reports trusted metadata written by the internal
// standing-order queue path. It deliberately does not inspect the subject.
func AgentMessageIsStandingOrder(messageID int64) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = d.QueryRow(`
		SELECT COUNT(*) FROM agent_standing_order_messages WHERE message_id = ?`,
		messageID).Scan(&n)
	return n > 0, err
}

// ArmStandingOrderTurnOrigin durably announces that messageID is about to be
// submitted as an OpenCode prompt for targetAgent. A live active marker is not
// overwritten: overlapping internal turns cannot be attributed safely.
func ArmStandingOrderTurnOrigin(
	targetAgent string,
	messageID int64,
	now time.Time,
	pendingFor time.Duration,
) error {
	targetAgent = strings.TrimSpace(targetAgent)
	if targetAgent == "" || messageID <= 0 || pendingFor <= 0 {
		return fmt.Errorf("invalid standing-order turn origin")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	nowRaw := formatStandingTime(now)
	expiresRaw := formatStandingTime(now.Add(pendingFor))
	res, err := d.Exec(`
		INSERT INTO agent_standing_order_turn_origins
			(target_agent, message_id, state, armed_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(target_agent) DO UPDATE SET
			message_id = excluded.message_id,
			state = excluded.state,
			armed_at = excluded.armed_at,
			expires_at = excluded.expires_at
		WHERE agent_standing_order_turn_origins.state != ?
		   OR agent_standing_order_turn_origins.expires_at <= ?`,
		targetAgent, messageID, standingOrderTurnOriginPending, nowRaw, expiresRaw,
		standingOrderTurnOriginActive, nowRaw)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("standing-order turn already active for agent %s", targetAgent)
	}
	return nil
}

// ActivateStandingOrderTurnOrigin consumes a non-expired pending handshake at
// the first observed event from that turn. The marker remains active until the
// projector sees the turn end, so tool events later in the same turn inherit
// the origin even across a daemon restart.
func ActivateStandingOrderTurnOrigin(
	targetAgent string,
	now time.Time,
	activeFor time.Duration,
) (bool, error) {
	targetAgent = strings.TrimSpace(targetAgent)
	if targetAgent == "" || activeFor <= 0 {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`
		UPDATE agent_standing_order_turn_origins
		   SET state = ?, expires_at = ?
		 WHERE target_agent = ? AND state = ? AND expires_at > ?`,
		standingOrderTurnOriginActive, formatStandingTime(now.Add(activeFor)),
		targetAgent, standingOrderTurnOriginPending, formatStandingTime(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// StandingOrderTurnOriginActive reports whether a non-expired active marker
// survives for targetAgent. It is used when the OpenCode SSE projector
// reconnects in the middle of a standing-order-originated turn.
func StandingOrderTurnOriginActive(targetAgent string, now time.Time) (bool, error) {
	targetAgent = strings.TrimSpace(targetAgent)
	if targetAgent == "" {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	var n int
	err = d.QueryRow(`
		SELECT COUNT(*) FROM agent_standing_order_turn_origins
		 WHERE target_agent = ? AND state = ? AND expires_at > ?`,
		targetAgent, standingOrderTurnOriginActive, formatStandingTime(now)).Scan(&n)
	return n > 0, err
}

// CancelPendingStandingOrderTurnOrigin removes the exact handshake when the
// prompt submission failed before OpenCode accepted it. It never clears an
// active turn or a newer message's pending marker.
func CancelPendingStandingOrderTurnOrigin(targetAgent string, messageID int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`
		DELETE FROM agent_standing_order_turn_origins
		 WHERE target_agent = ? AND message_id = ? AND state = ?`,
		strings.TrimSpace(targetAgent), messageID, standingOrderTurnOriginPending)
	return err
}

// ClearStandingOrderTurnOrigin closes pending or active origin state after the
// projector observes the turn end.
func ClearStandingOrderTurnOrigin(targetAgent string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(
		`DELETE FROM agent_standing_order_turn_origins WHERE target_agent = ?`,
		strings.TrimSpace(targetAgent))
	return err
}
