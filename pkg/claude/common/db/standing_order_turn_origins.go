package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const (
	StandingOrderTurnOriginPending = "pending"
	StandingOrderTurnOriginActive  = "active"
)

// StandingOrderAgentMessageOrigin is trusted metadata written atomically with
// an internal reminder. OpenCodeMessageID is supplied to prompt_async so SSE
// can correlate the assistant turn to this exact user message.
type StandingOrderAgentMessageOrigin struct {
	MessageID         int64
	OrderID           int64
	OrderRevision     int64
	OpenCodeMessageID string
}

// StandingOrderTurnOrigin is the one reminder turn currently pending or
// active for a stable agent. TargetConv is a routing-generation guard, not the
// durable key: late events from an old generation must not consume a marker
// armed for the agent's new head.
type StandingOrderTurnOrigin struct {
	TargetAgent       string
	TargetConv        string
	MessageID         int64
	OpenCodeMessageID string
	State             string
}

func newStandingOrderOpenCodeMessageID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate OpenCode message id: %w", err)
	}
	return "msg_tclaude_" + hex.EncodeToString(raw[:]), nil
}

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
	openCodeMessageID, err := newStandingOrderOpenCodeMessageID()
	if err != nil {
		return 0, err
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
			(message_id, order_id, order_revision, opencode_message_id)
		VALUES (?, ?, ?, ?)`,
		messageID, orderID, orderRevision, openCodeMessageID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return messageID, nil
}

// AgentMessageStandingOrderOrigin returns trusted metadata written by the
// internal standing-order queue path. It deliberately does not inspect the
// sender-controlled subject.
func AgentMessageStandingOrderOrigin(
	messageID int64,
) (*StandingOrderAgentMessageOrigin, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var origin StandingOrderAgentMessageOrigin
	err = d.QueryRow(`
		SELECT message_id, order_id, order_revision, opencode_message_id
		  FROM agent_standing_order_messages
		 WHERE message_id = ?`,
		messageID).Scan(
		&origin.MessageID, &origin.OrderID, &origin.OrderRevision,
		&origin.OpenCodeMessageID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &origin, err
}

// ArmStandingOrderTurnOrigin durably announces that messageID is about to be
// submitted as one specific OpenCode user message. A live marker is never
// overwritten: without an authoritative OpenCode message identity, two
// pending prompts could steal each other's turn attribution.
func ArmStandingOrderTurnOrigin(
	targetAgent, targetConv string,
	messageID int64,
	openCodeMessageID string,
	now time.Time,
	pendingFor time.Duration,
) error {
	targetAgent = strings.TrimSpace(targetAgent)
	targetConv = strings.TrimSpace(targetConv)
	openCodeMessageID = strings.TrimSpace(openCodeMessageID)
	if targetAgent == "" || targetConv == "" || messageID <= 0 ||
		!strings.HasPrefix(openCodeMessageID, "msg") || pendingFor <= 0 {
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
			(target_agent, target_conv, message_id, opencode_message_id,
			 state, armed_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(target_agent) DO UPDATE SET
			target_conv = excluded.target_conv,
			message_id = excluded.message_id,
			opencode_message_id = excluded.opencode_message_id,
			state = excluded.state,
			armed_at = excluded.armed_at,
			expires_at = excluded.expires_at
		WHERE agent_standing_order_turn_origins.expires_at <= ?`,
		targetAgent, targetConv, messageID, openCodeMessageID,
		StandingOrderTurnOriginPending, nowRaw, expiresRaw, nowRaw)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("standing-order turn already pending or active for agent %s", targetAgent)
	}
	return nil
}

// RefreshPendingStandingOrderTurnOrigin extends a known-accepted prompt's
// marker. It cannot revive an expired marker or modify another message.
func RefreshPendingStandingOrderTurnOrigin(
	targetAgent, targetConv string,
	messageID int64,
	openCodeMessageID string,
	now time.Time,
	pendingFor time.Duration,
) error {
	d, err := Open()
	if err != nil {
		return err
	}
	res, err := d.Exec(`
		UPDATE agent_standing_order_turn_origins
		   SET expires_at = ?
		 WHERE target_agent = ? AND target_conv = ? AND message_id = ?
		   AND opencode_message_id = ? AND state = ? AND expires_at > ?`,
		formatStandingTime(now.Add(pendingFor)),
		strings.TrimSpace(targetAgent), strings.TrimSpace(targetConv), messageID,
		strings.TrimSpace(openCodeMessageID), StandingOrderTurnOriginPending,
		formatStandingTime(now))
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("standing-order pending turn origin changed or expired")
	}
	return nil
}

// ActivateStandingOrderTurnOrigin promotes only the pending marker whose
// OpenCode user message is the parent of the observed assistant turn.
func ActivateStandingOrderTurnOrigin(
	targetAgent, targetConv, openCodeMessageID string,
	now time.Time,
	activeFor time.Duration,
) (bool, error) {
	targetAgent = strings.TrimSpace(targetAgent)
	targetConv = strings.TrimSpace(targetConv)
	openCodeMessageID = strings.TrimSpace(openCodeMessageID)
	if targetAgent == "" || targetConv == "" || openCodeMessageID == "" ||
		activeFor <= 0 {
		return false, nil
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`
		UPDATE agent_standing_order_turn_origins
		   SET state = ?, expires_at = ?
		 WHERE target_agent = ? AND target_conv = ?
		   AND opencode_message_id = ? AND state = ? AND expires_at > ?`,
		StandingOrderTurnOriginActive, formatStandingTime(now.Add(activeFor)),
		targetAgent, targetConv, openCodeMessageID,
		StandingOrderTurnOriginPending, formatStandingTime(now))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// GetStandingOrderTurnOrigin returns a live marker for exactly one stable
// agent and conversation generation. Expired and other-generation markers are
// deliberately invisible.
func GetStandingOrderTurnOrigin(
	targetAgent, targetConv string,
	now time.Time,
) (*StandingOrderTurnOrigin, error) {
	targetAgent = strings.TrimSpace(targetAgent)
	targetConv = strings.TrimSpace(targetConv)
	if targetAgent == "" || targetConv == "" {
		return nil, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var origin StandingOrderTurnOrigin
	err = d.QueryRow(`
		SELECT target_agent, target_conv, message_id, opencode_message_id, state
		  FROM agent_standing_order_turn_origins
		 WHERE target_agent = ? AND target_conv = ? AND expires_at > ?`,
		targetAgent, targetConv, formatStandingTime(now)).Scan(
		&origin.TargetAgent, &origin.TargetConv, &origin.MessageID,
		&origin.OpenCodeMessageID, &origin.State)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &origin, err
}

// CancelPendingStandingOrderTurnOrigin removes the exact handshake after a
// definitive prompt rejection. It never clears an active turn or another
// message's marker.
func CancelPendingStandingOrderTurnOrigin(
	targetAgent, targetConv string,
	messageID int64,
	openCodeMessageID string,
) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`
		DELETE FROM agent_standing_order_turn_origins
		 WHERE target_agent = ? AND target_conv = ? AND message_id = ?
		   AND opencode_message_id = ? AND state = ?`,
		strings.TrimSpace(targetAgent), strings.TrimSpace(targetConv), messageID,
		strings.TrimSpace(openCodeMessageID), StandingOrderTurnOriginPending)
	return err
}

// CompleteStandingOrderTurnOrigin closes only an active, generation-matched
// reminder turn after the projector observes its Stop boundary. A Stop from an
// unrelated turn cannot consume a still-pending marker.
func CompleteStandingOrderTurnOrigin(targetAgent, targetConv string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`
		DELETE FROM agent_standing_order_turn_origins
		 WHERE target_agent = ? AND target_conv = ? AND state = ?`,
		strings.TrimSpace(targetAgent), strings.TrimSpace(targetConv),
		StandingOrderTurnOriginActive)
	return err
}
