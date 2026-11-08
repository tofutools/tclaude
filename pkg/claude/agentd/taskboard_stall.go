package agentd

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Idle without report (tcl-5jtcbp, docs/taskboard.md).
//
// An open task is stalled when its assignee has sat idle, with nothing running
// in the background, for taskboard.idle_grace (default 5 min, 0 off) since the
// later of its last activity and the start of its current stretch of work on
// the task (the assignment, or an unblock or reopen). Stalled is derived on every
// read, never stored, so a new turn clears it; a report ends the assignment
// and with it the stall. A 30 s sweep announces each stall once per
// assignment: a "stalled" event, a message to the assigner (inbox for an
// agent, Messages for the operator) and an inbox nudge to the assignee with
// its report commands. It never sends keys, and stalled never fails, blocks
// or unassigns a task by itself.

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
// assignee once.
type taskboardStaller struct {
	now   time.Time
	grace time.Duration
	seen  map[string]taskboardIdle
}

func newTaskboardStaller(now time.Time) *taskboardStaller {
	cfg, _ := config.Load()
	return &taskboardStaller{now: now, grace: cfg.TaskboardIdleGrace(), seen: map[string]taskboardIdle{}}
}

// stalled reports whether t is stalled, and since when its assignee has been
// idle on it.
func (s *taskboardStaller) stalled(t *db.GroupTask) (time.Time, bool) {
	if s == nil || s.grace <= 0 || t.Status != db.GroupTaskOpen || t.AssigneeID == "" ||
		t.AssigneeID == db.OperatorAssignee {
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
	if t.AssignedAt.After(since) {
		since = t.AssignedAt
	}
	if since.IsZero() || s.now.Sub(since) < s.grace {
		return time.Time{}, false
	}
	// Time spent blocked or closed is not idling on the task: the clock
	// restarts when it comes back to open. Read only for a candidate.
	if work, err := db.GroupTaskWorkSince(t.ID); err == nil && work.After(since) {
		since = work
		if s.now.Sub(since) < s.grace {
			return time.Time{}, false
		}
	}
	return since, true
}

// startTaskboardIdleSweep runs sweepTaskboardIdle every 30 s.
func startTaskboardIdleSweep(stop <-chan struct{}) {
	go func() {
		t := time.NewTicker(taskboardIdleSweepInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sweepTaskboardIdle(time.Now())
			}
		}
	}()
}

// sweepTaskboardIdle announces every newly stalled task. It does nothing
// while features.taskboard is off or taskboard.idle_grace is 0.
func sweepTaskboardIdle(now time.Time) {
	cfg, _ := config.Load()
	if !cfg.TaskboardEnabled() || cfg.TaskboardIdleGrace() <= 0 {
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
		announceTaskboardStall(g, t, idleFor)
	}
}

// announceTaskboardStall tells the assigner and nudges the assignee.
func announceTaskboardStall(g *db.AgentGroup, t *db.GroupTask, idleFor string) {
	who := taskboardAgentName(t.AssigneeID)
	if who == "" {
		who = t.AssigneeID
	}
	tail := fmt.Sprintf("%s went idle without reporting (%s)", who, idleFor)
	text := fmt.Sprintf("Task %s %q in %s: %s went idle %s ago without reporting.\n"+
		"It stays assigned; nothing has failed. Ask %s how it is going, or take the task back with\n"+
		"  tclaude agent taskboard unassign %s\n", t.ID, t.Title, g.Name, who, idleFor, who, t.ID)
	assigner, byAgent := strings.CutPrefix(t.AssignedBy, "agent:")
	switch {
	case byAgent && assigner == t.AssigneeID:
		// A task the agent took for itself has no one else to tell.
	case byAgent:
		if conv := activeTaskboardAgentConv(assigner); conv != "" {
			systemMailTaskboard(g, conv, taskboardSubject(g, t, tail), text)
		}
	default:
		if _, err := db.InsertHumanMessage(&db.HumanMessage{FromTitle: "Task board", GroupName: g.Name,
			Subject: taskboardSubject(g, t, tail), Body: text}); err != nil {
			slog.Warn("taskboard: operator message failed", "task", t.ID, "error", err)
		}
	}
	if conv := activeGroupMember(g, t.AssigneeID); conv != "" {
		var b strings.Builder
		fmt.Fprintf(&b, "You hold task %s %q in %s and have been idle for %s without reporting.\n", t.ID, t.Title, g.Name, idleFor)
		b.WriteString("If you are still on it, carry on; otherwise say how it went:\n\n")
		fmt.Fprintf(&b, "When it is done:   tclaude agent taskboard report done --task %s --note \"what you did\"\n", t.ID)
		fmt.Fprintf(&b, "If you cannot:     tclaude agent taskboard report failed --task %s --note \"why\"\n", t.ID)
		fmt.Fprintf(&b, "Give it back:      tclaude agent taskboard release %s\n", t.ID)
		b.WriteString("\nReporting does not end your session.\n")
		systemMailTaskboard(g, conv, taskboardSubject(g, t, "you went idle without reporting"), b.String())
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
