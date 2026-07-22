package db

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/strictjson"
)

const (
	InitialProcessRunStateVersion = int64(1)

	// Runtime JSON uses the same 4 MiB scale as template authoring. Params and
	// one evidence payload use the existing 256 KiB process-snippet envelope
	// scale. Per-call/page caps keep one daemon request bounded while cursors
	// still allow arbitrarily long-lived runs and any number of active runs.
	MaxProcessRunTemplateSnapshotBytes = 4 << 20
	MaxProcessRunCheckpointBytes       = 4 << 20
	MaxProcessRunParamsBytes           = 256 << 10
	MaxProcessRunAuthorizationsBytes   = 256 << 10
	MaxProcessRunEventPayloadBytes     = 256 << 10
	MaxProcessRunEventsPerTransition   = 256
	MaxProcessRunReadPage              = 32
	MaxProcessRunEventReadPage         = 256

	MaxProcessRunIDBytes               = 128
	MaxProcessRunTemplateRef           = 512
	MaxProcessRunStatusBytes           = 64
	MaxProcessRunNodeIDBytes           = 256
	MaxProcessRunEventKind             = 128
	MaxProcessRunEventActor            = 256
	MaxProcessRunAuthorizationProfiles = model.MaxNormalizedNodes
	MaxProcessRunAuthorizationProfile  = MaxProcessRunAuthorizationsBytes - 4
	maxProcessRunTimestampSize         = 64
)

const (
	ProcessRunStatusCompleted = "completed"
	ProcessRunStatusFailed    = "failed"
	ProcessRunStatusCanceled  = "canceled"
)

var (
	ErrProcessRunNotFound           = errors.New("process run not found")
	ErrProcessRunExists             = errors.New("process run already exists")
	ErrProcessRunVersionConflict    = errors.New("process run state version conflict")
	ErrProcessRunEventSequence      = errors.New("process run evidence sequence conflict")
	ErrProcessRunInvalid            = errors.New("invalid process run data")
	ErrProcessRunCorrupt            = errors.New("process run store is inconsistent")
	processRuntimeIdentifierPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
)

// ProcessRunVersionConflictError reports both sides of a failed checkpoint
// compare-and-swap. Callers may use errors.Is with ErrProcessRunVersionConflict.
type ProcessRunVersionConflictError struct {
	Expected int64
	Actual   int64
}

func (e *ProcessRunVersionConflictError) Error() string {
	return fmt.Sprintf("%v: expected %d, found %d", ErrProcessRunVersionConflict, e.Expected, e.Actual)
}

func (e *ProcessRunVersionConflictError) Unwrap() error { return ErrProcessRunVersionConflict }

// ProcessRun is the canonical cold-load record. CheckpointJSON is returned
// exactly as stored; evidence is intentionally absent and has a separate,
// paginated reader because it is not a state reconstruction source.
type ProcessRun struct {
	ID                        string
	TemplateRef               string
	TemplateSnapshotJSON      json.RawMessage
	ParamsJSON                json.RawMessage
	ProgramAuthorizationsJSON json.RawMessage
	Status                    string
	StateVersion              int64
	CheckpointJSON            json.RawMessage
	CreatedAt                 time.Time
	UpdatedAt                 time.Time
}

// ProcessRunSummary is the bounded metadata projection used by aggregate
// reads. It deliberately excludes every potentially multi-megabyte JSON
// column; callers that need run state must use GetProcessRun for one ID.
type ProcessRunSummary struct {
	ID           string
	TemplateRef  string
	Status       string
	StateVersion int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ProcessRunCreate is the immutable and initial mutable state committed when a
// run is created. InitialEvents, when present, commit with the checkpoint.
type ProcessRunCreate struct {
	ID                        string
	TemplateRef               string
	TemplateSnapshotJSON      json.RawMessage
	ParamsJSON                json.RawMessage
	ProgramAuthorizationsJSON json.RawMessage
	Status                    string
	CheckpointJSON            json.RawMessage
	InitialEvents             []ProcessRunEvent
}

// ProcessRunEvent is one append-only human-facing evidence row. Sequence is
// positive and monotonically increasing within the run. A transition may leave
// every event sequence zero to assign the next sequence(s) transactionally.
type ProcessRunEvent struct {
	RunID       string
	Sequence    int64
	OccurredAt  time.Time
	NodeID      string
	Kind        string
	PayloadJSON json.RawMessage
	Actor       string
}

// ProcessRunTransition is the only checkpoint mutation shape. The store
// increments ExpectedStateVersion by one and commits every evidence row in the
// same SQLite transaction.
type ProcessRunTransition struct {
	ExpectedStateVersion int64
	Status               string
	CheckpointJSON       json.RawMessage
	Events               []ProcessRunEvent
}

// NewProcessRunID returns a locally unique, filesystem-safe run identifier.
func NewProcessRunID() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("db: crypto/rand failed generating process run id: " + err.Error())
	}
	return "run_" + hex.EncodeToString(value[:])
}

// DecodeCheckpoint strictly decodes the stored checkpoint into dst. Unknown
// fields are rejected for typed destinations, as are malformed or trailing
// JSON values.
func (r *ProcessRun) DecodeCheckpoint(dst any) error {
	if r == nil {
		return fmt.Errorf("%w: nil process run", ErrProcessRunInvalid)
	}
	return decodeBoundedProcessJSON("checkpoint", r.CheckpointJSON, MaxProcessRunCheckpointBytes, dst)
}

// DecodeParams strictly decodes the run's creation-time parameters into dst.
func (r *ProcessRun) DecodeParams(dst any) error {
	if r == nil {
		return fmt.Errorf("%w: nil process run", ErrProcessRunInvalid)
	}
	return decodeBoundedProcessJSON("params", r.ParamsJSON, MaxProcessRunParamsBytes, dst)
}

// DecodeProgramAuthorizations decodes the immutable, explicit program
// profiles authorized when the run was created. An empty list authorizes no
// program, including a command whose profile is empty.
func (r *ProcessRun) DecodeProgramAuthorizations(dst *[]string) error {
	if r == nil {
		return fmt.Errorf("%w: nil process run", ErrProcessRunInvalid)
	}
	return decodeProcessRunAuthorizations(r.ProgramAuthorizationsJSON, dst)
}

// DecodePayload strictly decodes an evidence payload into dst.
func (e *ProcessRunEvent) DecodePayload(dst any) error {
	if e == nil {
		return fmt.Errorf("%w: nil process run event", ErrProcessRunInvalid)
	}
	return decodeBoundedProcessJSON("event payload", e.PayloadJSON, MaxProcessRunEventPayloadBytes, dst)
}

func decodeBoundedProcessJSON(name string, data []byte, maximum int, dst any) error {
	if dst == nil {
		return fmt.Errorf("%w: %s decode destination is nil", ErrProcessRunInvalid, name)
	}
	if len(data) == 0 || len(data) > maximum {
		return fmt.Errorf("%w: %s must contain 1..%d bytes", ErrProcessRunInvalid, name, maximum)
	}
	if !utf8.Valid(data) {
		return fmt.Errorf("%w: %s contains invalid UTF-8", ErrProcessRunInvalid, name)
	}
	if err := strictjson.Decode(data, dst); err != nil {
		return fmt.Errorf("%w: decode %s: %v", ErrProcessRunInvalid, name, err)
	}
	return nil
}

func validateProcessJSONObject(name string, data []byte, maximum int) error {
	var object map[string]json.RawMessage
	if err := decodeBoundedProcessJSON(name, data, maximum, &object); err != nil {
		return err
	}
	if object == nil {
		return fmt.Errorf("%w: %s must be a JSON object", ErrProcessRunInvalid, name)
	}
	return nil
}

func decodeProcessRunAuthorizations(data []byte, dst *[]string) error {
	if dst == nil {
		return fmt.Errorf("%w: program authorizations decode destination is nil", ErrProcessRunInvalid)
	}
	if len(data) < 2 || len(data) > MaxProcessRunAuthorizationsBytes || !utf8.Valid(data) {
		return fmt.Errorf("%w: program authorizations must contain 2..%d bytes of UTF-8", ErrProcessRunInvalid, MaxProcessRunAuthorizationsBytes)
	}
	var profiles []string
	if err := strictjson.Decode(data, &profiles); err != nil || profiles == nil {
		return fmt.Errorf("%w: decode program authorizations", ErrProcessRunInvalid)
	}
	if len(profiles) > MaxProcessRunAuthorizationProfiles {
		return fmt.Errorf("%w: at most %d program authorization profiles are allowed", ErrProcessRunInvalid, MaxProcessRunAuthorizationProfiles)
	}
	seen := make(map[string]struct{}, len(profiles))
	for _, profile := range profiles {
		if !validProcessRuntimeText(profile, MaxProcessRunAuthorizationProfile, true) {
			return fmt.Errorf("%w: invalid program authorization profile", ErrProcessRunInvalid)
		}
		if _, exists := seen[profile]; exists {
			return fmt.Errorf("%w: duplicate program authorization profile %q", ErrProcessRunInvalid, profile)
		}
		seen[profile] = struct{}{}
	}
	*dst = profiles
	return nil
}

func validateProcessTemplateSnapshotCreate(ref string, snapshot []byte) error {
	if len(ref) == 0 || len(ref) > MaxProcessRunTemplateRef {
		return fmt.Errorf("%w: template ref must contain 1..%d bytes", ErrProcessRunInvalid, MaxProcessRunTemplateRef)
	}
	var tmpl model.Template
	if err := decodeBoundedProcessJSON("template snapshot", snapshot, MaxProcessRunTemplateSnapshotBytes, &tmpl); err != nil {
		return err
	}
	canonical, err := model.CanonicalSemanticJSON(&tmpl)
	if err != nil {
		return fmt.Errorf("%w: canonicalize template snapshot: %v", ErrProcessRunInvalid, err)
	}
	if !bytes.Equal(snapshot, canonical) {
		return fmt.Errorf("%w: template snapshot must be canonical semantic JSON", ErrProcessRunInvalid)
	}
	hash, err := model.SemanticHash(&tmpl)
	if err != nil {
		return fmt.Errorf("%w: hash template snapshot: %v", ErrProcessRunInvalid, err)
	}
	if expected := model.TemplateRef(tmpl.ID, hash); ref != expected {
		return fmt.Errorf("%w: template ref %q does not match snapshot %q", ErrProcessRunInvalid, ref, expected)
	}
	return nil
}

func validProcessRuntimeIdentifier(value string, maximum int, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	return len(value) <= maximum && processRuntimeIdentifierPattern.MatchString(value)
}

func validProcessRuntimeText(value string, maximum int, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validProcessRunEventTime(value time.Time) bool {
	if value.IsZero() {
		return false
	}
	formatted := value.UTC().Format(time.RFC3339Nano)
	parsed, err := time.Parse(time.RFC3339Nano, formatted)
	return err == nil && parsed.Equal(value.UTC())
}

func validateProcessRunCreate(input ProcessRunCreate) error {
	if !validProcessRuntimeIdentifier(input.ID, MaxProcessRunIDBytes, false) {
		return fmt.Errorf("%w: invalid run id", ErrProcessRunInvalid)
	}
	if !validProcessRuntimeIdentifier(input.Status, MaxProcessRunStatusBytes, false) {
		return fmt.Errorf("%w: invalid run status", ErrProcessRunInvalid)
	}
	if err := validateProcessTemplateSnapshotCreate(input.TemplateRef, input.TemplateSnapshotJSON); err != nil {
		return err
	}
	if err := validateProcessJSONObject("params", input.ParamsJSON, MaxProcessRunParamsBytes); err != nil {
		return err
	}
	var authorizations []string
	if err := decodeProcessRunAuthorizations(input.ProgramAuthorizationsJSON, &authorizations); err != nil {
		return err
	}
	if err := validateProcessJSONObject("checkpoint", input.CheckpointJSON, MaxProcessRunCheckpointBytes); err != nil {
		return err
	}
	return validateProcessRunEvents(input.InitialEvents, false)
}

func validateProcessRunEvents(events []ProcessRunEvent, allowAutoSequence bool) error {
	if len(events) > MaxProcessRunEventsPerTransition {
		return fmt.Errorf("%w: at most %d evidence events may commit per transition", ErrProcessRunInvalid, MaxProcessRunEventsPerTransition)
	}
	autoSequence := allowAutoSequence && len(events) > 0 && events[0].Sequence == 0
	var prior int64
	for index, event := range events {
		if event.RunID != "" {
			return fmt.Errorf("%w: event %d must not set run id", ErrProcessRunInvalid, index)
		}
		if autoSequence && event.Sequence != 0 {
			return fmt.Errorf("%w: transition evidence sequences must be all zero or all caller-assigned", ErrProcessRunEventSequence)
		}
		if !autoSequence && (event.Sequence <= 0 || (index > 0 && event.Sequence <= prior)) {
			return fmt.Errorf("%w: evidence sequences must be positive and strictly increasing", ErrProcessRunEventSequence)
		}
		if !validProcessRunEventTime(event.OccurredAt) {
			return fmt.Errorf("%w: event %d occurrence time must be representable as RFC3339", ErrProcessRunInvalid, index)
		}
		if !validProcessRuntimeIdentifier(event.NodeID, MaxProcessRunNodeIDBytes, true) {
			return fmt.Errorf("%w: invalid event node id", ErrProcessRunInvalid)
		}
		if !validProcessRuntimeIdentifier(event.Kind, MaxProcessRunEventKind, false) {
			return fmt.Errorf("%w: invalid event kind", ErrProcessRunInvalid)
		}
		if !validProcessRuntimeText(event.Actor, MaxProcessRunEventActor, true) {
			return fmt.Errorf("%w: invalid event actor", ErrProcessRunInvalid)
		}
		if err := validateProcessJSONObject("event payload", event.PayloadJSON, MaxProcessRunEventPayloadBytes); err != nil {
			return err
		}
		prior = event.Sequence
	}
	return nil
}

// CreateProcessRun atomically inserts the initial canonical checkpoint and any
// initial evidence. The state version always starts at one.
func CreateProcessRun(input ProcessRunCreate) error {
	if len(input.ProgramAuthorizationsJSON) == 0 {
		// Pre-runtime integration callers construct no authorization field. Keep
		// that source-compatible while failing closed: an omitted value means the
		// explicit empty authorization set, never authorization inferred from the
		// template.
		input.ProgramAuthorizationsJSON = json.RawMessage(`[]`)
	}
	if err := validateProcessRunCreate(input); err != nil {
		return err
	}
	d, err := Open()
	if err != nil {
		return err
	}
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.Exec(`INSERT INTO process_runs
		(id, template_ref, template_snapshot_json, params_json, program_authorizations_json, status,
		 state_version, checkpoint_json, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO NOTHING`,
		input.ID, input.TemplateRef, string(input.TemplateSnapshotJSON), string(input.ParamsJSON), string(input.ProgramAuthorizationsJSON), input.Status,
		InitialProcessRunStateVersion, string(input.CheckpointJSON), now, now)
	if err != nil {
		return fmt.Errorf("create process run: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return ErrProcessRunExists
	}
	if err := insertProcessRunEvents(tx, input.ID, input.InitialEvents); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit process run creation: %w", err)
	}
	return nil
}

// TransitionProcessRun performs one optimistic checkpoint transition. A stale
// expected version, evidence conflict, or any other error rolls back the
// checkpoint/status/version update together with every evidence insert.
func TransitionProcessRun(runID string, transition ProcessRunTransition) (int64, error) {
	if !validProcessRuntimeIdentifier(runID, MaxProcessRunIDBytes, false) {
		return 0, fmt.Errorf("%w: invalid run id", ErrProcessRunInvalid)
	}
	if transition.ExpectedStateVersion <= 0 || transition.ExpectedStateVersion == math.MaxInt64 {
		return 0, fmt.Errorf("%w: invalid expected state version", ErrProcessRunInvalid)
	}
	if !validProcessRuntimeIdentifier(transition.Status, MaxProcessRunStatusBytes, false) {
		return 0, fmt.Errorf("%w: invalid run status", ErrProcessRunInvalid)
	}
	if err := validateProcessJSONObject("checkpoint", transition.CheckpointJSON, MaxProcessRunCheckpointBytes); err != nil {
		return 0, err
	}
	if err := validateProcessRunEvents(transition.Events, true); err != nil {
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

	nextVersion := transition.ExpectedStateVersion + 1
	result, err := tx.Exec(`UPDATE process_runs
		SET status = ?, state_version = ?, checkpoint_json = ?, updated_at = ?
		WHERE id = ? AND state_version = ?`, transition.Status, nextVersion,
		string(transition.CheckpointJSON), time.Now().UTC().Format(time.RFC3339Nano),
		runID, transition.ExpectedStateVersion)
	if err != nil {
		return 0, fmt.Errorf("update process run checkpoint: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if rows != 1 {
		var actual int64
		if err := tx.QueryRow(`SELECT state_version FROM process_runs WHERE id = ?`, runID).Scan(&actual); errors.Is(err, sql.ErrNoRows) {
			return 0, ErrProcessRunNotFound
		} else if err != nil {
			return 0, err
		}
		return 0, &ProcessRunVersionConflictError{Expected: transition.ExpectedStateVersion, Actual: actual}
	}
	events := transition.Events
	if len(events) > 0 {
		var lastSequence int64
		if err := tx.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM process_run_events WHERE run_id = ?`, runID).Scan(&lastSequence); err != nil {
			return 0, err
		}
		if events[0].Sequence == 0 {
			if lastSequence > math.MaxInt64-int64(len(events)) {
				return 0, fmt.Errorf("%w: evidence sequence exhausted", ErrProcessRunEventSequence)
			}
			events = append([]ProcessRunEvent(nil), events...)
			for index := range events {
				events[index].Sequence = lastSequence + int64(index) + 1
			}
		} else if events[0].Sequence <= lastSequence {
			return 0, fmt.Errorf("%w: next sequence %d is not after %d", ErrProcessRunEventSequence, events[0].Sequence, lastSequence)
		}
	}
	if err := insertProcessRunEvents(tx, runID, events); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit process run transition: %w", err)
	}
	return nextVersion, nil
}

func insertProcessRunEvents(tx *sql.Tx, runID string, events []ProcessRunEvent) error {
	for _, event := range events {
		result, err := tx.Exec(`INSERT INTO process_run_events
			(run_id, sequence, occurred_at, node_id, kind, payload_json, actor)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(run_id, sequence) DO NOTHING`, runID, event.Sequence,
			event.OccurredAt.UTC().Format(time.RFC3339Nano), event.NodeID, event.Kind,
			string(event.PayloadJSON), event.Actor)
		if err != nil {
			return fmt.Errorf("append process run evidence sequence %d: %w", event.Sequence, err)
		}
		rows, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rows != 1 {
			return fmt.Errorf("%w: sequence %d", ErrProcessRunEventSequence, event.Sequence)
		}
	}
	return nil
}

type processRunScanner interface{ Scan(...any) error }

func scanProcessRun(scanner processRunScanner) (*ProcessRun, error) {
	var run ProcessRun
	var id, templateRef, snapshot, params, authorizations, status, checkpoint, created, updated sql.NullString
	var stateVersion sql.NullInt64
	if err := scanner.Scan(&id, &templateRef, &snapshot, &params, &authorizations, &status,
		&stateVersion, &checkpoint, &created, &updated); err != nil {
		return nil, err
	}
	if !id.Valid || !templateRef.Valid || !snapshot.Valid || !params.Valid || !authorizations.Valid || !status.Valid ||
		!stateVersion.Valid || !checkpoint.Valid || !created.Valid || !updated.Valid {
		return nil, ErrProcessRunCorrupt
	}
	run.ID, run.TemplateRef, run.Status = id.String, templateRef.String, status.String
	run.StateVersion = stateVersion.Int64
	if !validProcessRuntimeIdentifier(run.ID, MaxProcessRunIDBytes, false) ||
		!validProcessRuntimeIdentifier(run.Status, MaxProcessRunStatusBytes, false) ||
		run.StateVersion <= 0 {
		return nil, ErrProcessRunCorrupt
	}
	if err := validateProcessJSONObject("params", []byte(params.String), MaxProcessRunParamsBytes); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProcessRunCorrupt, err)
	}
	var authorizationProfiles []string
	if err := decodeProcessRunAuthorizations([]byte(authorizations.String), &authorizationProfiles); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProcessRunCorrupt, err)
	}
	if err := validateProcessJSONObject("checkpoint", []byte(checkpoint.String), MaxProcessRunCheckpointBytes); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrProcessRunCorrupt, err)
	}
	var err error
	if run.CreatedAt, err = time.Parse(time.RFC3339Nano, created.String); err != nil {
		return nil, fmt.Errorf("%w: invalid created timestamp", ErrProcessRunCorrupt)
	}
	if run.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated.String); err != nil {
		return nil, fmt.Errorf("%w: invalid updated timestamp", ErrProcessRunCorrupt)
	}
	run.TemplateSnapshotJSON = json.RawMessage(strings.Clone(snapshot.String))
	run.ParamsJSON = json.RawMessage(strings.Clone(params.String))
	run.ProgramAuthorizationsJSON = json.RawMessage(strings.Clone(authorizations.String))
	run.CheckpointJSON = json.RawMessage(strings.Clone(checkpoint.String))
	return &run, nil
}

// Every variable-sized column is length-gated inside SQLite before the driver
// receives it. CAST(... AS BLOB) makes length count bytes, not Unicode code
// points. A corrupt oversized value becomes NULL and scanProcessRun fails
// closed without materializing it in Go.
const processRunSelect = `SELECT
	CASE WHEN typeof(id) = 'text' AND length(CAST(id AS BLOB)) BETWEEN 1 AND ? THEN id END,
	CASE WHEN typeof(template_ref) = 'text' AND length(CAST(template_ref AS BLOB)) BETWEEN 1 AND ? THEN template_ref END,
	CASE WHEN typeof(template_snapshot_json) = 'text' AND length(CAST(template_snapshot_json AS BLOB)) BETWEEN 1 AND ? THEN template_snapshot_json END,
	CASE WHEN typeof(params_json) = 'text' AND length(CAST(params_json AS BLOB)) BETWEEN 1 AND ? THEN params_json END,
	CASE WHEN typeof(program_authorizations_json) = 'text' AND length(CAST(program_authorizations_json AS BLOB)) BETWEEN 2 AND ? THEN program_authorizations_json END,
	CASE WHEN typeof(status) = 'text' AND length(CAST(status AS BLOB)) BETWEEN 1 AND ? THEN status END,
	CASE WHEN typeof(state_version) = 'integer' AND state_version > 0 THEN state_version END,
	CASE WHEN typeof(checkpoint_json) = 'text' AND length(CAST(checkpoint_json AS BLOB)) BETWEEN 1 AND ? THEN checkpoint_json END,
	CASE WHEN typeof(created_at) = 'text' AND length(CAST(created_at AS BLOB)) BETWEEN 1 AND ? THEN created_at END,
	CASE WHEN typeof(updated_at) = 'text' AND length(CAST(updated_at AS BLOB)) BETWEEN 1 AND ? THEN updated_at END
	FROM process_runs`

func processRunSelectArgs() []any {
	return []any{
		MaxProcessRunIDBytes, MaxProcessRunTemplateRef, MaxProcessRunTemplateSnapshotBytes,
		MaxProcessRunParamsBytes, MaxProcessRunAuthorizationsBytes, MaxProcessRunStatusBytes, MaxProcessRunCheckpointBytes,
		maxProcessRunTimestampSize, maxProcessRunTimestampSize,
	}
}

// GetProcessRun reads the one canonical checkpoint row. It never consults the
// evidence table. A missing run returns ErrProcessRunNotFound.
func GetProcessRun(runID string) (*ProcessRun, error) {
	if !validProcessRuntimeIdentifier(runID, MaxProcessRunIDBytes, false) {
		return nil, fmt.Errorf("%w: invalid run id", ErrProcessRunInvalid)
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	args := append(processRunSelectArgs(), runID)
	run, err := scanProcessRun(d.QueryRow(processRunSelect+` WHERE id = ?`, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrProcessRunNotFound
	}
	return run, err
}

// ListActiveProcessRuns returns a bounded ID-ordered page of canonical
// checkpoints. afterID is an exclusive cursor; pass "" for the first page.
func ListActiveProcessRuns(afterID string, limit int) ([]ProcessRun, error) {
	if afterID != "" && !validProcessRuntimeIdentifier(afterID, MaxProcessRunIDBytes, false) {
		return nil, fmt.Errorf("%w: invalid active-run cursor", ErrProcessRunInvalid)
	}
	if limit <= 0 || limit > MaxProcessRunReadPage {
		return nil, fmt.Errorf("%w: active-run page size must be 1..%d", ErrProcessRunInvalid, MaxProcessRunReadPage)
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	args := append(processRunSelectArgs(), afterID, limit)
	rows, err := d.Query(processRunSelect+`
		WHERE id > ? AND status NOT IN ('completed', 'failed', 'canceled')
		ORDER BY id LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	runs := make([]ProcessRun, 0, limit)
	for rows.Next() {
		run, err := scanProcessRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	return runs, rows.Err()
}

// ListProcessRunSummaries returns one bounded ID-ordered metadata page,
// including terminal runs. It never selects snapshots, params,
// authorizations, or checkpoints.
func ListProcessRunSummaries(afterID string, limit int) ([]ProcessRunSummary, error) {
	if afterID != "" && !validProcessRuntimeIdentifier(afterID, MaxProcessRunIDBytes, false) {
		return nil, fmt.Errorf("%w: invalid run cursor", ErrProcessRunInvalid)
	}
	if limit <= 0 || limit > MaxProcessRunReadPage {
		return nil, fmt.Errorf("%w: run page size must be 1..%d", ErrProcessRunInvalid, MaxProcessRunReadPage)
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT
		CASE WHEN typeof(id) = 'text' AND length(CAST(id AS BLOB)) BETWEEN 1 AND ? THEN id END,
		CASE WHEN typeof(template_ref) = 'text' AND length(CAST(template_ref AS BLOB)) BETWEEN 1 AND ? THEN template_ref END,
		CASE WHEN typeof(status) = 'text' AND length(CAST(status AS BLOB)) BETWEEN 1 AND ? THEN status END,
		CASE WHEN typeof(state_version) = 'integer' AND state_version > 0 THEN state_version END,
		CASE WHEN typeof(created_at) = 'text' AND length(CAST(created_at AS BLOB)) BETWEEN 1 AND ? THEN created_at END,
		CASE WHEN typeof(updated_at) = 'text' AND length(CAST(updated_at AS BLOB)) BETWEEN 1 AND ? THEN updated_at END
		FROM process_runs WHERE id > ? ORDER BY id LIMIT ?`,
		MaxProcessRunIDBytes, MaxProcessRunTemplateRef, MaxProcessRunStatusBytes,
		maxProcessRunTimestampSize, maxProcessRunTimestampSize, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	runs := make([]ProcessRunSummary, 0, limit)
	for rows.Next() {
		var id, templateRef, status, createdAt, updatedAt sql.NullString
		var stateVersion sql.NullInt64
		if err := rows.Scan(&id, &templateRef, &status, &stateVersion, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		if !id.Valid || !templateRef.Valid || !status.Valid || !stateVersion.Valid || !createdAt.Valid || !updatedAt.Valid ||
			!validProcessRuntimeIdentifier(id.String, MaxProcessRunIDBytes, false) ||
			!validProcessRuntimeText(templateRef.String, MaxProcessRunTemplateRef, false) ||
			!validProcessRuntimeIdentifier(status.String, MaxProcessRunStatusBytes, false) {
			return nil, ErrProcessRunCorrupt
		}
		created, err := time.Parse(time.RFC3339Nano, createdAt.String)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid run created timestamp", ErrProcessRunCorrupt)
		}
		updated, err := time.Parse(time.RFC3339Nano, updatedAt.String)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid run updated timestamp", ErrProcessRunCorrupt)
		}
		runs = append(runs, ProcessRunSummary{
			ID: id.String, TemplateRef: templateRef.String, Status: status.String,
			StateVersion: stateVersion.Int64, CreatedAt: created, UpdatedAt: updated,
		})
	}
	return runs, rows.Err()
}

// ListRunnableProcessRunIDs returns only active runs whose checkpoint has no
// outstanding command. The JSON predicate is evaluated by SQLite so recovery
// does not materialize snapshots or checkpoints merely to reject a run that
// requires human reconciliation.
func ListRunnableProcessRunIDs(afterID string, limit int) ([]string, error) {
	if afterID != "" && !validProcessRuntimeIdentifier(afterID, MaxProcessRunIDBytes, false) {
		return nil, fmt.Errorf("%w: invalid runnable-run cursor", ErrProcessRunInvalid)
	}
	if limit <= 0 || limit > MaxProcessRunReadPage {
		return nil, fmt.Errorf("%w: runnable-run page size must be 1..%d", ErrProcessRunInvalid, MaxProcessRunReadPage)
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT
		CASE WHEN typeof(id) = 'text' AND length(CAST(id AS BLOB)) BETWEEN 1 AND ? THEN id END
		FROM process_runs
		WHERE id > ?
			AND status NOT IN ('completed', 'failed', 'canceled')
			AND CASE WHEN json_valid(checkpoint_json) = 1
				THEN json_type(checkpoint_json, '$.outstandingCommand') IS NULL
				ELSE 0 END
		ORDER BY id LIMIT ?`, MaxProcessRunIDBytes, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	ids := make([]string, 0, limit)
	for rows.Next() {
		var id sql.NullString
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !id.Valid || !validProcessRuntimeIdentifier(id.String, MaxProcessRunIDBytes, false) {
			return nil, ErrProcessRunCorrupt
		}
		ids = append(ids, id.String)
	}
	return ids, rows.Err()
}

func scanProcessRunEvent(scanner processRunScanner) (ProcessRunEvent, error) {
	var event ProcessRunEvent
	var runID, occurred, nodeID, kind, payload, actor sql.NullString
	var sequence sql.NullInt64
	if err := scanner.Scan(&runID, &sequence, &occurred, &nodeID,
		&kind, &payload, &actor); err != nil {
		return ProcessRunEvent{}, err
	}
	if !runID.Valid || !sequence.Valid || !occurred.Valid || !nodeID.Valid ||
		!kind.Valid || !payload.Valid || !actor.Valid {
		return ProcessRunEvent{}, ErrProcessRunCorrupt
	}
	event.RunID, event.Sequence, event.NodeID = runID.String, sequence.Int64, nodeID.String
	event.Kind, event.Actor = kind.String, actor.String
	if !validProcessRuntimeIdentifier(event.RunID, MaxProcessRunIDBytes, false) || event.Sequence <= 0 ||
		!validProcessRuntimeIdentifier(event.NodeID, MaxProcessRunNodeIDBytes, true) ||
		!validProcessRuntimeIdentifier(event.Kind, MaxProcessRunEventKind, false) ||
		!validProcessRuntimeText(event.Actor, MaxProcessRunEventActor, true) {
		return ProcessRunEvent{}, ErrProcessRunCorrupt
	}
	if err := validateProcessJSONObject("event payload", []byte(payload.String), MaxProcessRunEventPayloadBytes); err != nil {
		return ProcessRunEvent{}, fmt.Errorf("%w: %v", ErrProcessRunCorrupt, err)
	}
	var err error
	if event.OccurredAt, err = time.Parse(time.RFC3339Nano, occurred.String); err != nil {
		return ProcessRunEvent{}, fmt.Errorf("%w: invalid event timestamp", ErrProcessRunCorrupt)
	}
	event.PayloadJSON = json.RawMessage(strings.Clone(payload.String))
	return event, nil
}

const processRunEventSelect = `SELECT
	CASE WHEN typeof(run_id) = 'text' AND length(CAST(run_id AS BLOB)) BETWEEN 1 AND ? THEN run_id END,
	CASE WHEN typeof(sequence) = 'integer' AND sequence > 0 THEN sequence END,
	CASE WHEN typeof(occurred_at) = 'text' AND length(CAST(occurred_at AS BLOB)) BETWEEN 1 AND ? THEN occurred_at END,
	CASE WHEN typeof(node_id) = 'text' AND length(CAST(node_id AS BLOB)) <= ? THEN node_id END,
	CASE WHEN typeof(kind) = 'text' AND length(CAST(kind AS BLOB)) BETWEEN 1 AND ? THEN kind END,
	CASE WHEN typeof(payload_json) = 'text' AND length(CAST(payload_json AS BLOB)) BETWEEN 1 AND ? THEN payload_json END,
	CASE WHEN typeof(actor) = 'text' AND length(CAST(actor AS BLOB)) <= ? THEN actor END
	FROM process_run_events`

func processRunEventSelectArgs() []any {
	return []any{
		MaxProcessRunIDBytes, maxProcessRunTimestampSize, MaxProcessRunNodeIDBytes,
		MaxProcessRunEventKind, MaxProcessRunEventPayloadBytes, MaxProcessRunEventActor,
	}
}

// ListProcessRunEvents returns evidence after afterSequence, oldest first. It
// is deliberately separate from checkpoint reads and always bounded.
func ListProcessRunEvents(runID string, afterSequence int64, limit int) ([]ProcessRunEvent, error) {
	if !validProcessRuntimeIdentifier(runID, MaxProcessRunIDBytes, false) || afterSequence < 0 {
		return nil, fmt.Errorf("%w: invalid evidence cursor", ErrProcessRunInvalid)
	}
	if limit <= 0 || limit > MaxProcessRunEventReadPage {
		return nil, fmt.Errorf("%w: evidence page size must be 1..%d", ErrProcessRunInvalid, MaxProcessRunEventReadPage)
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	args := append(processRunEventSelectArgs(), runID, afterSequence, limit)
	rows, err := d.Query(processRunEventSelect+`
		WHERE run_id = ? AND sequence > ? ORDER BY sequence LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	events := make([]ProcessRunEvent, 0, limit)
	for rows.Next() {
		event, err := scanProcessRunEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// WipeProcessRuntimeData removes only SQLite run checkpoints and their
// cascading evidence rows. Filesystem template versions, heads, layouts,
// snippets, and every non-runtime SQLite table are intentionally outside this
// operation. The returned count is the number of run checkpoints removed.
func WipeProcessRuntimeData() (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`DELETE FROM process_runs`)
	if err != nil {
		return 0, fmt.Errorf("wipe process runtime data: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit process runtime wipe: %w", err)
	}
	return count, nil
}
