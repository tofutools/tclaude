package session

import (
	"io"
	"os"
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
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
	t.Cleanup(db.ResetForTest)
	return dir
}

// fullClaudePosture is a conversation launched with a deliberately non-default
// posture on every field `session new -r` carries — every value here differs
// from what a flagless launch resolves to, which is what makes the carry
// observable at all. ToolGovernance is an OpenCode-only axis and auto-review a
// Codex-only one, so a Claude relaunch drops those two — carryoverHarness picks
// the harness that can actually honour each field.
func fullClaudePosture() *db.AgentRelaunchProfile {
	features := map[string]string{"bundled-skills": "off"}
	return &db.AgentRelaunchProfile{
		Version:                    db.RelaunchProfileVersion,
		HarnessBuiltinMode:         ptr(harness.ClaudeSandboxOn),
		SandboxImplementation:      ptr(string(sandboxpolicy.ImplementationTclaudeLayer)),
		ApprovalPolicy:             ptr("plan"),
		ApprovalAutoReview:         ptr(true),
		AskUserQuestionTimeout:     ptr("5m"),
		RemoteControl:              ptr(true),
		AutoMemory:                 ptr(true),
		ContextFeatures:            &features,
		AutoCompactWindow:          ptr("450000"),
		ConfiguredContextWindowMax: ptr(int64(100000)),
		CopilotAPI:                 ptr(true),
		FastMode:                   ptr(true),
		ToolGovernance:             ptr("ask"),
	}
}

// carryoverHarness returns a harness that supports the given carryover flag.
// Only --tools and --auto-review need anything other than Claude Code.
func carryoverHarness(t *testing.T, flag string) *harness.Harness {
	t.Helper()
	name := harness.DefaultName
	switch flag {
	case "tools":
		name = harness.OpenCodeName
	case "auto-review", "fast-mode":
		name = harness.CodexName
	case "context-window-max", "copilot-api":
		name = harness.CopilotName
	}
	h, err := harness.Resolve(name)
	require.NoError(t, err)
	return h
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

func TestApplyRecordedLaunchPosture_CarriesTemporarySandboxOverride(t *testing.T) {
	cwd := carryoverTestHome(t)
	normal := harness.ClaudeSandboxOn
	temporary := harness.ClaudeSandboxOff
	normalSource := `group default profile "confined"`
	seedResumableConv(t, cwd, &db.AgentRelaunchProfile{
		Version: db.RelaunchProfileVersion, HarnessBuiltinMode: &normal,
		HarnessBuiltinModeSource: &normalSource, TemporaryHarnessBuiltinMode: &temporary,
	})

	params := &NewParams{Resume: carryoverConvID, Dir: cwd}
	require.NoError(t, applyRecordedLaunchPosture(params, explicitLaunchFields{}))
	assert.Equal(t, harness.ClaudeSandboxOff, params.Sandbox)
	assert.Equal(t, db.TemporaryHarnessBuiltinModeSource, params.SandboxChosenBy)
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

// captureCarryoverStderr runs applyRecordedLaunchPosture with os.Stderr pointed
// at a pipe and returns everything it wrote there. The disclosure is an
// operator-facing side effect, so the only honest way to test it is to read the
// bytes the operator would see.
func captureCarryoverStderr(t *testing.T, params *NewParams, explicit explicitLaunchFields) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	orig := os.Stderr
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	applyErr := applyRecordedLaunchPosture(params, explicit)

	require.NoError(t, w.Close())
	os.Stderr = orig
	out, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, applyErr)
	return string(out)
}

// TestApplyRecordedLaunchPosture_DisclosesThePosturesThatChangeTheLaunch pins
// that a resume tells the operator which postures came back with a bare `-r`.
// Containment in particular: a human who typed no flags must not silently land
// in a sandboxed, plan-mode, app-reachable pane.
func TestApplyRecordedLaunchPosture_DisclosesThePosturesThatChangeTheLaunch(t *testing.T) {
	cwd := carryoverTestHome(t)
	seedResumableConv(t, cwd, fullClaudePosture())

	out := captureCarryoverStderr(t, &NewParams{Resume: carryoverConvID, Dir: cwd}, explicitLaunchFields{})

	for _, flag := range []string{"--sandbox", "--ask-for-approval", "--ask-user-question-timeout",
		"--remote-control", "--auto-memory", "--context-features", "--auto-compact-window"} {
		assert.Contains(t, out, flag, "a carried posture that changes the launch must be disclosed")
	}
}

// TestApplyRecordedLaunchPosture_SaysNothingWhenNothingWasPinned is the other
// half, and the one that keeps the line worth reading. A plain `tclaude session
// new` records an ASSERTED empty posture on every axis — that is the common
// case, not a rare one. Those values are still carried (the assertion must not
// decay back to unknown), but they resolve to exactly the launch an omitted flag
// gives, so announcing them would put a banner on every ordinary resume. An
// operator who has learned to skip that banner skips the `--sandbox off` one too.
func TestApplyRecordedLaunchPosture_SaysNothingWhenNothingWasPinned(t *testing.T) {
	cwd := carryoverTestHome(t)
	nothing := map[string]string{}
	seedResumableConv(t, cwd, &db.AgentRelaunchProfile{
		Version:                db.RelaunchProfileVersion,
		HarnessBuiltinMode:     ptr(""),
		ApprovalPolicy:         ptr(harness.ClaudePermissionInherit),
		ApprovalAutoReview:     ptr(false),
		AskUserQuestionTimeout: ptr(""),
		RemoteControl:          ptr(false),
		AutoMemory:             ptr(false),
		ContextFeatures:        &nothing,
		AutoCompactWindow:      ptr(""),
	})

	params := &NewParams{Resume: carryoverConvID, Dir: cwd}
	out := captureCarryoverStderr(t, params, explicitLaunchFields{})

	assert.Empty(t, out, "a resume that changes nothing must say nothing")
	// The values were still carried, they just did not need announcing.
	assert.Equal(t, harness.ClaudePermissionInherit, params.Approval)
	assert.False(t, params.AutoMemory)
	assert.Empty(t, params.AutoCompactWindow)
}

// TestApplyRecordedLaunchPosture_InheritIsNotAPinnedPosture is the same rule at
// the other spelling of "nothing pinned", and it is the COMMON path rather than
// an edge case: the daemon spawn path resolves a blank sandbox through
// harness.ResolveHarnessBuiltinMode, which applies claudeSandbox.DefaultMode() =
// inherit, so every agentd-spawned Claude agent that did not explicitly choose
// containment has sandbox_mode=inherit recorded against it. A human then typing
// `tclaude session new -r <that conv>` — the headline scenario for TCL-730 —
// must not be told that --sandbox came back, because it did not change anything.
func TestApplyRecordedLaunchPosture_InheritIsNotAPinnedPosture(t *testing.T) {
	cwd := carryoverTestHome(t)
	seedResumableConv(t, cwd, &db.AgentRelaunchProfile{
		Version:                db.RelaunchProfileVersion,
		HarnessBuiltinMode:     ptr(harness.ClaudeSandboxInherit),
		ApprovalPolicy:         ptr(harness.ClaudePermissionInherit),
		AskUserQuestionTimeout: ptr(harness.ClaudeAskTimeoutInherit),
	})

	params := &NewParams{Resume: carryoverConvID, Dir: cwd}
	out := captureCarryoverStderr(t, params, explicitLaunchFields{})

	assert.Empty(t, out, "inherit on all three axes is a launch identical to a fresh one")
	// Carried all the same: `inherit` is a first-class sentinel precisely so it
	// stays distinguishable from unrecorded, and dropping it here would let a
	// profile or group default win on the next hop.
	assert.Equal(t, harness.ClaudeSandboxInherit, params.Sandbox)
	assert.Equal(t, harness.ClaudePermissionInherit, params.Approval)
	assert.Equal(t, harness.ClaudeAskTimeoutInherit, params.AskUserQuestionTimeout)
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
	dropped := map[string]bool{}
	for _, field := range launchCarryoverFields {
		if field.classify(field.carry(codex, recorded, params)) == carryDropped {
			dropped[field.flag] = true
		}
	}
	// A dropped value must be reported as dropped, not as "nothing recorded" —
	// that distinction is what makes the operator warning possible.
	for _, flag := range []string{"sandbox", "auto-memory", "context-features",
		"auto-compact-window", "context-window-max", "remote-control", "ask-user-question-timeout"} {
		assert.True(t, dropped[flag], "a recorded --%s Codex cannot honour must report carryDropped", flag)
	}
	assert.False(t, params.AutoMemory)
	assert.Empty(t, params.ContextFeatures)
	assert.Empty(t, params.AutoCompactWindow)
	assert.False(t, params.RemoteControl)
	assert.Empty(t, params.AskUserQuestionTimeout)
	assert.Empty(t, params.Sandbox, "Claude's sandbox modes are not Codex's")
	assert.Empty(t, params.Approval, "Claude's permission modes are not Codex approval policies")
	assert.Empty(t, params.ToolGovernance, "tool governance is an OpenCode knob")
}

func TestLaunchCarryover_CopilotContextMaxCarries(t *testing.T) {
	recorded := fullClaudePosture()
	params := &NewParams{}
	for _, field := range launchCarryoverFields {
		if field.flag != "context-window-max" {
			continue
		}
		h, err := harness.Resolve(harness.CopilotName)
		require.NoError(t, err)
		assert.Equal(t, carryApplied, field.classify(field.carry(h, recorded, params)))
		assert.Equal(t, int64(100_000), params.ContextWindowMax)
	}
}

// TestLaunchCarryoverReadsTheFieldItDeclares closes the gap between the
// `recorded` label and what `carry` actually dereferences: the two structural
// guards check the label against db.AgentRelaunchProfile, but neither would
// notice a row whose closure reads a DIFFERENT field. Setting exactly one field
// on an otherwise-empty posture must make exactly that row carry.
func TestLaunchCarryoverReadsTheFieldItDeclares(t *testing.T) {
	full := reflect.ValueOf(*fullClaudePosture())

	for _, field := range launchCarryoverFields {
		t.Run(field.flag, func(t *testing.T) {
			h := carryoverHarness(t, field.flag)
			only := db.AgentRelaunchProfile{Version: db.RelaunchProfileVersion}
			value := full.FieldByName(field.recorded)
			require.True(t, value.IsValid(), "no such profile field")
			require.False(t, value.IsNil(), "fullClaudePosture must set %s for this test to mean anything", field.recorded)
			reflect.ValueOf(&only).Elem().FieldByName(field.recorded).Set(value)

			assert.Equal(t, carryApplied, field.classify(field.carry(h, &only, &NewParams{})),
				"--%s must carry when only %s is recorded", field.flag, field.recorded)

			empty := db.AgentRelaunchProfile{Version: db.RelaunchProfileVersion}
			assert.Equal(t, carryUnrecorded, field.classify(field.carry(h, &empty, &NewParams{})),
				"--%s must read %s, and report unknown when it is unset", field.flag, field.recorded)
		})
	}
}

// TestLaunchCarryoverReportsAnAssertedEmptyAsADefault is the structural guard
// for the disclosure: a posture that asserts "nothing pinned" is carried but
// must never be announced. Without this, one new row returning carryApplied
// unconditionally is enough to put the banner back on every ordinary resume,
// where it stops being read — and the `--sandbox off` line goes with it.
//
// It drives EVERY spelling of unpinned, not just the Go type's zero. Three axes
// have a first-class `inherit` sentinel that deliberately survives validation,
// and a guard that could only reach "" would have been structurally incapable of
// catching the case that actually needed it — which is what happened: sandbox
// and ask-user-question-timeout both announced `inherit` until the sentinel
// became a declared property of the row.
func TestLaunchCarryoverReportsAnAssertedEmptyAsADefault(t *testing.T) {
	for _, field := range launchCarryoverFields {
		t.Run(field.flag, func(t *testing.T) {
			h := carryoverHarness(t, field.flag)
			// The type's zero, unless this axis declares that zero is itself a
			// pinned choice, plus whatever else this row declares unpinned.
			spellings := []reflect.Value{}
			if !field.zeroMeaningful {
				zero := reflect.New(reflect.TypeOf(db.AgentRelaunchProfile{}).
					Field(profileFieldIndex(t, field.recorded)).Type.Elem())
				spellings = append(spellings, zero)
			}
			for _, sentinel := range field.unpinned {
				spellings = append(spellings, reflect.ValueOf(&sentinel))
			}

			for _, spelling := range spellings {
				pinned := db.AgentRelaunchProfile{Version: db.RelaunchProfileVersion}
				slot := reflect.ValueOf(&pinned).Elem().FieldByName(field.recorded)
				require.True(t, slot.IsValid(), "no such profile field")
				require.Equal(t, slot.Type(), spelling.Type(),
					"unpinned values must have the type of the field they are unpinned for")
				slot.Set(spelling)

				assert.Equal(t, carryAppliedDefault,
					field.classify(field.carry(h, &pinned, &NewParams{})),
					"%s recorded as %q resolves to the same launch as an omitted --%s, "+
						"so it must not be disclosed", field.recorded, spelling.Elem(), field.flag)
			}
		})
	}
}

func profileFieldIndex(t *testing.T, name string) int {
	t.Helper()
	f, ok := reflect.TypeOf(db.AgentRelaunchProfile{}).FieldByName(name)
	require.True(t, ok, "db.AgentRelaunchProfile has no field %s", name)
	return f.Index[0]
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
