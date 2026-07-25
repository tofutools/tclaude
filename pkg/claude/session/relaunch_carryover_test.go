package session

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

const carryoverConvID = "11111111-2222-3333-4444-555555555555"

// seedResumableConv makes convID resolvable by `session new -r` from cwd and
// gives it the recorded posture in `posture`. Nil means the conversation has a
// resume profile but no recorded launch posture at all — the legacy shape.
func seedResumableConv(t *testing.T, cwd string, posture *db.AgentRelaunchProfile) {
	t.Helper()
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:      carryoverConvID,
		ProjectDir:  convops.GetClaudeProjectPath(cwd),
		ProjectPath: cwd,
		Created:     "2026-01-01T00:00:00Z",
	}))
	require.NoError(t, db.SetConversationResumeProfile(carryoverConvID, db.ConversationResumeProfile{
		Version:          db.RelaunchProfileVersion,
		Harness:          harness.DefaultName,
		Cwd:              cwd,
		FallbackRelaunch: posture,
	}))
}

func carryoverTestHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	db.ResetForTest()
	return dir
}

// fullClaudePosture is a conversation launched with a deliberately non-default
// posture on every field `session new -r` carries.
func fullClaudePosture() *db.AgentRelaunchProfile {
	features := map[string]string{"bundled-skills": "off"}
	return &db.AgentRelaunchProfile{
		Version:                db.RelaunchProfileVersion,
		SandboxMode:            ptr(harness.ClaudeSandboxOn),
		ApprovalPolicy:         ptr("plan"),
		ApprovalAutoReview:     ptr(false),
		AskUserQuestionTimeout: ptr("5m"),
		RemoteControl:          ptr(true),
		AutoMemory:             ptr(true),
		ContextFeatures:        &features,
		AutoCompactWindow:      ptr("450000"),
	}
}

func ptr[T any](v T) *T { return &v }

// TestApplyRecordedLaunchPosture_CarriesEveryOmittedFlag is the TCL-730
// regression: a resume that passes no launch flags must relaunch with the
// posture the conversation was recorded under, not with today's defaults.
// Before the fix every one of these came back blank, and — because runNew
// records what it resolved — the blank was then asserted as intent, destroying
// the recorded value for every later relaunch too.
func TestApplyRecordedLaunchPosture_CarriesEveryOmittedFlag(t *testing.T) {
	cwd := carryoverTestHome(t)
	seedResumableConv(t, cwd, fullClaudePosture())

	params := &NewParams{Resume: carryoverConvID, Dir: cwd}
	require.NoError(t, applyRecordedLaunchPosture(params, explicitLaunchFields{}))

	assert.Equal(t, harness.ClaudeSandboxOn, params.Sandbox)
	assert.Equal(t, "plan", params.Approval)
	assert.Equal(t, "5m", params.AskUserQuestionTimeout)
	assert.True(t, params.RemoteControl)
	assert.True(t, params.AutoMemory)
	assert.Equal(t, "bundled-skills=off", params.ContextFeatures)
	assert.Equal(t, "450000", params.AutoCompactWindow)
}

// TestApplyRecordedLaunchPosture_ExplicitFlagWins pins the other half of the
// contract: an explicitly passed flag overrides the record, INCLUDING one set
// to the value that also happens to be the default. That distinction only
// exists in explicitLaunchFields — by the time `--auto-memory=false` reaches
// NewParams it is indistinguishable from an omitted flag.
func TestApplyRecordedLaunchPosture_ExplicitFlagWins(t *testing.T) {
	cwd := carryoverTestHome(t)
	seedResumableConv(t, cwd, fullClaudePosture())

	params := &NewParams{Resume: carryoverConvID, Dir: cwd, AutoCompactWindow: "200000"}
	explicit := explicitLaunchFields{
		"auto-memory":         true, // --auto-memory=false: keep memory OFF
		"auto-compact-window": true,
		"remote-control":      true, // --remote-control=false
	}
	require.NoError(t, applyRecordedLaunchPosture(params, explicit))

	assert.False(t, params.AutoMemory, "an explicit --auto-memory=false must not be overwritten by the record")
	assert.False(t, params.RemoteControl, "an explicit --remote-control=false must not be overwritten by the record")
	assert.Equal(t, "200000", params.AutoCompactWindow)
	// Everything the caller did NOT pass still carries.
	assert.Equal(t, "5m", params.AskUserQuestionTimeout)
	assert.Equal(t, harness.ClaudeSandboxOn, params.Sandbox)
}

// TestApplyRecordedLaunchPosture_ValueOnParamsWins covers the programmatic
// caller that never went through Cobra: a non-zero value already on params is
// the caller's own, so the record must not clobber it.
func TestApplyRecordedLaunchPosture_ValueOnParamsWins(t *testing.T) {
	cwd := carryoverTestHome(t)
	seedResumableConv(t, cwd, fullClaudePosture())

	params := &NewParams{
		Resume:                 carryoverConvID,
		Dir:                    cwd,
		AskUserQuestionTimeout: "never",
		Sandbox:                harness.ClaudeSandboxOff,
	}
	require.NoError(t, applyRecordedLaunchPosture(params, explicitLaunchFields{}))

	assert.Equal(t, "never", params.AskUserQuestionTimeout)
	assert.Equal(t, harness.ClaudeSandboxOff, params.Sandbox)
}

// TestApplyRecordedLaunchPosture_PermissionProfileOwnsContainment pins that a
// caller who selected containment through --permission-profile does not also
// get a recorded --sandbox added underneath: the two are mutually exclusive, so
// carrying one under the other would turn a valid launch into an error.
func TestApplyRecordedLaunchPosture_PermissionProfileOwnsContainment(t *testing.T) {
	cwd := carryoverTestHome(t)
	seedResumableConv(t, cwd, fullClaudePosture())

	params := &NewParams{Resume: carryoverConvID, Dir: cwd, PermissionProfile: harness.CodexAgentProfile}
	require.NoError(t, applyRecordedLaunchPosture(params, explicitLaunchFields{}))

	assert.Empty(t, params.Sandbox)
}

// TestApplyRecordedLaunchPosture_UnknownStaysUnknown pins the tri-state rule at
// the boundary: a conversation with nothing recorded must leave params exactly
// as the caller built them, so "unknown" never becomes an asserted default.
func TestApplyRecordedLaunchPosture_UnknownStaysUnknown(t *testing.T) {
	cwd := carryoverTestHome(t)
	seedResumableConv(t, cwd, nil)

	params := &NewParams{Resume: carryoverConvID, Dir: cwd}
	before := *params
	require.NoError(t, applyRecordedLaunchPosture(params, explicitLaunchFields{}))
	assert.Equal(t, before, *params)
}

// TestApplyRecordedLaunchPosture_KnownEmptyIsCarried is the mirror image: a
// posture that asserts "nothing pinned" must be replayed as nothing pinned, not
// treated as unknown and refilled from somewhere else.
func TestApplyRecordedLaunchPosture_KnownEmptyIsCarried(t *testing.T) {
	cwd := carryoverTestHome(t)
	empty := map[string]string{}
	seedResumableConv(t, cwd, &db.AgentRelaunchProfile{
		Version:           db.RelaunchProfileVersion,
		AutoMemory:        ptr(false),
		AutoCompactWindow: ptr(""),
		ContextFeatures:   &empty,
	})

	params := &NewParams{Resume: carryoverConvID, Dir: cwd, AutoMemory: false}
	require.NoError(t, applyRecordedLaunchPosture(params, explicitLaunchFields{}))

	assert.False(t, params.AutoMemory)
	assert.Empty(t, params.AutoCompactWindow)
	assert.Empty(t, params.ContextFeatures)
}

// TestLaunchCarryoverDropsValuesTheHarnessCannotHonour pins the fail-soft rule:
// a recorded value the relaunch harness has no switch for is dropped, never
// turned into a launch error. Losing a Claude-only posture on a Codex relaunch
// costs one capability; wedging the resume costs the whole agent.
func TestLaunchCarryoverDropsValuesTheHarnessCannotHonour(t *testing.T) {
	codex, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)

	recorded := fullClaudePosture()
	params := &NewParams{}
	for _, field := range launchCarryoverFields {
		if field.carry(codex, recorded, params) {
			assert.NotEqual(t, "auto-memory", field.flag)
			assert.NotEqual(t, "context-features", field.flag)
			assert.NotEqual(t, "auto-compact-window", field.flag)
			assert.NotEqual(t, "remote-control", field.flag)
			assert.NotEqual(t, "ask-user-question-timeout", field.flag)
		}
	}
	assert.False(t, params.AutoMemory)
	assert.Empty(t, params.ContextFeatures)
	assert.Empty(t, params.AutoCompactWindow)
	assert.False(t, params.RemoteControl)
	assert.Empty(t, params.AskUserQuestionTimeout)
	assert.Empty(t, params.Sandbox, "Claude's sandbox modes are not Codex's")
}

// TestApplyRecordedLaunchPosture_SkipsNonResumeAndManagedLaunches pins the two
// paths that must stay untouched: a fresh launch has no posture to inherit, and
// agentd has already resolved the whole relaunch profile and passes every field
// explicitly — so an omitted flag there is a deliberate default, not an absent
// one, and re-deriving it would override the daemon's own decision.
func TestApplyRecordedLaunchPosture_SkipsNonResumeAndManagedLaunches(t *testing.T) {
	cwd := carryoverTestHome(t)
	seedResumableConv(t, cwd, fullClaudePosture())

	fresh := &NewParams{Dir: cwd}
	require.NoError(t, applyRecordedLaunchPosture(fresh, explicitLaunchFields{}))
	assert.Equal(t, NewParams{Dir: cwd}, *fresh)

	managed := &NewParams{Resume: carryoverConvID, Dir: cwd, ManagedLaunch: true}
	require.NoError(t, applyRecordedLaunchPosture(managed, explicitLaunchFields{}))
	assert.Empty(t, managed.AskUserQuestionTimeout)
	assert.False(t, managed.AutoMemory)
}

// TestApplyRecordedLaunchPosture_UnresolvableConvIsRunNewsError pins that the
// carryover stays silent about a conversation it cannot resolve: runNew resolves
// it again a moment later and owns the user-facing "not found" message, so
// erroring here would just produce a worse duplicate.
func TestApplyRecordedLaunchPosture_UnresolvableConvIsRunNewsError(t *testing.T) {
	cwd := carryoverTestHome(t)
	params := &NewParams{Resume: "zzzzzzzz", Dir: cwd}
	require.NoError(t, applyRecordedLaunchPosture(params, explicitLaunchFields{}))
}

// TestLaunchCarryoverCoversEveryRecordedField is the structural guard TCL-730
// asks for: the erasure happened because the list of launch fields lived in
// several heads and one place forgot some. Every field on
// db.AgentRelaunchProfile must now be either carried by the table or explicitly
// excused with a reason, so the next field added inherits the contract — or
// fails this test until someone decides.
func TestLaunchCarryoverCoversEveryRecordedField(t *testing.T) {
	carried := map[string]bool{}
	for _, field := range launchCarryoverFields {
		require.NotEmpty(t, field.recorded, "carryover field %q must name the profile field it reads", field.flag)
		require.False(t, carried[field.recorded], "profile field %s is carried twice", field.recorded)
		carried[field.recorded] = true
	}

	profile := reflect.TypeOf(db.AgentRelaunchProfile{})
	for i := range profile.NumField() {
		name := profile.Field(i).Name
		excuse, excused := launchCarryoverExcused[name]
		switch {
		case carried[name] && excused:
			t.Errorf("db.AgentRelaunchProfile.%s is both carried and excused", name)
		case carried[name]:
		case excused:
			assert.NotEmpty(t, excuse, "excusing %s needs a reason", name)
		default:
			t.Errorf("db.AgentRelaunchProfile.%s is a recorded launch parameter that "+
				"`session new -r` neither carries nor excuses; add it to "+
				"launchCarryoverFields or to launchCarryoverExcused with a reason", name)
		}
	}
	for name := range launchCarryoverExcused {
		_, ok := profile.FieldByName(name)
		assert.True(t, ok, "launchCarryoverExcused names %s, which db.AgentRelaunchProfile no longer has", name)
	}
}

// TestLaunchCarryoverFlagsExist pins each table entry to a real `session new`
// flag. The flag name is what explicitLaunchFields is keyed by, so a typo would
// silently make that parameter impossible to override.
func TestLaunchCarryoverFlagsExist(t *testing.T) {
	flags := NewCmd().Flags()
	for _, field := range launchCarryoverFields {
		assert.NotNil(t, flags.Lookup(field.flag), "session new has no --%s flag", field.flag)
	}
}
