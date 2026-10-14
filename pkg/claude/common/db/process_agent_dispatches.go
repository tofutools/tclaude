package db

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Process agent dispatch states. The run checkpoint decides whether an agent
// command is outstanding; the dispatch row records how it is being performed.
const (
	// ProcessDispatchPending: the command is known but no performer has been
	// resolved yet (just planned, or the previous performer was lost).
	ProcessDispatchPending = "pending"
	// ProcessDispatchNeedsPerformer: resolution found nobody in scope who may
	// take the node. It is retried on every sweep and surfaces to the human.
	ProcessDispatchNeedsPerformer = "needs_performer"
	// ProcessDispatchDelivered: the node prompt sits in the performer's inbox.
	ProcessDispatchDelivered = "delivered"
	// ProcessDispatchReported: the performer reported. The report is durable
	// here before it is applied to the run, so a crash in between is finished
	// by the next sweep instead of being lost.
	ProcessDispatchReported = "reported"
	// ProcessDispatchClosed: the command left the outbox without (or after)
	// this dispatch's report — a person settled it, or the run ended.
	ProcessDispatchClosed = "closed"
)

const (
	MaxProcessDispatchWaitReasonBytes = 1024
	MaxProcessDispatchReportNoteBytes = 4096
	MaxProcessDispatchReadPage        = 256
)

// ErrProcessDispatchConflict reports a compare-and-swap that found the dispatch
// in another state (or bound to another performer) than the caller expected.
var ErrProcessDispatchConflict = errors.New("process agent dispatch changed concurrently")

// ProcessAgentDispatch is one row of process_agent_dispatches: the performer
// binding of exactly one (run, command) — and therefore one node attempt.
type ProcessAgentDispatch struct {
	RunID         string
	CommandID     string
	NodeID        string
	Attempt       int
	State         string
	AgentID       string
	Resolution    string
	WaitReason    string
	MessageID     int64
	Deliveries    int
	ReportOutcome string
	ReportNote    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DeliveredAt   time.Time
	ReportedAt    time.Time
}

// Open reports whether the dispatch still has work left for the dispatcher.
func (d *ProcessAgentDispatch) Open() bool {
	return d != nil && d.State != ProcessDispatchClosed
}

const processAgentDispatchColumns = `run_id, command_id, node_id, attempt, state, agent_id, resolution,
	wait_reason, message_id, deliveries, report_outcome, report_note, created_at, updated_at,
	delivered_at, reported_at`

func scanProcessAgentDispatch(scanner processRunScanner) (*ProcessAgentDispatch, error) {
	var row ProcessAgentDispatch
	var created, updated, delivered, reported dbTimestamp
	if err := scanner.Scan(&row.RunID, &row.CommandID, &row.NodeID, &row.Attempt, &row.State,
		&row.AgentID, &row.Resolution, &row.WaitReason, &row.MessageID, &row.Deliveries,
		&row.ReportOutcome, &row.ReportNote, &created, &updated, &delivered, &reported); err != nil {
		return nil, err
	}
	row.CreatedAt, row.UpdatedAt = created.Time(), updated.Time()
	row.DeliveredAt, row.ReportedAt = delivered.Time(), reported.Time()
	return &row, nil
}

func queryProcessAgentDispatches(query string, args ...any) ([]ProcessAgentDispatch, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+processAgentDispatchColumns+` FROM process_agent_dispatches `+query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ProcessAgentDispatch
	for rows.Next() {
		row, err := scanProcessAgentDispatch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *row)
	}
	return out, rows.Err()
}

// EnsureProcessAgentDispatch records one outstanding agent command, idempotently:
// planning and every rediscovery sweep may call it for the same command, and only
// the first creates a pending row. It returns the current row.
func EnsureProcessAgentDispatch(runID, commandID, nodeID string, attempt int) (*ProcessAgentDispatch, error) {
	if runID == "" || commandID == "" || nodeID == "" || attempt < 1 {
		return nil, fmt.Errorf("%w: dispatch requires run, command, node, and positive attempt", ErrProcessRunInvalid)
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	now := dbTime(time.Now().UTC())
	if _, err := d.Exec(`INSERT INTO process_agent_dispatches
		(run_id, command_id, node_id, attempt, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, command_id) DO NOTHING`,
		runID, commandID, nodeID, attempt, ProcessDispatchPending, now, now); err != nil {
		return nil, fmt.Errorf("ensure process agent dispatch: %w", err)
	}
	return GetProcessAgentDispatch(runID, commandID)
}

// GetProcessAgentDispatch returns one dispatch, or nil when none exists.
func GetProcessAgentDispatch(runID, commandID string) (*ProcessAgentDispatch, error) {
	rows, err := queryProcessAgentDispatches(`WHERE run_id = ? AND command_id = ?`, runID, commandID)
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

// ListProcessAgentDispatchesForRun returns every dispatch of one run, oldest
// attempt first per node. A run's dispatches are bounded by its prepared nodes
// times their attempt budgets, which is small.
func ListProcessAgentDispatchesForRun(runID string) ([]ProcessAgentDispatch, error) {
	return queryProcessAgentDispatches(`WHERE run_id = ? ORDER BY node_id, attempt`, runID)
}

// ListProcessAgentDispatchesForNode returns every attempt's dispatch for one
// node of one run, oldest first.
func ListProcessAgentDispatchesForNode(runID, nodeID string) ([]ProcessAgentDispatch, error) {
	return queryProcessAgentDispatches(`WHERE run_id = ? AND node_id = ? ORDER BY attempt`, runID, nodeID)
}

// ListOpenProcessAgentDispatches returns one page of dispatches the dispatcher
// still has work for, in (run, command) order after the cursor.
func ListOpenProcessAgentDispatches(afterRunID, afterCommandID string, limit int) ([]ProcessAgentDispatch, error) {
	if limit <= 0 || limit > MaxProcessDispatchReadPage {
		return nil, fmt.Errorf("%w: dispatch page size must be 1..%d", ErrProcessRunInvalid, MaxProcessDispatchReadPage)
	}
	return queryProcessAgentDispatches(`WHERE state IN (?, ?, ?, ?)
		AND (run_id > ? OR (run_id = ? AND command_id > ?))
		ORDER BY run_id, command_id LIMIT ?`,
		ProcessDispatchPending, ProcessDispatchNeedsPerformer, ProcessDispatchDelivered, ProcessDispatchReported,
		afterRunID, afterRunID, afterCommandID, limit)
}

// ListProcessAgentDispatchesForAgent returns the dispatches one agent has been
// given, newest first. With openOnly it is the agent's current process work.
func ListProcessAgentDispatchesForAgent(agentID string, openOnly bool, limit int) ([]ProcessAgentDispatch, error) {
	if agentID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > MaxProcessDispatchReadPage {
		return nil, fmt.Errorf("%w: dispatch page size must be 1..%d", ErrProcessRunInvalid, MaxProcessDispatchReadPage)
	}
	if openOnly {
		return queryProcessAgentDispatches(`WHERE agent_id = ? AND state = ?
			ORDER BY updated_at DESC, run_id, command_id LIMIT ?`, agentID, ProcessDispatchDelivered, limit)
	}
	return queryProcessAgentDispatches(`WHERE agent_id = ?
		ORDER BY updated_at DESC, run_id, command_id LIMIT ?`, agentID, limit)
}

func execProcessDispatchCAS(query string, args ...any) error {
	d, err := Open()
	if err != nil {
		return err
	}
	result, err := d.Exec(query, args...)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrProcessDispatchConflict
	}
	return nil
}

// MarkProcessAgentDispatchNeedsPerformer parks an unresolved dispatch with a
// human-facing reason. It never moves a dispatch that already has a report.
func MarkProcessAgentDispatchNeedsPerformer(runID, commandID, reason string) error {
	return execProcessDispatchCAS(`UPDATE process_agent_dispatches
		SET state = ?, agent_id = '', resolution = '', wait_reason = ?, updated_at = ?
		WHERE run_id = ? AND command_id = ? AND state IN (?, ?, ?)`,
		ProcessDispatchNeedsPerformer, boundedDispatchText(reason, MaxProcessDispatchWaitReasonBytes),
		dbTime(time.Now().UTC()), runID, commandID,
		ProcessDispatchPending, ProcessDispatchNeedsPerformer, ProcessDispatchDelivered)
}

// MarkProcessAgentDispatchDelivered binds a dispatch to the performer whose
// inbox now holds the node prompt. expectedState is the state the caller
// resolved from, so two dispatchers can never both deliver one attempt.
func MarkProcessAgentDispatchDelivered(runID, commandID, expectedState, agentID, resolution string, messageID int64) error {
	if agentID == "" {
		return fmt.Errorf("%w: a delivered dispatch needs a performer", ErrProcessRunInvalid)
	}
	now := time.Now().UTC()
	return execProcessDispatchCAS(`UPDATE process_agent_dispatches
		SET state = ?, agent_id = ?, resolution = ?, wait_reason = '', message_id = ?,
			deliveries = deliveries + 1, delivered_at = ?, updated_at = ?
		WHERE run_id = ? AND command_id = ? AND state = ?`,
		ProcessDispatchDelivered, agentID, resolution, messageID, dbTime(now), dbTime(now),
		runID, commandID, expectedState)
}

// ReleaseProcessAgentDispatch returns a delivered dispatch whose performer was
// lost (retired, deleted, or no longer in scope) to pending, so it is resolved
// again. It is bound to the performer being released.
func ReleaseProcessAgentDispatch(runID, commandID, agentID, reason string) error {
	return execProcessDispatchCAS(`UPDATE process_agent_dispatches
		SET state = ?, agent_id = '', resolution = '', wait_reason = ?, updated_at = ?
		WHERE run_id = ? AND command_id = ? AND state = ? AND agent_id = ?`,
		ProcessDispatchPending, boundedDispatchText(reason, MaxProcessDispatchWaitReasonBytes),
		dbTime(time.Now().UTC()), runID, commandID, ProcessDispatchDelivered, agentID)
}

// RecordProcessAgentDispatchReport durably accepts one performer's report. It
// is a compare-and-swap on the exact performer: only the agent the attempt was
// delivered to can move it, and only once.
func RecordProcessAgentDispatchReport(runID, commandID, agentID, outcome, note string) error {
	now := time.Now().UTC()
	return execProcessDispatchCAS(`UPDATE process_agent_dispatches
		SET state = ?, report_outcome = ?, report_note = ?, reported_at = ?, updated_at = ?
		WHERE run_id = ? AND command_id = ? AND state = ? AND agent_id = ?`,
		ProcessDispatchReported, outcome, boundedDispatchText(note, MaxProcessDispatchReportNoteBytes),
		dbTime(now), dbTime(now), runID, commandID, ProcessDispatchDelivered, agentID)
}

// CloseProcessAgentDispatch retires a dispatch whose command left the outbox.
// The performer and any report stay for history and avatars.
func CloseProcessAgentDispatch(runID, commandID, reason string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE process_agent_dispatches
		SET state = ?, wait_reason = CASE WHEN ? = '' THEN wait_reason ELSE ? END, updated_at = ?
		WHERE run_id = ? AND command_id = ? AND state != ?`,
		ProcessDispatchClosed, reason, boundedDispatchText(reason, MaxProcessDispatchWaitReasonBytes),
		dbTime(time.Now().UTC()), runID, commandID, ProcessDispatchClosed)
	return err
}

// ListProcessRunIDsWithAgentCommands returns one page of live runs whose
// checkpoint holds at least one outstanding agent command. It is the
// rediscovery source for a dispatch row lost between a planning commit and its
// row insert.
func ListProcessRunIDsWithAgentCommands(afterID string, limit int) ([]string, string, error) {
	if limit <= 0 || limit > MaxProcessRunReadPage {
		return nil, "", fmt.Errorf("%w: page size must be 1..%d", ErrProcessRunInvalid, MaxProcessRunReadPage)
	}
	d, err := Open()
	if err != nil {
		return nil, "", err
	}
	rows, err := d.Query(`SELECT id,
		CASE WHEN json_valid(checkpoint_json) = 1 AND json_type(checkpoint_json, '$.commands') = 'array'
			THEN (SELECT COUNT(*) FROM json_each(checkpoint_json, '$.commands')
				WHERE json_extract(value, '$.kind') = 'agent')
			ELSE 0 END
		FROM (
			SELECT id, checkpoint_json FROM process_runs
			WHERE id > ? AND status NOT IN ('completed', 'failed', 'canceled')
			ORDER BY id LIMIT ?
		) AS page ORDER BY id`, afterID, limit)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	last, examined := "", 0
	for rows.Next() {
		var id string
		var agents int
		if err := rows.Scan(&id, &agents); err != nil {
			return nil, "", err
		}
		examined++
		last = id
		if agents > 0 {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	if examined < limit {
		last = ""
	}
	return ids, last, nil
}

func boundedDispatchText(value string, maximum int) string {
	value = strings.ToValidUTF8(value, "?")
	if len(value) <= maximum {
		return value
	}
	cut := maximum
	for cut > 0 && !utf8RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}

func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }
