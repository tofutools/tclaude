package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/platform/execution"
)

// ResumeClaimHash returns the persisted digest of a private one-shot child
// credential. The credential itself must travel through an inherited FD and
// never be persisted or represented by public operation IDs.
func ResumeClaimHash(secret []byte) string {
	digest := sha256.Sum256(secret)
	return hex.EncodeToString(digest[:])
}

// ResumeOperationRow is the durable host-side representation of one managed
// Resume. ClaimHash is only a hash of the private one-shot child credential;
// OperationID and intended ExecutionID are correlation values, never bearer
// authority.
type ResumeOperationRow struct {
	ID                    execution.OperationID
	Kind                  string
	AgentID               string
	ConvID                string
	Predecessor           execution.AttemptRef
	Attempt               execution.AttemptRef
	LogicalConversationID string
	ClaimHash             string
	ClaimPID              int
	ClaimProcessStart     string
	TmuxSession           string
	PaneID                string
	GatePath              string
	State                 execution.ResumeState
	LaunchPhase           string
	Revision              int64
	RecoveryAgentID       string
	RecoveryGeneration    string
	DispatchDetail        string
	FailureDetail         string
	RequestedAt           time.Time
	AcceptedAt            time.Time
	StartedAt             time.Time
	ReadyAt               time.Time
}

func CreateResumeOperation(row ResumeOperationRow) error {
	if err := execution.ValidateOperationID(row.ID); err != nil {
		return err
	}
	if row.State != execution.ResumeRequested || row.Revision != 1 {
		return errors.New("resume operation must be created as requested at revision 1")
	}
	if strings.TrimSpace(row.Kind) == "" || strings.TrimSpace(row.ConvID) == "" {
		return errors.New("resume operation kind and conversation are required")
	}
	if strings.TrimSpace(row.Attempt.ExecutionID.String()) == "" {
		return errors.New("resume operation requires intended execution id")
	}
	if row.RequestedAt.IsZero() {
		row.RequestedAt = time.Now().UTC()
	}
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`INSERT INTO execution_operations (
		id, kind, agent_id, conv_id, predecessor_execution_id, predecessor_session_id,
		intended_execution_id, intended_session_id, logical_conversation_id, claim_hash, claim_pid,
		claim_process_start, tmux_session, pane_id, gate_path, state, launch_phase, revision, recovery_agent_id,
		recovery_generation, dispatch_detail, failure_detail, requested_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		row.ID.String(), row.Kind, row.AgentID, row.ConvID,
		row.Predecessor.ExecutionID.String(), row.Predecessor.LegacySessionID,
		row.Attempt.ExecutionID.String(), row.Attempt.LegacySessionID, row.LogicalConversationID, row.ClaimHash,
		row.ClaimPID, row.ClaimProcessStart, row.TmuxSession, row.PaneID, row.GatePath,
		row.State, row.LaunchPhase, row.Revision,
		row.RecoveryAgentID, row.RecoveryGeneration, row.DispatchDetail, row.FailureDetail,
		dbTime(row.RequestedAt))
	return err
}

// ClaimResumeOperation consumes the private child credential and binds the
// operation to one observed wrapper process and session identity. Public IDs
// alone cannot claim an accepted operation, and a consumed claim cannot be
// reused by a stale wrapper.
func ClaimResumeOperation(id execution.OperationID, intended execution.ID, secret []byte, convID, sessionID string, pid int, processStart string) (bool, error) {
	if err := execution.ValidateOperationID(id); err != nil {
		return false, err
	}
	if _, err := execution.ParseID(intended.String()); err != nil {
		return false, err
	}
	if len(secret) == 0 || strings.TrimSpace(convID) == "" || strings.TrimSpace(sessionID) == "" || pid <= 0 || strings.TrimSpace(processStart) == "" {
		return false, errors.New("resume child claim requires secret, conversation, session, pid, and process start")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	result, err := d.Exec(`UPDATE execution_operations SET
		intended_session_id = ?, claim_hash = '', claim_pid = ?,
		claim_process_start = ?, launch_phase = 'child_claimed', revision = revision + 1
		WHERE id = ? AND intended_execution_id = ? AND conv_id = ? AND claim_hash = ?
		AND state = 'accepted' AND launch_phase = 'accepted'`,
		sessionID, pid, processStart, id.String(), intended.String(), convID, ResumeClaimHash(secret))
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return n == 1, err
}

func GetResumeOperation(id execution.OperationID) (*ResumeOperationRow, error) {
	if err := execution.ValidateOperationID(id); err != nil {
		return nil, err
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := &ResumeOperationRow{}
	var requested, accepted, started, ready dbTimestamp
	err = d.QueryRow(`SELECT id, kind, agent_id, conv_id,
		predecessor_execution_id, predecessor_session_id, intended_execution_id,
		intended_session_id, logical_conversation_id, claim_hash, claim_pid, claim_process_start,
		tmux_session, pane_id, gate_path, state,
		launch_phase, revision, recovery_agent_id, recovery_generation,
		dispatch_detail, failure_detail, requested_at, accepted_at, started_at, ready_at
		FROM execution_operations WHERE id = ?`, id.String()).Scan(
		&row.ID, &row.Kind, &row.AgentID, &row.ConvID,
		&row.Predecessor.ExecutionID, &row.Predecessor.LegacySessionID,
		&row.Attempt.ExecutionID, &row.Attempt.LegacySessionID, &row.LogicalConversationID, &row.ClaimHash,
		&row.ClaimPID, &row.ClaimProcessStart, &row.TmuxSession, &row.PaneID,
		&row.GatePath, &row.State, &row.LaunchPhase,
		&row.Revision, &row.RecoveryAgentID, &row.RecoveryGeneration,
		&row.DispatchDetail, &row.FailureDetail, &requested, &accepted, &started, &ready)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.RequestedAt, row.AcceptedAt = requested.Time(), accepted.Time()
	row.StartedAt, row.ReadyAt = started.Time(), ready.Time()
	return row, nil
}

// RegisterResumeLaunch records the exact still-gated OS attempt. It is the
// child side of the cancellation race: once cancellation revokes accepted,
// this CAS fails and the caller must remove the pane without publishing go.
func RegisterResumeLaunch(id execution.OperationID, intended execution.ID, convID, sessionID, tmuxSession, paneID, gatePath string, pid int, processStart string) (bool, error) {
	if err := execution.ValidateOperationID(id); err != nil {
		return false, err
	}
	if _, err := execution.ParseID(intended.String()); err != nil {
		return false, err
	}
	if strings.TrimSpace(convID) == "" || strings.TrimSpace(sessionID) == "" ||
		strings.TrimSpace(tmuxSession) == "" || strings.TrimSpace(paneID) == "" ||
		strings.TrimSpace(gatePath) == "" || pid <= 0 || strings.TrimSpace(processStart) == "" {
		return false, errors.New("resume launch registration requires exact conversation, session, pane, gate, and process identity")
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE execution_operations SET tmux_session = ?, pane_id = ?, gate_path = ?,
		launch_phase = 'registered_pending', revision = revision + 1
		WHERE id = ? AND intended_execution_id = ? AND conv_id = ? AND intended_session_id = ?
		AND claim_pid = ? AND claim_process_start = ? AND state = 'accepted'
		AND launch_phase = 'child_claimed'`, tmuxSession, paneID, gatePath, id.String(),
		intended.String(), convID, sessionID, pid, processStart)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// GrantResumeRelease is the durable single-winner decision immediately before
// the child publishes go. A cancelling operation can never win this CAS.
func GrantResumeRelease(id execution.OperationID, intended execution.ID, sessionID, tmuxSession, paneID, gatePath string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE execution_operations SET launch_phase = 'release_granted', revision = revision + 1
		WHERE id = ? AND intended_execution_id = ? AND intended_session_id = ?
		AND tmux_session = ? AND pane_id = ? AND gate_path = ?
		AND state = 'accepted' AND launch_phase = 'registered_pending'`, id.String(), intended.String(),
		sessionID, tmuxSession, paneID, gatePath)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// MarkResumeReleased records exact gate acknowledgement. Spawn receipt and
// tmux creation never call this method.
func MarkResumeReleased(id execution.OperationID, intended execution.ID, sessionID, tmuxSession, paneID string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	now := dbTime(time.Now().UTC())
	res, err := d.Exec(`UPDATE execution_operations SET state = 'started', launch_phase = 'released',
		started_at = ?, revision = revision + 1 WHERE id = ? AND intended_execution_id = ?
		AND intended_session_id = ? AND tmux_session = ? AND pane_id = ?
		AND state IN ('accepted','unknown') AND launch_phase = 'release_granted'`, now, id.String(), intended.String(), sessionID, tmuxSession, paneID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 1 {
		return n == 1, err
	}
	// An exact SessionStart may reach the daemon after gate acknowledgement but
	// before the wrapper records it. That stronger main-workload evidence marks
	// ready transactionally; the late bookkeeping call is then idempotent.
	var ready int
	err = d.QueryRow(`SELECT COUNT(*) FROM execution_operations WHERE id=?
		AND intended_execution_id=? AND intended_session_id=? AND tmux_session=? AND pane_id=?
		AND state='ready' AND launch_phase='ready'`, id.String(), intended.String(), sessionID, tmuxSession, paneID).Scan(&ready)
	return ready == 1, err
}

// RequestResumeCancellation revokes the unused private claim and arbitrates
// against release_granted. It never writes final cancelled by itself.
func RequestResumeCancellation(id execution.OperationID, expectedRevision int64, detail string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE execution_operations SET state = 'cancelling', launch_phase = 'revoked',
		claim_hash = '', failure_detail = ?, revision = revision + 1
		WHERE id = ? AND revision = ? AND state IN ('requested','accepted','started','unknown')
		AND launch_phase <> 'release_granted' AND launch_phase <> 'released'`, detail, id.String(), expectedRevision)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RequestReleasedResumeCancellation covers cancellation after release won.
// The exact attempt must be stopped and observed gone before finalization.
func RequestReleasedResumeCancellation(id execution.OperationID, expectedRevision int64, detail string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE execution_operations SET state = 'cancelling',
		failure_detail = ?, revision = revision + 1 WHERE id = ? AND revision = ?
		AND state IN ('accepted','started','unknown') AND launch_phase IN ('release_granted','released')`, detail, id.String(), expectedRevision)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func FinalizeResumeCancelled(id execution.OperationID, expectedRevision int64, detail string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE execution_operations SET state = 'cancelled', launch_phase = 'cleanup_proven',
		failure_detail = ?, revision = revision + 1 WHERE id = ? AND revision = ? AND state = 'cancelling'`, detail, id.String(), expectedRevision)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func FinalizeResumeFailed(id execution.OperationID, expectedRevision int64, detail string) (bool, error) {
	d, err := Open()
	if err != nil {
		return false, err
	}
	res, err := d.Exec(`UPDATE execution_operations SET state = 'failed', launch_phase = 'cleanup_proven',
		failure_detail = ?, revision = revision + 1 WHERE id = ? AND revision = ?
		AND state = 'cancelling' AND claim_hash = ''`, detail, id.String(), expectedRevision)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func ResumeOperationForExecution(intended execution.ID) (*ResumeOperationRow, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	var raw string
	err = d.QueryRow(`SELECT id FROM execution_operations WHERE intended_execution_id = ? ORDER BY requested_at DESC LIMIT 1`, intended.String()).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return GetResumeOperation(execution.OperationID(raw))
}

// TransitionResumeOperation performs the portable state CAS. Host adapters
// must separately prove launch phase, private claim, process identity, and
// cleanup before requesting transitions such as ready, failed, or cancelled.
func TransitionResumeOperation(id execution.OperationID, expectedRevision int64, to execution.ResumeState, launchPhase, detail string) error {
	row, err := GetResumeOperation(id)
	if err != nil {
		return err
	}
	if row == nil {
		return sql.ErrNoRows
	}
	if err := execution.ValidateTransition(execution.ResumeOperation{
		ID: row.ID, State: row.State, Attempt: row.Attempt, Revision: row.Revision,
	}, to, expectedRevision); err != nil {
		return err
	}
	if (to == execution.ResumeFailed || to == execution.ResumeCancelled) && launchPhase != "cleanup_proven" {
		return errors.New("terminal resume failure or cancellation requires cleanup proof")
	}
	d, err := Open()
	if err != nil {
		return err
	}
	now := dbTime(time.Now().UTC())
	column := ""
	switch to {
	case execution.ResumeAccepted:
		column = ", accepted_at = ?"
	case execution.ResumeStarted:
		column = ", started_at = ?"
	case execution.ResumeReady:
		column = ", ready_at = ?"
	}
	args := []any{string(to), launchPhase, detail, detail}
	if column != "" {
		args = append(args, now)
	}
	args = append(args, id.String(), expectedRevision, string(row.State))
	query := `UPDATE execution_operations SET state = ?, launch_phase = ?,
		dispatch_detail = CASE WHEN ? <> '' THEN ? ELSE dispatch_detail END,
		revision = revision + 1` + column + ` WHERE id = ? AND revision = ? AND state = ?`
	// Keep failure detail separate from dispatch diagnostics.
	if to == execution.ResumeFailed || to == execution.ResumeCancelled {
		query = `UPDATE execution_operations SET state = ?, launch_phase = ?,
			failure_detail = ?, revision = revision + 1` + column +
			` WHERE id = ? AND revision = ? AND state = ?`
		args = []any{string(to), launchPhase, detail}
		if column != "" {
			args = append(args, now)
		}
		args = append(args, id.String(), expectedRevision, string(row.State))
	}
	var newRevision int64
	err = d.QueryRow(query+` RETURNING revision`, args...).Scan(&newRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("resume operation transition lost CAS for %s", id)
	}
	if err != nil {
		return err
	}
	if newRevision != expectedRevision+1 {
		return fmt.Errorf("resume operation transition returned revision %d, want %d", newRevision, expectedRevision+1)
	}
	return nil
}

// ActiveResumeOperations lists every nonterminal operation so ordinary manual
// resumes are reconciled independently of the recovery-episode table.
func ActiveResumeOperations() ([]ResumeOperationRow, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id FROM execution_operations
		WHERE state NOT IN ('ready', 'rejected', 'failed', 'cancelled')
		ORDER BY requested_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ResumeOperationRow
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		row, err := GetResumeOperation(execution.OperationID(raw))
		if err != nil {
			return nil, err
		}
		if row != nil {
			out = append(out, *row)
		}
	}
	return out, rows.Err()
}
