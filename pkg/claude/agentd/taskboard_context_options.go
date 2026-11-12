package agentd

import (
	"github.com/tofutools/tclaude/pkg/claude/common/boardopts"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// taskboardContextOptions is the part of a board's options its section in
// the Groups tab shows without opening them: the version (the Board options
// drawer reads the whole document again when it moves), whether the idle
// check runs, and which keys the board sets (the section line's summary of
// what differs from the inherited behaviour). The whole document is
// GET /v1/groups/{name}/taskboard/options (taskboard_options.go).
type taskboardContextOptions struct {
	Version int64 `json:"version"`
	// IdleActive is whether the idle check runs on the board.
	IdleActive bool `json:"idle_active"`
	// IdleGloballyOff is taskboard.idle_grace "0": off on every board.
	IdleGloballyOff bool `json:"idle_globally_off,omitempty"`
	// IdleSource is where idle.enabled comes from: default, global or board.
	IdleSource string `json:"idle_source"`
	// Set lists the option keys the board sets, in boardopts.Keys order;
	// a column's limit keys (limits.columns.<slug>.max, .at_limit) come
	// last, in the board's column order.
	Set []string `json:"set"`
	// Limits are the board's work-in-progress limits in force, for the
	// lanes' heads; absent while no lane has one.
	Limits *taskboardContextLimits `json:"limits,omitempty"`
}

// taskboardContextLimits are the limited lanes, each with how full it is.
type taskboardContextLimits struct {
	// Per is board or assignee: what a limit counts.
	Per   string                 `json:"per"`
	Lanes []taskboardContextLane `json:"lanes"`
}

// taskboardContextLane is one limited lane: in_progress or a column slug.
type taskboardContextLane struct {
	Lane    string `json:"lane"`
	Max     int    `json:"max"`
	AtLimit string `json:"at_limit"`
	// Count is how many tasks stand in the lane; per assignee, Counts has
	// each assignee's (agent id) count, and Count is the fullest one's.
	Count  int            `json:"count"`
	Counts map[string]int `json:"counts,omitempty"`
	// Queued is how many tasks wait in line for the lane.
	Queued int `json:"queued,omitempty"`
}

// taskboardContextOptionsOf summarises g's options for its context entry.
// live are the board's live tasks, which the limited lanes are counted
// from.
func taskboardContextOptionsOf(g *db.AgentGroup, globals boardopts.Globals, live []*db.GroupTask) (*taskboardContextOptions, error) {
	row, o, cols, err := taskboardOptions(g.ID)
	if err != nil {
		return nil, err
	}
	r := boardopts.Resolve(o, globals, cols)
	out := &taskboardContextOptions{
		Version: row.Version, IdleActive: r.Idle.Active, IdleGloballyOff: r.Idle.GloballyOff,
		IdleSource: r.Idle.Enabled.Source, Set: []string{},
	}
	board := map[string]bool{
		"idle.enabled":       r.Idle.Enabled.Source == boardopts.SourceBoard,
		"idle.grace":         r.Idle.Grace.Source == boardopts.SourceBoard,
		"idle.nudge":         r.Idle.Nudge.Source == boardopts.SourceBoard,
		"idle.tell":          r.Idle.Tell.Source == boardopts.SourceBoard,
		"idle.check_columns": r.Idle.CheckColumns.Source == boardopts.SourceBoard,
	}
	for _, e := range r.Events {
		board["events."+e.Name+".enabled"] = e.Enabled.Source == boardopts.SourceBoard
		board["events."+e.Name+".to"] = e.To.Source == boardopts.SourceBoard
	}
	// The limits as stored: a lane's at_limit reads as the board's in
	// resolved when only the board-wide one is set.
	if l := o.Limits; l != nil {
		board["limits.at_limit"], board["limits.per"] = l.AtLimit != "", l.Per != ""
		if l.InProgress != nil {
			board["limits.in_progress.max"], board["limits.in_progress.at_limit"] = l.InProgress.Max > 0, l.InProgress.AtLimit != ""
		}
	}
	for _, k := range boardopts.Keys {
		if board[k] {
			out.Set = append(out.Set, k)
		}
	}
	// Each column's limit keys, in the board's column order.
	if o.Limits != nil {
		for _, slug := range cols {
			if c := o.Limits.Columns[slug]; c != nil {
				if c.Max > 0 {
					out.Set = append(out.Set, "limits.columns."+slug+".max")
				}
				if c.AtLimit != "" {
					out.Set = append(out.Set, "limits.columns."+slug+".at_limit")
				}
			}
		}
	}
	out.Limits = taskboardContextLimitsOf(r.Limits, live)
	return out, nil
}

// taskboardContextLimitsOf counts the limited lanes; nil when none is.
func taskboardContextLimitsOf(limits boardopts.ResolvedLimits, live []*db.GroupTask) *taskboardContextLimits {
	per := limits.PerAssignee()
	var lanes []taskboardContextLane
	for _, l := range limits.Lanes {
		if l.Max.Value <= 0 {
			continue
		}
		lane := taskboardContextLane{Lane: l.Lane, Max: l.Max.Value, AtLimit: l.AtLimit.Value}
		for _, t := range live {
			if t.QueuedFor == l.Lane {
				lane.Queued++
			}
			if !t.InGroupTaskLane(l.Lane) {
				continue
			}
			if !per {
				lane.Count++
				continue
			}
			// Per assignee, a task nobody holds counts against no one.
			if t.AssigneeID == "" || t.AssigneeID == db.OperatorAssignee {
				continue
			}
			if lane.Counts == nil {
				lane.Counts = map[string]int{}
			}
			lane.Counts[t.AssigneeID]++
			lane.Count = max(lane.Count, lane.Counts[t.AssigneeID])
		}
		lanes = append(lanes, lane)
	}
	if len(lanes) == 0 {
		return nil
	}
	return &taskboardContextLimits{Per: limits.Per.Value, Lanes: lanes}
}
