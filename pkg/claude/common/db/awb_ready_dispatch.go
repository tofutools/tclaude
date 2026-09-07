package db

import (
	"database/sql"
	"errors"
	"time"
)

type AWBReadyDispatch struct {
	Workspace, IssueID, Phase, AgentID, LatestError string
	CreatedAt, UpdatedAt                            time.Time
}

func GetAWBReadyDispatch(workspace string) (*AWBReadyDispatch, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var row AWBReadyDispatch
	var created, updated int64
	err = d.QueryRow(`SELECT workspace,issue_id,phase,agent_id,latest_error,created_at,updated_at FROM awb_ready_dispatches WHERE workspace=?`, workspace).
		Scan(&row.Workspace, &row.IssueID, &row.Phase, &row.AgentID, &row.LatestError, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.CreatedAt = time.Unix(0, created)
	row.UpdatedAt = time.Unix(0, updated)
	return &row, nil
}

// SelectAWBReadyDispatch is a compare-and-set: concurrent or duplicate workers
// cannot replace the workspace's current issue.
func SelectAWBReadyDispatch(workspace, issueID, agentID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	now := time.Now().UnixNano()
	r, err := d.Exec(`INSERT OR IGNORE INTO awb_ready_dispatches(workspace,issue_id,phase,agent_id,created_at,updated_at) VALUES(?,?,'selected',?,?,?)`, workspace, issueID, agentID, now, now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func UpdateAWBReadyDispatch(workspace, issueID, phase, latestError string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	now := time.Now().UnixNano()
	r, err := d.Exec(`UPDATE awb_ready_dispatches SET phase=?,latest_error=?,updated_at=? WHERE workspace=? AND issue_id=?`, phase, latestError, now, workspace, issueID)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func SetAWBReadyDispatchAgent(workspace, issueID, agentID string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE awb_ready_dispatches SET agent_id=?,updated_at=? WHERE workspace=? AND issue_id=?`, agentID, time.Now().UnixNano(), workspace, issueID)
	return err
}

func ClearAWBReadyDispatch(workspace, issueID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	r, err := d.Exec(`DELETE FROM awb_ready_dispatches WHERE workspace=? AND issue_id=?`, workspace, issueID)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}
