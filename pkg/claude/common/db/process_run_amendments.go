package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Run amendments (tk_e2fa2bd0 3c): changes to one run's own copy of its
// template. A proposal waits for the operator; an applied one swapped the
// run's template snapshot in the same transaction as its checkpoint
// (TransitionProcessRun with AmendmentID).

const (
	ProcessAmendmentProposed = "proposed"
	ProcessAmendmentApplied  = "applied"
	ProcessAmendmentRejected = "rejected"
	// ProcessAmendmentStale is a proposal the run moved on from before it was
	// decided: it was formed against a frontier the run has left.
	ProcessAmendmentStale = "stale"

	// MaxProposedProcessAmendmentsPerRun bounds proposals waiting on a run.
	MaxProposedProcessAmendmentsPerRun = 8
	// MaxProcessAmendmentRead bounds a run's amendments one read returns.
	MaxProcessAmendmentRead = 32
)

var (
	ErrProcessAmendmentNotFound = errors.New("process run amendment not found")
	ErrProcessAmendmentDecided  = errors.New("process run amendment was already decided")
	ErrProcessAmendmentLimit    = errors.New("too many amendments waiting on this run")
)

// ProcessRunAmendment is one proposed or decided change to a run's template.
type ProcessRunAmendment struct {
	ID               int64
	RunID            string
	State            string
	ProposedBy       string
	Note             string
	BaseStateVersion int64
	TemplateJSON     json.RawMessage
	SummaryJSON      json.RawMessage
	HumanMessageID   int64
	DecidedBy        string
	DecisionNote     string
	SavedTemplateRef string
	CreatedAt        time.Time
	DecidedAt        time.Time
}

const processAmendmentColumns = `id, run_id, state, proposed_by, note, base_state_version, template_json, summary_json,
	human_message_id, decided_by, decision_note, saved_template_ref, created_at, decided_at`

func scanProcessAmendment(row interface{ Scan(...any) error }) (ProcessRunAmendment, error) {
	var a ProcessRunAmendment
	var tmpl, summary string
	var created, decided dbTimestamp
	if err := row.Scan(&a.ID, &a.RunID, &a.State, &a.ProposedBy, &a.Note, &a.BaseStateVersion, &tmpl, &summary,
		&a.HumanMessageID, &a.DecidedBy, &a.DecisionNote, &a.SavedTemplateRef, &created, &decided); err != nil {
		return ProcessRunAmendment{}, err
	}
	a.TemplateJSON, a.SummaryJSON = json.RawMessage(tmpl), json.RawMessage(summary)
	a.CreatedAt, a.DecidedAt = created.Time(), decided.Time()
	return a, nil
}

// InsertProcessRunAmendment stores a proposal (state proposed) or the record
// of an amendment about to be applied, and returns its id.
func InsertProcessRunAmendment(a ProcessRunAmendment) (int64, error) {
	if err := validateProcessJSONObject("amendment template", a.TemplateJSON, MaxProcessRunCheckpointBytes); err != nil {
		return 0, err
	}
	if len(a.SummaryJSON) == 0 {
		a.SummaryJSON = json.RawMessage(`{}`)
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
	var waiting int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM process_run_amendments WHERE run_id = ? AND state = ?`,
		a.RunID, ProcessAmendmentProposed).Scan(&waiting); err != nil {
		return 0, err
	}
	if waiting >= MaxProposedProcessAmendmentsPerRun {
		return 0, ErrProcessAmendmentLimit
	}
	result, err := tx.Exec(`INSERT INTO process_run_amendments
		(run_id, state, proposed_by, note, base_state_version, template_json, summary_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		a.RunID, ProcessAmendmentProposed, a.ProposedBy, a.Note, a.BaseStateVersion,
		string(a.TemplateJSON), string(a.SummaryJSON), dbTime(time.Now().UTC()))
	if err != nil {
		return 0, fmt.Errorf("insert process run amendment: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// GetProcessRunAmendment reads one amendment of a run.
func GetProcessRunAmendment(runID string, id int64) (ProcessRunAmendment, error) {
	d, err := Open()
	if err != nil {
		return ProcessRunAmendment{}, err
	}
	a, err := scanProcessAmendment(d.QueryRow(`SELECT `+processAmendmentColumns+` FROM process_run_amendments
		WHERE run_id = ? AND id = ?`, runID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunAmendment{}, ErrProcessAmendmentNotFound
	}
	return a, err
}

// GetProcessRunAmendmentForHumanMessage finds the proposal whose operator
// card is human message id; ok is false when it is no amendment's.
func GetProcessRunAmendmentForHumanMessage(id int64) (ProcessRunAmendment, bool, error) {
	if id <= 0 {
		return ProcessRunAmendment{}, false, nil
	}
	d, err := Open()
	if err != nil {
		return ProcessRunAmendment{}, false, err
	}
	a, err := scanProcessAmendment(d.QueryRow(`SELECT `+processAmendmentColumns+` FROM process_run_amendments
		WHERE human_message_id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunAmendment{}, false, nil
	}
	return a, err == nil, err
}

// ListProcessRunAmendments returns a run's amendments, newest first, at most
// MaxProcessAmendmentRead.
func ListProcessRunAmendments(runID string) ([]ProcessRunAmendment, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+processAmendmentColumns+` FROM process_run_amendments
		WHERE run_id = ? ORDER BY id DESC LIMIT ?`, runID, MaxProcessAmendmentRead)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ProcessRunAmendment
	for rows.Next() {
		a, err := scanProcessAmendment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetProcessRunAmendmentMessage records the operator card a proposal waits on.
func SetProcessRunAmendmentMessage(id, messageID int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE process_run_amendments SET human_message_id = ? WHERE id = ?`, messageID, id)
	return err
}

// DecideProcessRunAmendment settles a proposal as rejected or stale. It fails
// with ErrProcessAmendmentDecided when it was decided already.
func DecideProcessRunAmendment(runID string, id int64, state, by, note string) error {
	if state != ProcessAmendmentRejected && state != ProcessAmendmentStale {
		return fmt.Errorf("%w: an amendment is decided rejected or stale here, not %q", ErrProcessRunInvalid, state)
	}
	d, err := Open()
	if err != nil {
		return err
	}
	result, err := d.Exec(`UPDATE process_run_amendments SET state = ?, decided_by = ?, decision_note = ?, decided_at = ?
		WHERE run_id = ? AND id = ? AND state = ?`, state, by, boundedDispatchText(note, 4096), dbTime(time.Now().UTC()),
		runID, id, ProcessAmendmentProposed)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrProcessAmendmentDecided
	}
	return nil
}

// SetProcessRunAmendmentSaved records the template version an applied
// amendment was saved back as.
func SetProcessRunAmendmentSaved(runID string, id int64, ref string) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE process_run_amendments SET saved_template_ref = ? WHERE run_id = ? AND id = ? AND state = ?`,
		ref, runID, id, ProcessAmendmentApplied)
	return err
}

// applyProcessRunAmendmentTx marks a proposal applied inside the run
// transition that swaps the run's template.
func applyProcessRunAmendmentTx(tx *sql.Tx, runID string, id int64, by string, snapshot json.RawMessage) error {
	if _, err := tx.Exec(`UPDATE process_runs SET template_snapshot_json = ? WHERE id = ?`, string(snapshot), runID); err != nil {
		return fmt.Errorf("swap process run template: %w", err)
	}
	result, err := tx.Exec(`UPDATE process_run_amendments SET state = ?, decided_by = ?, decided_at = ?
		WHERE run_id = ? AND id = ? AND state = ?`, ProcessAmendmentApplied, by, dbTime(time.Now().UTC()),
		runID, id, ProcessAmendmentProposed)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return ErrProcessAmendmentDecided
	}
	return nil
}
