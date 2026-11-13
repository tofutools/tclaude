// Package boardopts is a task board's own options (docs/taskboard.md "Board
// options"): how one group's board behaves, stored with the board and read
// the same on a linked node's board.
//
// A board stores only what it sets. Every value it leaves unset is inherited:
// from the operator's global config where one exists (taskboard.idle_check,
// taskboard.idle_grace), else from the built-in default. Resolve turns the
// stored options into the values in force, each with where it came from.
//
// The options are one JSON document per board. A key names one value
// ("idle.grace"); Set and Reset change a document by key, which is what the
// CLI and the PATCH route take, and Validate checks a whole document, which
// is what the PUT route takes.
package boardopts

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Options is a board's stored options. Every field is optional: nil (or
// empty, for Grace) means inherited.
type Options struct {
	Idle *Idle `json:"idle,omitempty"`
	// Events are the board's automatic messages, by event name (EventNames).
	Events map[string]*Event `json:"events,omitempty"`
	// Limits are the board's work-in-progress limits.
	Limits *Limits `json:"limits,omitempty"`
}

// Limits caps how many tasks a lane of the board holds at once: In progress
// (LaneInProgress) and each custom column. A task that would go into a full
// lane is queued (the default), refused, or let in with a warning.
type Limits struct {
	// AtLimit is what happens at a full lane, for every lane that does not
	// set its own: AtLimitQueue, AtLimitRefuse or AtLimitWarn.
	AtLimit string `json:"at_limit,omitempty"`
	// Per is what a limit counts: PerBoard (every task in the lane) or
	// PerAssignee (each assignee's tasks in the lane on their own).
	Per        string            `json:"per,omitempty"`
	InProgress *Limit            `json:"in_progress,omitempty"`
	Columns    map[string]*Limit `json:"columns,omitempty"`
}

// Limit is one lane's limit.
type Limit struct {
	// Max is how many tasks the lane holds; 0 is no limit.
	Max int `json:"max,omitempty"`
	// AtLimit overrides the board's at_limit for this lane.
	AtLimit string `json:"at_limit,omitempty"`
}

// What happens when a task would go into a full lane.
const (
	// AtLimitQueue keeps the task where it is, queued for the lane: a task
	// for In progress stays in Next, held; one for a column stays in its
	// lane. The first in line goes in when a slot frees.
	AtLimitQueue = "queue"
	// AtLimitRefuse refuses the change.
	AtLimitRefuse = "refuse"
	// AtLimitWarn lets the task in, over the limit, and says so.
	AtLimitWarn = "warn"
)

// AtLimits are the at_limit choices.
var AtLimits = []string{AtLimitQueue, AtLimitRefuse, AtLimitWarn}

// What a limit counts.
const (
	PerBoard    = "board"
	PerAssignee = "assignee"
)

// LaneInProgress names the In progress lane: started tasks of agents, in no
// custom column. A custom column's lane is named by its slug.
const LaneInProgress = "in_progress"

// MaxLimit bounds a lane's limit.
const MaxLimit = 999

// Event is one automatic message the board sends: whether it is sent, and
// to whom.
type Event struct {
	// Enabled sends the message. Every event is on by default.
	Enabled *bool `json:"enabled,omitempty"`
	// To is who gets it: any of the event recipients (EventRecipients). An
	// empty list sends it to no one.
	To *[]string `json:"to,omitempty"`
}

// Idle is the idle check (taskboard_stall.go): an assigned, started task
// whose agent sits idle without reporting for the grace period is marked
// stalled, the assignee is nudged and the recipients in Tell are told.
type Idle struct {
	// Enabled turns the idle check on for the board. Off by default.
	Enabled *bool `json:"enabled,omitempty"`
	// Grace is how long the assignee may sit idle: a Go duration ("10m").
	Grace string `json:"grace,omitempty"`
	// Nudge sends the idle assignee an inbox nudge with its report commands.
	Nudge *bool `json:"nudge,omitempty"`
	// Tell is who hears of a stall: RecipientAssigner, RecipientOwners,
	// RecipientOperator. An empty list tells no one.
	Tell *[]string `json:"tell,omitempty"`
	// CheckColumns are the custom columns (slugs) whose tasks are checked
	// too. A task in any other custom column waits on something outside its
	// agent's turn (a review, CI), so it is never stalled.
	CheckColumns *[]string `json:"check_columns,omitempty"`
}

// The recipients of a board message.
const (
	// RecipientAssigner is whoever assigned the task: an agent's inbox, or
	// the operator's Messages when the operator assigned it.
	RecipientAssigner = "assigner"
	// RecipientOwners is the group's owner agents.
	RecipientOwners = "owners"
	// RecipientOperator is the operator's Messages.
	RecipientOperator = "operator"
)

// More recipients of a board message, relative to the task it is about.
const (
	// RecipientAssignee is the agent that holds the task (for an unassign,
	// the one that held it).
	RecipientAssignee = "assignee"
	// RecipientCreator is the agent that created the task.
	RecipientCreator = "creator"
)

// EventRecipients are the recipients an event message may go to, in the
// order they are listed. In an event message, assignee, assigner and
// creator name agents; the operator is told only as operator.
var EventRecipients = []string{RecipientAssignee, RecipientAssigner, RecipientCreator, RecipientOwners, RecipientOperator}

// The board's events, each with its message.
const (
	EventAssigned     = "assigned"
	EventStarted      = "started"
	EventUnassigned   = "unassigned"
	EventReopened     = "reopened"
	EventMovedAway    = "moved_away"
	EventComment      = "comment"
	EventReportDone   = "report_done"
	EventReportFailed = "report_failed"
	EventUnblocked    = "unblocked"
	EventEpicClose    = "epic_close"
	EventLimitReached = "limit_reached"
)

// EventNames are the board's events, in display order.
var EventNames = []string{EventAssigned, EventStarted, EventUnassigned, EventReopened, EventMovedAway,
	EventComment, EventReportDone, EventReportFailed, EventUnblocked, EventEpicClose, EventLimitReached}

// eventInfo is an event's description and default recipients: today's
// messages, each to whom it has always gone.
var eventInfo = map[string]struct {
	label string
	to    []string
}{
	EventAssigned:     {"A task is assigned (or reassigned) to an agent", []string{RecipientAssignee}},
	EventStarted:      {"A held task is started for its assignee", []string{RecipientAssignee}},
	EventUnassigned:   {"A task is taken back from its assignee, or reassigned away", []string{RecipientAssignee}},
	EventReopened:     {"A closed task is reopened", []string{RecipientAssignee}},
	EventMovedAway:    {"A task is moved to another group or node", []string{RecipientAssignee}},
	EventComment:      {"Someone comments on a task", []string{RecipientAssignee, RecipientCreator}},
	EventReportDone:   {"The assignee reports done", []string{RecipientAssigner}},
	EventReportFailed: {"The assignee reports failed", []string{RecipientAssigner}},
	EventUnblocked:    {"A task another task waits on is closed", []string{RecipientAssignee}},
	EventEpicClose:    {"Every task of an epic is closed: close the epic?", []string{RecipientCreator}},
	EventLimitReached: {"A task meets a full lane: queued, refused or let in over the limit", []string{RecipientAssignee, RecipientAssigner}},
}

// EventLabel describes an event.
func EventLabel(name string) string { return eventInfo[name].label }

// idleTellRecipients are the recipients a stall may be told to, in the
// order they are listed.
var idleTellRecipients = []string{RecipientAssigner, RecipientOwners, RecipientOperator}

// Where a resolved value comes from.
const (
	SourceDefault = "default"
	SourceGlobal  = "global"
	SourceBoard   = "board"
)

// Defaults and bounds.
const (
	DefaultIdleGrace = 5 * time.Minute
	MinIdleGrace     = time.Minute
	MaxIdleGrace     = 7 * 24 * time.Hour
)

// Keys are every key Set and Reset take, in display order.
var Keys = func() []string {
	keys := []string{"idle.enabled", "idle.grace", "idle.nudge", "idle.tell", "idle.check_columns"}
	for _, name := range EventNames {
		keys = append(keys, "events."+name+".enabled", "events."+name+".to")
	}
	return append(keys, "limits.at_limit", "limits.per", "limits.in_progress.max", "limits.in_progress.at_limit",
		"limits.columns.<column>.max", "limits.columns.<column>.at_limit")
}()

// Sections are what Reset takes besides keys.
var Sections = []string{"idle", "events", "limits", "all"}

// Parse reads a stored document; "" is no options.
func Parse(raw string) (Options, error) {
	var o Options
	if strings.TrimSpace(raw) == "" {
		return o, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&o); err != nil {
		return Options{}, fmt.Errorf("board options: %w", err)
	}
	// An event this version does not know is a document it cannot read
	// (one a newer tclaude wrote), as an unknown field is.
	for name := range o.Events {
		if _, ok := eventInfo[name]; !ok {
			return Options{}, fmt.Errorf("board options: no event %q; the events are %s", name, strings.Join(EventNames, ", "))
		}
	}
	return o, nil
}

// Encode writes a document for storage; no options at all is "{}".
func (o Options) Encode() (string, error) {
	o.compact()
	b, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// IsZero reports whether the board sets nothing.
func (o Options) IsZero() bool {
	o.compact()
	return o.Idle == nil && o.Events == nil && o.Limits == nil
}

// compact drops empty sections, so an options document that sets nothing
// reads as {}.
func (o *Options) compact() {
	if o.Idle != nil && *o.Idle == (Idle{}) {
		o.Idle = nil
	}
	for name, e := range o.Events {
		if e == nil || *e == (Event{}) {
			delete(o.Events, name)
		}
	}
	if len(o.Events) == 0 {
		o.Events = nil
	}
	if l := o.Limits; l != nil {
		if l.InProgress != nil && *l.InProgress == (Limit{}) {
			l.InProgress = nil
		}
		for slug, c := range l.Columns {
			if c == nil || *c == (Limit{}) {
				delete(l.Columns, slug)
			}
		}
		if len(l.Columns) == 0 {
			l.Columns = nil
		}
		if l.AtLimit == "" && l.Per == "" && l.InProgress == nil && l.Columns == nil {
			o.Limits = nil
		}
	}
}

// limits is o's limits, made when absent.
func (o *Options) limits() *Limits {
	if o.Limits == nil {
		o.Limits = &Limits{}
	}
	return o.Limits
}

// lane is o's limit for lane (LaneInProgress or a column slug), made when
// absent.
func (o *Options) lane(lane string) *Limit {
	l := o.limits()
	if lane == LaneInProgress {
		if l.InProgress == nil {
			l.InProgress = &Limit{}
		}
		return l.InProgress
	}
	if l.Columns == nil {
		l.Columns = map[string]*Limit{}
	}
	if l.Columns[lane] == nil {
		l.Columns[lane] = &Limit{}
	}
	return l.Columns[lane]
}

// event is o's entry for an event, made when absent.
func (o *Options) event(name string) *Event {
	if o.Events == nil {
		o.Events = map[string]*Event{}
	}
	if o.Events[name] == nil {
		o.Events[name] = &Event{}
	}
	return o.Events[name]
}

// Validate checks a document and puts it in canonical form (lists deduped
// and ordered). columns are the board's custom column slugs; a column named
// in check_columns must be one of them.
func (o *Options) Validate(columns []string) error {
	for name, e := range o.Events {
		if _, ok := eventInfo[name]; !ok {
			return fmt.Errorf("events: no event %q; the events are %s", name, strings.Join(EventNames, ", "))
		}
		if e != nil && e.To != nil {
			to, err := canonicalRecipients("events."+name+".to", *e.To, EventRecipients)
			if err != nil {
				return err
			}
			e.To = &to
		}
	}
	if err := o.Limits.validate(columns); err != nil {
		return err
	}
	if o.Idle == nil {
		o.compact()
		return nil
	}
	if g := o.Idle.Grace; g != "" {
		if _, err := ParseGrace(g); err != nil {
			return err
		}
	}
	if o.Idle.Tell != nil {
		tell, err := canonicalRecipients("idle.tell", *o.Idle.Tell, idleTellRecipients)
		if err != nil {
			return err
		}
		o.Idle.Tell = &tell
	}
	if o.Idle.CheckColumns != nil {
		cols, err := canonicalColumns(*o.Idle.CheckColumns, columns)
		if err != nil {
			return err
		}
		o.Idle.CheckColumns = &cols
	}
	o.compact()
	return nil
}

func (l *Limits) validate(columns []string) error {
	if l == nil {
		return nil
	}
	if l.AtLimit != "" && !slices.Contains(AtLimits, l.AtLimit) {
		return fmt.Errorf("limits.at_limit is %s, not %q", strings.Join(AtLimits, ", "), l.AtLimit)
	}
	if l.Per != "" && l.Per != PerBoard && l.Per != PerAssignee {
		return fmt.Errorf("limits.per is %s or %s, not %q", PerBoard, PerAssignee, l.Per)
	}
	check := func(key string, lim *Limit) error {
		if lim == nil {
			return nil
		}
		if lim.Max < 0 || lim.Max > MaxLimit {
			return fmt.Errorf("%s.max is between 1 and %d (0 or none for no limit), not %d", key, MaxLimit, lim.Max)
		}
		if lim.AtLimit != "" && !slices.Contains(AtLimits, lim.AtLimit) {
			return fmt.Errorf("%s.at_limit is %s, not %q", key, strings.Join(AtLimits, ", "), lim.AtLimit)
		}
		return nil
	}
	if err := check("limits.in_progress", l.InProgress); err != nil {
		return err
	}
	for _, slug := range slices.Sorted(maps.Keys(l.Columns)) {
		if !slices.Contains(columns, slug) {
			if len(columns) == 0 {
				return fmt.Errorf("limits.columns: the board has no custom column %q (it has none)", slug)
			}
			return fmt.Errorf("limits.columns: the board has no custom column %q; its columns are %s", slug, strings.Join(columns, ", "))
		}
		if err := check("limits.columns."+slug, l.Columns[slug]); err != nil {
			return err
		}
	}
	return nil
}

// Prune drops what names a column the board no longer has: a column removed
// after it was named in check_columns or given a limit. It reports whether
// anything changed.
func (o *Options) Prune(columns []string) bool {
	changed := false
	if o.Idle != nil && o.Idle.CheckColumns != nil {
		kept := slices.DeleteFunc(slices.Clone(*o.Idle.CheckColumns), func(s string) bool { return !slices.Contains(columns, s) })
		if len(kept) != len(*o.Idle.CheckColumns) {
			o.Idle.CheckColumns = &kept
			changed = true
		}
	}
	if o.Limits != nil {
		for slug := range o.Limits.Columns {
			if !slices.Contains(columns, slug) {
				delete(o.Limits.Columns, slug)
				changed = true
			}
		}
		if changed {
			o.compact()
		}
	}
	return changed
}

// ParseGrace reads an idle grace: a Go duration between MinIdleGrace and
// MaxIdleGrace. A board turns the check off with idle.enabled, not a 0 grace.
func ParseGrace(s string) (time.Duration, error) {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("idle.grace is a duration such as 10m or 1h30m: %w", err)
	}
	if d < MinIdleGrace || d > MaxIdleGrace {
		return 0, fmt.Errorf("idle.grace is between %s and %s; turn the check off with idle.enabled=off", MinIdleGrace, fmtDuration(MaxIdleGrace))
	}
	return d, nil
}

func canonicalRecipients(key string, in, allowed []string) ([]string, error) {
	out := []string{}
	for _, a := range allowed {
		for _, v := range in {
			if strings.TrimSpace(v) == a {
				out = append(out, a)
				break
			}
		}
	}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if !slices.Contains(allowed, v) {
			return nil, fmt.Errorf("%s: %q is not a recipient; give any of %s", key, v, strings.Join(allowed, ", "))
		}
	}
	return out, nil
}

func canonicalColumns(in, columns []string) ([]string, error) {
	out := []string{}
	for _, c := range columns {
		if slices.Contains(in, c) {
			out = append(out, c)
		}
	}
	for _, v := range in {
		if !slices.Contains(columns, v) {
			if len(columns) == 0 {
				return nil, fmt.Errorf("idle.check_columns: the board has no custom column %q (it has none)", v)
			}
			return nil, fmt.Errorf("idle.check_columns: the board has no custom column %q; its columns are %s", v, strings.Join(columns, ", "))
		}
	}
	return out, nil
}

// ErrUnknownKey is a key that names no option.
var ErrUnknownKey = errors.New("no such board option")

// Set sets one option from its text form. "default" resets it. Lists are
// comma-separated; an empty list is "" or "none". Booleans are on/off (also
// true/false, yes/no). The document still needs Validate against the board.
func (o *Options) Set(key, value string) error {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "default") {
		return o.Reset(key)
	}
	idle := func() *Idle {
		if o.Idle == nil {
			o.Idle = &Idle{}
		}
		return o.Idle
	}
	switch key {
	case "idle.enabled":
		b, err := parseBool(key, value)
		if err != nil {
			return err
		}
		idle().Enabled = &b
	case "idle.nudge":
		b, err := parseBool(key, value)
		if err != nil {
			return err
		}
		idle().Nudge = &b
	case "idle.grace":
		if _, err := ParseGrace(value); err != nil {
			return err
		}
		idle().Grace = value
	case "idle.tell":
		l := parseList(value)
		idle().Tell = &l
	case "idle.check_columns":
		l := parseList(value)
		idle().CheckColumns = &l
	case "limits.at_limit":
		if !slices.Contains(AtLimits, value) {
			return fmt.Errorf("%s is %s, not %q", key, strings.Join(AtLimits, ", "), value)
		}
		o.limits().AtLimit = value
	case "limits.per":
		if value != PerBoard && value != PerAssignee {
			return fmt.Errorf("%s is %s or %s, not %q", key, PerBoard, PerAssignee, value)
		}
		o.limits().Per = value
	default:
		if lane, field, ok := limitKey(key); ok {
			return o.setLimit(key, lane, field, value)
		}
		name, field, ok := eventKey(key)
		if !ok {
			return unknownKey(key)
		}
		switch field {
		case "enabled":
			b, err := parseBool(key, value)
			if err != nil {
				return err
			}
			o.event(name).Enabled = &b
		case "to":
			l := parseList(value)
			o.event(name).To = &l
		}
	}
	return nil
}

func (o *Options) setLimit(key, lane, field, value string) error {
	if field == "at_limit" {
		if !slices.Contains(AtLimits, value) {
			return fmt.Errorf("%s is %s, not %q", key, strings.Join(AtLimits, ", "), value)
		}
		o.lane(lane).AtLimit = value
		return nil
	}
	if strings.EqualFold(value, "none") {
		value = "0"
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 || n > MaxLimit {
		return fmt.Errorf("%s is a number of tasks between 1 and %d (0 or none for no limit), not %q", key, MaxLimit, value)
	}
	o.lane(lane).Max = n
	o.compact()
	return nil
}

// limitKey splits "limits.in_progress.<field>" and
// "limits.columns.<slug>.<field>". The slug is checked against the board by
// Validate.
func limitKey(key string) (lane, field string, ok bool) {
	rest, ok := strings.CutPrefix(key, "limits.")
	if !ok {
		return "", "", false
	}
	if f, ok := strings.CutPrefix(rest, LaneInProgress+"."); ok {
		lane, field = LaneInProgress, f
	} else if r, ok := strings.CutPrefix(rest, "columns."); ok {
		i := strings.LastIndex(r, ".")
		if i <= 0 {
			return "", "", false
		}
		lane, field = r[:i], r[i+1:]
	} else {
		return "", "", false
	}
	if field != "max" && field != "at_limit" {
		return "", "", false
	}
	return lane, field, true
}

// eventKey splits "events.<name>.<field>" for a known event and field.
func eventKey(key string) (name, field string, ok bool) {
	rest, ok := strings.CutPrefix(key, "events.")
	if !ok {
		return "", "", false
	}
	name, field, ok = strings.Cut(rest, ".")
	if _, known := eventInfo[name]; !ok || !known || (field != "enabled" && field != "to") {
		return "", "", false
	}
	return name, field, true
}

// Reset drops one option, an event ("events.comment"), a whole section
// ("idle", "events"), or everything ("all"), so it is inherited again.
func (o *Options) Reset(key string) error {
	switch key {
	case "all":
		*o = Options{}
		return nil
	case "idle":
		o.Idle = nil
		return nil
	case "events":
		o.Events = nil
		return nil
	case "limits":
		o.Limits = nil
		return nil
	}
	if done, err := o.resetLimit(key); done || err != nil {
		return err
	}
	if name, ok := strings.CutPrefix(key, "events."); ok {
		if _, known := eventInfo[name]; known {
			delete(o.Events, name)
			o.compact()
			return nil
		}
		name, field, ok := eventKey(key)
		if !ok {
			return unknownKey(key)
		}
		if e := o.Events[name]; e != nil {
			if field == "enabled" {
				e.Enabled = nil
			} else {
				e.To = nil
			}
		}
		o.compact()
		return nil
	}
	if !slices.Contains(Keys, key) {
		return unknownKey(key)
	}
	if o.Idle == nil {
		return nil
	}
	switch key {
	case "idle.enabled":
		o.Idle.Enabled = nil
	case "idle.grace":
		o.Idle.Grace = ""
	case "idle.nudge":
		o.Idle.Nudge = nil
	case "idle.tell":
		o.Idle.Tell = nil
	case "idle.check_columns":
		o.Idle.CheckColumns = nil
	}
	o.compact()
	return nil
}

// resetLimit is Reset for a limits key: "limits.at_limit", "limits.per", a
// lane ("limits.in_progress", "limits.columns.<slug>"), every column
// ("limits.columns"), or one of a lane's fields. done is false for a key
// outside limits.
func (o *Options) resetLimit(key string) (done bool, err error) {
	rest, ok := strings.CutPrefix(key, "limits.")
	if !ok {
		return false, nil
	}
	defer o.compact()
	l := o.Limits
	if l == nil {
		l = &Limits{}
	}
	switch {
	case rest == "at_limit":
		l.AtLimit = ""
	case rest == "per":
		l.Per = ""
	case rest == LaneInProgress:
		l.InProgress = nil
	case rest == "columns":
		l.Columns = nil
	case strings.HasPrefix(rest, "columns.") && !strings.Contains(strings.TrimPrefix(rest, "columns."), "."):
		delete(l.Columns, strings.TrimPrefix(rest, "columns."))
	default:
		lane, field, ok := limitKey(key)
		if !ok {
			return true, unknownKey(key)
		}
		var lim *Limit
		if lane == LaneInProgress {
			lim = l.InProgress
		} else {
			lim = l.Columns[lane]
		}
		if lim != nil {
			if field == "max" {
				lim.Max = 0
			} else {
				lim.AtLimit = ""
			}
		}
	}
	return true, nil
}

func unknownKey(key string) error {
	return fmt.Errorf("%w %q; the options are idle.enabled, idle.grace, idle.nudge, idle.tell, idle.check_columns, "+
		"events.<event>.enabled and events.<event>.to (events: %s), limits.at_limit, limits.per, "+
		"limits.in_progress.max, limits.in_progress.at_limit, limits.columns.<column>.max and limits.columns.<column>.at_limit, "+
		"or a section: idle, events, limits, all",
		ErrUnknownKey, key, strings.Join(EventNames, ", "))
}

func parseBool(key, s string) (bool, error) {
	switch strings.ToLower(s) {
	case "on", "true", "yes", "1":
		return true, nil
	case "off", "false", "no", "0":
		return false, nil
	}
	return false, fmt.Errorf("%s is on or off, not %q", key, s)
}

func parseList(s string) []string {
	out := []string{}
	if s == "" || strings.EqualFold(s, "none") {
		return out
	}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// SetKeys lists the keys o sets, in Keys order; a column's limit keys
// (limits.columns.<slug>.max, .at_limit) come last, in the order of
// columns.
func (o Options) SetKeys(columns []string) []string {
	set := map[string]bool{}
	if i := o.Idle; i != nil {
		set["idle.enabled"], set["idle.grace"], set["idle.nudge"] = i.Enabled != nil, i.Grace != "", i.Nudge != nil
		set["idle.tell"], set["idle.check_columns"] = i.Tell != nil, i.CheckColumns != nil
	}
	for name, e := range o.Events {
		if e != nil {
			set["events."+name+".enabled"], set["events."+name+".to"] = e.Enabled != nil, e.To != nil
		}
	}
	l := o.Limits
	if l != nil {
		set["limits.at_limit"], set["limits.per"] = l.AtLimit != "", l.Per != ""
		if l.InProgress != nil {
			set["limits.in_progress.max"], set["limits.in_progress.at_limit"] = l.InProgress.Max > 0, l.InProgress.AtLimit != ""
		}
	}
	out := []string{}
	for _, k := range Keys {
		if set[k] {
			out = append(out, k)
		}
	}
	if l != nil {
		for _, slug := range columns {
			if c := l.Columns[slug]; c != nil {
				if c.Max > 0 {
					out = append(out, "limits.columns."+slug+".max")
				}
				if c.AtLimit != "" {
					out = append(out, "limits.columns."+slug+".at_limit")
				}
			}
		}
	}
	return out
}

// Globals are the operator's global task board settings a board inherits
// (~/.tclaude/data/config.json, taskboard.*). A nil field is unset.
type Globals struct {
	// IdleCheck is taskboard.idle_check: the idle check's default for every
	// board that does not set its own.
	IdleCheck *bool
	// IdleGrace is taskboard.idle_grace. Zero turns the idle check off on
	// every board, whatever the board sets.
	IdleGrace *time.Duration
}

// Value is one resolved option and where it came from.
type Value[T any] struct {
	Value  T      `json:"value"`
	Source string `json:"source"`
}

// Resolved is the options in force on a board.
type Resolved struct {
	Idle ResolvedIdle `json:"idle"`
	// Events are every event, in EventNames order.
	Events []ResolvedEvent `json:"events"`
	Limits ResolvedLimits  `json:"limits"`
}

// ResolvedLimits are the work-in-progress limits in force.
type ResolvedLimits struct {
	AtLimit Value[string] `json:"at_limit"`
	Per     Value[string] `json:"per"`
	// Lanes are In progress, then each custom column in the board's order.
	Lanes []ResolvedLane `json:"lanes"`
}

// ResolvedLane is one lane's limit in force.
type ResolvedLane struct {
	// Lane is LaneInProgress or a column slug.
	Lane string `json:"lane"`
	// Max is how many tasks the lane holds; 0 is no limit.
	Max Value[int] `json:"max"`
	// AtLimit is the lane's own, else the board's.
	AtLimit Value[string] `json:"at_limit"`
}

// Lane is the named lane's limit; an unknown lane has none.
func (r ResolvedLimits) Lane(lane string) ResolvedLane {
	for _, l := range r.Lanes {
		if l.Lane == lane {
			return l
		}
	}
	return ResolvedLane{Lane: lane, Max: Value[int]{0, SourceDefault}, AtLimit: r.AtLimit}
}

// PerAssignee reports whether the limits count each assignee's tasks on
// their own.
func (r ResolvedLimits) PerAssignee() bool { return r.Per.Value == PerAssignee }

// ResolvedEvent is one event's message in force.
type ResolvedEvent struct {
	Name    string          `json:"name"`
	Label   string          `json:"label"`
	Enabled Value[bool]     `json:"enabled"`
	To      Value[[]string] `json:"to"`
}

// Sends reports whether the event's message goes to recipient.
func (e ResolvedEvent) Sends(recipient string) bool {
	return e.Enabled.Value && slices.Contains(e.To.Value, recipient)
}

// Event is the named event in force; an unknown name is off.
func (r Resolved) Event(name string) ResolvedEvent {
	for _, e := range r.Events {
		if e.Name == name {
			return e
		}
	}
	return ResolvedEvent{Name: name}
}

// ResolvedIdle is the idle check in force.
type ResolvedIdle struct {
	// Active is whether the check runs on the board: Enabled, and not
	// switched off globally.
	Active bool `json:"active"`
	// GloballyOff is set when taskboard.idle_grace is 0, which turns the
	// check off on every board.
	GloballyOff  bool                  `json:"globally_off,omitempty"`
	Enabled      Value[bool]           `json:"enabled"`
	Grace        Value[DurationString] `json:"grace"`
	Nudge        Value[bool]           `json:"nudge"`
	Tell         Value[[]string]       `json:"tell"`
	CheckColumns Value[[]string]       `json:"check_columns"`
}

// DurationString is a duration that reads and writes as "10m".
type DurationString time.Duration

// Duration is d as a time.Duration.
func (d DurationString) Duration() time.Duration { return time.Duration(d) }

func (d DurationString) String() string { return fmtDuration(time.Duration(d)) }

// MarshalJSON writes "10m".
func (d DurationString) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON reads "10m".
func (d *DurationString) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = DurationString(v)
	return nil
}

// fmtDuration writes a duration without the zero units time.Duration.String
// adds ("10m", not "10m0s").
func fmtDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// Resolve is the options in force on a board that stores o, under g.
// columns are the board's custom column slugs: a stale one in o is dropped.
func Resolve(o Options, g Globals, columns []string) Resolved {
	o.Prune(columns)
	in := o.Idle
	if in == nil {
		in = &Idle{}
	}
	var r ResolvedIdle
	switch {
	case in.Enabled != nil:
		r.Enabled = Value[bool]{*in.Enabled, SourceBoard}
	case g.IdleCheck != nil:
		r.Enabled = Value[bool]{*g.IdleCheck, SourceGlobal}
	default:
		r.Enabled = Value[bool]{false, SourceDefault}
	}
	r.Grace = Value[DurationString]{DurationString(DefaultIdleGrace), SourceDefault}
	if g.IdleGrace != nil && *g.IdleGrace > 0 {
		r.Grace = Value[DurationString]{DurationString(*g.IdleGrace), SourceGlobal}
	}
	if in.Grace != "" {
		if d, err := ParseGrace(in.Grace); err == nil {
			r.Grace = Value[DurationString]{DurationString(d), SourceBoard}
		}
	}
	r.Nudge = Value[bool]{true, SourceDefault}
	if in.Nudge != nil {
		r.Nudge = Value[bool]{*in.Nudge, SourceBoard}
	}
	r.Tell = Value[[]string]{[]string{RecipientAssigner}, SourceDefault}
	if in.Tell != nil {
		r.Tell = Value[[]string]{slices.Clone(*in.Tell), SourceBoard}
	}
	r.CheckColumns = Value[[]string]{[]string{}, SourceDefault}
	if in.CheckColumns != nil {
		r.CheckColumns = Value[[]string]{slices.Clone(*in.CheckColumns), SourceBoard}
	}
	r.GloballyOff = g.IdleGrace != nil && *g.IdleGrace <= 0
	r.Active = r.Enabled.Value && !r.GloballyOff
	events := make([]ResolvedEvent, 0, len(EventNames))
	for _, name := range EventNames {
		info := eventInfo[name]
		e := ResolvedEvent{Name: name, Label: info.label,
			Enabled: Value[bool]{true, SourceDefault}, To: Value[[]string]{slices.Clone(info.to), SourceDefault}}
		if set := o.Events[name]; set != nil {
			if set.Enabled != nil {
				e.Enabled = Value[bool]{*set.Enabled, SourceBoard}
			}
			if set.To != nil {
				e.To = Value[[]string]{slices.Clone(*set.To), SourceBoard}
			}
		}
		events = append(events, e)
	}
	return Resolved{Idle: r, Events: events, Limits: resolveLimits(o.Limits, columns)}
}

func resolveLimits(in *Limits, columns []string) ResolvedLimits {
	if in == nil {
		in = &Limits{}
	}
	r := ResolvedLimits{AtLimit: Value[string]{AtLimitQueue, SourceDefault}, Per: Value[string]{PerBoard, SourceDefault}}
	if in.AtLimit != "" {
		r.AtLimit = Value[string]{in.AtLimit, SourceBoard}
	}
	if in.Per != "" {
		r.Per = Value[string]{in.Per, SourceBoard}
	}
	lane := func(name string, set *Limit) ResolvedLane {
		l := ResolvedLane{Lane: name, Max: Value[int]{0, SourceDefault}, AtLimit: r.AtLimit}
		if set != nil {
			if set.Max > 0 {
				l.Max = Value[int]{set.Max, SourceBoard}
			}
			if set.AtLimit != "" {
				l.AtLimit = Value[string]{set.AtLimit, SourceBoard}
			}
		}
		return l
	}
	r.Lanes = append(r.Lanes, lane(LaneInProgress, in.InProgress))
	for _, slug := range columns {
		r.Lanes = append(r.Lanes, lane(slug, in.Columns[slug]))
	}
	return r
}

// Tells reports whether a stall is told to recipient.
func (r ResolvedIdle) Tells(recipient string) bool { return slices.Contains(r.Tell.Value, recipient) }

// Checks reports whether a task in custom column slug ("" for none) is
// idle-checked.
func (r ResolvedIdle) Checks(column string) bool {
	return column == "" || slices.Contains(r.CheckColumns.Value, column)
}
