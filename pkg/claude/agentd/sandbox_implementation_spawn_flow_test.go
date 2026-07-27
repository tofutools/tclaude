package agentd_test

import (
	"encoding/json"
	"net/http"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario (TCL-769): the sandbox IMPLEMENTATION — which layer owns OS-level
// containment — becomes a spawn parameter: a spawn-profile field, a CLI flag,
// and a dashboard dialog choice. It stays default-off everywhere.
//
// The contract these pin, in the order it matters:
//
//  1. Nothing changes for a spawn that says nothing. That is the whole
//     default-off promise and it is the first test below.
//  2. A profile can pin it, through every precedence tier, and an explicit
//     request beats them all.
//  3. OpenCode accepts the same outer implementation on Linux and normalizes
//     its persisted mode to the executor-specific tclaude-layer posture.
//  4. HOST unavailability NEVER falls through. It refuses, from any tier, and
//     says which capability is missing.
//
// Harness applicability and host availability remain separate mechanisms:
// merging them would turn "this machine has no bwrap" into "quietly resolved
// to harness-builtin" for a lower-tier profile.

// TestSpawn_SandboxImplementationDefaultsUnset is the load-bearing regression:
// a plain spawn must reach the launch with NOTHING pinned, so the argv builder
// emits no --sandbox-impl at all and every existing user is unaffected.
func TestSpawn_SandboxImplementationDefaultsUnset(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnHarness("crew", "plain-worker", "claude")

	got, ok := f.World.SpawnSandboxImplementation(spawn.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Empty(t, got,
		"a plain spawn must pin no sandbox implementation, leaving the harness-owned default in charge")
	assertSandboxLayerCalls(t, f)
}

// TestSpawn_ExplicitTclaudeLayerReachesLaunch: the opt-in works and arrives
// intact at the launch boundary, which is where it becomes --sandbox-impl.
func TestSpawn_ExplicitTclaudeLayerReachesLaunch(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "layered",
		"sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, 200, resp.Code, "explicit tclaude-layer spawn; body=%s", resp.Raw)

	got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Equal(t, "tclaude-layer", got)
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerInteractive)
}

// TestSpawn_SandboxImplementationFromProfileTiers: the field rides the standard
// per-field precedence, at every tier — the named --profile, the group default,
// and the global default. This is the path an operator actually uses: pin it
// once, get it on every agent spawned through that tier.
func TestSpawn_SandboxImplementationFromProfileTiers(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire func(t *testing.T, f *testharness.Flow)
		body map[string]any
	}{
		{
			name: "named profile",
			wire: func(t *testing.T, f *testharness.Flow) {
				rec := createProfile(t, f, map[string]any{
					"name": "layered", "harness": "claude", "sandbox_implementation": "tclaude-layer",
				})
				require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
			},
			body: map[string]any{"name": "worker", "profile": "layered"},
		},
		{
			name: "group default profile",
			wire: func(t *testing.T, f *testharness.Flow) {
				rec := createProfile(t, f, map[string]any{
					"name": "team-default", "harness": "claude", "sandbox_implementation": "tclaude-layer",
				})
				require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
				rec = setGroupProfile(t, f, "crew", "team-default")
				require.Equalf(t, http.StatusOK, rec.Code, "set default_profile body=%s", rec.Body.String())
			},
			body: map[string]any{"name": "worker"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			tc.wire(t, f)

			resp := f.AsHuman().SpawnWith("crew", tc.body)
			require.Equalf(t, 200, resp.Code, "spawn; body=%s", resp.Raw)

			got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
			require.True(t, ok, "the spawn should have been observed by the sim spawner")
			assert.Equal(t, "tclaude-layer", got,
				"a profile's sandbox implementation must reach the launch")
			assertSandboxLayerCalls(t, f, testharness.SandboxLayerInteractive)
		})
	}
}

// TestSpawn_ExplicitHarnessBuiltinPinsAgainstProfile: "" (unset) and an explicit
// "harness-builtin" are deliberately different values. Unset falls through;
// harness-builtin PINS the legacy implementation so a group default cannot flip
// the agent onto the experimental layer. Without that distinction an operator
// would have no way to opt one spawn back out.
func TestSpawn_ExplicitHarnessBuiltinPinsAgainstProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	rec := createProfile(t, f, map[string]any{
		"name": "team-default", "harness": "claude", "sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	rec = setGroupProfile(t, f, "crew", "team-default")
	require.Equalf(t, http.StatusOK, rec.Code, "set default_profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "opted-out", "sandbox_implementation": "harness-builtin",
	})
	require.Equalf(t, 200, resp.Code, "explicit harness-builtin spawn; body=%s", resp.Raw)

	got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Equal(t, "harness-builtin", got,
		"an explicit harness-builtin must beat a group default profile's tclaude-layer")
	assertSandboxLayerCalls(t, f)
}

// TestSpawn_ExplicitSandboxImplementationRejectsUnknownValue: an explicit value
// is direct intent, so a typo is a loud 400 rather than a silent fallback to
// the default.
func TestSpawn_ExplicitSandboxImplementationRejectsUnknownValue(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "typo", "sandbox_implementation": "tclaude_layer",
	})
	require.Equal(t, http.StatusBadRequest, resp.Code,
		"an unknown implementation must be refused; body=%s", resp.Raw)
	assert.Contains(t, string(resp.Raw), "invalid sandbox implementation",
		"the refusal must name what was wrong")
}

func TestSpawn_OpenCodeExplicitHarnessBuiltinRefuses(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "false-wall",
		"harness":                harness.OpenCodeName,
		"sandbox_implementation": string(sandboxpolicy.ImplementationHarnessBuiltin),
	})
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code,
		"a semantic harness mismatch must be a typed 422; body=%s", resp.Raw)
	failure := decodeFailure(t, resp.Raw)
	assert.Equal(t, "invalid_sandbox_implementation", failure.Code)
	assert.Equal(t,
		`sandbox implementation "harness-builtin" is invalid for OpenCode: `+
			`OpenCode has no built-in OS sandbox; its access-control mode is a command filter, `+
			`not confinement; use tclaude-layer or spawn with the sandbox off`,
		failure.Error)
	assertSandboxLayerCalls(t, f)
}

func TestSpawn_OpenCodeForeignHarnessBuiltinTierSkipsAndDiscloses(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	rec := createProfile(t, f, map[string]any{
		"name": "claude-builtin", "harness": harness.DefaultName,
		"sandbox_implementation": string(sandboxpolicy.ImplementationHarnessBuiltin),
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	rec = setGroupProfile(t, f, "crew", "claude-builtin")
	require.Equalf(t, http.StatusOK, rec.Code, "set default_profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "oc-worker", "harness": harness.OpenCodeName,
	})
	require.Equalf(t, http.StatusOK, resp.Code,
		"a foreign profile tier must skip and fall through; body=%s", resp.Raw)
	var wire struct {
		Resolved struct {
			Notes []string `json:"notes"`
		} `json:"resolved"`
	}
	require.NoError(t, json.Unmarshal(resp.Raw, &wire))
	assert.Contains(t, wire.Resolved.Notes,
		`group default profile "claude-builtin" sandbox_implementation ignored (not valid for opencode)`)
	got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
	require.True(t, ok)
	assert.Empty(t, got, "the skipped foreign pin must leave OpenCode's implementation unset")
	assertSandboxLayerCalls(t, f)
}

func TestResume_OpenCodeUnsetImplementationRemainsGrandfathered(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnHarness("crew", "oc-worker", harness.OpenCodeName)
	initial, ok := f.World.SpawnSandboxImplementation(spawn.ConvID)
	require.True(t, ok)
	require.Empty(t, initial, "precondition: the fresh launch left the implementation unset")

	f.MarkOffline(spawn.TmuxSession)
	resume := f.AsHuman().Resume(spawn.ConvID)
	require.Equal(t, "resumed", resume.Action, "resume action: %s", resume.Raw)

	replayed, ok := f.World.SpawnSandboxImplementation(spawn.ConvID)
	require.True(t, ok, "the resume must reach the simulated spawner")
	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin), replayed,
		"the legacy/default recorded spelling is a pure replay and must not strand the agent")
}

func TestForks_OpenCodeUnsetImplementationRemainsGrandfathered(t *testing.T) {
	tests := []struct {
		name string
		fork func(*testharness.Flow, string) string
	}{
		{
			name: "reincarnate",
			fork: func(f *testharness.Flow, convID string) string {
				return f.AsHuman().Reincarnate(convID, "continue").NewConv
			},
		},
		{
			name: "clone",
			fork: func(f *testharness.Flow, convID string) string {
				return f.AsHuman().CloneFresh(convID).NewConv
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			source := f.AsHuman().SpawnHarness("crew", "oc-source", harness.OpenCodeName)

			forkedConv := tt.fork(f, source.ConvID)
			require.NotEmpty(t, forkedConv)
			replayed, ok := f.World.SpawnSandboxImplementation(forkedConv)
			require.True(t, ok, "the fork must reach the simulated spawner")
			assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin), replayed,
				"forking an ordinary unset OpenCode agent must replay the legacy/default spelling")
		})
	}
}

func TestReincarnate_TemporaryOffDisablesTclaudeOuterLayer(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	source := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "layered-source",
		"harness":                harness.DefaultName,
		"sandbox":                harness.ClaudeSandboxOn,
		"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
	})
	require.Equalf(t, http.StatusOK, source.Code, "spawn body=%s", source.Raw)

	override := harness.ClaudeSandboxOff
	require.NoError(t, db.SetTemporarySandboxModeForConv(
		source.ConvID, harness.ClaudeSandboxOn, "", &override,
	))

	successor := f.AsHuman().Reincarnate(source.ConvID, "continue").NewConv
	require.NotEmpty(t, successor)
	implementation, ok := f.World.SpawnSandboxImplementation(successor)
	require.True(t, ok, "reincarnation must reach the simulated spawner")
	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin), implementation,
		"temporary off must omit the tclaude outer wrapper for the successor")
	mode, ok := f.World.SpawnSandbox(successor)
	require.True(t, ok, "reincarnation must record the successor's sandbox mode")
	assert.Equal(t, harness.ClaudeSandboxOff, mode,
		"temporary off must also disable the harness-native sandbox")
}

func TestSpawn_ExplicitTclaudeLayerWrapsOpenCodeExecutor(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "oc-worker",
		"harness":                "opencode",
		"sandbox_implementation": "tclaude-layer",
	})
	if runtime.GOOS == "darwin" {
		require.Equal(t, http.StatusBadRequest, resp.Code,
			"macOS must refuse the Linux-only OpenCode executor boundary")
		failure := decodeFailure(t, resp.Raw)
		assert.Equal(t, "invalid_sandbox_implementation", failure.Code)
		assert.Contains(t, failure.Error, "does not support OpenCode on macOS")
		assertSandboxLayerCalls(t, f)
		return
	}
	require.Equal(t, http.StatusOK, resp.Code,
		"tclaude-layer on OpenCode must launch; body=%s", resp.Raw)
	implementation, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer), implementation)
	mode, ok := f.World.SpawnSandbox(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, harness.OpenCodeSandboxTclaudeLayer, mode,
		"the launch record must name the executor boundary, not soft access control")
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerServer)
}

func TestSpawn_OpenCodeModeAndImplementationContradictionsRefuse(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	for _, body := range []map[string]any{
		{
			"name": "mode-without-layer", "harness": "opencode",
			"sandbox": harness.OpenCodeSandboxTclaudeLayer,
		},
		{
			"name": "off-with-layer", "harness": "opencode",
			"sandbox":                harness.OpenCodeSandboxOff,
			"sandbox_implementation": string(sandboxpolicy.ImplementationTclaudeLayer),
		},
	} {
		resp := f.AsHuman().SpawnWith("crew", body)
		require.Equalf(t, http.StatusBadRequest, resp.Code,
			"contradictory OpenCode sandbox axes must fail; body=%s", resp.Raw)
		assert.Contains(t, string(resp.Raw), "invalid_sandbox")
	}
}

func TestSpawn_OpenCodeAcceptsLayerFromLowerTier(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	rec := createProfile(t, f, map[string]any{
		"name": "claude-layered", "harness": "claude", "sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	rec = setGroupProfile(t, f, "crew", "claude-layered")
	require.Equalf(t, http.StatusOK, rec.Code, "set default_profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "oc-worker", "harness": "opencode",
	})
	require.Equalf(t, 200, resp.Code,
		"the lower-tier implementation either applies or is disclosed and skipped; body=%s", resp.Raw)

	got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	if runtime.GOOS == "darwin" {
		assert.Empty(t, got,
			"an ambient Claude profile cannot apply OpenCode's Linux-only server boundary on macOS")
		mode, ok := f.World.SpawnSandbox(resp.ConvID)
		require.True(t, ok)
		assert.Equal(t, harness.OpenCodeSandboxAccessControl, mode,
			"the skipped ambient pin must fall through to OpenCode's default")
		assert.Contains(t, string(resp.Raw),
			"sandbox_implementation ignored (not supported for OpenCode on macOS)",
			"the platform-driven tier skip must be disclosed")
		assertSandboxLayerCalls(t, f)
		return
	}
	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer), got)
	mode, ok := f.World.SpawnSandbox(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, harness.OpenCodeSandboxTclaudeLayer, mode)
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerServer)
}

// decodeFailure reads the daemon's error envelope. Asserting on the DECODED
// message rather than the raw body matters here: a real capability error
// carries quoted text (exec: "bwrap": not found), which JSON escapes — so a
// substring check against the raw bytes would fail for exactly the messages
// this feature must carry through verbatim.
func decodeFailure(t *testing.T, raw []byte) struct {
	Code  string `json:"code"`
	Error string `json:"error"`
} {
	t.Helper()
	var failure struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	require.NoErrorf(t, json.Unmarshal(raw, &failure), "decode failure body=%s", raw)
	return failure
}

func assertSandboxLayerCalls(
	t *testing.T,
	f *testharness.Flow,
	want ...testharness.SandboxLayerBoundary,
) {
	t.Helper()
	got := f.World.SandboxLayer.Calls()
	if len(want) == 0 {
		assert.Empty(t, got, "production unexpectedly probed sandbox-layer host capability")
		return
	}
	require.Len(t, want, 1, "helper accepts one expected boundary or none")
	require.NotEmpty(t, got, "production never probed the expected sandbox-layer boundary")
	for _, boundary := range got {
		assert.Equal(t, want[0], boundary,
			"production selected a different sandbox-layer capability boundary")
	}
}

func sandboxLayerUnavailableMessage(reason error) string {
	return "sandbox implementation tclaude-layer is not available on this host: " +
		reason.Error() +
		"; refusing the launch rather than falling back to harness-builtin"
}

// TestSpawn_TclaudeLayerRefusedWhenHostLacksCapability: refuse-don't-degrade.
// The refusal names the missing capability so the operator can fix it, and says
// out loud that it is not falling back — the distinction that separates this
// from malformed or contradictory launch fields.
func TestSpawn_TclaudeLayerRefusedWhenHostLacksCapability(t *testing.T) {
	f := newFlow(t)
	f.World.SandboxLayer.SetAvailability(testharness.SandboxLayerInteractive, errNoBwrap)
	f.HaveGroup("crew")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "doomed", "sandbox_implementation": "tclaude-layer",
	})
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code,
		"a host that cannot run the layer must refuse the spawn; body=%s", resp.Raw)
	failure := decodeFailure(t, resp.Raw)
	assert.Equal(t, "sandbox_implementation_unavailable", failure.Code,
		"the refusal kind must distinguish host-unavailable from an inapplicable value")
	assert.Equal(t, sandboxLayerUnavailableMessage(errNoBwrap), failure.Error,
		"the named simulator refusal must pass through the exact production literal")
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerInteractive)
}

func TestSpawn_OpenCodeRefusedWhenServerBoundaryLacksCapability(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("OpenCode tclaude-layer is inapplicable before the host gate on macOS")
	}
	f := newFlow(t)
	f.World.SandboxLayer.SetAvailability(testharness.SandboxLayerServer, errNoBwrap)
	f.HaveGroup("crew")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "doomed-server",
		"harness":                "opencode",
		"sandbox_implementation": "tclaude-layer",
	})
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code,
		"a host without the server boundary must refuse OpenCode; body=%s", resp.Raw)
	failure := decodeFailure(t, resp.Raw)
	assert.Equal(t, "sandbox_implementation_unavailable", failure.Code)
	assert.Equal(t, sandboxLayerUnavailableMessage(errNoBwrap), failure.Error)
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerServer)
}

// TestSpawn_TclaudeLayerFromProfileStillRefusedWhenHostLacksCapability: the
// case the design note singles out. The value arrived from a group default
// profile rather than from this request, and it must STILL refuse. If host
// availability rode the per-harness validator instead of its own gate, this is
// the spawn that would have silently resolved to harness-builtin.
func TestSpawn_TclaudeLayerFromProfileStillRefusedWhenHostLacksCapability(t *testing.T) {
	f := newFlow(t)
	f.World.SandboxLayer.SetAvailability(testharness.SandboxLayerInteractive, errNoBwrap)
	f.HaveGroup("crew")

	rec := createProfile(t, f, map[string]any{
		"name": "team-default", "harness": "claude", "sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	rec = setGroupProfile(t, f, "crew", "team-default")
	require.Equalf(t, http.StatusOK, rec.Code, "set default_profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("crew", map[string]any{"name": "inherits-doom"})
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code,
		"a profile-supplied tclaude-layer must refuse just as loudly; body=%s", resp.Raw)
	failure := decodeFailure(t, resp.Raw)
	assert.Equal(t, "sandbox_implementation_unavailable", failure.Code)
	assert.Equal(t, sandboxLayerUnavailableMessage(errNoBwrap), failure.Error)
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerInteractive)
}

// TestSpawn_ResolvedLaunchEchoesSandboxImplementationSource: an agent whose wall
// was chosen by a profile it never named must be able to see that. The echo
// names the tier, not just the value.
func TestSpawn_ResolvedLaunchEchoesSandboxImplementationSource(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	rec := createProfile(t, f, map[string]any{
		"name": "team-default", "harness": "claude", "sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())
	rec = setGroupProfile(t, f, "crew", "team-default")
	require.Equalf(t, http.StatusOK, rec.Code, "set default_profile body=%s", rec.Body.String())

	resp := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker"})
	require.Equalf(t, 200, resp.Code, "spawn; body=%s", resp.Raw)

	var parsed struct {
		Resolved struct {
			SandboxImpl struct {
				Value  string `json:"value"`
				Source string `json:"source"`
			} `json:"sandbox_implementation"`
		} `json:"resolved"`
	}
	require.NoError(t, json.Unmarshal(resp.Raw, &parsed))
	assert.Equal(t, "tclaude-layer", parsed.Resolved.SandboxImpl.Value)
	assert.Contains(t, parsed.Resolved.SandboxImpl.Source, "team-default",
		"the echo must name the profile that chose the containment layer")
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerInteractive)
}

// TestTaskForceDeploy_TclaudeLayerRefusedWhenHostLacksCapability covers the
// SECOND host gate — the one in applyDefaultProfile rather than at the HTTP
// spawn boundary.
//
// The distinction is the point. A template deploy builds spawnParams itself and
// calls executeSpawn directly, so it never passes handleGroupSpawn's gate. If
// the gate inside applyDefaultProfile were removed, every other test in this
// file would still pass while a template pinning tclaude-layer deployed happily
// onto a host that cannot run it — the refusal arriving only later, in a
// detached pane nobody is watching.
func TestTaskForceDeploy_TclaudeLayerRefusedWhenHostLacksCapability(t *testing.T) {
	f := newFlow(t)
	f.World.SandboxLayer.SetAvailability(testharness.SandboxLayerInteractive, errNoBwrap)

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "layered", "harness": "claude", "sandbox_implementation": "tclaude-layer",
	}).Code, "create profile")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "team",
		"agents": []map[string]any{
			{"name": "worker", "role": "dev", "spawn_profile": "layered"},
		},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/deploy", map[string]any{
		"group_name": "phoenix",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	agentd.WaitForBackgroundForTest()

	require.Equal(t, 0, res.Spawned, "the member must not launch on a host that cannot run the layer")
	require.Equal(t, 1, res.Failed, "the deploy must report the member as failed, not quietly downgraded")
	require.Len(t, res.Agents, 1)
	assert.Contains(t, res.Agents[0].Error, errNoBwrap.Error(),
		"the per-member failure must name the concrete missing capability")
	assert.Contains(t, res.Agents[0].Error, "rather than falling back",
		"the per-member failure must say it is not degrading to harness-builtin")
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerInteractive)
}

// TestTaskForceDeploy_TclaudeLayerDeploysWhenHostSupportsIt is the positive
// half: the same template on a capable host reaches the launch with the layer
// intact, so the gate above is refusing for the right reason rather than
// blocking template deploys outright.
func TestTaskForceDeploy_TclaudeLayerDeploysWhenHostSupportsIt(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "layered", "harness": "claude", "sandbox_implementation": "tclaude-layer",
	}).Code, "create profile")

	require.Equalf(t, http.StatusCreated, humanReq(t, f, http.MethodPost, "/v1/templates", map[string]any{
		"name": "team",
		"agents": []map[string]any{
			{"name": "worker", "role": "dev", "spawn_profile": "layered"},
		},
	}).Code, "create template")

	rec := humanReq(t, f, http.MethodPost, "/v1/templates/team/deploy", map[string]any{
		"group_name": "phoenix",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "deploy: %s", rec.Body.String())
	var res instantiateResult
	testharness.DecodeJSON(t, rec, &res)
	agentd.WaitForBackgroundForTest()

	require.Equalf(t, 1, res.Spawned, "the member should deploy: %+v", res.Agents)
	require.Equal(t, 0, res.Failed)
	got, ok := f.World.SpawnSandboxImplementation(res.Agents[0].ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Equal(t, "tclaude-layer", got,
		"a template member's pinned implementation must reach the launch")
	assertSandboxLayerCalls(t, f, testharness.SandboxLayerInteractive)
}
