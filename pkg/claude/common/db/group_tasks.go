package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Group task board (tcl-uzmtah): a short-term, ordered list of tasks per agent
// group. The board is the view of right now and the next few tasks; the
// durable backlog lives in an external tracker, which a task may link to.
//
// A task has a stage (idea | next, the operator's own line between a captured
// idea and the queue) and a status (open | blocked | done | dropped). Tasks are
// ordered within their stage by a lexicographic rank key. done and dropped
// tasks leave the live list at once, are archived (hidden from --all) after a
// while, and are purged after the retention period by the sweep.

const (
	GroupTaskStageIdea = "idea"
	GroupTaskStageNext = "next"

	GroupTaskOpen    = "open"
	GroupTaskBlocked = "blocked"
	GroupTaskDone    = "done"
	GroupTaskDropped = "dropped"

	// GroupTaskLiveCap is how many live (open or blocked) tasks a group holds.
	GroupTaskLiveCap = 500
	// GroupTaskDepsCap is how many tasks one task may depend on.
	GroupTaskDepsCap = 50
	// GroupTaskEventDetailMax bounds an event's detail, in bytes.
	GroupTaskEventDetailMax = 512
)

// Event kinds written so far. The assign/report kinds (assigned, unassigned,
// taken, released, reported, stalled) arrive with the slice that writes them.
const (
	GroupTaskEventCreated   = "created"
	GroupTaskEventImported  = "imported"
	GroupTaskEventEdited    = "edited"
	GroupTaskEventMoved     = "moved"
	GroupTaskEventPromoted  = "promoted"
	GroupTaskEventDemoted   = "demoted"
	GroupTaskEventBlocked   = "blocked"
	GroupTaskEventUnblocked = "unblocked"
	GroupTaskEventDone      = "done"
	GroupTaskEventDropped   = "dropped"
	GroupTaskEventReopened  = "reopened"
	GroupTaskEventArchived  = "archived"
)

var (
	// ErrGroupTaskNotFound is a task id that names no task.
	ErrGroupTaskNotFound = errors.New("no such task")
	// ErrGroupTaskCap is a create past GroupTaskLiveCap.
	ErrGroupTaskCap = fmt.Errorf("the group already holds %d live tasks", GroupTaskLiveCap)
)

// GroupTaskConflict is a change the task's current state refuses (a block of
// a closed task, a dependency cycle, a move relative to a task in another
// group). Callers answer it with 409 or 400 depending on Code.
type GroupTaskConflict struct {
	Code string
	Msg  string
}

func (e *GroupTaskConflict) Error() string { return e.Msg }

func taskConflict(code, format string, args ...any) error {
	return &GroupTaskConflict{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// GroupTask is one row of group_tasks.
type GroupTask struct {
	ID          string
	GroupID     int64
	Title       string
	Body        string
	Stage       string
	Status      string
	Rank        string
	BlockReason string
	// RefKind, RefURL and RefLabel are the task's first ref; MoreRefs are
	// the rest, in order (tk_01ad0884).
	RefKind     string
	RefURL      string
	RefLabel    string
	MoreRefs    []GroupTaskRef
	AssigneeID  string
	AssignedBy  string
	AssignedAt  time.Time
	Outcome     string
	OutcomeNote string
	Failures    int
	CreatedBy   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	ClosedAt    time.Time
	ArchivedAt  time.Time
	// BlockedOn is the operator task a blocked task waits on: closing that
	// task as done reopens this one (tcl-p4t78f).
	BlockedOn string
	// StartedAt is when its assignee started it (tk_3826822a); zero for an
	// assigned task it holds without starting, in its stage.
	StartedAt time.Time
	// Column is the slug of the group's custom column a live task of stage
	// next stands in (In review, In CI, …), "" for none (tcl-plby5h).
	Column string
	// OperatorRequest marks a question task ({"kind":"question"}, see
	// IsQuestionTask); empty for any other task. Its questions are rows of
	// group_task_questions.
	OperatorRequest string
	// Kind is "" for a task, GroupTaskKindEpic for an epic: a container of
	// tasks, with no assignee or work stage of its own (group_task_epics.go).
	Kind string
	// ParentID is the epic this task belongs to, "" for none.
	ParentID string
	// CloseSuggestedAt marks an open epic all of whose children are closed:
	// the board suggests closing it; zero otherwise.
	CloseSuggestedAt time.Time
	// DependsOn is filled by the single-task reads and by ListGroupTasks.
	DependsOn []string
}

// IsEpic reports whether the task is an epic.
func (t *GroupTask) IsEpic() bool { return t.Kind == GroupTaskKindEpic }

// GroupTaskRef is one of a task's external refs: what it is (awb, linear,
// github, gitlab, url), its link and its shown label.
type GroupTaskRef struct {
	Kind  string `json:"kind"`
	URL   string `json:"url,omitempty"`
	Label string `json:"label,omitempty"`
}

// MaxGroupTaskRefs is how many refs a task may have.
const MaxGroupTaskRefs = 10

// Refs are all of the task's refs, the first one first; none without one.
func (t *GroupTask) Refs() []GroupTaskRef {
	var out []GroupTaskRef
	if t.RefKind != "" || t.RefURL != "" || t.RefLabel != "" {
		out = append(out, GroupTaskRef{Kind: t.RefKind, URL: t.RefURL, Label: t.RefLabel})
	}
	return append(out, t.MoreRefs...)
}

// encodeMoreRefs is more_refs as stored: empty for none.
func encodeMoreRefs(refs []GroupTaskRef) string {
	if len(refs) == 0 {
		return ""
	}
	data, err := json.Marshal(refs)
	if err != nil {
		return ""
	}
	return string(data)
}

// decodeMoreRefs reads more_refs; a value it cannot read is no refs.
func decodeMoreRefs(raw string) []GroupTaskRef {
	if raw == "" {
		return nil
	}
	var refs []GroupTaskRef
	if json.Unmarshal([]byte(raw), &refs) != nil {
		return nil
	}
	return refs
}

// Started reports whether the task is assigned and started: in progress.
func (t *GroupTask) Started() bool { return t.AssigneeID != "" && !t.StartedAt.IsZero() }

// Live reports whether the task is on the live list (open or blocked).
func (t *GroupTask) Live() bool { return t.Status == GroupTaskOpen || t.Status == GroupTaskBlocked }

// GroupTaskEvent is one entry of a task's activity stream: a change, or a
// comment (group_task_comments.go).
type GroupTaskEvent struct {
	ID     int64
	TaskID string
	At     time.Time
	Actor  string
	Kind   string
	Detail string
	// Changes are a field change's before and after values.
	Changes []GroupTaskChange
	// Body is a comment's Markdown; on a comment_edited entry, the body
	// the comment had before that edit.
	Body string
	// CommentKind is what a comment is (GroupTaskComment*).
	CommentKind string
	ClientKey   string
	// CommentID ties a comment_edited entry to its comment.
	CommentID int64
	EditedAt  time.Time
	DeletedAt time.Time
}

const groupTaskCols = `id, group_id, title, body, stage, status, rank, block_reason, ref_kind, ref_url, ref_label,
	CASE WHEN assignee_kind = 'operator' THEN 'operator' ELSE COALESCE(assignee_id, '') END, assigned_by, assigned_at,
	outcome, outcome_note, failures, created_by, created_at, updated_at, closed_at, archived_at, blocked_on,
	started_at, column_slug, operator_request, more_refs, kind, COALESCE(parent_id, ''), close_suggested_at`

func scanGroupTask(s rowScanner) (*GroupTask, error) {
	var t GroupTask
	var assigned, created, updated, closed, archived, started, suggested dbTimestamp
	var moreRefs string
	if err := s.Scan(&t.ID, &t.GroupID, &t.Title, &t.Body, &t.Stage, &t.Status, &t.Rank, &t.BlockReason,
		&t.RefKind, &t.RefURL, &t.RefLabel, &t.AssigneeID, &t.AssignedBy, &assigned, &t.Outcome, &t.OutcomeNote,
		&t.Failures, &t.CreatedBy, &created, &updated, &closed, &archived, &t.BlockedOn, &started, &t.Column, &t.OperatorRequest,
		&moreRefs, &t.Kind, &t.ParentID, &suggested); err != nil {
		return nil, err
	}
	t.CloseSuggestedAt = suggested.Time()
	t.MoreRefs = decodeMoreRefs(moreRefs)
	t.AssignedAt, t.CreatedAt, t.UpdatedAt = assigned.Time(), created.Time(), updated.Time()
	t.ClosedAt, t.ArchivedAt, t.StartedAt = closed.Time(), archived.Time(), started.Time()
	return &t, nil
}

func newGroupTaskID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("db: crypto/rand failed generating task id: " + err.Error())
	}
	return "tk_" + hex.EncodeToString(b[:])
}

// IsGroupTaskID reports whether s has the shape of a task id.
func IsGroupTaskID(s string) bool {
	if len(s) != 11 || !strings.HasPrefix(s, "tk_") {
		return false
	}
	_, err := hex.DecodeString(s[3:])
	return err == nil && strings.ToLower(s) == s
}

// GroupTaskPosition places a task within its stage. At most one field is set;
// none means the end of the stage. Before and After name a task in the same
// group, and the task takes that task's stage.
type GroupTaskPosition struct {
	Top    bool
	Bottom bool
	Before string
	After  string
}

// NewGroupTask is a create.
type NewGroupTask struct {
	GroupID  int64
	Title    string
	Body     string
	Stage    string
	RefKind  string
	RefURL   string
	RefLabel string
	// MoreRefs are the refs after the first (RefKind, RefURL, RefLabel).
	MoreRefs  []GroupTaskRef
	DependsOn []string
	Position  GroupTaskPosition
	Actor     string
	// Imported records the task's first event as imported (a ref import)
	// rather than created.
	Imported bool
	// Origin, when set, is added to the first event's detail (a clone names
	// the task it was cloned from).
	Origin string
	// Attachments are files the task starts with, written in the same
	// transaction (a create with files, a clone, a task moved in from
	// another node). Their blobs must already be stored.
	Attachments []GroupTaskAttachment
	// The task's start state, set in the same transaction as the create:
	// Status open (the default), blocked (BlockReason says why) or done
	// (work logged after the fact: closed now). AssigneeID, when set, is who
	// holds it (in progress) or, done, who did it; AssignedBy records who
	// assigned it. An assigned task lands in next, started, unless Hold
	// names the stage (idea or next) its assignee holds it in, not started.
	Status      string
	BlockReason string
	AssigneeID  string
	AssignedBy  string
	Hold        string
	// Blocks, when set, names a live task of the same group that waits on
	// the new one: in the same transaction it gains the new task as a
	// dependency and is blocked on it (BlockGroupTaskOn). BlocksExpect is
	// the holder the caller was authorised against for it.
	Blocks       string
	BlocksExpect string
	// Column, when set, is the custom column (its slug) the task starts in,
	// set in the same transaction: a column stands in next, so the task is
	// queued there, and only a live task sits in one.
	Column string
	// Question makes the new task a question for the operator (its
	// assignee): its first question (askGroupTaskQuestionTx), answered
	// with AnswerGroupTaskQuestion.
	Question *GroupTaskQuestion
	// Kind is "" (a task) or GroupTaskKindEpic. An epic starts open, with
	// no assignee, column, question or parent.
	Kind string
	// ParentID, when set, is the epic (of the same group) the task is in.
	ParentID string
}

type taskTx interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func getGroupTaskTx(q taskTx, id string) (*GroupTask, error) {
	t, err := scanGroupTask(q.QueryRow(`SELECT `+groupTaskCols+` FROM group_tasks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrGroupTaskNotFound
	}
	if err != nil {
		return nil, err
	}
	deps, err := groupTaskDepsTx(q, id)
	if err != nil {
		return nil, err
	}
	t.DependsOn = deps
	return t, nil
}

func groupTaskDepsTx(q taskTx, id string) ([]string, error) {
	rows, err := q.Query(`SELECT depends_on FROM group_task_deps WHERE task_id = ? ORDER BY depends_on`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var dep string
		if err := rows.Scan(&dep); err != nil {
			return nil, err
		}
		out = append(out, dep)
	}
	return out, rows.Err()
}

func addGroupTaskEventTx(q taskTx, taskID, actor, kind, detail string, at time.Time) error {
	return addGroupTaskChangeTx(q, taskID, actor, kind, detail, nil, at)
}

// addGroupTaskChangeTx records a change entry with its field changes.
func addGroupTaskChangeTx(q taskTx, taskID, actor, kind, detail string, changes []GroupTaskChange, at time.Time) error {
	if len(detail) > GroupTaskEventDetailMax {
		detail = truncateUTF8(detail, GroupTaskEventDetailMax)
	}
	_, err := q.Exec(`INSERT INTO group_task_events (task_id, at, actor, kind, detail, changes) VALUES (?, ?, ?, ?, ?, ?)`,
		taskID, dbTime(at), actor, kind, detail, encodeGroupTaskChanges(changes))
	return err
}

// truncateUTF8 cuts s to at most n bytes without splitting a rune.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// stageRankBounds returns the lowest and highest rank of the live tasks of a
// group's stage, "" when it has none. exclude leaves one task out (the one
// being moved).
func stageRankBounds(q taskTx, groupID int64, stage, exclude string) (lo, hi string, err error) {
	var min, max sql.NullString
	err = q.QueryRow(`SELECT MIN(rank), MAX(rank) FROM group_tasks
		WHERE group_id = ? AND stage = ? AND status IN ('open','blocked') AND id <> ?`,
		groupID, stage, exclude).Scan(&min, &max)
	return min.String, max.String, err
}

// neighbourRank returns the rank of the live task of the same group and stage
// next to rank: the one just below it (before=true) or just above it.
func neighbourRank(q taskTx, groupID int64, stage, rank, exclude string, before bool) (string, error) {
	query := `SELECT MAX(rank) FROM group_tasks WHERE group_id = ? AND stage = ? AND status IN ('open','blocked')
		AND id <> ? AND rank < ?`
	if !before {
		query = `SELECT MIN(rank) FROM group_tasks WHERE group_id = ? AND stage = ? AND status IN ('open','blocked')
		AND id <> ? AND rank > ?`
	}
	var v sql.NullString
	err := q.QueryRow(query, groupID, stage, exclude, rank).Scan(&v)
	return v.String, err
}

// placeTask resolves pos to (stage, rank) for task self (empty for a new
// task) of groupID, whose stage is stage unless pos names an anchor.
func placeTask(q taskTx, groupID int64, stage, self string, pos GroupTaskPosition) (string, string, error) {
	anchorID, before := pos.Before, true
	if anchorID == "" && pos.After != "" {
		anchorID, before = pos.After, false
	}
	if anchorID != "" {
		if anchorID == self {
			return "", "", taskConflict("invalid_arg", "a task cannot be placed relative to itself")
		}
		anchor, err := getGroupTaskTx(q, anchorID)
		if errors.Is(err, ErrGroupTaskNotFound) || err == nil && anchor.GroupID != groupID {
			return "", "", taskConflict("invalid_arg", "no task %s in this group", anchorID)
		}
		if err != nil {
			return "", "", err
		}
		if !anchor.Live() {
			return "", "", taskConflict("invalid_arg", "task %s is %s, not on the list", anchorID, anchor.Status)
		}
		other, err := neighbourRank(q, groupID, anchor.Stage, anchor.Rank, self, before)
		if err != nil {
			return "", "", err
		}
		if before {
			return anchor.Stage, taskRankBetween(other, anchor.Rank), nil
		}
		return anchor.Stage, taskRankBetween(anchor.Rank, other), nil
	}
	lo, hi, err := stageRankBounds(q, groupID, stage, self)
	if err != nil {
		return "", "", err
	}
	if pos.Top {
		return stage, taskRankBetween("", lo), nil
	}
	return stage, taskRankBetween(hi, ""), nil
}

// setGroupTaskDepsTx adds and removes dependencies of task t, refusing a
// dependency on a task of another group, a cycle, and more than the cap.
func setGroupTaskDepsTx(q taskTx, t *GroupTask, add, remove []string) error {
	for _, dep := range remove {
		if _, err := q.Exec(`DELETE FROM group_task_deps WHERE task_id = ? AND depends_on = ?`, t.ID, dep); err != nil {
			return err
		}
	}
	for _, dep := range add {
		if dep == t.ID {
			return taskConflict("invalid_arg", "a task cannot depend on itself")
		}
		other, err := getGroupTaskTx(q, dep)
		if errors.Is(err, ErrGroupTaskNotFound) || err == nil && other.GroupID != t.GroupID {
			return taskConflict("invalid_arg", "no task %s in this group", dep)
		}
		if err != nil {
			return err
		}
		cycle, err := groupTaskReaches(q, dep, t.ID)
		if err != nil {
			return err
		}
		if cycle {
			return taskConflict("dep_cycle", "%s already depends on %s; the dependency would make a cycle", dep, t.ID)
		}
		if _, err := q.Exec(`INSERT OR IGNORE INTO group_task_deps (task_id, depends_on) VALUES (?, ?)`, t.ID, dep); err != nil {
			return err
		}
	}
	var n int
	if err := q.QueryRow(`SELECT COUNT(*) FROM group_task_deps WHERE task_id = ?`, t.ID).Scan(&n); err != nil {
		return err
	}
	if n > GroupTaskDepsCap {
		return taskConflict("invalid_arg", "a task depends on at most %d tasks", GroupTaskDepsCap)
	}
	return nil
}

// groupTaskReaches reports whether from depends, directly or not, on to.
func groupTaskReaches(q taskTx, from, to string) (bool, error) {
	var found int
	err := q.QueryRow(`WITH RECURSIVE reach(id) AS (
			SELECT ?
			UNION
			SELECT d.depends_on FROM group_task_deps d JOIN reach ON d.task_id = reach.id
		) SELECT EXISTS(SELECT 1 FROM reach WHERE id = ?)`, from, to).Scan(&found)
	return found != 0, err
}

// CreateGroupTask adds a task to a group's board and records its creation.
func CreateGroupTask(in NewGroupTask) (*GroupTask, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	status := in.Status
	if status == "" {
		status = GroupTaskOpen
	}
	switch status {
	case GroupTaskOpen, GroupTaskBlocked, GroupTaskDone:
	default:
		return nil, taskConflict("invalid_arg", "a new task starts open, blocked or done, not %s", status)
	}
	if status == GroupTaskBlocked && strings.TrimSpace(in.BlockReason) == "" {
		return nil, taskConflict("invalid_arg", "a blocked task needs its reason")
	}
	if err := checkGroupTaskHold(in.Hold); err != nil {
		return nil, err
	}
	if err := checkNewGroupTaskKind(in, status); err != nil {
		return nil, err
	}
	// A task something waits on is queued, never an idea to triage.
	if in.Blocks != "" && in.Hold == GroupTaskStageIdea {
		in.Hold = GroupTaskStageNext
	}
	if in.Hold != "" && (in.AssigneeID == "" || status == GroupTaskDone) {
		return nil, taskConflict("invalid_arg", "only a live task with an assignee is held in a stage")
	}
	// A done task is not live: logging work never meets the cap.
	if status != GroupTaskDone {
		var live int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM group_tasks WHERE group_id = ? AND status IN ('open','blocked')`,
			in.GroupID).Scan(&live); err != nil {
			return nil, err
		}
		if live >= GroupTaskLiveCap {
			return nil, ErrGroupTaskCap
		}
	}
	stage := in.Stage
	if stage == "" {
		stage = GroupTaskStageIdea
	}
	columnLabel := ""
	if in.Column != "" {
		switch {
		case status == GroupTaskDone:
			return nil, taskConflict("invalid_arg", "a task logged as done sits in no column")
		case in.Hold == GroupTaskStageIdea || in.Stage == GroupTaskStageIdea:
			return nil, taskConflict("invalid_arg", "a column stands in next, not the ideas")
		}
		cols, err := listGroupTaskColumnsTx(tx, in.GroupID)
		if err != nil {
			return nil, err
		}
		i := columnIndex(cols, in.Column)
		if i < 0 {
			return nil, taskConflict("invalid_arg", "the board has no column %s", in.Column)
		}
		columnLabel = cols[i].Label
		stage = GroupTaskStageNext
	}
	// An assigned task is queued, done or not: assign promotes an idea, and
	// a done task reopened comes back assigned. A held one sits in the stage
	// it is held in.
	if in.AssigneeID != "" {
		stage = GroupTaskStageNext
		if in.Hold != "" {
			stage = in.Hold
		}
	}
	stage, rank, err := placeTask(tx, in.GroupID, stage, "", in.Position)
	if err != nil {
		return nil, err
	}
	// An anchor (before/after) names its own stage; an assigned task stays in
	// its stage (next, or the one it is held in), at its end.
	if want := firstNonEmptyString(in.Hold, GroupTaskStageNext); in.AssigneeID != "" && stage != want {
		if stage, rank, err = placeTask(tx, in.GroupID, want, "", GroupTaskPosition{Bottom: true}); err != nil {
			return nil, err
		}
	}
	id := newGroupTaskID()
	for range 8 {
		var exists int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM group_tasks WHERE id = ?)`, id).Scan(&exists); err != nil {
			return nil, err
		}
		if exists == 0 {
			break
		}
		id = newGroupTaskID()
	}
	now := time.Now()
	if _, err := tx.Exec(`INSERT INTO group_tasks (id, group_id, title, body, stage, status, rank, ref_kind, ref_url,
			ref_label, more_refs, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, in.GroupID, in.Title, in.Body, stage, rank, in.RefKind, in.RefURL, in.RefLabel, encodeMoreRefs(in.MoreRefs), in.Actor,
		dbTime(now), dbTime(now)); err != nil {
		return nil, err
	}
	if in.Column != "" {
		// An anchor (before/after) names its own stage: one among the ideas
		// cannot place a task that starts in a column.
		if stage != GroupTaskStageNext {
			return nil, taskConflict("invalid_arg", "a column stands in next; place the task beside a queued task, not an idea")
		}
		if err := refuseCordonedColumnTx(tx, in.GroupID, in.Column, ""); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET column_slug = ? WHERE id = ?`, in.Column, id); err != nil {
			return nil, err
		}
	}
	t := &GroupTask{ID: id, GroupID: in.GroupID}
	if err := setGroupTaskDepsTx(tx, t, in.DependsOn, nil); err != nil {
		return nil, err
	}
	if in.Kind != "" {
		if _, err := tx.Exec(`UPDATE group_tasks SET kind = ? WHERE id = ?`, in.Kind, id); err != nil {
			return nil, err
		}
	}
	if in.ParentID != "" {
		if _, err := checkGroupTaskParentTx(tx, in.GroupID, t, in.ParentID); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET parent_id = ? WHERE id = ?`, in.ParentID, id); err != nil {
			return nil, err
		}
		if err := addGroupTaskEventTx(tx, in.ParentID, in.Actor, GroupTaskEventChildAdded, id+" "+truncateUTF8(in.Title, 200), now); err != nil {
			return nil, err
		}
	}
	detail := "in " + firstNonEmptyString(columnLabel, stage)
	if in.Kind == GroupTaskKindEpic {
		detail = "epic, " + detail
	}
	if in.ParentID != "" {
		detail += ", in epic " + in.ParentID
	}
	if in.RefLabel != "" || in.RefURL != "" {
		detail += ", ref " + firstNonEmptyString(in.RefLabel, in.RefURL)
	}
	for _, ref := range in.MoreRefs {
		detail += ", ref " + firstNonEmptyString(ref.Label, ref.URL)
	}
	if in.Origin != "" {
		detail += ", " + in.Origin
	}
	kind := GroupTaskEventCreated
	if in.Imported {
		kind = GroupTaskEventImported
	}
	if err := addGroupTaskEventTx(tx, id, in.Actor, kind, detail, now); err != nil {
		return nil, err
	}
	if in.Question != nil {
		if in.AssigneeID != OperatorAssignee || status != GroupTaskOpen {
			return nil, taskConflict("invalid_arg", "a question is an open task for the operator")
		}
		// A question task: its first question is the one it asks, titled
		// as the task is (its body is the task's).
		if _, err := tx.Exec(`UPDATE group_tasks SET operator_request = ? WHERE id = ?`, groupTaskQuestionMarker, id); err != nil {
			return nil, err
		}
		in.Question.AskedBy, in.Question.Title, in.Question.Body, in.Question.Blocks = in.Actor, in.Title, "", false
		in.Question.AttachmentIDs = nil
		if err := askGroupTaskQuestionTx(tx, &GroupTask{ID: id, GroupID: in.GroupID}, in.Question, now); err != nil {
			return nil, err
		}
	}
	if in.AssigneeID != "" {
		if err := setGroupTaskAssigneeTx(tx, id, in.AssigneeID, in.AssignedBy, in.Hold == "", now); err != nil {
			return nil, err
		}
		if err := addGroupTaskEventTx(tx, id, in.Actor, GroupTaskEventAssigned, "→ "+in.AssigneeID+heldDetail(in.Hold), now); err != nil {
			return nil, err
		}
	}
	switch status {
	case GroupTaskBlocked:
		reason := strings.TrimSpace(in.BlockReason)
		if _, err := tx.Exec(`UPDATE group_tasks SET status = 'blocked', block_reason = ? WHERE id = ?`, reason, id); err != nil {
			return nil, err
		}
		if err := addGroupTaskEventTx(tx, id, in.Actor, GroupTaskEventBlocked, reason, now); err != nil {
			return nil, err
		}
		if err := addGroupTaskCommentTx(tx, id, in.Actor, GroupTaskCommentBlock, reason, now); err != nil {
			return nil, err
		}
	case GroupTaskDone:
		if _, err := tx.Exec(`UPDATE group_tasks SET status = 'done', closed_at = ? WHERE id = ?`, dbTime(now), id); err != nil {
			return nil, err
		}
		if err := addGroupTaskEventTx(tx, id, in.Actor, GroupTaskEventDone, "logged as done", now); err != nil {
			return nil, err
		}
	}
	files := append([]GroupTaskAttachment(nil), in.Attachments...)
	if err := insertGroupTaskAttachmentsTx(tx, id, 0, files, in.Actor, now); err != nil {
		return nil, err
	}
	if in.Question != nil {
		// A question task's files are its question's to show.
		if err := linkQuestionTaskFilesTx(tx, id, in.Question, files); err != nil {
			return nil, err
		}
	}
	if in.Blocks != "" {
		if status != GroupTaskOpen {
			return nil, taskConflict("invalid_arg", "a task that blocks another starts open")
		}
		created, err := getGroupTaskTx(tx, id)
		if err != nil {
			return nil, err
		}
		waiter, err := getGroupTaskTx(tx, in.Blocks)
		if err != nil {
			return nil, err
		}
		if err := blockGroupTaskOnTx(tx, waiter, created, in.Actor, in.BlocksExpect, now); err != nil {
			return nil, err
		}
	}
	// A new open child takes back the epic's close suggestion; one logged
	// as done may complete it.
	suggested, err := refreshEpicSuggestTx(tx, in.ParentID, now)
	if err != nil {
		return nil, err
	}
	out, err := getGroupTaskTx(tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if suggested {
		epicsCloseSuggested([]string{in.ParentID})
	}
	return out, nil
}

func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// GetGroupTask reads one task with its dependencies.
func GetGroupTask(id string) (*GroupTask, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return getGroupTaskTx(d, id)
}

// GroupTaskListOpts filters ListGroupTasks.
type GroupTaskListOpts struct {
	// Stage limits the list to one stage ("" for both).
	Stage string
	// Closed also lists done and dropped tasks not yet archived.
	Closed bool
	// Archived lists only the archived tasks, most recently closed first.
	Archived bool
	// Kind limits the list to epics (GroupTaskKindEpic) or to tasks
	// ("task"); "" lists both.
	Kind string
	// Parent limits the list to the children of that epic.
	Parent string
}

// ListGroupTasks lists a group's board in the operator's order: the live
// next tasks, then the live ideas, each by rank; then, with Closed, the done
// and dropped tasks not yet archived, most recently closed first.
func ListGroupTasks(groupID int64, opts GroupTaskListOpts) ([]*GroupTask, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	// Each read is one index range, so the board costs what it shows, not
	// what the group has archived: the live tasks by idx_group_tasks_group,
	// the recently closed ones by the partial idx_group_tasks_history. (An
	// OR of the two narrowed to group_id only and walked every archived row.)
	stage, stageArgs := "", []any{}
	if opts.Stage != "" {
		stage, stageArgs = ` AND stage = ?`, []any{opts.Stage}
	}
	switch opts.Kind {
	case "":
	case GroupTaskKindEpic:
		stage += ` AND kind = 'epic'`
	case "task":
		stage += ` AND kind = ''`
	default:
		return nil, taskConflict("invalid_arg", "kind is epic or task, not %s", opts.Kind)
	}
	if opts.Parent != "" {
		stage += ` AND parent_id = ?`
		stageArgs = append(stageArgs, opts.Parent)
	}
	read := func(where, order string) ([]*GroupTask, error) {
		return queryRows(d, scanGroupTask, `SELECT `+groupTaskCols+` FROM group_tasks WHERE `+where+stage+` ORDER BY `+order,
			append([]any{groupID}, stageArgs...)...)
	}
	var tasks []*GroupTask
	if opts.Archived {
		tasks, err = read(`group_id = ? AND archived_at IS NOT NULL`, `closed_at DESC, id`)
	} else {
		tasks, err = read(`group_id = ? AND status IN ('open','blocked')`,
			`CASE stage WHEN 'next' THEN 0 ELSE 1 END, rank, closed_at DESC, id`)
		if err == nil && opts.Closed {
			var closed []*GroupTask
			closed, err = read(`group_id = ? AND archived_at IS NULL AND status IN ('done','dropped')`, `closed_at DESC, id`)
			tasks = append(tasks, closed...)
		}
	}
	if err != nil {
		return nil, err
	}
	deps, err := groupTaskDepsOf(d, tasks)
	if err != nil {
		return nil, err
	}
	byID := map[string]*GroupTask{}
	for _, t := range tasks {
		byID[t.ID] = t
	}
	for _, p := range deps {
		if t := byID[p[0]]; t != nil {
			t.DependsOn = append(t.DependsOn, p[1])
		}
	}
	return tasks, nil
}

// groupTaskDepsOf reads the dependencies of the given tasks only, as
// (task, depends_on) pairs ordered by both.
func groupTaskDepsOf(q taskTx, tasks []*GroupTask) ([][2]string, error) {
	var out [][2]string
	for i := 0; i < len(tasks); i += 500 {
		chunk := tasks[i:min(i+500, len(tasks))]
		args := make([]any, len(chunk))
		for j, t := range chunk {
			args[j] = t.ID
		}
		pairs, err := queryRows(q, func(s rowScanner) ([2]string, error) {
			var p [2]string
			err := s.Scan(&p[0], &p[1])
			return p, err
		}, `SELECT task_id, depends_on FROM group_task_deps WHERE task_id IN (?`+strings.Repeat(",?", len(chunk)-1)+`)
			ORDER BY task_id, depends_on`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, pairs...)
	}
	return out, nil
}

// ListGroupTaskEvents is a task's history, oldest first.
func ListGroupTaskEvents(taskID string) ([]*GroupTaskEvent, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return queryRows(d, scanGroupTaskEvent, `SELECT `+groupTaskEventCols+` FROM group_task_events WHERE task_id = ? ORDER BY at, id`, taskID)
}

// GroupTaskEdit changes a task's text, ref and dependencies; a nil field is
// left as it is.
type GroupTaskEdit struct {
	Title    *string
	Body     *string
	RefKind  *string
	RefURL   *string
	RefLabel *string
	// MoreRefs, when set, replaces the refs after the first.
	MoreRefs *[]GroupTaskRef
	AddDeps  []string
	RmDeps   []string
	// ParentID, when set, moves the task into that epic ("" takes it out
	// of its epic), recorded as its own event on the task and the epics.
	ParentID *string
	Actor    string
}

// withGroupTask runs fn on task id inside a transaction and returns the task
// as it is afterwards.
func withGroupTask(id string, fn func(tx *sql.Tx, t *GroupTask, now time.Time) error) (*GroupTask, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	tx, err := d.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	t, err := getGroupTaskTx(tx, id)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	oldParent := t.ParentID
	if err := fn(tx, t, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE group_tasks SET updated_at = ? WHERE id = ?`, dbTime(now), id); err != nil {
		return nil, err
	}
	out, err := getGroupTaskTx(tx, id)
	if err != nil {
		return nil, err
	}
	// Any change to a task may complete its epic, or undo that; any change
	// to an epic may too (reopened, closed).
	epics := []string{oldParent, out.ParentID}
	if out.IsEpic() {
		epics = append(epics, id)
	}
	var suggested []string
	for _, epic := range epics {
		ok, err := refreshEpicSuggestTx(tx, epic, now)
		if err != nil {
			return nil, err
		}
		// The board flags an epic reopened with its tasks all closed, but
		// whoever reopened it is not mailed about it.
		if ok && epic != id {
			suggested = append(suggested, epic)
		}
	}
	if out.IsEpic() {
		if out, err = getGroupTaskTx(tx, id); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	epicsCloseSuggested(suggested)
	return out, nil
}

// EditGroupTask applies an edit and records which fields changed.
func EditGroupTask(id string, e GroupTaskEdit) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		var changed []string
		var changes []GroupTaskChange
		set := func(col string, cur string, v *string) error {
			if v == nil || *v == cur {
				return nil
			}
			changed = append(changed, strings.TrimPrefix(col, "ref_"))
			changes = append(changes, GroupTaskChange{Field: col, Before: cur, After: *v})
			_, err := tx.Exec(`UPDATE group_tasks SET `+col+` = ? WHERE id = ?`, *v, id)
			return err
		}
		for _, f := range []struct {
			col string
			cur string
			v   *string
		}{
			{"title", t.Title, e.Title}, {"body", t.Body, e.Body}, {"ref_kind", t.RefKind, e.RefKind},
			{"ref_url", t.RefURL, e.RefURL}, {"ref_label", t.RefLabel, e.RefLabel},
		} {
			if err := set(f.col, f.cur, f.v); err != nil {
				return err
			}
		}
		if e.MoreRefs != nil {
			before, after := encodeMoreRefs(t.MoreRefs), encodeMoreRefs(*e.MoreRefs)
			if before != after {
				changed = append(changed, "refs")
				changes = append(changes, GroupTaskChange{Field: "more_refs", Before: before, After: after})
				if _, err := tx.Exec(`UPDATE group_tasks SET more_refs = ? WHERE id = ?`, after, id); err != nil {
					return err
				}
			}
		}
		if len(e.AddDeps) > 0 || len(e.RmDeps) > 0 {
			before := strings.Join(t.DependsOn, " ")
			if err := setGroupTaskDepsTx(tx, t, e.AddDeps, e.RmDeps); err != nil {
				return err
			}
			deps, err := groupTaskDepsTx(tx, id)
			if err != nil {
				return err
			}
			if after := strings.Join(deps, " "); after != before {
				changes = append(changes, GroupTaskChange{Field: "depends_on", Before: before, After: after})
			}
			for _, dep := range e.AddDeps {
				changed = append(changed, "+after "+dep)
			}
			for _, dep := range e.RmDeps {
				changed = append(changed, "-after "+dep)
				// A task no longer waits on a dependency it dropped.
				if _, err := tx.Exec(`UPDATE group_tasks SET blocked_on = '' WHERE id = ? AND blocked_on = ?`, id, dep); err != nil {
					return err
				}
			}
		}
		if e.ParentID != nil && *e.ParentID != t.ParentID {
			// withGroupTask brings both epics' close suggestions up to date.
			if err := setGroupTaskParentTx(tx, t, *e.ParentID, e.Actor, now); err != nil {
				return err
			}
		}
		if len(changed) == 0 {
			return nil
		}
		return addGroupTaskChangeTx(tx, id, e.Actor, GroupTaskEventEdited, strings.Join(changed, ", "), changes, now)
	})
}

// MoveGroupTask places a live task; a move relative to a task of the other
// stage also promotes or demotes it, in the same write.
func MoveGroupTask(id string, pos GroupTaskPosition, actor string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if !t.Live() {
			return taskConflict("not_live", "task %s is %s; only an open or blocked task moves", id, t.Status)
		}
		stage, rank, err := placeTask(tx, t.GroupID, t.Stage, id, pos)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET stage = ?, rank = ? WHERE id = ?`, stage, rank, id); err != nil {
			return err
		}
		if err := addGroupTaskEventTx(tx, id, actor, GroupTaskEventMoved, describePosition(pos), now); err != nil {
			return err
		}
		return stageChangeEvent(tx, t, stage, actor, "on move", now)
	})
}

func describePosition(pos GroupTaskPosition) string {
	switch {
	case pos.Top:
		return "to the top"
	case pos.Before != "":
		return "before " + pos.Before
	case pos.After != "":
		return "after " + pos.After
	default:
		return "to the bottom"
	}
}

func stageChangeEvent(tx *sql.Tx, t *GroupTask, stage, actor, detail string, now time.Time) error {
	switch {
	case t.Stage == stage:
		return nil
	case stage == GroupTaskStageNext:
		return addGroupTaskChangeTx(tx, t.ID, actor, GroupTaskEventPromoted, detail, stageChange(t.Stage, stage), now)
	default:
		return addGroupTaskChangeTx(tx, t.ID, actor, GroupTaskEventDemoted, detail, stageChange(t.Stage, stage), now)
	}
}

// SetGroupTaskStage promotes a task to the end of the queue or demotes it to
// the top of the ideas. A task already in that stage is left as it is.
func SetGroupTaskStage(id, stage, actor string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if t.Stage == stage {
			return nil
		}
		if !t.Live() {
			return taskConflict("not_live", "task %s is %s; reopen it first", id, t.Status)
		}
		if !t.Started() {
			if err := checkOperatorIdeaTx(tx, t, t.AssigneeID, stage); err != nil {
				return err
			}
		}
		pos := GroupTaskPosition{Bottom: true}
		if stage == GroupTaskStageIdea {
			pos = GroupTaskPosition{Top: true}
		}
		_, rank, err := placeTask(tx, t.GroupID, stage, id, pos)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET stage = ?, rank = ? WHERE id = ?`, stage, rank, id); err != nil {
			return err
		}
		return stageChangeEvent(tx, t, stage, actor, "", now)
	})
}

// SetGroupTaskStatus changes a task's status: block (with a reason) and
// unblock a live task, close it as done or dropped, or reopen a closed one
// where it was, in its stage and rank.
func SetGroupTaskStatus(id, status, reason, actor string) (*GroupTask, error) {
	return SetGroupTaskStatusFor(id, status, reason, actor, AnyAssignee)
}

// AnyAssignee is the expectAssignee of a change that does not depend on who
// holds the task.
const AnyAssignee = "\x00any"

// errAssigneeChanged is a change gated against an assignee that no longer
// holds the task.
func errAssigneeChanged(id string) error {
	return taskConflict("assignee_changed", "task %s changed hands meanwhile; look again and retry", id)
}

// SetGroupTaskStatusFor is SetGroupTaskStatus for a caller authorised against
// expectAssignee ("" for unassigned): it refuses when the task is held by
// someone else by the time it is changed.
func SetGroupTaskStatusFor(id, status, reason, actor, expectAssignee string) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		return setGroupTaskStatusTx(tx, t, status, reason, actor, expectAssignee, now)
	})
}

// SetGroupTaskStatusForced is SetGroupTaskStatusFor that, with force, also
// closes an epic as done while children of it are still open.
func SetGroupTaskStatusForced(id, status, reason, actor, expectAssignee string, force bool) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		return setGroupTaskStatusOptsTx(tx, t, status, reason, actor, expectAssignee, force, now)
	})
}

// setGroupTaskStatusTx is SetGroupTaskStatusFor's change, in tx.
func setGroupTaskStatusTx(tx *sql.Tx, t *GroupTask, status, reason, actor, expectAssignee string, now time.Time) error {
	return setGroupTaskStatusOptsTx(tx, t, status, reason, actor, expectAssignee, false, now)
}

func setGroupTaskStatusOptsTx(tx *sql.Tx, t *GroupTask, status, reason, actor, expectAssignee string, force bool, now time.Time) error {
	id := t.ID
	if expectAssignee != AnyAssignee && t.AssigneeID != expectAssignee {
		return errAssigneeChanged(id)
	}
	if t.Status == status && status != GroupTaskBlocked {
		return nil
	}
	if t.IsEpic() {
		if err := checkEpicStatusTx(tx, t, status, force); err != nil {
			return err
		}
		if status == GroupTaskDropped {
			// Dropping an epic leaves its work: the children stay, out of it.
			if err := releaseGroupTaskChildrenTx(tx, t, actor, now); err != nil {
				return err
			}
		}
	}
	var kind, detail string
	switch status {
	case GroupTaskBlocked:
		if !t.Live() {
			return taskConflict("not_live", "task %s is %s; reopen it first", id, t.Status)
		}
		if t.Status == GroupTaskBlocked && t.BlockReason == reason {
			return nil
		}
		if _, err := tx.Exec(`UPDATE group_tasks SET status = 'blocked', block_reason = ?, blocked_on = '' WHERE id = ?`, reason, id); err != nil {
			return err
		}
		kind, detail = GroupTaskEventBlocked, reason
	case GroupTaskOpen:
		kind = GroupTaskEventUnblocked
		// An unblock may say why (a question answered).
		detail = reason
		if !t.Live() {
			kind = GroupTaskEventReopened
			detail = "was " + t.Status
		}
		// Unblocking or reopening gives the task a fresh run of attempts.
		if _, err := tx.Exec(`UPDATE group_tasks SET status = 'open', block_reason = '', blocked_on = '', closed_at = NULL,
			archived_at = NULL, failures = 0 WHERE id = ?`, id); err != nil {
			return err
		}
		// A task closed while assigned comes back without an assignee that
		// has since been retired (the retire trigger only covers live tasks).
		if _, err := tx.Exec(`UPDATE group_tasks SET assignee_id = NULL, assignee_kind = '', started_at = NULL, assigned_by = '', assigned_at = NULL
			WHERE id = ? AND assignee_id IN (SELECT agent_id FROM agents WHERE retired_at IS NOT NULL)`, id); err != nil {
			return err
		}
		if kind == GroupTaskEventReopened {
			var live int
			if err := tx.QueryRow(`SELECT COUNT(*) FROM group_tasks WHERE group_id = ? AND status IN ('open','blocked')`,
				t.GroupID).Scan(&live); err != nil {
				return err
			}
			if live > GroupTaskLiveCap {
				return ErrGroupTaskCap
			}
			// Its rank may have been handed out while it was closed (closed
			// tasks leave the rank bounds): keep it where it was, just after
			// the live task that now holds that rank.
			var taken int
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM group_tasks WHERE group_id = ? AND stage = ?
				AND status IN ('open','blocked') AND rank = ? AND id <> ?)`,
				t.GroupID, t.Stage, t.Rank, id).Scan(&taken); err != nil {
				return err
			}
			if taken != 0 {
				above, err := neighbourRank(tx, t.GroupID, t.Stage, t.Rank, id, false)
				if err != nil {
					return err
				}
				if _, err := tx.Exec(`UPDATE group_tasks SET rank = ? WHERE id = ?`,
					taskRankBetween(t.Rank, above), id); err != nil {
					return err
				}
			}
		}
	case GroupTaskDone, GroupTaskDropped:
		if _, err := tx.Exec(`UPDATE group_tasks SET status = ?, closed_at = ?, archived_at = NULL, blocked_on = '' WHERE id = ?`,
			status, dbTime(now), id); err != nil {
			return err
		}
		// Done releases the tasks blocked waiting on this one.
		if status == GroupTaskDone {
			if err := releaseGroupTaskWaitersTx(tx, t, actor, now); err != nil {
				return err
			}
		}
		kind, detail = status, reason
		if t.Status == GroupTaskBlocked {
			detail = firstNonEmptyString(reason, "was blocked: "+t.BlockReason)
		}
	default:
		return taskConflict("invalid_arg", "unknown status %q", status)
	}
	var changes []GroupTaskChange
	if t.Status != status {
		changes = statusChange(t.Status, status)
	}
	if err := addGroupTaskChangeTx(tx, id, actor, kind, detail, changes, now); err != nil {
		return err
	}
	// A reason is a typed comment, committed with its transition.
	switch status {
	case GroupTaskBlocked:
		return addGroupTaskCommentTx(tx, id, actor, GroupTaskCommentBlock, reason, now)
	case GroupTaskDone:
		return addGroupTaskCommentTx(tx, id, actor, GroupTaskCommentDone, reason, now)
	case GroupTaskDropped:
		return addGroupTaskCommentTx(tx, id, actor, GroupTaskCommentDrop, reason, now)
	}
	return nil
}

// SweepGroupTasks archives the done and dropped tasks closed before
// archiveBefore (they leave --all) and purges those closed before
// purgeBefore, with their history. A zero bound skips that step.
func SweepGroupTasks(archiveBefore, purgeBefore time.Time) (archived, purged int64, err error) {
	d, err := Open()
	if err != nil {
		return 0, 0, err
	}
	tx, err := d.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	if !purgeBefore.IsZero() {
		// By closed_at: only the tasks past retention are read. (By status
		// the planner would walk every closed task.)
		// Tasks first, then the epics none of whose children are left: an
		// epic outlives its children, which keep pointing at it.
		for _, kind := range []string{`kind <> 'epic'`, `kind = 'epic' AND NOT EXISTS(SELECT 1 FROM group_tasks c WHERE c.parent_id = group_tasks.id)`} {
			res, err := tx.Exec(`DELETE FROM group_tasks INDEXED BY idx_group_tasks_closed
				WHERE status IN ('done','dropped') AND closed_at IS NOT NULL AND closed_at < ? AND `+kind, dbTime(purgeBefore))
			if err != nil {
				return 0, 0, err
			}
			n, _ := res.RowsAffected()
			purged += n
		}
	}
	if !archiveBefore.IsZero() {
		// Only the closed tasks not yet archived are candidates; the partial
		// index holds exactly those, where idx_group_tasks_closed would walk
		// every archived row closed before the bound as well.
		ids, err := queryRows(tx, func(s rowScanner) (string, error) {
			var id string
			err := s.Scan(&id)
			return id, err
		}, `SELECT id FROM group_tasks INDEXED BY idx_group_tasks_history
			WHERE archived_at IS NULL AND status IN ('done','dropped') AND closed_at IS NOT NULL AND closed_at < ?
			ORDER BY kind = 'epic'`, dbTime(archiveBefore))
		if err != nil {
			return 0, 0, err
		}
		kept := 0
		for _, id := range ids {
			// Epics come last, and wait for their children.
			if waits, err := epicWaitsForChildrenTx(tx, id); err != nil {
				return 0, 0, err
			} else if waits {
				kept++
				continue
			}
			if _, err := tx.Exec(`UPDATE group_tasks SET archived_at = ? WHERE id = ?`, dbTime(now), id); err != nil {
				return 0, 0, err
			}
			if err := addGroupTaskEventTx(tx, id, "system:sweep", GroupTaskEventArchived, "", now); err != nil {
				return 0, 0, err
			}
		}
		archived = int64(len(ids) - kept)
	}
	return archived, purged, tx.Commit()
}

// GroupTaskStatuses maps the given task ids to their status; an id with no
// task (a purged dependency) is absent.
func GroupTaskStatuses(ids []string) (map[string]string, error) {
	out := map[string]string{}
	seen := map[string]bool{}
	uniq := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			uniq = append(uniq, id)
		}
	}
	ids = uniq
	if len(ids) == 0 {
		return out, nil
	}
	d, err := Open()
	if err != nil {
		return nil, err
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := queryRows(d, func(s rowScanner) ([2]string, error) {
		var p [2]string
		err := s.Scan(&p[0], &p[1])
		return p, err
	}, `SELECT id, status FROM group_tasks WHERE id IN (?`+strings.Repeat(",?", len(ids)-1)+`)`, args...)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		out[p[0]] = p[1]
	}
	return out, nil
}
