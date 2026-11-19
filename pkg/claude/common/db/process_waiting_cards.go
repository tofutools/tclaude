package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ProcessWaitingCard is the operator's Messages card for one thing a process
// run waits on a person for: a decision (Kind "decision") or a step a person
// performs (Kind "task"), at one attempt.
type ProcessWaitingCard struct {
	RunID          string
	NodeID         string
	Attempt        int
	Kind           string
	HumanMessageID int64
	CreatedAt      time.Time
	// Closed says the card was settled or withdrawn.
	Closed bool
}

const processWaitingCardColumns = `run_id, node_id, attempt, kind, human_message_id, created_at, closed_at IS NOT NULL`

func scanProcessWaitingCard(row interface{ Scan(...any) error }) (ProcessWaitingCard, error) {
	var c ProcessWaitingCard
	var created int64
	err := row.Scan(&c.RunID, &c.NodeID, &c.Attempt, &c.Kind, &c.HumanMessageID, &created, &c.Closed)
	c.CreatedAt = time.Unix(0, created)
	return c, err
}

// InsertProcessWaitingCardWithMessage puts up the card for (run, node,
// attempt, kind) with its message in one transaction. inserted is false, and
// no message is written, when that card already exists (open or closed).
func InsertProcessWaitingCardWithMessage(card ProcessWaitingCard, m *HumanMessage) (id int64, inserted bool, err error) {
	d, err := Open()
	if err != nil {
		return 0, false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM process_waiting_cards
		WHERE run_id = ? AND node_id = ? AND attempt = ? AND kind = ?`,
		card.RunID, card.NodeID, card.Attempt, card.Kind).Scan(&n); err != nil {
		return 0, false, fmt.Errorf("find process waiting card: %w", err)
	}
	if n > 0 {
		return 0, false, nil
	}
	id, err = insertHumanMessage(tx, m)
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(`INSERT INTO process_waiting_cards
		(run_id, node_id, attempt, kind, human_message_id, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		card.RunID, card.NodeID, card.Attempt, card.Kind, id, time.Now().UnixNano()); err != nil {
		return 0, false, fmt.Errorf("insert process waiting card: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// ListOpenProcessWaitingCards returns a run's cards not yet closed, or every
// run's when runID is empty.
func ListOpenProcessWaitingCards(runID string) ([]ProcessWaitingCard, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	query := `SELECT ` + processWaitingCardColumns + ` FROM process_waiting_cards WHERE closed_at IS NULL`
	var args []any
	if runID != "" {
		query += ` AND run_id = ?`
		args = append(args, runID)
	}
	rows, err := d.Query(query+` ORDER BY created_at, run_id, node_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("list process waiting cards: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []ProcessWaitingCard
	for rows.Next() {
		c, err := scanProcessWaitingCard(rows)
		if err != nil {
			return nil, fmt.Errorf("list process waiting cards: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CloseProcessWaitingCard marks a card closed; closed is false when it
// already was.
func CloseProcessWaitingCard(card ProcessWaitingCard) (closed bool, err error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE process_waiting_cards SET closed_at = ?
		WHERE run_id = ? AND node_id = ? AND attempt = ? AND kind = ? AND closed_at IS NULL`,
		time.Now().UnixNano(), card.RunID, card.NodeID, card.Attempt, card.Kind)
	if err != nil {
		return false, fmt.Errorf("close process waiting card: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetProcessWaitingCardForHumanMessage finds the card whose message is id;
// ok is false when it is no process waiting card's.
func GetProcessWaitingCardForHumanMessage(id int64) (ProcessWaitingCard, bool, error) {
	if id <= 0 {
		return ProcessWaitingCard{}, false, nil
	}
	d, err := Open()
	if err != nil {
		return ProcessWaitingCard{}, false, err
	}
	c, err := scanProcessWaitingCard(d.QueryRow(`SELECT `+processWaitingCardColumns+` FROM process_waiting_cards
		WHERE human_message_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessWaitingCard{}, false, nil
	}
	return c, err == nil, err
}

// ListProcessRunIDsWaitingOnPerson returns the runs that are not over and
// wait on a person, a decision or a step a person performs, plus those that
// still hold an open card, by id. It is the restart sweep's list: what a
// crash between a commit and its card could have left unmatched.
func ListProcessRunIDsWaitingOnPerson() ([]string, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id FROM process_runs
		WHERE status NOT IN ('completed', 'failed', 'canceled') AND json_valid(checkpoint_json) = 1
		  AND (COALESCE(json_array_length(checkpoint_json, '$.awaitingDecisions'), 0) > 0
		       OR EXISTS (SELECT 1 FROM json_each(checkpoint_json, '$.commands')
		                  WHERE json_extract(value, '$.kind') = 'human'))
		UNION SELECT DISTINCT run_id FROM process_waiting_cards WHERE closed_at IS NULL
		ORDER BY 1`)
	if err != nil {
		return nil, fmt.Errorf("list runs waiting on a person: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list runs waiting on a person: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
