package agentd

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Idle without report (tcl-5jtcbp, docs/taskboard.md).
//
// The idle check is a board's own option (taskboard_options.go): off unless
// the board turns it on (idle.enabled), or the operator's global
// taskboard.idle_check does for the boards that leave it unset;
// taskboard.idle_grace "0" turns it off everywhere. On a board that has it
// on, an open task is stalled when its assignee has sat idle, with nothing
// running in the background, for the board's grace (idle.grace, else
// taskboard.idle_grace, else 5 min) since the later of its last activity
// and the start of its current stretch of work on the task (the assignment,
// or an unblock or reopen). A task that waits on something outside its
// agent's turn never stalls: a queued one (a dependency not done yet; the
// clock then starts when its last dependency closes), one in line for a
// full lane (docs/taskboard.md "Limits"), or one in a custom column the
// board does not list in idle.check_columns.
// Stalled is derived on every read, never stored, so a new turn clears it; a
// report ends the assignment and with it the stall. A 30 s sweep announces
// each stall once per assignment: a "stalled" event, a message to the
// board's idle.tell recipients (the assigner by default: inbox for an agent,
// Messages for the operator) and, unless idle.nudge is off, an inbox nudge
// to the assignee with its report commands. It never sends keys, and stalled
// never fails, blocks or unassigns a task by itself.

// taskboardIdleSweepInterval is how often the idle sweep looks.
const taskboardIdleSweepInterval = 30 * time.Second

// taskboardIdle is one assignee's idle state: Idle when its live session is
// idle with no subagents, background shells or monitors, Since when it last
// did something.
type taskboardIdle struct {
	Idle  bool
	Since time.Time
}

// taskboardIdleProbe reads an agent's idle state; a var so flow tests that
// cannot drive a real session's hooks can still reach the read paths.
var taskboardIdleProbe = probeTaskboardIdle

func probeTaskboardIdle(agentID string) taskboardIdle {
	conv, err := db.CurrentConvForAgent(agentID)
	if err != nil || conv == "" {
		return taskboardIdle{}
	}
	rows, err := db.FindSessionsByConvID(conv)
	if err != nil || len(rows) == 0 {
		return taskboardIdle{}
	}
	alive, _ := liveTmuxCache.get()
	if !isConvOnlineInSessions(rows, alive) {
		return taskboardIdle{}
	}
	state := stateForConvInSessions(rows, alive)
	if state.Status != session.StatusIdle || state.SubagentCount > 0 || state.BgShellCount > 0 || state.MonitorCount > 0 {
		return taskboardIdle{}
	}
	var since time.Time
	for _, r := range rows {
		at := r.LastHook
		if at.IsZero() {
			at = r.UpdatedAt
		}
		if at.After(since) {
			since = at
		}
	}
	return taskboardIdle{Idle: true, Since: since}
}

// taskboardStaller decides stalled for a batch of tasks, probing each
// assignee once and resolving each board's options once.
type taskboardStaller struct {
	now     time.Time
	globals boardopts.Globals
	boards  map[int64]boardopts.ResolvedIdle
	seen    map[string]taskboardIdle
}

func newTaskboardStaller(now time.Time) *taskboardStaller {
	return &taskboardStaller{now: now, globals: taskboardOptionsGlobals(),
		boards: map[int64]boardopts.ResolvedIdle{}, seen: map[string]taskboardIdle{}}
}

// board is the idle check in force on the board of group groupID.
func (s *taskboardStaller) board(groupID int64) boardopts.ResolvedIdle {
	if r, ok := s.boards[groupID]; ok {
		return r
	}
	_, o, cols, err := taskboardOptions(groupID)
	if err != nil {
		slog.Warn("taskboard idle check: board options unreadable; using the inherited ones", "group", groupID, "error", err)
	}
	r := boardopts.Resolve(o, s.globals, cols).Idle
	s.boards[groupID] = r
	return r
}

// stalled reports whether t is stalled, and since when its assignee has been
// idle on it.
func (s *taskboardStaller) stalled(t *db.GroupTask) (time.Time, bool) {
	// A task in line for a full lane (one waiting for a column it is to
	// move into; one for In progress is held, not started) waits on the
	// board, not on its agent.
	if s == nil || t.Status != db.GroupTaskOpen || !t.Started() || t.AssigneeID == db.OperatorAssignee || t.QueuedFor != "" {
		return time.Time{}, false
	}
	board := s.board(t.GroupID)
	// A task in a custom column (In review, In CI, …) waits on something
	// outside the agent's turn, so its agent's idling is not a stall, unless
	// the board checks that column too.
	grace := board.Grace.Value.Duration()
	if !board.Active || grace <= 0 || !board.Checks(t.Column) {
		return time.Time{}, false
	}
	idle, ok := s.seen[t.AssigneeID]
	if !ok {
		idle = taskboardIdleProbe(t.AssigneeID)
		s.seen[t.AssigneeID] = idle
	}
	if !idle.Idle {
		return time.Time{}, false
	}
	since := idle.Since
	if t.StartedAt.After(since) {
		since = t.StartedAt
	}
	if since.IsZero() || s.now.Sub(since) < grace {
		return time.Time{}, false
	}
	// Time spent blocked or closed is not idling on the task: the clock
	// restarts when it comes back to open. Read only for a candidate.
	if work, err := db.GroupTaskWorkSince(t.ID); err == nil && work.After(since) {
		since = work
		if s.now.Sub(since) < grace {
			return time.Time{}, false
		}
	}
	// A queued task waits on a dependency that is not done yet, and the time
	// it waited is not idling either: the clock starts when the last
	// dependency was closed.
	if len(t.DependsOn) > 0 {
		queued, readyAt, err := db.GroupTaskDepsWait(t.ID)
		if err != nil || queued {
			return time.Time{}, false
		}
		if readyAt.After(since) {
			since = readyAt
			if s.now.Sub(since) < grace {
				return time.Time{}, false
			}
		}
	}
	return since, true
}

// startTaskboardIdleSweep runs sweepTaskboardIdle every 30 s, and with it
// the queue sweep (taskboard_limits.go).
func startTaskboardIdleSweep(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(taskboardIdleSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sweepTaskboardQueues()
				sweepTaskboardIdle(time.Now())
			}
		}
	}()
}

// sweepTaskboardIdle announces every newly stalled task. It does nothing
// while features.taskboard is off or taskboard.idle_grace is 0.
func sweepTaskboardIdle(now time.Time) {
	cfg, _ := config.Load()
	if !cfg.TaskboardEnabled() {
		return
	}
	if d, ok := cfg.TaskboardIdleGraceSetting(); ok && d <= 0 {
		return
	}
	tasks, err := db.ListOpenAssignedGroupTasks()
	if err != nil {
		slog.Warn("taskboard idle sweep failed", "error", err)
		return
	}
	s := newTaskboardStaller(now)
	for _, at := range tasks {
		t := at.Task
		since, ok := s.stalled(t)
		if !ok {
			continue
		}
		idleFor := fleetAge(now.Sub(since))
		wrote, err := db.MarkGroupTaskStalled(t.ID, t.AssigneeID, t.AssignedAt, "idle "+idleFor+" without reporting")
		if err != nil {
			slog.Warn("taskboard idle sweep: mark failed", "task", t.ID, "error", err)
			continue
		}
		if !wrote {
			continue
		}
		g, err := db.GetAgentGroupByID(t.GroupID)
		if err != nil || g == nil {
			continue
		}
		announceTaskboardStall(g, t, idleFor, s.board(t.GroupID))
	}
}

// announceTaskboardStall tells the board's idle.tell recipients and, unless
// idle.nudge is off, nudges the assignee. Each recipient hears once, and
// never about its own task.
func announceTaskboardStall(g *db.AgentGroup, t *db.GroupTask, idleFor string, board boardopts.ResolvedIdle) {
	who := taskboardAgentName(t.AssigneeID)
	if who == "" {
		who = t.AssigneeID
	}
	tail := fmt.Sprintf("%s went idle without reporting (%s)", who, idleFor)
	text := fmt.Sprintf("Task %s %q in %s: %s went idle %s ago without reporting.\n"+
		"It stays assigned; nothing has failed. Ask %s how it is going, or take the task back with\n"+
		"  tclaude agent taskboard unassign %s\n\n"+
		"This board's idle check is on (grace %s). Change it or turn it off:\n"+
		"  tclaude agent taskboard options set %s idle.enabled=off\n",
		t.ID, t.Title, g.Name, who, idleFor, who, t.ID, board.Grace.Value, g.Name)
	subject, body := taskboardSubject(g, t, tail), taskboardLinked(g, t, text)
	told := map[string]bool{t.AssigneeID: true}
	tellAgent := func(agentID string) {
		if agentID == "" || told[agentID] {
			return
		}
		told[agentID] = true
		if conv := activeTaskboardAgentConv(agentID); conv != "" {
			systemMailTaskboard(g, conv, subject, body)
		}
	}
	operatorTold := false
	tellOperator := func() {
		if operatorTold {
			return
		}
		operatorTold = true
		if _, err := db.InsertHumanMessage(&db.HumanMessage{FromTitle: "Task board", GroupName: g.Name,
			Subject: subject, Body: body}); err != nil {
			slog.Warn("taskboard: operator message failed", "task", t.ID, "error", err)
		}
	}
	if board.Tells(boardopts.RecipientAssigner) {
		// A task the agent took for itself has no one else to tell.
		if assigner, byAgent := strings.CutPrefix(t.AssignedBy, "agent:"); byAgent {
			tellAgent(assigner)
		} else {
			tellOperator()
		}
	}
	if board.Tells(boardopts.RecipientOwners) {
		owners, err := db.ListAgentGroupOwners(g.ID)
		if err != nil {
			slog.Warn("taskboard: owners unreadable", "group", g.Name, "error", err)
		}
		for _, o := range owners {
			tellAgent(o.AgentID)
		}
	}
	if board.Tells(boardopts.RecipientOperator) {
		tellOperator()
	}
	if !board.Nudge.Value {
		return
	}
	if conv := activeGroupMember(g, t.AssigneeID); conv != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "You hold task %s %q in %s and have been idle for %s without reporting.\n", t.ID, t.Title, g.Name, idleFor)
		b.WriteString("If you are still on it, carry on; otherwise say how it went:\n\n")
		fmt.Fprintf(&b, "When it is done:   tclaude agent taskboard report done --task %s --note \"what you did\"\n", t.ID)
		fmt.Fprintf(&b, "If you cannot:     tclaude agent taskboard report failed --task %s --note \"why\"\n", t.ID)
		fmt.Fprintf(&b, "Give it back:      tclaude agent taskboard release %s\n", t.ID)
		b.WriteString("\nReporting does not end your session.\n")
		systemMailTaskboard(g, conv, taskboardSubject(g, t, "you went idle without reporting"), taskboardLinked(g, t, b.String()))
	}
}

// activeTaskboardAgentConv is a non-retired agent's current conv, "" otherwise.
func activeTaskboardAgentConv(agentID string) string {
	conv, err := db.CurrentConvForAgent(agentID)
	if err != nil || conv == "" {
		return ""
	}
	if state, err := db.AgentState(conv); err != nil || state == db.AgentStateRetired {
		return ""
	}
	return conv
}

// systemMailTaskboard queues inbox mail from the task board itself.
func systemMailTaskboard(g *db.AgentGroup, toConv, subject, body string) {
	m := &db.AgentMessage{GroupID: g.ID, ToConv: toConv, Subject: subject, Body: body, ToRecipients: []string{toConv}}
	if _, err := queueAgentMessage(m); err != nil {
		slog.Warn("taskboard: mail failed", "to", toConv, "error", err)
	}
}
