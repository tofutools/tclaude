package boardopts

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ptr[T any](v T) *T { return &v }

// A board that sets nothing inherits: on, with the global grace or 5m.
func TestResolveInherits(t *testing.T) {
	r := Resolve(Options{}, Globals{}, nil).Idle
	assert.True(t, r.Active, "the idle check is on by default")
	assert.Equal(t, Value[bool]{true, SourceDefault}, r.Enabled)
	assert.Equal(t, DefaultIdleGrace, r.Grace.Value.Duration())
	assert.Equal(t, SourceDefault, r.Grace.Source)
	assert.Equal(t, Value[bool]{true, SourceDefault}, r.Nudge)
	assert.Equal(t, []string{RecipientAssigner}, r.Tell.Value)
	assert.Empty(t, r.CheckColumns.Value)

	r = Resolve(Options{}, Globals{IdleCheck: ptr(true), IdleGrace: ptr(2 * time.Minute)}, nil).Idle
	assert.True(t, r.Active)
	assert.Equal(t, Value[bool]{true, SourceGlobal}, r.Enabled)
	assert.Equal(t, 2*time.Minute, r.Grace.Value.Duration())
	assert.Equal(t, SourceGlobal, r.Grace.Source)

	r = Resolve(Options{}, Globals{IdleCheck: ptr(false)}, nil).Idle
	assert.False(t, r.Active, "taskboard.idle_check false turns it off for the boards that leave it unset")
	assert.Equal(t, Value[bool]{false, SourceGlobal}, r.Enabled)

	r = Resolve(Options{}, Globals{IdleGrace: ptr(time.Duration(0))}, nil).Idle
	assert.False(t, r.Active, "taskboard.idle_grace 0 stays the kill switch")
	assert.True(t, r.GloballyOff)
}

// A board's own values win over the globals, except a global grace of 0,
// which turns the check off everywhere.
func TestResolveBoardWinsAndKillSwitch(t *testing.T) {
	o := Options{Idle: &Idle{Enabled: ptr(true), Grace: "10m", Nudge: ptr(false), Tell: &[]string{RecipientOwners},
		CheckColumns: &[]string{"in-ci", "gone"}}}
	r := Resolve(o, Globals{IdleCheck: ptr(false), IdleGrace: ptr(time.Minute)}, []string{"in-review", "in-ci"}).Idle
	assert.True(t, r.Active)
	assert.Equal(t, Value[bool]{true, SourceBoard}, r.Enabled)
	assert.Equal(t, 10*time.Minute, r.Grace.Value.Duration())
	assert.Equal(t, Value[bool]{false, SourceBoard}, r.Nudge)
	assert.True(t, r.Tells(RecipientOwners))
	assert.False(t, r.Tells(RecipientAssigner))
	assert.Equal(t, []string{"in-ci"}, r.CheckColumns.Value, "a removed column drops out")
	assert.True(t, r.Checks(""))
	assert.True(t, r.Checks("in-ci"))
	assert.False(t, r.Checks("in-review"))

	r = Resolve(o, Globals{IdleGrace: ptr(time.Duration(0))}, nil).Idle
	assert.False(t, r.Active)
	assert.True(t, r.GloballyOff)
	assert.True(t, r.Enabled.Value, "the board's own choice still reads as set")
}

func TestSetResetValidate(t *testing.T) {
	var o Options
	require.NoError(t, o.Set("idle.enabled", "on"))
	require.NoError(t, o.Set("idle.grace", "90s"))
	require.NoError(t, o.Set("idle.tell", "operator, assigner,operator"))
	require.NoError(t, o.Set("idle.check_columns", "in-ci"))
	require.NoError(t, o.Validate([]string{"in-review", "in-ci"}))
	assert.Equal(t, []string{RecipientAssigner, RecipientOperator}, *o.Idle.Tell, "canonical order, deduped")
	enc, err := o.Encode()
	require.NoError(t, err)
	assert.JSONEq(t, `{"idle":{"enabled":true,"grace":"90s","tell":["assigner","operator"],"check_columns":["in-ci"]}}`, enc)
	back, err := Parse(enc)
	require.NoError(t, err)
	assert.Equal(t, o, back)

	require.NoError(t, o.Set("idle.tell", "none"))
	require.NoError(t, o.Validate([]string{"in-ci"}))
	assert.Equal(t, []string{}, *o.Idle.Tell, "an empty list tells no one, unlike unset")

	require.NoError(t, o.Set("idle.grace", "default"))
	assert.Empty(t, o.Idle.Grace)
	require.NoError(t, o.Reset("idle"))
	assert.True(t, o.IsZero())
	enc, err = o.Encode()
	require.NoError(t, err)
	assert.Equal(t, "{}", enc)

	for key, value := range map[string]string{
		"idle.enabled": "maybe", "idle.grace": "0", "idle.nudge": "", "nope": "on",
	} {
		assert.Error(t, (&Options{}).Set(key, value), key)
	}
	assert.ErrorContains(t, (&Options{}).Set("idle.grace", "200h"), "between")
	assert.ErrorIs(t, (&Options{}).Reset("idle.nope"), ErrUnknownKey)

	bad := Options{Idle: &Idle{Tell: &[]string{"everyone"}}}
	assert.ErrorContains(t, bad.Validate(nil), "not a recipient")
	bad = Options{Idle: &Idle{CheckColumns: &[]string{"in-ci"}}}
	assert.ErrorContains(t, bad.Validate([]string{"in-review"}), "no custom column")
	bad = Options{Idle: &Idle{Grace: "1s"}}
	assert.Error(t, bad.Validate(nil))

	_, err = Parse(`{"idle":{"enabled":true,"surprise":1}}`)
	assert.Error(t, err, "an unknown field is refused")
}

func TestDurationText(t *testing.T) {
	for in, want := range map[time.Duration]string{
		10 * time.Minute: "10m", time.Hour: "1h", 90 * time.Second: "1m30s", 90 * time.Minute: "1h30m",
	} {
		assert.Equal(t, want, DurationString(in).String())
	}
}

// Every event is on by default, to today's recipients; a board switches
// one off or names its recipients, and a reset inherits again.
func TestEvents(t *testing.T) {
	r := Resolve(Options{}, Globals{}, nil)
	require.Len(t, r.Events, len(EventNames))
	comment := r.Event(EventComment)
	assert.Equal(t, Value[bool]{true, SourceDefault}, comment.Enabled)
	assert.Equal(t, []string{RecipientAssignee, RecipientCreator}, comment.To.Value)
	assert.True(t, comment.Sends(RecipientCreator))
	assert.False(t, comment.Sends(RecipientOwners))
	assert.Equal(t, []string{RecipientAssigner}, r.Event(EventReportFailed).To.Value)
	assert.NotEmpty(t, comment.Label)
	assert.False(t, r.Event("nope").Enabled.Value)

	var o Options
	require.NoError(t, o.Set("events.comment.enabled", "off"))
	require.NoError(t, o.Set("events.report_failed.to", "operator,owners,assigner"))
	require.NoError(t, o.Validate(nil))
	assert.Equal(t, []string{RecipientAssigner, RecipientOwners, RecipientOperator}, *o.Events[EventReportFailed].To)
	r = Resolve(o, Globals{}, nil)
	assert.Equal(t, Value[bool]{false, SourceBoard}, r.Event(EventComment).Enabled)
	assert.False(t, r.Event(EventComment).Sends(RecipientAssignee), "off sends to no one")
	assert.Equal(t, SourceBoard, r.Event(EventReportFailed).To.Source)
	assert.Equal(t, SourceDefault, r.Event(EventReportFailed).Enabled.Source)

	enc, err := o.Encode()
	require.NoError(t, err)
	assert.JSONEq(t, `{"events":{"comment":{"enabled":false},"report_failed":{"to":["assigner","owners","operator"]}}}`, enc)

	require.NoError(t, o.Reset("events.comment.enabled"))
	assert.NotContains(t, o.Events, EventComment, "an event that sets nothing drops out")
	require.NoError(t, o.Reset("events.report_failed"))
	assert.True(t, o.IsZero())
	require.NoError(t, o.Set("events.unblocked.to", "none"))
	require.NoError(t, o.Reset("events"))
	assert.True(t, o.IsZero())

	assert.ErrorIs(t, (&Options{}).Set("events.nope.enabled", "on"), ErrUnknownKey)
	assert.ErrorIs(t, (&Options{}).Set("events.comment.color", "red"), ErrUnknownKey)
	assert.ErrorIs(t, (&Options{}).Reset("events.nope"), ErrUnknownKey)
	bad := Options{Events: map[string]*Event{"nope": {Enabled: ptr(true)}}}
	assert.ErrorContains(t, bad.Validate(nil), "no event")
	bad = Options{Events: map[string]*Event{EventComment: {To: &[]string{"everyone"}}}}
	assert.ErrorContains(t, bad.Validate(nil), "not a recipient")
	assert.Contains(t, Keys, "events.epic_close.to")
}

// A stored document naming an event this version does not know (a newer
// tclaude wrote it) does not parse, as an unknown field does not.
func TestParseRefusesUnknownEvent(t *testing.T) {
	_, err := Parse(`{"events":{"lane_moved":{"enabled":false}}}`)
	assert.ErrorContains(t, err, "no event")
	_, err = Parse(`{"events":{"comment":{"enabled":false}}}`)
	assert.NoError(t, err)
}

// Limits: none by default, queue at a full lane, counted per board; a lane
// overrides the board's at_limit; a removed column's limit drops out.
func TestLimits(t *testing.T) {
	r := Resolve(Options{}, Globals{}, []string{"in-review"}).Limits
	assert.Equal(t, Value[string]{AtLimitQueue, SourceDefault}, r.AtLimit)
	assert.Equal(t, Value[string]{PerBoard, SourceDefault}, r.Per)
	require.Len(t, r.Lanes, 2)
	assert.Equal(t, LaneInProgress, r.Lanes[0].Lane)
	assert.Equal(t, "in-review", r.Lanes[1].Lane)
	assert.Zero(t, r.Lane(LaneInProgress).Max.Value)

	var o Options
	require.NoError(t, o.Set("limits.in_progress.max", "3"))
	require.NoError(t, o.Set("limits.at_limit", "refuse"))
	require.NoError(t, o.Set("limits.per", "assignee"))
	require.NoError(t, o.Set("limits.columns.in-review.max", "2"))
	require.NoError(t, o.Set("limits.columns.in-review.at_limit", "warn"))
	require.NoError(t, o.Validate([]string{"in-review"}))
	enc, err := o.Encode()
	require.NoError(t, err)
	assert.JSONEq(t, `{"limits":{"at_limit":"refuse","per":"assignee","in_progress":{"max":3},"columns":{"in-review":{"max":2,"at_limit":"warn"}}}}`, enc)
	back, err := Parse(enc)
	require.NoError(t, err)
	assert.Equal(t, o, back)

	r = Resolve(o, Globals{}, []string{"in-review"}).Limits
	assert.True(t, r.PerAssignee())
	assert.Equal(t, Value[int]{3, SourceBoard}, r.Lane(LaneInProgress).Max)
	assert.Equal(t, Value[string]{AtLimitRefuse, SourceBoard}, r.Lane(LaneInProgress).AtLimit, "the board's choice")
	assert.Equal(t, Value[string]{AtLimitWarn, SourceBoard}, r.Lane("in-review").AtLimit, "the column's own")

	assert.ErrorContains(t, o.Validate([]string{"in-ci"}), "no custom column")
	assert.True(t, o.Prune([]string{"in-ci"}))
	assert.Nil(t, o.Limits.Columns)

	require.NoError(t, o.Set("limits.in_progress.max", "none"))
	require.NoError(t, o.Reset("limits.at_limit"))
	require.NoError(t, o.Reset("limits.per"))
	assert.True(t, o.IsZero(), "nothing left")

	require.NoError(t, o.Set("limits.columns.in-ci.max", "1"))
	require.NoError(t, o.Reset("limits.columns.in-ci"))
	assert.True(t, o.IsZero())
	require.NoError(t, o.Set("limits.in_progress.at_limit", "warn"))
	require.NoError(t, o.Reset("limits"))
	assert.True(t, o.IsZero())

	for key, value := range map[string]string{
		"limits.in_progress.max": "-1", "limits.columns.x.max": "1000", "limits.at_limit": "maybe",
		"limits.per": "team", "limits.in_progress.at_limit": "drop",
	} {
		assert.Error(t, (&Options{}).Set(key, value), key)
	}
	assert.ErrorIs(t, (&Options{}).Set("limits.in_progress.color", "1"), ErrUnknownKey)
	assert.ErrorIs(t, (&Options{}).Set("limits.columns.max", "1"), ErrUnknownKey)
	assert.ErrorIs(t, (&Options{}).Reset("limits.nope.max"), ErrUnknownKey)
	assert.Equal(t, []string{RecipientAssignee, RecipientAssigner}, Resolve(Options{}, Globals{}, nil).Event(EventLimitReached).To.Value)
}
