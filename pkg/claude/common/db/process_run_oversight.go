package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Run oversight (tk_e2fa2bd0). A run has one overseer: the agent (or the
// operator) that answers concerns about it, may pause and resume it, and
// escalates what it cannot settle. A concern is raised by anyone taking part
// in the run, or by the engine itself, and climbs the escalation chain —
// overseer, then the group's escalation agent, then the operator — until
// someone at its level resolves it. Every change is recorded in the run's
// evidence (process_run_events), so the run log shows who raised what, who
// it went to, and what was decided.

// Concern sources: who raised it.
const (
	ProcessConcernSourceAgent    = "agent"
	ProcessConcernSourceOperator = "operator"
	ProcessConcernSourceEngine   = "engine"
)

// Concern levels: where on the escalation chain it waits.
const (
	ProcessConcernLevelOverseer   = "overseer"
	ProcessConcernLevelEscalation = "escalation"
	ProcessConcernLevelOperator   = "operator"
)

const (
	ProcessConcernOpen     = "open"
	ProcessConcernResolved = "resolved"
)

// Evidence kinds the oversight layer records.
const (
	ProcessEventOverseerSet      = "overseer_set"
	ProcessEventConcernRaised    = "concern_raised"
	ProcessEventConcernEscalated = "concern_escalated"
	ProcessEventConcernResolved  = "concern_resolved"
)

const (
	MaxProcessConcernReason     = 4096
	MaxProcessConcernNote       = 4096
	MaxProcessConcernResolution = 64
	MaxProcessConcernSignal     = 64
	// MaxOpenProcessConcernsPerRun bounds the open concerns one run holds, so
	// a looping participant cannot flood its overseer or the operator.
	MaxOpenProcessConcernsPerRun = 16
	// MaxProcessConcernsListed bounds one listing of a run's concerns.
	MaxProcessConcernsListed = 64
)

var (
	ErrProcessConcernNotFound = errors.New("process run concern not found")
	ErrProcessConcernClosed   = errors.New("process run concern is already resolved")
	ErrProcessConcernLimit    = errors.New("process run has too many open concerns")
)

// ProcessRunConcern is one concern about a run.
type ProcessRunConcern struct {
	ID     int64
	RunID  string
	NodeID string
	// Source is agent, operator or engine; Signal names the engine's stuck
	// signal ("" for a raised concern).
	Source   string
	Signal   string
	RaisedBy string
	Reason   string
	State    string
	// Level is where it waits; Holder the actor ref that holds it there
	// ("agent:<id>" at the overseer and escalation levels, "" at the
	// operator). AnswerBy is when an unanswered holder is passed over (zero
	// at the operator, who is never passed over).
	Level          string
	Holder         string
	AnswerBy       time.Time
	HumanMessageID int64
	Resolution     string
	ResolvedBy     string
	Note           string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	ResolvedAt     time.Time
}

// Open reports whether nobody has resolved the concern yet.
func (c ProcessRunConcern) Open() bool { return c.State == ProcessConcernOpen }

// ProcessRunOverseer is a run's explicitly set overseer.
type ProcessRunOverseer struct {
	RunID    string
	Overseer string
	SetBy    string
	SetAt    time.Time
}

func validProcessActorRef(value string, allowEmpty bool) bool {
	return validProcessRuntimeText(value, MaxProcessRunEventActor, allowEmpty)
}

// GetProcessRunOverseer reads a run's explicit overseer; ok is false when it
// has none and takes the default.
func GetProcessRunOverseer(runID string) (ProcessRunOverseer, bool, error) {
	d, err := Open()
	if err != nil {
		return ProcessRunOverseer{}, false, err
	}
	var row ProcessRunOverseer
	var setAt dbTimestamp
	err = d.QueryRow(`SELECT run_id, overseer, set_by, set_at FROM process_run_overseers WHERE run_id = ?`, runID).
		Scan(&row.RunID, &row.Overseer, &row.SetBy, &setAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunOverseer{}, false, nil
	}
	if err != nil {
		return ProcessRunOverseer{}, false, err
	}
	row.SetAt = setAt.Time()
	return row, true, nil
}

// SetProcessRunOverseer sets (overseer != "") or clears a run's explicit
// overseer and records overseer_set evidence. Setting what is already set
// changes nothing and records nothing.
func SetProcessRunOverseer(runID, overseer, actor string) (bool, error) {
	if !validProcessRuntimeIdentifier(runID, MaxProcessRunIDBytes, false) {
		return false, fmt.Errorf("%w: invalid run id", ErrProcessRunInvalid)
	}
	if !validProcessActorRef(overseer, true) || !validProcessActorRef(actor, true) {
		return false, fmt.Errorf("%w: invalid overseer or actor", ErrProcessRunInvalid)
	}
	d, err := Open()
	if err != nil {
		return false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireProcessRunTx(tx, runID); err != nil {
		return false, err
	}
	var current string
	err = tx.QueryRow(`SELECT overseer FROM process_run_overseers WHERE run_id = ?`, runID).Scan(&current)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if current == overseer {
		return false, nil
	}
	now := time.Now().UTC()
	if overseer == "" {
		_, err = tx.Exec(`DELETE FROM process_run_overseers WHERE run_id = ?`, runID)
	} else {
		_, err = tx.Exec(`INSERT INTO process_run_overseers (run_id, overseer, set_by, set_at) VALUES (?, ?, ?, ?)
			ON CONFLICT(run_id) DO UPDATE SET overseer = excluded.overseer, set_by = excluded.set_by, set_at = excluded.set_at`,
			runID, overseer, actor, dbTime(now))
	}
	if err != nil {
		return false, fmt.Errorf("set process run overseer: %w", err)
	}
	if err := appendProcessOversightEventTx(tx, runID, "", ProcessEventOverseerSet, actor, now, map[string]any{
		"overseer": overseer, "previous": current,
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit process run overseer: %w", err)
	}
	return true, nil
}

func requireProcessRunTx(tx *sql.Tx, runID string) error {
	var one int
	err := tx.QueryRow(`SELECT 1 FROM process_runs WHERE id = ?`, runID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrProcessRunNotFound
	}
	return err
}

func appendProcessOversightEventTx(tx *sql.Tx, runID, nodeID, kind, actor string, at time.Time, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return appendProcessRunEventTx(tx, runID, ProcessRunEvent{
		OccurredAt: at, NodeID: nodeID, Kind: kind, PayloadJSON: raw, Actor: actor,
	})
}

// RaiseProcessRunConcern records a new open concern (ID, State, timestamps
// and resolution fields are set here) with concern_raised evidence, and
// returns it with its id. A concern on a finished run is refused, as is one
// past MaxOpenProcessConcernsPerRun.
func RaiseProcessRunConcern(c ProcessRunConcern) (ProcessRunConcern, error) {
	if err := validateProcessConcern(c); err != nil {
		return ProcessRunConcern{}, err
	}
	d, err := Open()
	if err != nil {
		return ProcessRunConcern{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return ProcessRunConcern{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var status string
	err = tx.QueryRow(`SELECT status FROM process_runs WHERE id = ?`, c.RunID).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunConcern{}, ErrProcessRunNotFound
	}
	if err != nil {
		return ProcessRunConcern{}, err
	}
	switch status {
	case ProcessRunStatusCompleted, ProcessRunStatusFailed, ProcessRunStatusCanceled:
		return ProcessRunConcern{}, fmt.Errorf("%w: run is %s", ErrProcessRunInvalid, status)
	}
	var open int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM process_run_concerns WHERE run_id = ? AND state = 'open'`, c.RunID).Scan(&open); err != nil {
		return ProcessRunConcern{}, err
	}
	if open >= MaxOpenProcessConcernsPerRun {
		return ProcessRunConcern{}, fmt.Errorf("%w: %d open; settle some first", ErrProcessConcernLimit, open)
	}
	now := time.Now().UTC()
	c.State, c.CreatedAt, c.UpdatedAt = ProcessConcernOpen, now, now
	c.Resolution, c.ResolvedBy, c.Note, c.ResolvedAt = "", "", "", time.Time{}
	result, err := tx.Exec(`INSERT INTO process_run_concerns
		(run_id, node_id, source, signal, raised_by, reason, state, level, holder, answer_by, human_message_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?)`,
		c.RunID, c.NodeID, c.Source, c.Signal, c.RaisedBy, c.Reason, c.Level, c.Holder,
		nullableDBTime(c.AnswerBy), c.HumanMessageID, dbTime(now), dbTime(now))
	if err != nil {
		return ProcessRunConcern{}, fmt.Errorf("raise process run concern: %w", err)
	}
	if c.ID, err = result.LastInsertId(); err != nil {
		return ProcessRunConcern{}, err
	}
	if err := appendProcessOversightEventTx(tx, c.RunID, c.NodeID, ProcessEventConcernRaised, c.RaisedBy, now, map[string]any{
		"concern": c.ID, "reason": c.Reason, "source": c.Source, "signal": c.Signal, "level": c.Level, "holder": c.Holder,
	}); err != nil {
		return ProcessRunConcern{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProcessRunConcern{}, fmt.Errorf("commit process run concern: %w", err)
	}
	return c, nil
}

func validateProcessConcern(c ProcessRunConcern) error {
	switch {
	case !validProcessRuntimeIdentifier(c.RunID, MaxProcessRunIDBytes, false):
		return fmt.Errorf("%w: invalid run id", ErrProcessRunInvalid)
	case !validProcessRuntimeNodeID(c.NodeID):
		return fmt.Errorf("%w: invalid node id", ErrProcessRunInvalid)
	case c.Source != ProcessConcernSourceAgent && c.Source != ProcessConcernSourceOperator && c.Source != ProcessConcernSourceEngine:
		return fmt.Errorf("%w: invalid concern source", ErrProcessRunInvalid)
	case !validProcessRuntimeIdentifier(c.Signal, MaxProcessConcernSignal, true):
		return fmt.Errorf("%w: invalid concern signal", ErrProcessRunInvalid)
	case !validProcessActorRef(c.RaisedBy, false) || !validProcessActorRef(c.Holder, true):
		return fmt.Errorf("%w: invalid actor", ErrProcessRunInvalid)
	case !validProcessConcernText(c.Reason, MaxProcessConcernReason, false):
		return fmt.Errorf("%w: a concern needs a reason of at most %d bytes", ErrProcessRunInvalid, MaxProcessConcernReason)
	case !validProcessConcernLevel(c.Level):
		return fmt.Errorf("%w: invalid concern level", ErrProcessRunInvalid)
	}
	return nil
}

func validProcessConcernLevel(level string) bool {
	return level == ProcessConcernLevelOverseer || level == ProcessConcernLevelEscalation || level == ProcessConcernLevelOperator
}

// validProcessConcernText admits free text with line breaks and tabs, the
// shape a reason or a note is typed in; other control characters are refused.
func validProcessConcernText(value string, maximum int, allowEmpty bool) bool {
	if strings.TrimSpace(value) == "" {
		return allowEmpty && value == ""
	}
	return validProcessRuntimeText(strings.NewReplacer("\n", " ", "\t", " ", "\r", " ").Replace(value), maximum, false)
}

const processConcernColumns = `id, run_id, node_id, source, signal, raised_by, reason, state, level, holder,
	answer_by, human_message_id, resolution, resolved_by, note, created_at, updated_at, resolved_at`

func scanProcessConcern(row interface{ Scan(...any) error }) (ProcessRunConcern, error) {
	var c ProcessRunConcern
	var answerBy, created, updated, resolved dbTimestamp
	if err := row.Scan(&c.ID, &c.RunID, &c.NodeID, &c.Source, &c.Signal, &c.RaisedBy, &c.Reason, &c.State,
		&c.Level, &c.Holder, &answerBy, &c.HumanMessageID, &c.Resolution, &c.ResolvedBy, &c.Note,
		&created, &updated, &resolved); err != nil {
		return ProcessRunConcern{}, err
	}
	c.AnswerBy, c.CreatedAt, c.UpdatedAt, c.ResolvedAt = answerBy.Time(), created.Time(), updated.Time(), resolved.Time()
	return c, nil
}

// GetProcessRunConcern reads one concern of a run.
func GetProcessRunConcern(runID string, id int64) (ProcessRunConcern, error) {
	d, err := Open()
	if err != nil {
		return ProcessRunConcern{}, err
	}
	c, err := scanProcessConcern(d.QueryRow(`SELECT `+processConcernColumns+` FROM process_run_concerns
		WHERE run_id = ? AND id = ?`, runID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunConcern{}, ErrProcessConcernNotFound
	}
	return c, err
}

// GetProcessRunConcernForHumanMessage finds the concern whose operator
// decision card is human message id; ok is false when it is no concern's.
func GetProcessRunConcernForHumanMessage(id int64) (ProcessRunConcern, bool, error) {
	if id <= 0 {
		return ProcessRunConcern{}, false, nil
	}
	d, err := Open()
	if err != nil {
		return ProcessRunConcern{}, false, err
	}
	c, err := scanProcessConcern(d.QueryRow(`SELECT `+processConcernColumns+` FROM process_run_concerns
		WHERE human_message_id = ? ORDER BY id DESC LIMIT 1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunConcern{}, false, nil
	}
	if err != nil {
		return ProcessRunConcern{}, false, err
	}
	return c, true, nil
}

// ListProcessRunConcerns returns a run's open concerns and its most recently
// resolved ones, oldest first, at most MaxProcessConcernsListed in all.
func ListProcessRunConcerns(runID string) ([]ProcessRunConcern, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+processConcernColumns+` FROM (
			SELECT * FROM process_run_concerns WHERE run_id = ?
			ORDER BY (state = 'open') DESC, id DESC LIMIT ?
		) ORDER BY id`, runID, MaxProcessConcernsListed)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ProcessRunConcern
	for rows.Next() {
		c, err := scanProcessConcern(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CountOpenProcessRunConcerns counts each listed run's open concerns, and
// how many of them wait on the operator.
func CountOpenProcessRunConcerns(runIDs []string) (open, operator map[string]int, err error) {
	open, operator = map[string]int{}, map[string]int{}
	if len(runIDs) == 0 {
		return open, operator, nil
	}
	d, err := Open()
	if err != nil {
		return nil, nil, err
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(runIDs)), ",")
	args := make([]any, len(runIDs))
	for i, id := range runIDs {
		args[i] = id
	}
	rows, err := d.Query(`SELECT run_id, COUNT(*), SUM(level = 'operator') FROM process_run_concerns
		WHERE state = 'open' AND run_id IN (`+placeholders+`) GROUP BY run_id`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		var n, ops int
		if err := rows.Scan(&id, &n, &ops); err != nil {
			return nil, nil, err
		}
		open[id], operator[id] = n, ops
	}
	return open, operator, rows.Err()
}

// ListDueProcessRunConcerns returns open concerns whose holder's answer time
// has passed, oldest due first.
func ListDueProcessRunConcerns(now time.Time, limit int) ([]ProcessRunConcern, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT `+processConcernColumns+` FROM process_run_concerns
		WHERE state = 'open' AND answer_by IS NOT NULL AND answer_by <= ?
		ORDER BY answer_by, id LIMIT ?`, dbTime(now), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ProcessRunConcern
	for rows.Next() {
		c, err := scanProcessConcern(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ProcessConcernEscalation moves an open concern up the chain.
type ProcessConcernEscalation struct {
	Level          string
	Holder         string
	AnswerBy       time.Time
	HumanMessageID int64
	// Why is what moved it: the holder's escalation note, or that the holder
	// did not answer.
	Why   string
	Actor string
	// FromLevel, when set, makes the move conditional on the concern still
	// waiting there: a sweep and a holder's answer racing for one concern
	// cannot both move it.
	FromLevel string
}

// EscalateProcessRunConcern moves an open concern to the next level and
// records concern_escalated evidence. It refuses a resolved concern, and one
// no longer at FromLevel.
func EscalateProcessRunConcern(runID string, id int64, e ProcessConcernEscalation) (ProcessRunConcern, error) {
	if !validProcessConcernLevel(e.Level) || !validProcessActorRef(e.Holder, true) || !validProcessActorRef(e.Actor, false) {
		return ProcessRunConcern{}, fmt.Errorf("%w: invalid escalation", ErrProcessRunInvalid)
	}
	e.Why = clipProcessConcernText(e.Why, MaxProcessConcernNote)
	d, err := Open()
	if err != nil {
		return ProcessRunConcern{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return ProcessRunConcern{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanProcessConcern(tx.QueryRow(`SELECT `+processConcernColumns+` FROM process_run_concerns
		WHERE run_id = ? AND id = ?`, runID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunConcern{}, ErrProcessConcernNotFound
	}
	if err != nil {
		return ProcessRunConcern{}, err
	}
	if !current.Open() {
		return ProcessRunConcern{}, ErrProcessConcernClosed
	}
	if e.FromLevel != "" && (current.Level != e.FromLevel) {
		return ProcessRunConcern{}, fmt.Errorf("%w: it moved to the %s", ErrProcessConcernClosed, current.Level)
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(`UPDATE process_run_concerns SET level = ?, holder = ?, answer_by = ?, human_message_id = ?, updated_at = ?
		WHERE id = ?`, e.Level, e.Holder, nullableDBTime(e.AnswerBy), e.HumanMessageID, dbTime(now), id); err != nil {
		return ProcessRunConcern{}, fmt.Errorf("escalate process run concern: %w", err)
	}
	if err := appendProcessOversightEventTx(tx, runID, current.NodeID, ProcessEventConcernEscalated, e.Actor, now, map[string]any{
		"concern": id, "from": current.Level, "level": e.Level, "holder": e.Holder, "why": e.Why,
	}); err != nil {
		return ProcessRunConcern{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProcessRunConcern{}, fmt.Errorf("commit process run concern escalation: %w", err)
	}
	current.Level, current.Holder, current.AnswerBy, current.HumanMessageID, current.UpdatedAt = e.Level, e.Holder, e.AnswerBy, e.HumanMessageID, now
	return current, nil
}

// SetProcessRunConcernMessage records the operator decision card a concern
// at the operator level was put on.
func SetProcessRunConcernMessage(runID string, id, messageID int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE process_run_concerns SET human_message_id = ? WHERE run_id = ? AND id = ? AND state = 'open'`,
		messageID, runID, id)
	return err
}

// ResolveProcessRunConcern closes an open concern with resolution (continue,
// paused, resume, skip, cancel, …) and records concern_resolved evidence.
// Only the first resolution counts: a resolved concern is refused.
func ResolveProcessRunConcern(runID string, id int64, resolution, note, actor string) (ProcessRunConcern, error) {
	if !validProcessRuntimeIdentifier(resolution, MaxProcessConcernResolution, false) || !validProcessActorRef(actor, false) {
		return ProcessRunConcern{}, fmt.Errorf("%w: invalid resolution", ErrProcessRunInvalid)
	}
	if !validProcessConcernText(note, MaxProcessConcernNote, true) {
		return ProcessRunConcern{}, fmt.Errorf("%w: a note is at most %d bytes", ErrProcessRunInvalid, MaxProcessConcernNote)
	}
	d, err := Open()
	if err != nil {
		return ProcessRunConcern{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return ProcessRunConcern{}, err
	}
	defer func() { _ = tx.Rollback() }()
	current, err := scanProcessConcern(tx.QueryRow(`SELECT `+processConcernColumns+` FROM process_run_concerns
		WHERE run_id = ? AND id = ?`, runID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunConcern{}, ErrProcessConcernNotFound
	}
	if err != nil {
		return ProcessRunConcern{}, err
	}
	if !current.Open() {
		return ProcessRunConcern{}, ErrProcessConcernClosed
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(`UPDATE process_run_concerns
		SET state = 'resolved', resolution = ?, resolved_by = ?, note = ?, answer_by = NULL, updated_at = ?, resolved_at = ?
		WHERE id = ?`, resolution, actor, note, dbTime(now), dbTime(now), id); err != nil {
		return ProcessRunConcern{}, fmt.Errorf("resolve process run concern: %w", err)
	}
	if err := appendProcessOversightEventTx(tx, runID, current.NodeID, ProcessEventConcernResolved, actor, now, map[string]any{
		"concern": id, "level": current.Level, "resolution": resolution, "note": note,
	}); err != nil {
		return ProcessRunConcern{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProcessRunConcern{}, fmt.Errorf("commit process run concern resolution: %w", err)
	}
	current.State, current.Resolution, current.ResolvedBy, current.Note = ProcessConcernResolved, resolution, actor, note
	current.AnswerBy, current.UpdatedAt, current.ResolvedAt = time.Time{}, now, now
	return current, nil
}

// clipProcessConcernText trims free text to at most limit bytes on a rune
// boundary, dropping control characters other than line breaks and tabs.
func clipProcessConcernText(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, strings.TrimSpace(value))
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8RuneStart(value[cut]) {
		cut--
	}
	return value[:cut]
}
