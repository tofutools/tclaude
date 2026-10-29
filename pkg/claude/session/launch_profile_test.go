package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func setGlobalLaunchProfile(t *testing.T, prof db.SpawnProfile) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	id, err := db.CreateSpawnProfile(&prof)
	require.NoError(t, err)
	require.NoError(t, db.SetDashboardProfileRef(
		db.DashboardDefaultSpawnProfileNamePrefKey,
		db.DashboardDefaultSpawnProfileIDPrefKey,
		prof.Name, id))
}

func setupEmptyLaunchProfileDB(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
}

func TestGlobalDefaultLaunchProfile_InheritsHarnessModelAndEffort(t *testing.T) {
	setGlobalLaunchProfile(t, db.SpawnProfile{
		Name: "terminal-default", Harness: "codex", Model: "gpt-5.6-sol", Effort: "high",
	})
	params := &NewParams{}
	require.NoError(t, applyGlobalDefaultLaunchProfile(params, explicitLaunchFields{}))
	assert.Equal(t, "codex", params.Harness)
	assert.Equal(t, "gpt-5.6-sol", params.Model)
	assert.Equal(t, "high", params.Effort)
}

func TestGlobalDefaultLaunchProfile_NoProfileUsesInstalledHarness(t *testing.T) {
	setupEmptyLaunchProfileDB(t)
	params := &NewParams{}
	lookPath := func(binary string) (string, error) {
		if binary == "codex" {
			return "/usr/bin/codex", nil
		}
		return "", errors.New("not found")
	}
	require.NoError(t, applyGlobalDefaultLaunchProfileWithLookPath(
		params, explicitLaunchFields{}, lookPath))
	assert.Equal(t, "codex", params.Harness)
	assert.Empty(t, params.Model)
	assert.Empty(t, params.Effort)
}

func TestGlobalDefaultLaunchProfile_GlobalFlagRemainsAHumanLaunch(t *testing.T) {
	setupEmptyLaunchProfileDB(t)
	params := &NewParams{Global: true}
	lookPath := func(binary string) (string, error) {
		if binary == "codex" {
			return "/usr/bin/codex", nil
		}
		return "", errors.New("not found")
	}
	require.NoError(t, applyGlobalDefaultLaunchProfileWithLookPath(
		params, explicitLaunchFields{}, lookPath))
	assert.Equal(t, "codex", params.Harness)
}

func TestFirstInstalledHarness_PrefersClaude(t *testing.T) {
	assert.Equal(t, "claude", firstInstalledHarness(func(binary string) (string, error) {
		if binary == "claude" || binary == "codex" {
			return "/usr/bin/" + binary, nil
		}
		return "", errors.New("not found")
	}))
	assert.Empty(t, firstInstalledHarness(func(string) (string, error) {
		return "", errors.New("not found")
	}))
}

func TestFirstInstalledHarness_ExcludesShellSentinel(t *testing.T) {
	assert.Empty(t, firstInstalledHarness(func(binary string) (string, error) {
		if binary == ShellHarnessName {
			return "/bin/sh", nil
		}
		return "", errors.New("not found")
	}))
}

func TestGlobalDefaultLaunchProfile_ExplicitFieldsWinIndependently(t *testing.T) {
	tests := []struct {
		name     string
		params   NewParams
		explicit explicitLaunchFields
		want     NewParams
	}{
		{
			name:     "harness",
			params:   NewParams{Harness: "claude"},
			explicit: explicitLaunchFields{"harness": true},
			want:     NewParams{Harness: "claude"},
		},
		{
			name:     "model",
			params:   NewParams{Model: "gpt-5.4"},
			explicit: explicitLaunchFields{"model": true},
			want:     NewParams{Harness: "codex", Model: "gpt-5.4", Effort: "high"},
		},
		{
			name:     "effort",
			params:   NewParams{Effort: "low"},
			explicit: explicitLaunchFields{"effort": true},
			want:     NewParams{Harness: "codex", Model: "gpt-5.6-sol", Effort: "low"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setGlobalLaunchProfile(t, db.SpawnProfile{
				Name: "terminal-default", Harness: "codex", Model: "gpt-5.6-sol", Effort: "high",
			})
			params := tt.params
			require.NoError(t, applyGlobalDefaultLaunchProfile(&params, tt.explicit))
			assert.Equal(t, tt.want.Harness, params.Harness)
			assert.Equal(t, tt.want.Model, params.Model)
			assert.Equal(t, tt.want.Effort, params.Effort)
		})
	}
}

func TestGlobalDefaultLaunchProfile_ForeignModelAndEffortDoNotReachCopilot(t *testing.T) {
	setGlobalLaunchProfile(t, db.SpawnProfile{
		Name: "claude-default", Harness: harness.DefaultName,
		Model: "opus[1m]", Effort: "high",
	})
	params := &NewParams{Harness: harness.CopilotName}
	require.NoError(t, applyGlobalDefaultLaunchProfile(
		params, explicitLaunchFields{"harness": true},
	))
	assert.Equal(t, harness.CopilotName, params.Harness)
	assert.Empty(t, params.Model,
		"a Claude default profile's model must not fill a Copilot terminal launch")
	assert.Empty(t, params.Effort,
		"a Claude default profile's effort must not fill a Copilot terminal launch")
}

func TestGlobalDefaultLaunchProfile_ExplicitEmptyFieldUsesHarnessDefault(t *testing.T) {
	setGlobalLaunchProfile(t, db.SpawnProfile{
		Name: "terminal-default", Harness: "codex", Model: "gpt-5.6-sol", Effort: "high",
	})
	params := &NewParams{}
	require.NoError(t, applyGlobalDefaultLaunchProfile(params, explicitLaunchFields{"model": true}))
	assert.Equal(t, "codex", params.Harness)
	assert.Empty(t, params.Model, "an explicit --model= must not be filled from the profile")
	assert.Equal(t, "high", params.Effort)
}

func TestGlobalDefaultLaunchProfile_DoesNotReResolveDaemonOrResumeLaunches(t *testing.T) {
	setGlobalLaunchProfile(t, db.SpawnProfile{
		Name: "terminal-default", Harness: "codex", Model: "gpt-5.6-sol", Effort: "high",
	})
	for _, params := range []*NewParams{
		{ManagedLaunch: true},
		{Resume: "conversation-id"},
		{JoinGroup: "workers"},
	} {
		require.NoError(t, applyGlobalDefaultLaunchProfile(params, explicitLaunchFields{}))
		assert.Empty(t, params.Harness)
		assert.Empty(t, params.Model)
		assert.Empty(t, params.Effort)
	}
}

func TestGlobalDefaultLaunchProfile_DisabledProfileBlocksFreshTerminalLaunch(t *testing.T) {
	setGlobalLaunchProfile(t, db.SpawnProfile{
		Name: "paused", Disabled: true, DisabledReason: "maintenance",
	})
	err := applyGlobalDefaultLaunchProfile(&NewParams{}, explicitLaunchFields{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `global default spawn profile "paused" is disabled: maintenance`)
}

func TestSandboxImplementationDecision_RejectsFreshOpenCodeBuiltinButGrandfathersReplay(t *testing.T) {
	opencode, ok := harness.Get(harness.OpenCodeName)
	require.True(t, ok)

	err := validateSandboxImplementationDecision(
		opencode,
		string(sandboxpolicy.ImplementationHarnessBuiltin),
		sandboxpolicy.ImplementationHarnessBuiltin,
		true,
	)
	require.EqualError(t, err,
		`sandbox implementation "harness-builtin" is invalid for OpenCode: `+
			`OpenCode has no built-in OS sandbox; its access-control mode is a command filter, `+
			`not confinement; use tclaude’s sandbox or spawn with the sandbox off`)

	assert.NoError(t, validateSandboxImplementationDecision(
		opencode,
		"",
		sandboxpolicy.ImplementationHarnessBuiltin,
		true,
	), "unset is not a harness-builtin pin")
	assert.NoError(t, validateSandboxImplementationDecision(
		opencode,
		string(sandboxpolicy.ImplementationHarnessBuiltin),
		sandboxpolicy.ImplementationHarnessBuiltin,
		false,
	), "a historical pure replay remains launchable")
}

// postureProfile is a Claude default profile that pins a non-default value on
// every launch-posture field a terminal launch takes from it.
func postureProfile() db.SpawnProfile {
	return db.SpawnProfile{
		Name: "posture-default", Harness: harness.DefaultName,
		Sandbox:                harness.ClaudeSandboxOff,
		SandboxImplementation:  string(sandboxpolicy.ImplementationOff),
		Approval:               "acceptEdits",
		AskUserQuestionTimeout: "5m",
		AutoCompactWindow:      "450000",
		TrustDir:               ptr(true),
		RemoteControl:          ptr(true),
		AutoMemory:             ptr(true),
		PeerMessaging:          ptr(true),
		ContextFeatures:        map[string]string{"bundled-skills": "off", "artifact": "off"},
	}
}

func TestGlobalDefaultLaunchProfile_FreshLaunchInheritsLaunchPosture(t *testing.T) {
	setGlobalLaunchProfile(t, postureProfile())
	params := &NewParams{}
	require.NoError(t, applyGlobalDefaultLaunchProfile(params, explicitLaunchFields{}))
	assert.Equal(t, harness.DefaultName, params.Harness)
	assert.Equal(t, harness.ClaudeSandboxOff, params.Sandbox)
	assert.Equal(t, `global default profile "posture-default"`, params.SandboxChosenBy)
	assert.Equal(t, string(sandboxpolicy.ImplementationOff), params.SandboxImpl)
	assert.True(t, params.sandboxImplExplicit, "a profile implementation is a fresh decision")
	assert.Equal(t, "acceptEdits", params.Approval)
	assert.Equal(t, "5m", params.AskUserQuestionTimeout)
	assert.Equal(t, "450000", params.AutoCompactWindow)
	assert.True(t, params.TrustDir)
	assert.True(t, params.RemoteControl)
	assert.True(t, params.AutoMemory)
	assert.True(t, params.PeerMessaging)
	assert.Equal(t, "artifact=off,bundled-skills=off", params.ContextFeatures)
}

func TestGlobalDefaultLaunchProfile_ExplicitPostureFlagsWin(t *testing.T) {
	setGlobalLaunchProfile(t, postureProfile())
	params := &NewParams{Sandbox: harness.ClaudeSandboxOn}
	require.NoError(t, applyGlobalDefaultLaunchProfile(params, explicitLaunchFields{
		"sandbox": true, "auto-memory": true,
	}))
	assert.Equal(t, harness.ClaudeSandboxOn, params.Sandbox)
	assert.Empty(t, params.SandboxImpl,
		"an explicit --sandbox keeps the profile's implementation out too: off would force it off")
	assert.False(t, params.AutoMemory, "an explicit --auto-memory=false must not be filled")
	assert.Equal(t, "acceptEdits", params.Approval, "omitted fields still come from the profile")
}

func TestGlobalDefaultLaunchProfile_ForeignHarnessPostureIsNotApplied(t *testing.T) {
	setGlobalLaunchProfile(t, postureProfile())
	params := &NewParams{Harness: "codex"}
	require.NoError(t, applyGlobalDefaultLaunchProfile(params, explicitLaunchFields{"harness": true}))
	assert.Empty(t, params.Sandbox)
	assert.Empty(t, params.SandboxImpl)
	assert.Empty(t, params.Approval)
	assert.False(t, params.AutoMemory)
}

func setGlobalLaunchProfileInCurrentHome(t *testing.T, prof db.SpawnProfile) {
	t.Helper()
	id, err := db.CreateSpawnProfile(&prof)
	require.NoError(t, err)
	require.NoError(t, db.SetDashboardProfileRef(
		db.DashboardDefaultSpawnProfileNamePrefKey,
		db.DashboardDefaultSpawnProfileIDPrefKey,
		prof.Name, id))
}

// A resume keeps every posture the conversation recorded — including one
// recorded as "nothing pinned" — and takes the profile only for the rest.
func TestResumeDefaultLaunchProfile_FillsOnlyUnrecordedFields(t *testing.T) {
	cwd := carryoverTestHome(t)
	setGlobalLaunchProfileInCurrentHome(t, postureProfile())
	seedResumableConv(t, cwd, &db.AgentRelaunchProfile{
		Version:            db.RelaunchProfileVersion,
		HarnessBuiltinMode: ptr(harness.ClaudeSandboxOn),
		AutoMemory:         ptr(false),
	})

	params := &NewParams{Resume: carryoverConvID, Dir: cwd}
	explicit := explicitLaunchFields{"ask-for-approval": true}
	require.NoError(t, applyRecordedLaunchPosture(params, explicit))
	require.NoError(t, applyResumeDefaultLaunchProfile(params, explicit))

	assert.Equal(t, harness.ClaudeSandboxOn, params.Sandbox, "recorded mode wins over the profile")
	assert.False(t, params.AutoMemory, "a recorded false is the conversation's decision")
	assert.Empty(t, params.Approval, "an explicit flag wins over the profile")
	assert.Empty(t, params.SandboxImpl, "a recorded sandbox mode keeps the whole containment decision")
	assert.Equal(t, "450000", params.AutoCompactWindow, "an unrecorded field falls back to the profile")
	assert.Empty(t, params.Harness, "a resume never takes the profile's harness")
	assert.Empty(t, params.Model)
}

func TestResumeDefaultLaunchProfile_LegacyConversationGetsProfile(t *testing.T) {
	cwd := carryoverTestHome(t)
	setGlobalLaunchProfileInCurrentHome(t, postureProfile())
	seedResumableConv(t, cwd, nil)

	params := &NewParams{Resume: carryoverConvID, Dir: cwd}
	require.NoError(t, applyRecordedLaunchPosture(params, explicitLaunchFields{}))
	require.NoError(t, applyResumeDefaultLaunchProfile(params, explicitLaunchFields{}))
	assert.Equal(t, harness.ClaudeSandboxOff, params.Sandbox)
	assert.Equal(t, string(sandboxpolicy.ImplementationOff), params.SandboxImpl)
}

func TestResumeDefaultLaunchProfile_SkipsForeignHarnessAndManagedLaunches(t *testing.T) {
	carryoverTestHome(t)
	setGlobalLaunchProfileInCurrentHome(t, postureProfile())
	for _, params := range []*NewParams{
		{Resume: carryoverConvID, Harness: "codex"},
		{Resume: carryoverConvID, ManagedLaunch: true},
		{},
	} {
		require.NoError(t, applyResumeDefaultLaunchProfile(params, explicitLaunchFields{}))
		assert.Empty(t, params.Sandbox)
		assert.Empty(t, params.SandboxImpl)
	}
}

func TestGlobalDefaultLaunchProfile_PermissionProfileKeepsProfileSandboxOut(t *testing.T) {
	setGlobalLaunchProfile(t, db.SpawnProfile{
		Name: "codex-default", Harness: "codex", Sandbox: "workspace-write",
	})
	params := &NewParams{Harness: "codex", PermissionProfile: "mine"}
	require.NoError(t, applyGlobalDefaultLaunchProfile(params, explicitLaunchFields{
		"harness": true, "permission-profile": true,
	}))
	assert.Empty(t, params.Sandbox, "--permission-profile and --sandbox are mutually exclusive")
}

func TestResumeDefaultLaunchProfile_RecordedSandboxKeepsProfileImplementationOut(t *testing.T) {
	cwd := carryoverTestHome(t)
	setGlobalLaunchProfileInCurrentHome(t, postureProfile())
	seedResumableConv(t, cwd, &db.AgentRelaunchProfile{
		Version:            db.RelaunchProfileVersion,
		HarnessBuiltinMode: ptr(harness.ClaudeSandboxOn),
	})

	params := &NewParams{Resume: carryoverConvID, Dir: cwd}
	require.NoError(t, applyRecordedLaunchPosture(params, explicitLaunchFields{}))
	require.NoError(t, applyResumeDefaultLaunchProfile(params, explicitLaunchFields{}))
	assert.Equal(t, harness.ClaudeSandboxOn, params.Sandbox)
	assert.Empty(t, params.SandboxImpl,
		"the profile's off implementation would silently drop the recorded --sandbox on")
}

// The terminal command path is what makes the resume fallback live; the fill
// functions above are only reachable through it. runNew launches tmux, so the
// wiring is pinned structurally: record first, then the profile, then launch.
func TestTerminalResumeAppliesProfileAfterRecordedPosture(t *testing.T) {
	fn := guardFunc(guardParse(t, "launch_profile.go", ""), "runNewWithGlobalDefault")
	require.NotNil(t, fn)
	order := callOrder(fn, "applyRecordedLaunchPosture", "applyResumeDefaultLaunchProfile", "runNew")
	recorded, okRecorded := order["applyRecordedLaunchPosture"]
	profile, okProfile := order["applyResumeDefaultLaunchProfile"]
	launch, okLaunch := order["runNew"]
	require.True(t, okRecorded && okProfile && okLaunch,
		"runNewWithGlobalDefault must call all three: %v", order)
	assert.Less(t, int(recorded), int(profile), "the record must win over the profile")
	assert.Less(t, int(profile), int(launch), "the profile must be applied before launch")
}
