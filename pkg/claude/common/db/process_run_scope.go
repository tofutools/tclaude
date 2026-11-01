package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// processRunScopeActor is the evidence actor for scope-state changes the
// runtime makes on its own (orphaning). It is not an engine transition.
const processRunScopeActor = "tclaude:scope"

// processRunFilterSQL renders a ProcessRunFilter as AND-ed predicates over
// process_runs, served by the partial scope indexes.
func processRunFilterSQL(filter ProcessRunFilter) (string, []any) {
	var where strings.Builder
	var args []any
	if filter.GroupID != 0 {
		where.WriteString(` AND scope_kind = 'group' AND scope_group_id = ?`)
		args = append(args, filter.GroupID)
	}
	if filter.AgentID != "" {
		where.WriteString(` AND scope_kind = 'agent' AND scope_agent_id = ?`)
		args = append(args, filter.AgentID)
	}
	if filter.Performer != "" {
		// An agent performs a run it is bound to, and any run with a node
		// attempt dispatched to it.
		where.WriteString(` AND ((scope_kind = 'agent' AND scope_agent_id = ?)
			OR id IN (SELECT run_id FROM process_agent_dispatches WHERE agent_id = ?))`)
		args = append(args, filter.Performer, filter.Performer)
	}
	return where.String(), args
}

// GetProcessRunScope reads only a run's scope binding — the cheap read the
// dispatch layer uses per node without materializing the checkpoint.
func GetProcessRunScope(runID string) (ProcessRunScope, error) {
	if !validProcessRuntimeIdentifier(runID, MaxProcessRunIDBytes, false) {
		return ProcessRunScope{}, fmt.Errorf("%w: invalid run id", ErrProcessRunInvalid)
	}
	d, err := Open()
	if err != nil {
		return ProcessRunScope{}, err
	}
	var columns processRunScopeColumns
	err = d.QueryRow(`SELECT `+processRunScopeColumnsSelect+` FROM process_runs WHERE id = ?`, runID).
		Scan(columns.targets()...)
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunScope{}, ErrProcessRunNotFound
	}
	if err != nil {
		return ProcessRunScope{}, err
	}
	return columns.decode()
}

// orphanProcessRunsTx flips every bound, non-terminal run matching predicate
// to orphaned and appends one run_orphaned evidence row per run, all inside
// tx. predicate is a trusted SQL fragment over process_runs.
func orphanProcessRunsTx(tx *sql.Tx, predicate string, reason string, args ...any) (int, error) {
	rows, err := tx.Query(`SELECT id FROM process_runs
		WHERE scope_state = 'bound' AND status NOT IN ('completed', 'failed', 'canceled')
		  AND (`+predicate+`) ORDER BY id`, args...)
	if err != nil {
		return 0, fmt.Errorf("select runs to orphan: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	payload, err := json.Marshal(struct {
		Reason string `json:"reason"`
	}{Reason: reason})
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE process_runs SET scope_state = 'orphaned', updated_at = ? WHERE id = ?`,
			dbTime(now), id); err != nil {
			return 0, fmt.Errorf("orphan process run %s: %w", id, err)
		}
		if err := appendProcessRunEventTx(tx, id, ProcessRunEvent{
			OccurredAt: now, Kind: "run_orphaned", PayloadJSON: payload, Actor: processRunScopeActor,
		}); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// appendProcessRunEventTx appends one evidence row at the next sequence.
func appendProcessRunEventTx(tx *sql.Tx, runID string, event ProcessRunEvent) error {
	var last int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(sequence), 0) FROM process_run_events WHERE run_id = ?`, runID).Scan(&last); err != nil {
		return err
	}
	event.Sequence = last + 1
	return insertProcessRunEvents(tx, runID, []ProcessRunEvent{event})
}

// orphanGroupProcessRunsTx is called from DeleteAgentGroup's transaction so
// the orphan flip is atomic with the group row's removal. Doing it there
// rather than lazily matters because group ids can be reused by a later
// group: a lazy existence check would then see a stranger's group.
func orphanGroupProcessRunsTx(tx *sql.Tx, groupID int64) error {
	_, err := orphanProcessRunsTx(tx, `scope_kind = 'group' AND scope_group_id = ?`, "group_deleted", groupID)
	return err
}

// OrphanProcessRunsWithMissingScope is the catch-up pass the runtime sweep
// runs: it orphans agent-scoped runs whose bound agent is retired or gone
// (retirement has many entry points; agent ids are never reused, so a lazy
// check is exact), and group-scoped runs whose group row no longer exists
// (deleted by a path that predates the in-transaction flip).
func OrphanProcessRunsWithMissingScope() (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	agents, err := orphanProcessRunsTx(tx, `scope_kind = 'agent' AND NOT EXISTS (
			SELECT 1 FROM agents WHERE agents.agent_id = process_runs.scope_agent_id AND agents.retired_at IS NULL)`,
		"agent_retired")
	if err != nil {
		return 0, err
	}
	groups, err := orphanProcessRunsTx(tx, `scope_kind = 'group' AND NOT EXISTS (
			SELECT 1 FROM agent_groups WHERE agent_groups.id = process_runs.scope_group_id)`,
		"group_deleted")
	if err != nil {
		return 0, err
	}
	if agents+groups == 0 {
		return 0, nil
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit process run orphaning: %w", err)
	}
	return agents + groups, nil
}

// RescopeProcessRun is the one explicit scope change: a human re-binding an
// orphaned run, or promoting a bound agent-scoped run to a group scope. It
// clears the orphaned state and records run_rescoped evidence. The caller
// validates that the new target exists and that a promotion's group contains
// the agent; this function enforces the state rules.
func RescopeProcessRun(runID string, next ProcessRunScope, actor string) (ProcessRunScope, error) {
	if !validProcessRuntimeIdentifier(runID, MaxProcessRunIDBytes, false) {
		return ProcessRunScope{}, fmt.Errorf("%w: invalid run id", ErrProcessRunInvalid)
	}
	next = normalizeProcessRunScope(next)
	if !validProcessRunScopeBinding(next.Kind, next.GroupID, next.AgentID) {
		return ProcessRunScope{}, fmt.Errorf("%w: invalid %s scope binding", ErrProcessRunInvalid, next.Kind)
	}
	if !validProcessRuntimeText(actor, MaxProcessRunEventActor, true) {
		return ProcessRunScope{}, fmt.Errorf("%w: invalid actor", ErrProcessRunInvalid)
	}
	d, err := Open()
	if err != nil {
		return ProcessRunScope{}, err
	}
	tx, err := d.Begin()
	if err != nil {
		return ProcessRunScope{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var columns processRunScopeColumns
	var status string
	err = tx.QueryRow(`SELECT `+processRunScopeColumnsSelect+`, status FROM process_runs WHERE id = ?`, runID).
		Scan(append(columns.targets(), &status)...)
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunScope{}, ErrProcessRunNotFound
	}
	if err != nil {
		return ProcessRunScope{}, err
	}
	current, err := columns.decode()
	if err != nil {
		return ProcessRunScope{}, err
	}
	switch status {
	case ProcessRunStatusCompleted, ProcessRunStatusFailed, ProcessRunStatusCanceled:
		return ProcessRunScope{}, fmt.Errorf("%w: run is %s", ErrProcessRunRescope, status)
	}
	promotion := current.State == ProcessRunScopeBound && current.Kind == ProcessRunScopeAgent && next.Kind == ProcessRunScopeGroup
	if !current.Orphaned() && !promotion {
		return ProcessRunScope{}, fmt.Errorf("%w: a bound run can only be promoted from agent to group scope", ErrProcessRunRescope)
	}
	payload, err := json.Marshal(struct {
		FromKind    string `json:"fromKind"`
		FromGroupID int64  `json:"fromGroupId,omitempty"`
		FromAgentID string `json:"fromAgentId,omitempty"`
		Kind        string `json:"kind"`
		GroupID     int64  `json:"groupId,omitempty"`
		AgentID     string `json:"agentId,omitempty"`
	}{current.Kind, current.GroupID, current.AgentID, next.Kind, next.GroupID, next.AgentID})
	if err != nil {
		return ProcessRunScope{}, err
	}
	now := time.Now().UTC()
	if _, err := tx.Exec(`UPDATE process_runs
		SET scope_kind = ?, scope_group_id = ?, scope_agent_id = ?, scope_state = 'bound', updated_at = ?
		WHERE id = ?`, next.Kind, next.GroupID, next.AgentID, dbTime(now), runID); err != nil {
		return ProcessRunScope{}, fmt.Errorf("rescope process run: %w", err)
	}
	if err := appendProcessRunEventTx(tx, runID, ProcessRunEvent{
		OccurredAt: now, Kind: "run_rescoped", PayloadJSON: payload, Actor: actor,
	}); err != nil {
		return ProcessRunScope{}, err
	}
	if err := tx.Commit(); err != nil {
		return ProcessRunScope{}, fmt.Errorf("commit process run rescope: %w", err)
	}
	next.Owner, next.State = current.Owner, ProcessRunScopeBound
	return next, nil
}

// SetProcessRunPaused pauses (paused=true) or resumes a non-terminal run and
// records run_paused / run_resumed evidence. A pause is an operator hold, not
// an engine transition: a paused run plans no new agent turns or program
// commands, while attempts already in flight finish and are recorded. Asking
// for the state the run is already in changes nothing and records nothing;
// changed reports which happened.
func SetProcessRunPaused(runID string, paused bool, actor string) (scope ProcessRunScope, changed bool, err error) {
	if !validProcessRuntimeIdentifier(runID, MaxProcessRunIDBytes, false) {
		return ProcessRunScope{}, false, fmt.Errorf("%w: invalid run id", ErrProcessRunInvalid)
	}
	if !validProcessRuntimeText(actor, MaxProcessRunEventActor, true) || len(actor) > MaxProcessRunOwner {
		return ProcessRunScope{}, false, fmt.Errorf("%w: invalid actor", ErrProcessRunInvalid)
	}
	d, err := Open()
	if err != nil {
		return ProcessRunScope{}, false, err
	}
	tx, err := d.Begin()
	if err != nil {
		return ProcessRunScope{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	var columns processRunScopeColumns
	var status string
	err = tx.QueryRow(`SELECT `+processRunScopeColumnsSelect+`, status FROM process_runs WHERE id = ?`, runID).
		Scan(append(columns.targets(), &status)...)
	if errors.Is(err, sql.ErrNoRows) {
		return ProcessRunScope{}, false, ErrProcessRunNotFound
	}
	if err != nil {
		return ProcessRunScope{}, false, err
	}
	current, err := columns.decode()
	if err != nil {
		return ProcessRunScope{}, false, err
	}
	if current.Paused() == paused {
		return current, false, nil
	}
	// A run that ended while paused may still be released; only pausing a
	// finished run is meaningless.
	switch status {
	case ProcessRunStatusCompleted, ProcessRunStatusFailed, ProcessRunStatusCanceled:
		if paused {
			return ProcessRunScope{}, false, fmt.Errorf("%w: run is %s", ErrProcessRunPause, status)
		}
	}
	now := time.Now().UTC()
	kind := "run_resumed"
	next := current
	next.PausedAt, next.PausedBy = time.Time{}, ""
	if paused {
		kind = "run_paused"
		next.PausedAt, next.PausedBy = now, actor
	}
	if _, err := tx.Exec(`UPDATE process_runs SET paused_at = ?, paused_by = ?, updated_at = ? WHERE id = ?`,
		nullableDBTime(next.PausedAt), next.PausedBy, dbTime(now), runID); err != nil {
		return ProcessRunScope{}, false, fmt.Errorf("pause process run: %w", err)
	}
	if err := appendProcessRunEventTx(tx, runID, ProcessRunEvent{
		OccurredAt: now, Kind: kind, PayloadJSON: json.RawMessage(`{}`), Actor: actor,
	}); err != nil {
		return ProcessRunScope{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ProcessRunScope{}, false, fmt.Errorf("commit process run pause: %w", err)
	}
	return next, true, nil
}

// MaxProcessRunContextRuns bounds one in-context read: the scoped runs the
// dashboard overlays project per poll, across every scope.
const MaxProcessRunContextRuns = 64

// ProcessRunContextQuery selects which scopes an in-context read covers and
// how many runs each scope contributes. All covers every scope; otherwise
// only the listed groups and the one bound agent. Every scope (one group,
// one agent) gets at most PerScope runs, so one busy scope cannot starve
// the others, and the whole read stays under MaxProcessRunContextRuns.
type ProcessRunContextQuery struct {
	All      bool
	GroupIDs []int64
	AgentID  string
	PerScope int
}

// ListScopedActiveProcessRuns returns the most recently updated non-terminal
// runs bound to a group or an agent, newest first within each scope. It is
// the candidate set of the in-context read model; unscoped runs appear in
// `tclaude process runs ls` only.
func ListScopedActiveProcessRuns(query ProcessRunContextQuery) ([]ProcessRunSummary, error) {
	return listProcessRunContext(query, `status NOT IN ('completed', 'failed', 'canceled')`)
}

// ListRecentTerminalGroupProcessRuns returns each covered group's most
// recently finished runs — the Done bucket of the in-context read.
func ListRecentTerminalGroupProcessRuns(query ProcessRunContextQuery) ([]ProcessRunSummary, error) {
	query.AgentID = ""
	return listProcessRunContext(query, `scope_kind = 'group' AND status IN ('completed', 'failed', 'canceled')`)
}

func listProcessRunContext(query ProcessRunContextQuery, predicate string) ([]ProcessRunSummary, error) {
	if query.PerScope <= 0 || query.PerScope > MaxProcessRunContextRuns {
		return nil, fmt.Errorf("%w: per-scope context size must be 1..%d", ErrProcessRunInvalid, MaxProcessRunContextRuns)
	}
	var visible strings.Builder
	var args []any
	if !query.All {
		visible.WriteString(` AND (0`)
		for _, id := range query.GroupIDs {
			visible.WriteString(` OR (scope_kind = 'group' AND scope_group_id = ?)`)
			args = append(args, id)
		}
		if query.AgentID != "" {
			visible.WriteString(` OR (scope_kind = 'agent' AND scope_agent_id = ?)`)
			args = append(args, query.AgentID)
		}
		visible.WriteString(`)`)
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	args = append(args, query.PerScope, MaxProcessRunContextRuns)
	return queryProcessRunSummaries(d, `WHERE id IN (
			SELECT id FROM (
				SELECT id, ROW_NUMBER() OVER (
					PARTITION BY scope_kind, scope_group_id, scope_agent_id
					ORDER BY updated_at DESC, id) AS scope_rank
				FROM process_runs
				WHERE scope_kind != 'none' AND `+predicate+visible.String()+`
			) WHERE scope_rank <= ?)
		ORDER BY updated_at DESC, id LIMIT ?`, MaxProcessRunContextRuns, args...)
}
