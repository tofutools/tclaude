package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// StandingDebounce is one durable trailing-edge candidate. TargetAgent is the
// key; TargetConv is only the generation observed by the last matching event.
type StandingDebounce struct {
	OrderID       int64
	OrderRevision int64
	TargetAgent   string
	TargetConv    string
	Epoch         string
	Harness       string
	Detail        string
	DueAt         time.Time
	MaxDueAt      time.Time
	UpdatedAt     time.Time
}

// ScheduleStandingDebounce moves the trailing edge to dueAt while preserving
// the first event's maximum deadline. A new delivery revision replaces the
// previous candidate instead of letting obsolete guidance fire.
func ScheduleStandingDebounce(p *StandingDebounce) error {
	if p == nil || p.OrderID <= 0 || p.OrderRevision <= 0 ||
		strings.TrimSpace(p.TargetAgent) == "" ||
		strings.TrimSpace(p.TargetConv) == "" ||
		p.DueAt.IsZero() || p.MaxDueAt.IsZero() ||
		p.DueAt.After(p.MaxDueAt) {
		return fmt.Errorf("%w: invalid standing-order debounce", ErrStandingOrderInvalid)
	}
	d, err := Open()
	if err != nil {
		return err
	}
	now := p.UpdatedAt
	if now.IsZero() {
		now = time.Now()
	}
	_, err = d.Exec(`
		INSERT INTO agent_standing_order_debounce
			(order_id, order_revision, target_agent, target_conv, epoch,
			 harness, detail, due_at, max_due_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(order_id, target_agent) DO UPDATE SET
			order_revision = excluded.order_revision,
			target_conv = excluded.target_conv,
			epoch = excluded.epoch,
			harness = excluded.harness,
			detail = excluded.detail,
			due_at = CASE
				WHEN agent_standing_order_debounce.order_revision = excluded.order_revision
				THEN MIN(excluded.due_at, agent_standing_order_debounce.max_due_at)
				ELSE excluded.due_at END,
			max_due_at = CASE
				WHEN agent_standing_order_debounce.order_revision = excluded.order_revision
				THEN agent_standing_order_debounce.max_due_at
				ELSE excluded.max_due_at END,
			updated_at = excluded.updated_at`,
		p.OrderID, p.OrderRevision, strings.TrimSpace(p.TargetAgent),
		strings.TrimSpace(p.TargetConv), p.Epoch, p.Harness, p.Detail,
		formatStandingTime(p.DueAt), formatStandingTime(p.MaxDueAt),
		formatStandingTime(now))
	return err
}

func scanStandingDebounce(s rowScanner) (*StandingDebounce, error) {
	var p StandingDebounce
	var dueRaw, maxRaw, updatedRaw string
	if err := s.Scan(
		&p.OrderID, &p.OrderRevision, &p.TargetAgent, &p.TargetConv,
		&p.Epoch, &p.Harness, &p.Detail, &dueRaw, &maxRaw, &updatedRaw,
	); err != nil {
		return nil, err
	}
	p.DueAt = parseStandingTime(dueRaw)
	p.MaxDueAt = parseStandingTime(maxRaw)
	p.UpdatedAt = parseStandingTime(updatedRaw)
	return &p, nil
}

const standingDebounceSelect = `SELECT order_id, order_revision, target_agent,
	target_conv, epoch, harness, detail, due_at, max_due_at, updated_at
	FROM agent_standing_order_debounce`

func ListDueStandingDebounces(now time.Time) ([]*StandingDebounce, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(standingDebounceSelect+
		` WHERE due_at <= ? ORDER BY due_at, order_id, target_agent`,
		formatStandingTime(now))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*StandingDebounce
	for rows.Next() {
		p, err := scanStandingDebounce(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func GetDueStandingDebounce(
	orderID int64,
	targetAgent string,
	now time.Time,
) (*StandingDebounce, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	p, err := scanStandingDebounce(d.QueryRow(standingDebounceSelect+
		` WHERE order_id = ? AND target_agent = ? AND due_at <= ?`,
		orderID, strings.TrimSpace(targetAgent), formatStandingTime(now)))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return p, nil
}

func DeleteStandingDebounce(orderID int64, targetAgent string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM agent_standing_order_debounce
		WHERE order_id = ? AND target_agent = ?`,
		orderID, strings.TrimSpace(targetAgent))
	return err
}

// ConsumeStandingDebounceIntoAgentMessage atomically queues one reminder and
// removes the exact pending candidate. A crash cannot leave both a visible
// inbox row and a retryable candidate that would duplicate it.
func ConsumeStandingDebounceIntoAgentMessage(
	p *StandingDebounce,
	m *AgentMessage,
	delivery *StandingDelivery,
) (int64, error) {
	if p == nil || m == nil || delivery == nil ||
		delivery.OrderID != p.OrderID ||
		delivery.OrderRevision != p.OrderRevision ||
		delivery.TargetAgent != p.TargetAgent ||
		delivery.Outcome != StandingOutcomeDelivered ||
		delivery.Transport != StandingTransportMessage {
		return 0, fmt.Errorf("%w: invalid debounce consume", ErrStandingOrderInvalid)
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
		messageID, delivery.OrderID, delivery.OrderRevision,
		openCodeMessageID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO agent_standing_order_deliveries
		(order_id, order_revision, target_conv, target_agent, epoch,
		 outcome, transport, harness, detail, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		delivery.OrderID, delivery.OrderRevision,
		delivery.TargetConv, delivery.TargetAgent, delivery.Epoch,
		delivery.Outcome, delivery.Transport, delivery.Harness, delivery.Detail,
		formatStandingTime(time.Now())); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM agent_standing_order_debounce
		WHERE order_id = ? AND order_revision = ? AND target_agent = ?
		  AND updated_at = ?`,
		p.OrderID, p.OrderRevision, p.TargetAgent, formatStandingTime(p.UpdatedAt))
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("standing-order debounce changed before delivery")
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return messageID, nil
}
