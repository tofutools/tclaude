package db

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
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

// Event kinds written by this slice. The assign/report kinds (assigned,
// unassigned, taken, released, reported, stalled) and imported arrive with the
// slices that write them.
const (
	GroupTaskEventCreated   = "created"
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
	RefKind     string
	RefURL      string
	RefLabel    string
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
	// DependsOn is filled by the single-task reads and by ListGroupTasks.
	DependsOn []string
}

// Live reports whether the task is on the live list (open or blocked).
func (t *GroupTask) Live() bool { return t.Status == GroupTaskOpen || t.Status == GroupTaskBlocked }

// GroupTaskEvent is one row of a task's history.
type GroupTaskEvent struct {
	ID     int64
	TaskID string
	At     time.Time
	Actor  string
	Kind   string
	Detail string
}

const groupTaskCols = `id, group_id, title, body, stage, status, rank, block_reason, ref_kind, ref_url, ref_label,
	COALESCE(assignee_id, ''), assigned_by, assigned_at, outcome, outcome_note, failures, created_by,
	created_at, updated_at, closed_at, archived_at`

func scanGroupTask(s rowScanner) (*GroupTask, error) {
	var t GroupTask
	var assigned, created, updated, closed, archived dbTimestamp
	if err := s.Scan(&t.ID, &t.GroupID, &t.Title, &t.Body, &t.Stage, &t.Status, &t.Rank, &t.BlockReason,
		&t.RefKind, &t.RefURL, &t.RefLabel, &t.AssigneeID, &t.AssignedBy, &assigned, &t.Outcome, &t.OutcomeNote,
		&t.Failures, &t.CreatedBy, &created, &updated, &closed, &archived); err != nil {
		return nil, err
	}
	t.AssignedAt, t.CreatedAt, t.UpdatedAt = assigned.Time(), created.Time(), updated.Time()
	t.ClosedAt, t.ArchivedAt = closed.Time(), archived.Time()
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
	GroupID   int64
	Title     string
	Body      string
	Stage     string
	RefKind   string
	RefURL    string
	RefLabel  string
	DependsOn []string
	Position  GroupTaskPosition
	Actor     string
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
	if len(detail) > GroupTaskEventDetailMax {
		detail = truncateUTF8(detail, GroupTaskEventDetailMax)
	}
	_, err := q.Exec(`INSERT INTO group_task_events (task_id, at, actor, kind, detail) VALUES (?, ?, ?, ?, ?)`,
		taskID, dbTime(at), actor, kind, detail)
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
	var live int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM group_tasks WHERE group_id = ? AND status IN ('open','blocked')`,
		in.GroupID).Scan(&live); err != nil {
		return nil, err
	}
	if live >= GroupTaskLiveCap {
		return nil, ErrGroupTaskCap
	}
	stage := in.Stage
	if stage == "" {
		stage = GroupTaskStageIdea
	}
	stage, rank, err := placeTask(tx, in.GroupID, stage, "", in.Position)
	if err != nil {
		return nil, err
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
			ref_label, created_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'open', ?, ?, ?, ?, ?, ?, ?)`,
		id, in.GroupID, in.Title, in.Body, stage, rank, in.RefKind, in.RefURL, in.RefLabel, in.Actor,
		dbTime(now), dbTime(now)); err != nil {
		return nil, err
	}
	t := &GroupTask{ID: id, GroupID: in.GroupID}
	if err := setGroupTaskDepsTx(tx, t, in.DependsOn, nil); err != nil {
		return nil, err
	}
	detail := "in " + stage
	if in.RefLabel != "" || in.RefURL != "" {
		detail += ", ref " + firstNonEmptyString(in.RefLabel, in.RefURL)
	}
	if err := addGroupTaskEventTx(tx, id, in.Actor, GroupTaskEventCreated, detail, now); err != nil {
		return nil, err
	}
	out, err := getGroupTaskTx(tx, id)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
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
}

// ListGroupTasks lists a group's board in the operator's order: the live
// next tasks, then the live ideas, each by rank; then, with Closed, the done
// and dropped tasks not yet archived, most recently closed first.
func ListGroupTasks(groupID int64, opts GroupTaskListOpts) ([]*GroupTask, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	where := `group_id = ? AND (status IN ('open','blocked')`
	if opts.Closed {
		where += ` OR archived_at IS NULL`
	}
	where += `)`
	args := []any{groupID}
	if opts.Stage != "" {
		where += ` AND stage = ?`
		args = append(args, opts.Stage)
	}
	tasks, err := queryRows(d, scanGroupTask, `SELECT `+groupTaskCols+` FROM group_tasks WHERE `+where+`
		ORDER BY CASE WHEN status IN ('open','blocked') THEN 0 ELSE 1 END,
			CASE WHEN status IN ('open','blocked') THEN CASE stage WHEN 'next' THEN 0 ELSE 1 END ELSE 0 END,
			CASE WHEN status IN ('open','blocked') THEN rank END,
			closed_at DESC, id`, args...)
	if err != nil {
		return nil, err
	}
	deps, err := queryRows(d, func(s rowScanner) ([2]string, error) {
		var p [2]string
		err := s.Scan(&p[0], &p[1])
		return p, err
	}, `SELECT d.task_id, d.depends_on FROM group_task_deps d JOIN group_tasks t ON t.id = d.task_id
		WHERE t.group_id = ? ORDER BY d.task_id, d.depends_on`, groupID)
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

// ListGroupTaskEvents is a task's history, oldest first.
func ListGroupTaskEvents(taskID string) ([]*GroupTaskEvent, error) {
	d, err := Open()
	if err != nil {
		return nil, err
	}
	return queryRows(d, func(s rowScanner) (*GroupTaskEvent, error) {
		var e GroupTaskEvent
		var at dbTimestamp
		if err := s.Scan(&e.ID, &e.TaskID, &at, &e.Actor, &e.Kind, &e.Detail); err != nil {
			return nil, err
		}
		e.At = at.Time()
		return &e, nil
	}, `SELECT id, task_id, at, actor, kind, detail FROM group_task_events WHERE task_id = ? ORDER BY id`, taskID)
}

// GroupTaskEdit changes a task's text, ref and dependencies; a nil field is
// left as it is.
type GroupTaskEdit struct {
	Title    *string
	Body     *string
	RefKind  *string
	RefURL   *string
	RefLabel *string
	AddDeps  []string
	RmDeps   []string
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
	return out, tx.Commit()
}

// EditGroupTask applies an edit and records which fields changed.
func EditGroupTask(id string, e GroupTaskEdit) (*GroupTask, error) {
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		var changed []string
		set := func(col string, cur string, v *string) error {
			if v == nil || *v == cur {
				return nil
			}
			changed = append(changed, strings.TrimPrefix(col, "ref_"))
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
		if len(e.AddDeps) > 0 || len(e.RmDeps) > 0 {
			if err := setGroupTaskDepsTx(tx, t, e.AddDeps, e.RmDeps); err != nil {
				return err
			}
			for _, dep := range e.AddDeps {
				changed = append(changed, "+after "+dep)
			}
			for _, dep := range e.RmDeps {
				changed = append(changed, "-after "+dep)
			}
		}
		if len(changed) == 0 {
			return nil
		}
		return addGroupTaskEventTx(tx, id, e.Actor, GroupTaskEventEdited, strings.Join(changed, ", "), now)
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
		return addGroupTaskEventTx(tx, t.ID, actor, GroupTaskEventPromoted, detail, now)
	default:
		return addGroupTaskEventTx(tx, t.ID, actor, GroupTaskEventDemoted, detail, now)
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
	return withGroupTask(id, func(tx *sql.Tx, t *GroupTask, now time.Time) error {
		if t.Status == status && status != GroupTaskBlocked {
			return nil
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
			if _, err := tx.Exec(`UPDATE group_tasks SET status = 'blocked', block_reason = ? WHERE id = ?`, reason, id); err != nil {
				return err
			}
			kind, detail = GroupTaskEventBlocked, reason
		case GroupTaskOpen:
			kind = GroupTaskEventUnblocked
			if !t.Live() {
				kind = GroupTaskEventReopened
				detail = "was " + t.Status
			}
			if _, err := tx.Exec(`UPDATE group_tasks SET status = 'open', block_reason = '', closed_at = NULL,
				archived_at = NULL WHERE id = ?`, id); err != nil {
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
			if _, err := tx.Exec(`UPDATE group_tasks SET status = ?, closed_at = ?, archived_at = NULL WHERE id = ?`,
				status, dbTime(now), id); err != nil {
				return err
			}
			kind, detail = status, reason
			if t.Status == GroupTaskBlocked {
				detail = firstNonEmptyString(reason, "was blocked: "+t.BlockReason)
			}
		default:
			return taskConflict("invalid_arg", "unknown status %q", status)
		}
		return addGroupTaskEventTx(tx, id, actor, kind, detail, now)
	})
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
		res, err := tx.Exec(`DELETE FROM group_tasks WHERE status IN ('done','dropped') AND closed_at IS NOT NULL
			AND closed_at < ?`, dbTime(purgeBefore))
		if err != nil {
			return 0, 0, err
		}
		purged, _ = res.RowsAffected()
	}
	if !archiveBefore.IsZero() {
		ids, err := queryRows(tx, func(s rowScanner) (string, error) {
			var id string
			err := s.Scan(&id)
			return id, err
		}, `SELECT id FROM group_tasks WHERE status IN ('done','dropped') AND archived_at IS NULL
			AND closed_at IS NOT NULL AND closed_at < ?`, dbTime(archiveBefore))
		if err != nil {
			return 0, 0, err
		}
		for _, id := range ids {
			if _, err := tx.Exec(`UPDATE group_tasks SET archived_at = ? WHERE id = ?`, dbTime(now), id); err != nil {
				return 0, 0, err
			}
			if err := addGroupTaskEventTx(tx, id, "system:sweep", GroupTaskEventArchived, "", now); err != nil {
				return 0, 0, err
			}
		}
		archived = int64(len(ids))
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
