package agentd

import (
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Work-in-progress limits (docs/taskboard.md "Limits"). A board may cap how
// many tasks a lane holds at once: In progress (an agent's started tasks in
// no custom column) and each custom column, counted over the board or per
// assignee. A change that would put a task into a full lane meets the lane's
// at_limit:
//
//   - queue (the default): the change is made short of the lane. A task for
//     In progress is held in Next; a task for a column stays where it is.
//     Either way it is queued for the lane, and the first in line goes in
//     when a slot frees (taskboardFillLanes).
//   - refuse: the change is refused, 409 limit_reached.
//   - warn: the task goes in over the limit, and the answer says so.
//
// The operator may go over any limit with over_limit (after a confirm in the
// dashboard); an agent follows the rule. Each time a task meets a full lane
// the board's limit_reached message is sent.
//
// A blocked task keeps its slot: it is still the lane's work.

// taskboardLaneLocks serialises a board's lane changes, so two writers
// cannot both take the last slot: one lock per group, held from the count to
// the write (and by the fill).
var taskboardLaneLocks sync.Map // int64 → *sync.Mutex

func lockTaskboardLanes(groupID int64) (unlock func()) {
	mu, _ := taskboardLaneLocks.LoadOrStore(groupID, &sync.Mutex{})
	m := mu.(*sync.Mutex)
	m.Lock()
	return m.Unlock
}

// taskboardLimitNote is what a write's answer says of a limit it met.
type taskboardLimitNote struct {
	// Lane is in_progress or a column slug; Label names it ("In progress").
	Lane  string `json:"lane"`
	Label string `json:"label"`
	Max   int    `json:"max"`
	// Count is how many tasks were in the lane (the assignee's, per
	// assignee) before the change.
	Count int    `json:"count"`
	Per   string `json:"per"`
	// Action is queued, warned, or over (the operator went over the limit).
	Action string `json:"action"`
	// Position is the task's place in line, from 1, when queued.
	Position int `json:"position,omitempty"`
}

// Limit actions in a taskboardLimitNote.
const (
	taskboardLimitQueued = "queued"
	taskboardLimitWarned = "warned"
	taskboardLimitOver   = "over"
)

// taskboardLimitHit is a full lane a change met, and what came of it.
type taskboardLimitHit struct {
	note taskboardLimitNote
}

// queued reports whether the change is to be made short of the lane, the
// task queued for it.
func (h *taskboardLimitHit) queued() bool { return h != nil && h.note.Action == taskboardLimitQueued }

// queued reports whether the note is of a task put in line.
func (n *taskboardLimitNote) queued() bool { return n != nil && n.Action == taskboardLimitQueued }

// laneLabel names a lane for people.
func taskboardLaneLabel(groupID int64, lane string) string {
	if lane == db.QueueLaneInProgress {
		return "In progress"
	}
	cols, err := db.ListGroupTaskColumns(groupID)
	if err == nil {
		for _, c := range cols {
			if c.Slug == lane {
				return c.Label
			}
		}
	}
	return lane
}

// freshTaskboardTask re-reads t once the board's lane lock is held: a lane
// decision is made on the task as it is now, not as the route read it before
// another writer changed it. A task that changed hands meanwhile is refused,
// as the write itself would refuse it (assignee_changed).
func freshTaskboardTask(w http.ResponseWriter, t *db.GroupTask) (*db.GroupTask, bool) {
	fresh, err := db.GetGroupTask(t.ID)
	if err != nil {
		writeTaskboardError(w, err)
		return nil, false
	}
	if fresh.AssigneeID != t.AssigneeID {
		writeError(w, http.StatusConflict, "assignee_changed", "task "+t.ID+" changed hands meanwhile; look again and retry")
		return nil, false
	}
	return fresh, true
}

// requireTaskboardOverLimit refuses over_limit from anyone but the operator:
// agents follow the board's limits.
func requireTaskboardOverLimit(w http.ResponseWriter, r *http.Request, over bool) bool {
	if over && !taskboardCallerIsOperator(r) {
		writeError(w, http.StatusForbidden, "permission", "only the operator goes over a board's limit (over_limit)")
		return false
	}
	return true
}

// taskboardCallerIsOperator reports whether the caller is this node's
// operator.
func taskboardCallerIsOperator(r *http.Request) bool {
	return confirmedAgentConv(r) == "" && callerIsHuman(r) && taskboardPeerAuthor(r) == ""
}

// checkTaskboardLimit checks a change that leaves a task as want (on g's
// board) against the board's limits; was is the task before it (nil for a
// new one). It answers nil when the task meets no full lane: it enters
// none, or one with room. A refused change has been answered (ok false).
// Hold the board's lane lock from the check through the write.
func checkTaskboardLimit(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, was, want *db.GroupTask, over bool) (hit *taskboardLimitHit, ok bool) {
	if !requireTaskboardOverLimit(w, r, over) {
		return nil, false
	}
	lane := want.GroupTaskLane()
	if lane == "" {
		return nil, true
	}
	_, o, cols, err := taskboardOptions(g.ID)
	if err != nil {
		slog.Warn("taskboard: board options unreadable; no limits", "group", g.Name, "error", err)
		return nil, true
	}
	limits := boardopts.Resolve(o, taskboardOptionsGlobals(), cols).Limits
	l := limits.Lane(lane)
	if l.Max.Value <= 0 {
		return nil, true
	}
	assignee := ""
	if limits.PerAssignee() {
		// Per assignee, a task nobody holds counts against no one.
		if want.AssigneeID == "" || want.AssigneeID == db.OperatorAssignee {
			return nil, true
		}
		assignee = want.AssigneeID
	}
	// Already in the lane (and, per assignee, the same agent's): it takes
	// no new slot.
	if was != nil && was.GroupID == want.GroupID && was.InGroupTaskLane(lane) && (assignee == "" || was.AssigneeID == assignee) {
		return nil, true
	}
	// A cordoned column refuses the task anyway: leave that to the write.
	if lane != db.QueueLaneInProgress {
		if states, err := db.ListGroupTaskColumnStates(g.ID); err == nil && states[lane] == db.GroupTaskColumnCordoned {
			return nil, true
		}
	}
	count, err := db.GroupTaskLaneCount(g.ID, lane, assignee)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "db", err.Error())
		return nil, false
	}
	// In queue mode the tasks already in line for the lane hold their
	// places: a slot not yet filled is theirs, not a newcomer's.
	if count < l.Max.Value && l.AtLimit.Value == boardopts.AtLimitQueue {
		count += taskboardWaitingFor(g.ID, lane, assignee, was)
	}
	if count < l.Max.Value {
		return nil, true
	}
	hit = &taskboardLimitHit{note: taskboardLimitNote{Lane: lane, Label: taskboardLaneLabel(g.ID, lane),
		Max: l.Max.Value, Count: count, Per: limits.Per.Value}}
	full := hit.fullText()
	switch {
	case over:
		hit.note.Action = taskboardLimitOver
	case l.AtLimit.Value == boardopts.AtLimitRefuse:
		// A refused add made no task to write about: the caller has the 409.
		if was != nil {
			taskboardLimitReached(r, g, want, fmt.Sprintf("refused: %s", full), nil)
		}
		msg := full + "; the board refuses more"
		if taskboardCallerIsOperator(r) {
			msg += " (go over it with over_limit)"
		}
		writeError(w, http.StatusConflict, "limit_reached", msg)
		return nil, false
	case l.AtLimit.Value == boardopts.AtLimitWarn,
		// Already in the column (per assignee, changing hands): there is no
		// "short of the lane" to keep it in, so it stays, over the limit.
		lane != db.QueueLaneInProgress && was != nil && was.GroupID == want.GroupID && was.InGroupTaskLane(lane):
		hit.note.Action = taskboardLimitWarned
	default:
		hit.note.Action = taskboardLimitQueued
	}
	return hit, true
}

// recheckTaskboardInProgress checks In progress for a task a full column
// keeps out (want, as it was to be): started for an agent, it lands there
// instead. A full In progress decides; otherwise the column's queue stands.
func recheckTaskboardInProgress(w http.ResponseWriter, r *http.Request, g *db.AgentGroup, was, want *db.GroupTask,
	column *taskboardLimitHit, over bool) (*taskboardLimitHit, bool) {
	landed := *want
	landed.Column = ""
	hit, ok := checkTaskboardLimit(w, r, g, was, &landed, over)
	if !ok {
		return nil, false
	}
	if hit != nil {
		return hit, true
	}
	return column, true
}

// taskboardWaitingFor counts the tasks in line for lane (that assignee's,
// with one) that are ready to go in: none waits on a dependency. was, the
// task being changed, is not counted.
func taskboardWaitingFor(groupID int64, lane, assignee string, was *db.GroupTask) int {
	queue, err := db.ListGroupTaskQueue(groupID)
	if err != nil {
		return 0
	}
	n := 0
	for _, t := range queue {
		if t.QueuedFor != lane || (assignee != "" && t.AssigneeID != assignee) || (was != nil && t.ID == was.ID) {
			continue
		}
		if waits, _, err := db.GroupTaskDepsWait(t.ID); err == nil && !waits {
			n++
		}
	}
	return n
}

// fullText is "In progress is full (3 of 3)", or per assignee "builder's In
// progress is full (…)".
func (h *taskboardLimitHit) fullText() string {
	per := ""
	if h.note.Per == boardopts.PerAssignee {
		per = " for this assignee"
	}
	return fmt.Sprintf("%s is full%s (%d of %d)", h.note.Label, per, h.note.Count, h.note.Max)
}

// finishTaskboardLimit completes a change that met a full lane: a queued
// task is put in line (the change was made short of the lane), and the
// board's limit_reached message is sent. It returns the task as it now is and
// the note for the answer.
func finishTaskboardLimit(r *http.Request, g *db.AgentGroup, hit *taskboardLimitHit, out *db.GroupTask) (*db.GroupTask, *taskboardLimitNote) {
	out, note := queueTaskboardLimit(r, g, hit, out)
	announceTaskboardLimit(r, g, out, note, nil)
	return out, note
}

// queueTaskboardLimit is finishTaskboardLimit without the message: for a
// change that sends its own first (announceTaskboardLimit after it).
func queueTaskboardLimit(r *http.Request, g *db.AgentGroup, hit *taskboardLimitHit, out *db.GroupTask) (*db.GroupTask, *taskboardLimitNote) {
	if hit == nil {
		return out, nil
	}
	note := hit.note
	if note.Action != taskboardLimitQueued {
		return out, &note
	}
	queued, err := db.QueueGroupTask(out.ID, note.Lane, "for "+note.Label+": "+hit.fullText(), taskboardActor(r), out.AssigneeID)
	if err != nil {
		slog.Warn("taskboard: queueing failed", "task", out.ID, "lane", note.Lane, "error", err)
		return out, &note
	}
	note.Position = taskboardQueuePosition(g, queued)
	return queued, &note
}

// announceTaskboardLimit sends the board's limit_reached message for a note,
// to nobody in told (those the change's own message has told).
func announceTaskboardLimit(r *http.Request, g *db.AgentGroup, t *db.GroupTask, note *taskboardLimitNote, told map[string]bool) {
	if note == nil {
		return
	}
	full := (&taskboardLimitHit{note: *note}).fullText()
	what := ""
	switch note.Action {
	case taskboardLimitOver:
		what = "let in over the limit by the operator: " + full
	case taskboardLimitWarned:
		what = "let in over the limit: " + full
	default:
		what = fmt.Sprintf("queued for %s, %s in line: %s", note.Label, ordinal(note.Position), full)
	}
	taskboardLimitReached(r, g, t, what, told)
}

// taskboardLimitReached sends the board's limit_reached message about t to
// its recipients not in told.
func taskboardLimitReached(r *http.Request, g *db.AgentGroup, t *db.GroupTask, what string, told map[string]bool) {
	n := newTaskboardNotice(r, g, t, boardopts.EventLimitReached)
	for id := range told {
		n.told[id] = true
	}
	body := taskboardLinked(g, t, fmt.Sprintf("Task %s %q in %s: %s.\nRead it: tclaude agent taskboard show %s\n",
		t.ID, t.Title, g.Name, what, t.ID))
	n.others(what, body)
}

// taskboardQueuePosition is t's place in line for its lane, from 1 (0 when
// it waits for none). Per assignee, only that agent's tasks are in its line.
func taskboardQueuePosition(g *db.AgentGroup, t *db.GroupTask) int {
	if t.QueuedFor == "" {
		return 0
	}
	queue, err := db.ListGroupTaskQueue(g.ID)
	if err != nil {
		return 0
	}
	return taskboardQueuePositions(g.ID, queue)[t.ID]
}

// taskboardQueueLines reads each board's lines once for a listing.
type taskboardQueueLines map[int64]*taskboardQueueLine

type taskboardQueueLine struct {
	groupID   int64
	positions map[string]int
	labels    map[string]string
}

func (q taskboardQueueLines) of(groupID int64) *taskboardQueueLine {
	if l := q[groupID]; l != nil {
		return l
	}
	l := &taskboardQueueLine{groupID: groupID, labels: map[string]string{}}
	if queue, err := db.ListGroupTaskQueue(groupID); err == nil {
		l.positions = taskboardQueuePositions(groupID, queue)
	}
	q[groupID] = l
	return l
}

func (l *taskboardQueueLine) label(lane string) string {
	if s, ok := l.labels[lane]; ok {
		return s
	}
	l.labels[lane] = taskboardLaneLabel(l.groupID, lane)
	return l.labels[lane]
}

// taskboardQueuePositions places each task in a board's queue (first in
// line first) in its line.
func taskboardQueuePositions(groupID int64, queue []*db.GroupTask) map[string]int {
	if len(queue) == 0 {
		return nil
	}
	per := false
	if _, o, cols, err := taskboardOptions(groupID); err == nil {
		per = boardopts.Resolve(o, taskboardOptionsGlobals(), cols).Limits.PerAssignee()
	}
	type line struct{ lane, assignee string }
	seen := map[line]int{}
	out := map[string]int{}
	for _, t := range queue {
		k := line{lane: t.QueuedFor}
		if per {
			k.assignee = t.AssigneeID
		}
		seen[k]++
		out[t.ID] = seen[k]
	}
	return out
}

// taskboardQueuedNote is the mail an agent gets for a task assigned to it
// that waits in line for a full lane.
func taskboardQueuedNote(g *db.AgentGroup, t *db.GroupTask, by string, note *taskboardLimitNote) (subject, text string) {
	line := fmt.Sprintf("%s in line for %s", ordinal(note.Position), note.Label)
	var b strings.Builder
	fmt.Fprintf(&b, "%s put task %s %q in %s in your lane. %s is full (%d of %d), so it waits, %s.\n",
		by, t.ID, t.Title, g.Name, note.Label, note.Count, note.Max, line)
	b.WriteString("It starts for you, with its brief, when a slot frees. Do not start it before.\n")
	fmt.Fprintf(&b, "\nShow it:     tclaude agent taskboard show %s\n", t.ID)
	fmt.Fprintf(&b, "Give back:   tclaude agent taskboard release %s\n", t.ID)
	return taskboardSubject(g, t, "queued for you, "+line+", from "+by), taskboardLinked(g, t, b.String())
}

func ordinal(n int) string {
	suffix := "th"
	switch {
	case n%100 >= 11 && n%100 <= 13:
	case n%10 == 1:
		suffix = "st"
	case n%10 == 2:
		suffix = "nd"
	case n%10 == 3:
		suffix = "rd"
	}
	return fmt.Sprintf("%d%s", n, suffix)
}

// taskboardFillLanes lets the first in line into each lane with room, in
// order of when each got in line. A task still waiting on another task is
// passed over; it keeps its place. A task let into In progress is started
// for its assignee, who gets its brief; one let into a column moves there.
func taskboardFillLanes(g *db.AgentGroup) {
	if g == nil || g.IsArchived() {
		return
	}
	unlock := lockTaskboardLanes(g.ID)
	defer unlock()
	queue, err := db.ListGroupTaskQueue(g.ID)
	if err != nil || len(queue) == 0 {
		return
	}
	_, o, cols, err := taskboardOptions(g.ID)
	if err != nil {
		return
	}
	limits := boardopts.Resolve(o, taskboardOptionsGlobals(), cols).Limits
	states, _ := db.ListGroupTaskColumnStates(g.ID)
	// A task let in may free a slot of a lane passed over earlier (one
	// moving from In progress into its column): look again until a pass
	// lets none in.
	for range len(queue) {
		if !taskboardFillPass(g, limits, cols, states) {
			return
		}
	}
}

// taskboardFillPass lets in, in line order, each task in line whose lane has
// room, and reports whether it let any in.
func taskboardFillPass(g *db.AgentGroup, limits boardopts.ResolvedLimits, cols []string, states map[string]string) bool {
	queue, err := db.ListGroupTaskQueue(g.ID)
	if err != nil {
		return false
	}
	let := false
	// A line found full stays shut for the rest of the pass, so a slot it
	// frees later in the pass goes to its first in line, on the next pass.
	type line struct{ lane, assignee string }
	shut := map[line]bool{}
	for _, t := range queue {
		lane := t.QueuedFor
		if lane != db.QueueLaneInProgress && (!slices.Contains(cols, lane) || states[lane] == db.GroupTaskColumnCordoned) {
			continue
		}
		if waits, _, err := db.GroupTaskDepsWait(t.ID); err != nil || waits {
			continue
		}
		if most := limits.Lane(lane).Max.Value; most > 0 {
			k := line{lane: lane}
			if limits.PerAssignee() {
				k.assignee = t.AssigneeID
			}
			if shut[k] {
				continue
			}
			count, err := db.GroupTaskLaneCount(g.ID, lane, k.assignee)
			if err != nil || count >= most {
				shut[k] = true
				continue
			}
		}
		if taskboardLetIn(g, t, lane) {
			let = true
		}
	}
	return let
}

// taskboardLetIn moves t, first in line, into lane, and reports whether it
// did.
func taskboardLetIn(g *db.AgentGroup, t *db.GroupTask, lane string) bool {
	const actor = "system"
	label := taskboardLaneLabel(g.ID, lane)
	out := t
	var err error
	if lane == db.QueueLaneInProgress {
		if out.Column != "" {
			out, err = db.SetGroupTaskColumn(out.ID, "", actor, out.AssigneeID)
		}
		if err == nil && !out.Started() {
			out, err = db.StartGroupTask(out.ID, true, "", actor, out.AssigneeID)
		}
	} else {
		out, err = db.SetGroupTaskColumn(out.ID, lane, actor, out.AssigneeID)
	}
	if err != nil {
		slog.Warn("taskboard: letting a queued task in failed", "task", t.ID, "lane", lane, "error", err)
		return false
	}
	// In its lane, it is out of the line, whatever the triggers saw.
	if out.QueuedFor != "" {
		if err := db.ClearGroupTaskQueue(out.ID); err != nil {
			slog.Warn("taskboard: clearing a queued task's place failed", "task", t.ID, "error", err)
		}
	}
	if lane != db.QueueLaneInProgress || out.AssigneeID == db.OperatorAssignee {
		return true
	}
	// Started for its assignee: the brief, as a start by someone else.
	what := "a slot freed in " + label
	n := newTaskboardNotice(nil, g, out, boardopts.EventStarted)
	if conv := activeGroupMember(g, out.AssigneeID); conv != "" {
		n.mail(boardopts.RecipientAssignee, out.AssigneeID, conv, taskboardSubject(g, out, "started for you: "+what),
			taskboardLinked(g, out, taskboardBrief(g, out, "the board ("+what+")")))
	}
	n.others(fmt.Sprintf("started for %s: %s", taskboardAgentLabel(out.AssigneeID), what), "", boardopts.RecipientAssignee)
	return true
}

// sweepTaskboardQueues fills every board's lanes that has a task in line:
// the backstop for a slot freed by something no write route saw (an agent
// retired, a group left).
func sweepTaskboardQueues() {
	if cfg, _ := config.Load(); !cfg.TaskboardEnabled() {
		return
	}
	ids, err := db.ListGroupsWithQueues()
	if err != nil {
		slog.Warn("taskboard: queue sweep failed", "error", err)
		return
	}
	for _, id := range ids {
		g, err := db.GetAgentGroupByID(id)
		if err != nil || g == nil {
			continue
		}
		taskboardFillLanes(g)
	}
}
