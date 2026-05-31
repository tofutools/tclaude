package db

import (
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Workflow instance status values — the value stored in
// workflow_instances.status. An instance is created `running` and walks
// to exactly one terminal state.
const (
	WorkflowStatusRunning   = "running"
	WorkflowStatusCompleted = "completed"
	WorkflowStatusFailed    = "failed"
	WorkflowStatusCancelled = "cancelled"
)

// Workflow node status values — the value stored in workflow_nodes.status.
// Lifecycle: pending → ready → running → awaiting_verify → done, with
// failed as the error sink and skipped for a branch that was not taken.
const (
	WorkflowNodeStatusPending        = "pending"
	WorkflowNodeStatusReady          = "ready"
	WorkflowNodeStatusRunning        = "running"
	WorkflowNodeStatusAwaitingVerify = "awaiting_verify"
	WorkflowNodeStatusDone           = "done"
	WorkflowNodeStatusFailed         = "failed"
	WorkflowNodeStatusSkipped        = "skipped"
)

// Workflow event kinds — the value stored in workflow_events.kind. The
// engine appends one per state transition; the list is open (the column
// is free text), these are the well-known kinds the dashboard renders.
const (
	WorkflowEventInstanceCreated    = "instance_created"
	WorkflowEventNodeReady          = "node_ready"
	WorkflowEventNodeStarted        = "node_started"
	WorkflowEventNodeDone           = "node_done"
	WorkflowEventNodeFailed         = "node_failed"
	WorkflowEventNodeSkipped        = "node_skipped"
	WorkflowEventNodeApproved       = "node_approved"        // human-verify gate: approved (settles done + advances)
	WorkflowEventNodeRejected       = "node_rejected"        // human-verify gate: rejected (recorded, no advance)
	WorkflowEventNodeAwaitingVerify = "node_awaiting_verify" // ai-verify: executor done, judge round-trip pending
	WorkflowEventHandoff            = "handoff"              // engine delivered a predecessor's output to a bound successor's inbox (JOH-40)
	WorkflowEventNodeRetry          = "node_retry"          // engine re-armed a node for an in-place retry after its verify failed (JOH-39)
	WorkflowEventNodeReentry        = "node_reentry"        // engine re-armed a node + its loop body via a back-edge loop-back (JOH-39)
	WorkflowEventNodeEscalation     = "node_escalation"     // stuck-node sweep fired an escalation rung (warn/escalate/terminal); Message is the at-most-once marker (JOH-41)
)

// WorkflowInstance is a row in workflow_instances — one instantiation of
// a workflow template. Templates live on disk; Mermaid / Params / Vars
// are snapshotted here at instantiation so later edits to the template
// file never reshape a running instance.
//
// Params and Vars are stored as opaque JSON TEXT ('{}' when empty): the
// DB layer is deliberately ignorant of their shape, so the engine
// (Step 6) owns marshalling. GroupID is a soft link to a tclaude agent
// group (0 = none), not a foreign key — a finished instance keeps its
// history even if the group is later deleted.
type WorkflowInstance struct {
	ID           int64
	TemplateRef  string // "user:foo" | "example:foo" | path
	TemplateName string
	Title        string
	Status       string // WorkflowStatus* — running|completed|failed|cancelled
	Mermaid      string // snapshot of the chart at instantiation
	Params       string // JSON
	Vars         string // JSON captured vars
	GroupID      int64  // 0 = no linked group
	EngineMode   string // who drives the graph: "system" (default) | "agent" (JOH-15 B); snapshotted at create
	CreatedAt    time.Time
	UpdatedAt    time.Time
	CompletedAt  time.Time // zero value → not yet terminal
}

// WorkflowNode is a row in workflow_nodes — one node of one instance.
// Identified across the API by (InstanceID, NodeID); ID is the synthetic
// row key. Detail snapshots the node def as JSON; Output is the captured
// I/O summary; Assignee is the agent conv-id / human handle; Visits
// counts loop re-entries.
type WorkflowNode struct {
	ID           int64
	InstanceID   int64
	NodeID       string // the mermaid node id, unique within an instance
	Label        string
	ExecutorKind string
	Status       string // WorkflowNodeStatus*
	Outcome      string // enum value chosen (drives the branch taken)
	Detail       string // node-def snapshot JSON
	Output       string // captured I/O summary
	Assignee     string // agent conv id / human name
	Visits       int64
	StartedAt    time.Time // zero value → not started
	FinishedAt   time.Time // zero value → not finished
	UpdatedAt    time.Time
}

// WorkflowEvent is a row in workflow_events — one entry in the append-only
// audit/timeline. NodeID is "" for an instance-level event.
type WorkflowEvent struct {
	ID         int64
	InstanceID int64
	NodeID     string
	Kind       string // WorkflowEvent* — free text, these are the well-known kinds
	Message    string
	At         time.Time
}

// isTerminalWorkflowStatus reports whether a status is one of the three
// terminal states. UpdateWorkflowInstanceStatus uses this to decide
// whether to stamp completed_at.
func isTerminalWorkflowStatus(status string) bool {
	switch status {
	case WorkflowStatusCompleted, WorkflowStatusFailed, WorkflowStatusCancelled:
		return true
	default:
		return false
	}
}

// ----- instances -------------------------------------------------------

// InsertWorkflowInstance writes a new instance row and returns its ID.
// CreatedAt and UpdatedAt are stamped server-side (the caller's values
// are ignored). An empty Status defaults to running; empty Params/Vars
// default to "{}" so the columns are never blank JSON.
func InsertWorkflowInstance(w *WorkflowInstance) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	status := w.Status
	if status == "" {
		status = WorkflowStatusRunning
	}
	res, err := d.Exec(`INSERT INTO workflow_instances
		(template_ref, template_name, title, status, mermaid, params, vars,
		 group_id, engine_mode, created_at, updated_at, completed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '')`,
		w.TemplateRef, w.TemplateName, w.Title, status, w.Mermaid,
		jsonOrEmptyObject(w.Params), jsonOrEmptyObject(w.Vars), w.GroupID,
		engineModeOrDefault(w.EngineMode), now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// GetWorkflowInstance returns a single instance by ID, or nil if not found.
func GetWorkflowInstance(id int64) (*WorkflowInstance, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT id, template_ref, template_name, title, status,
		mermaid, params, vars, group_id, engine_mode, created_at, updated_at, completed_at
		FROM workflow_instances WHERE id = ?`, id)
	w, err := scanWorkflowInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return w, err
}

// ListWorkflowInstances returns every instance, ordered by ID asc.
func ListWorkflowInstances() ([]*WorkflowInstance, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, template_ref, template_name, title, status,
		mermaid, params, vars, group_id, engine_mode, created_at, updated_at, completed_at
		FROM workflow_instances ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*WorkflowInstance
	for rows.Next() {
		w, err := scanWorkflowInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateWorkflowInstanceStatus sets the instance status and bumps
// updated_at. Transitioning to a terminal status (completed/failed/
// cancelled) stamps completed_at; transitioning back to a non-terminal
// status clears it. Returns rows affected (0 → no such id).
func UpdateWorkflowInstanceStatus(id int64, status string) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	completed := ""
	if isTerminalWorkflowStatus(status) {
		completed = now
	}
	res, err := d.Exec(`UPDATE workflow_instances
		SET status = ?, updated_at = ?, completed_at = ? WHERE id = ?`,
		status, now, completed, id)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// UpdateWorkflowInstanceVars replaces the captured-vars JSON blob and
// bumps updated_at. An empty string is stored as "{}". Returns rows
// affected (0 → no such id).
func UpdateWorkflowInstanceVars(id int64, vars string) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`UPDATE workflow_instances SET vars = ?, updated_at = ? WHERE id = ?`,
		jsonOrEmptyObject(vars), time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// DeleteWorkflowInstance removes an instance by ID. Idempotent. The
// ON DELETE CASCADE foreign keys remove its nodes and events too (the
// DSN enables foreign_keys, so the cascade fires).
func DeleteWorkflowInstance(id int64) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`DELETE FROM workflow_instances WHERE id = ?`, id)
	return err
}

// ----- nodes -----------------------------------------------------------

// InsertWorkflowNode writes a single node row and returns its ID.
// UpdatedAt is stamped server-side. An empty Status defaults to pending;
// empty Detail defaults to "{}". A duplicate (instance_id, node_id)
// violates the UNIQUE constraint and surfaces as an error.
func InsertWorkflowNode(n *WorkflowNode) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	res, err := d.Exec(`INSERT INTO workflow_nodes
		(instance_id, node_id, label, executor_kind, status, outcome, detail,
		 output, assignee, visits, started_at, finished_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.InstanceID, n.NodeID, n.Label, n.ExecutorKind,
		nodeStatusOrDefault(n.Status), n.Outcome, jsonOrEmptyObject(n.Detail),
		n.Output, n.Assignee, n.Visits,
		timeOrEmpty(n.StartedAt), timeOrEmpty(n.FinishedAt),
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// InsertWorkflowNodes bulk-inserts the full node set for one instance in
// a single transaction — the instantiation path, where every node of the
// snapshotted chart lands at once. InstanceID on each node is overwritten
// with the passed instanceID so the caller need not set it. All-or-
// nothing: a failure (e.g. a duplicate node_id) rolls the whole batch back.
func InsertWorkflowNodes(instanceID int64, nodes []*WorkflowNode) error {
	if len(nodes) == 0 {
		return nil
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
	now := time.Now().UTC().Format(time.RFC3339)
	stmt, err := tx.Prepare(`INSERT INTO workflow_nodes
		(instance_id, node_id, label, executor_kind, status, outcome, detail,
		 output, assignee, visits, started_at, finished_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer func() { _ = stmt.Close() }()
	for _, n := range nodes {
		if _, err := stmt.Exec(
			instanceID, n.NodeID, n.Label, n.ExecutorKind,
			nodeStatusOrDefault(n.Status), n.Outcome, jsonOrEmptyObject(n.Detail),
			n.Output, n.Assignee, n.Visits,
			timeOrEmpty(n.StartedAt), timeOrEmpty(n.FinishedAt), now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListWorkflowNodes returns every node of one instance, ordered by ID asc
// (insertion order, which mirrors the chart order at instantiation).
func ListWorkflowNodes(instanceID int64) ([]*WorkflowNode, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, instance_id, node_id, label, executor_kind,
		status, outcome, detail, output, assignee, visits,
		started_at, finished_at, updated_at
		FROM workflow_nodes WHERE instance_id = ? ORDER BY id`, instanceID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*WorkflowNode
	for rows.Next() {
		n, err := scanWorkflowNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetWorkflowNode returns one node by its (instanceID, nodeID) identity,
// or nil if not found.
func GetWorkflowNode(instanceID int64, nodeID string) (*WorkflowNode, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	row := d.QueryRow(`SELECT id, instance_id, node_id, label, executor_kind,
		status, outcome, detail, output, assignee, visits,
		started_at, finished_at, updated_at
		FROM workflow_nodes WHERE instance_id = ? AND node_id = ?`, instanceID, nodeID)
	n, err := scanWorkflowNode(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return n, err
}

// ListAssignedWorkflowNodes returns every workflow node that has a non-empty
// assignee, across all instances, ordered by instance_id then id. The
// /v1/workflows/where handler resolves each row's assignee to its succession
// head (ResolveLatestConv) and keeps the rows whose head equals the caller, so
// an assignee that has since reincarnated still resolves to the live caller.
func ListAssignedWorkflowNodes() ([]*WorkflowNode, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	rows, err := d.Query(`SELECT id, instance_id, node_id, label, executor_kind,
		status, outcome, detail, output, assignee, visits,
		started_at, finished_at, updated_at
		FROM workflow_nodes WHERE assignee != '' ORDER BY instance_id, id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*WorkflowNode
	for rows.Next() {
		n, err := scanWorkflowNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountRunningWorkflowNodesByKind returns how many nodes of the given executor
// kind are currently `running`, across ALL instances. The workflow engine uses
// it for the global AI-node parallelism cap: a single COUNT query gives an
// always-fresh tally (spawns committed earlier in the same tick are visible),
// without walking every instance's node list.
func CountRunningWorkflowNodesByKind(executorKind string) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM workflow_nodes
		WHERE executor_kind = ? AND status = ?`,
		executorKind, WorkflowNodeStatusRunning).Scan(&n)
	return n, err
}

// CountAwaitingVerifyAssignedNodes returns how many nodes are in
// `awaiting_verify` with a non-empty assignee, across ALL instances. The
// workflow engine uses it to count verify-judges in flight toward the global
// agent cap: an awaiting_verify node carries an assignee ONLY once the engine
// has claimed/spawned a judge for it (the worker-park and the tool-verify defer
// both CLEAR the assignee; the human-verify approve gate never assigns), so this
// is exactly "judges currently occupying a slot". One COUNT query, always-fresh —
// mirrors CountRunningWorkflowNodesByKind.
//
// The invariant has one theoretical hole: a human/owner could manually PATCH an
// assignee onto an awaiting_verify human-verify node, which this would then
// miscount as a judge (consuming a cap slot it can never claim, since the judge
// pass only picks verify.kind:ai nodes). That requires deliberate operator
// action on a non-ai-verify node and only over-counts the cap (fail-safe — it
// throttles, never over-spawns), so it is left as a documented edge rather than
// re-querying verify.kind (which lives in the node-def JSON, not a column).
func CountAwaitingVerifyAssignedNodes() (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	var n int
	err = d.QueryRow(`SELECT COUNT(*) FROM workflow_nodes
		WHERE status = ? AND assignee != ''`,
		WorkflowNodeStatusAwaitingVerify).Scan(&n)
	return n, err
}

// WorkflowNodePatch is the partial-update shape for UpdateWorkflowNode.
// nil → leave the field unchanged. Pointer-shaped so callers can
// distinguish "set to zero/empty" from "don't touch" — mirrors
// UpdateCronPatch. The StartedAt/FinishedAt pointers carry a time.Time:
// a zero value writes ” (clears the stamp), a non-zero value writes the
// RFC3339 UTC timestamp.
type WorkflowNodePatch struct {
	Label        *string
	ExecutorKind *string
	Status       *string
	Outcome      *string
	Detail       *string
	Output       *string
	Assignee     *string
	Visits       *int64
	StartedAt    *time.Time
	FinishedAt   *time.Time
}

// UpdateWorkflowNode applies a partial update to one node identified by
// (instanceID, nodeID). Only non-nil patch fields are written; updated_at
// is always bumped when at least one field is set. Returns rows affected
// (0 → no such node, OR an empty patch). An empty patch is a no-op that
// does not even touch updated_at.
func UpdateWorkflowNode(instanceID int64, nodeID string, p WorkflowNodePatch) (int, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	sets := make([]string, 0, 11)
	args := make([]any, 0, 13)
	if p.Label != nil {
		sets = append(sets, "label = ?")
		args = append(args, *p.Label)
	}
	if p.ExecutorKind != nil {
		sets = append(sets, "executor_kind = ?")
		args = append(args, *p.ExecutorKind)
	}
	if p.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *p.Status)
	}
	if p.Outcome != nil {
		sets = append(sets, "outcome = ?")
		args = append(args, *p.Outcome)
	}
	if p.Detail != nil {
		sets = append(sets, "detail = ?")
		args = append(args, *p.Detail)
	}
	if p.Output != nil {
		sets = append(sets, "output = ?")
		args = append(args, *p.Output)
	}
	if p.Assignee != nil {
		sets = append(sets, "assignee = ?")
		args = append(args, *p.Assignee)
	}
	if p.Visits != nil {
		sets = append(sets, "visits = ?")
		args = append(args, *p.Visits)
	}
	if p.StartedAt != nil {
		sets = append(sets, "started_at = ?")
		args = append(args, timeOrEmpty(*p.StartedAt))
	}
	if p.FinishedAt != nil {
		sets = append(sets, "finished_at = ?")
		args = append(args, timeOrEmpty(*p.FinishedAt))
	}
	if len(sets) == 0 {
		return 0, nil
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	args = append(args, instanceID, nodeID)
	res, err := d.Exec(`UPDATE workflow_nodes SET `+strings.Join(sets, ", ")+
		` WHERE instance_id = ? AND node_id = ?`, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ----- events ----------------------------------------------------------

// AppendWorkflowEvent appends one timeline entry and returns its ID. A
// zero At is stamped server-side with the current UTC time; a caller that
// wants an explicit timestamp may set it. NodeID is "" for an
// instance-level event.
func AppendWorkflowEvent(e *WorkflowEvent) (int64, error) {
	d, err := Open()
	if err != nil {
		return 0, err
	}
	at := e.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	res, err := d.Exec(`INSERT INTO workflow_events
		(instance_id, node_id, kind, message, at)
		VALUES (?, ?, ?, ?, ?)`,
		e.InstanceID, e.NodeID, e.Kind, e.Message, at.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListWorkflowEvents returns the timeline for one instance, oldest first
// (insertion order via id asc). Pass an optional nodeID to filter to a
// single node's events — the form behind the per-node "open audit data"
// action. Extra nodeID arguments beyond the first are ignored.
func ListWorkflowEvents(instanceID int64, nodeID ...string) ([]*WorkflowEvent, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	q := `SELECT id, instance_id, node_id, kind, message, at
		FROM workflow_events WHERE instance_id = ?`
	args := []any{instanceID}
	if len(nodeID) > 0 {
		q += ` AND node_id = ?`
		args = append(args, nodeID[0])
	}
	q += ` ORDER BY id`
	rows, err := d.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []*WorkflowEvent
	for rows.Next() {
		var e WorkflowEvent
		var at string
		if err := rows.Scan(&e.ID, &e.InstanceID, &e.NodeID, &e.Kind, &e.Message, &at); err != nil {
			return nil, err
		}
		e.At = parseTimeOrZero(at)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ----- helpers ---------------------------------------------------------

// jsonOrEmptyObject normalizes a JSON TEXT column value: a blank string
// becomes "{}" so the column never holds invalid JSON.
func jsonOrEmptyObject(s string) string {
	if s == "" {
		return "{}"
	}
	return s
}

// nodeStatusOrDefault defaults a blank node status to pending.
func nodeStatusOrDefault(s string) string {
	if s == "" {
		return WorkflowNodeStatusPending
	}
	return s
}

// engineModeOrDefault defaults a blank engine mode to "system" so the column
// never holds an empty value (mirroring the migration's DEFAULT 'system'). The
// DB layer is deliberately ignorant of the valid set — the workflow loader
// validates system|agent; here we only guard against a blank write.
func engineModeOrDefault(s string) string {
	if s == "" {
		return "system"
	}
	return s
}

// timeOrEmpty formats a timestamp as RFC3339 UTC, or "" for the zero
// value (the "unset" sentinel the started_at/finished_at columns use).
func timeOrEmpty(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func scanWorkflowInstance(s rowScanner) (*WorkflowInstance, error) {
	var w WorkflowInstance
	var created, updated, completed string
	err := s.Scan(&w.ID, &w.TemplateRef, &w.TemplateName, &w.Title, &w.Status,
		&w.Mermaid, &w.Params, &w.Vars, &w.GroupID, &w.EngineMode, &created, &updated, &completed)
	if err != nil {
		return nil, err
	}
	w.CreatedAt = parseTimeOrZero(created)
	w.UpdatedAt = parseTimeOrZero(updated)
	w.CompletedAt = parseTimeOrZero(completed)
	return &w, nil
}

func scanWorkflowNode(s rowScanner) (*WorkflowNode, error) {
	var n WorkflowNode
	var started, finished, updated string
	err := s.Scan(&n.ID, &n.InstanceID, &n.NodeID, &n.Label, &n.ExecutorKind,
		&n.Status, &n.Outcome, &n.Detail, &n.Output, &n.Assignee, &n.Visits,
		&started, &finished, &updated)
	if err != nil {
		return nil, err
	}
	n.StartedAt = parseTimeOrZero(started)
	n.FinishedAt = parseTimeOrZero(finished)
	n.UpdatedAt = parseTimeOrZero(updated)
	return &n, nil
}

// SetWorkflowNodeUpdatedAtForTest back-dates a node's updated_at directly,
// bypassing the normal "stamp now" path, so a test can place a node at a precise
// idle age and exercise the escalation sweep's tier bands (warn / escalate /
// terminal) deterministically rather than racing wall-clock milliseconds. Stored
// in the same RFC3339 form the production writes use. Must only be called from
// tests.
func SetWorkflowNodeUpdatedAtForTest(instanceID int64, nodeID string, t time.Time) error {
	d, err := Open()
	if err != nil {
		return err
	}
	_, err = d.Exec(`UPDATE workflow_nodes SET updated_at = ? WHERE instance_id = ? AND node_id = ?`,
		t.UTC().Format(time.RFC3339), instanceID, nodeID)
	return err
}
