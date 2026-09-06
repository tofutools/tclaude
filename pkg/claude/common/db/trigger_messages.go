package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// InsertTriggerMessageOutcome atomically accepts a trigger message and records
// its terminal action outcome. The action-row uniqueness is the idempotency
// key: a duplicate returns the already committed outcome without creating a
// second inbox row.
func InsertTriggerMessageOutcome(m *AgentMessage, desired TriggerActionOutcome) (stored TriggerActionOutcome, inserted bool, err error) {
	d, err := Open()
	if err != nil {
		return TriggerActionOutcome{}, false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return TriggerActionOutcome{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.Exec(`INSERT INTO trigger_action_outcomes
		(firing_id,action_index,action_type,outcome,detail,spawned_agent,message_id,created_at)
		VALUES(?,?,?,?,?,?,NULL,?) ON CONFLICT(firing_id,action_index) DO NOTHING`,
		desired.FiringID, desired.ActionIndex, desired.ActionType, desired.Outcome,
		desired.Detail, desired.SpawnedAgent, dbTime(desired.CreatedAt.UTC()))
	if err != nil {
		return TriggerActionOutcome{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return TriggerActionOutcome{}, false, err
	}
	if n == 0 {
		stored, err = triggerActionOutcomeTx(tx, desired.FiringID, desired.ActionIndex)
		if err != nil {
			return TriggerActionOutcome{}, false, err
		}
		if stored.ActionType != desired.ActionType {
			return TriggerActionOutcome{}, false, fmt.Errorf("trigger action identity conflict at firing %d action %d: stored %q, requested %q", desired.FiringID, desired.ActionIndex, stored.ActionType, desired.ActionType)
		}
		if err := tx.Commit(); err != nil {
			return TriggerActionOutcome{}, false, err
		}
		return stored, false, nil
	}

	id, err := insertAgentMessage(tx, m)
	if err != nil {
		return TriggerActionOutcome{}, false, err
	}
	if m.OperatorAuthored {
		if _, err := tx.Exec(`INSERT INTO operator_agent_messages (message_id) VALUES (?)`, id); err != nil {
			return TriggerActionOutcome{}, false, err
		}
	}
	if _, err := tx.Exec(`UPDATE trigger_action_outcomes SET message_id=?
		WHERE firing_id=? AND action_index=?`, id, desired.FiringID, desired.ActionIndex); err != nil {
		return TriggerActionOutcome{}, false, err
	}
	desired.MessageID = id
	if err := tx.Commit(); err != nil {
		return TriggerActionOutcome{}, false, err
	}
	return desired, true, nil
}

// GetTriggerActionOutcome returns the terminal result owned by one firing
// action, or nil when that action has not committed a result yet.
func GetTriggerActionOutcome(firingID int64, actionIndex int) (*TriggerActionOutcome, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	o, err := triggerActionOutcomeTx(d, firingID, actionIndex)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

func triggerActionOutcomeTx(q dbExecQuerier, firingID int64, actionIndex int) (TriggerActionOutcome, error) {
	var o TriggerActionOutcome
	var ts dbTimestamp
	err := q.QueryRow(`SELECT id,firing_id,action_index,action_type,outcome,detail,spawned_agent,
		COALESCE(message_id,0),created_at FROM trigger_action_outcomes
		WHERE firing_id=? AND action_index=?`, firingID, actionIndex).Scan(
		&o.ID, &o.FiringID, &o.ActionIndex, &o.ActionType, &o.Outcome, &o.Detail,
		&o.SpawnedAgent, &o.MessageID, &ts)
	if err != nil {
		return TriggerActionOutcome{}, err
	}
	o.CreatedAt = ts.Time()
	return o, nil
}
