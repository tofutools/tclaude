package agentd_test

import (
	"encoding/json"
	"net/http"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
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

// availableHost / unavailableHost drive the host-capability predicate. Neither
// branch is reachable on demand otherwise: CI runners have no unprivileged user
// namespaces and would only ever produce the refusal, while a dev box with
// bwrap would only ever produce the success.
func availableHost(t *testing.T) {
	t.Helper()
	t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return nil }))
}

func unavailableHost(t *testing.T, reason error) {
	t.Helper()
	t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return reason }))
}

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
}

// TestSpawn_ExplicitTclaudeLayerReachesLaunch: the opt-in works and arrives
// intact at the launch boundary, which is where it becomes --sandbox-impl.
func TestSpawn_ExplicitTclaudeLayerReachesLaunch(t *testing.T) {
	f := newFlow(t)
	availableHost(t)
	f.HaveGroup("crew")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "layered",
		"sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, 200, resp.Code, "explicit tclaude-layer spawn; body=%s", resp.Raw)

	got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
	require.True(t, ok, "the spawn should have been observed by the sim spawner")
	assert.Equal(t, "tclaude-layer", got)
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
			availableHost(t)
			f.HaveGroup("crew")
			tc.wire(t, f)

			resp := f.AsHuman().SpawnWith("crew", tc.body)
			require.Equalf(t, 200, resp.Code, "spawn; body=%s", resp.Raw)

			got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
			require.True(t, ok, "the spawn should have been observed by the sim spawner")
			assert.Equal(t, "tclaude-layer", got,
				"a profile's sandbox implementation must reach the launch")
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
	availableHost(t)
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

func TestSpawn_ExplicitTclaudeLayerWrapsOpenCodeExecutor(t *testing.T) {
	f := newFlow(t)
	availableHost(t)
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
}

func TestSpawn_OpenCodeModeAndImplementationContradictionsRefuse(t *testing.T) {
	f := newFlow(t)
	availableHost(t)
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
	availableHost(t)
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
		assert.Contains(t, string(resp.Raw), "does not support OpenCode on macOS",
			"the platform-driven tier skip must be disclosed")
		return
	}
	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer), got)
	mode, ok := f.World.SpawnSandbox(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, harness.OpenCodeSandboxTclaudeLayer, mode)
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

// TestSpawn_TclaudeLayerRefusedWhenHostLacksCapability: refuse-don't-degrade.
// The refusal names the missing capability so the operator can fix it, and says
// out loud that it is not falling back — the distinction that separates this
// from malformed or contradictory launch fields.
func TestSpawn_TclaudeLayerRefusedWhenHostLacksCapability(t *testing.T) {
	f := newFlow(t)
	unavailableHost(t, errNoBwrap)
	f.HaveGroup("crew")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{
		"name": "doomed", "sandbox_implementation": "tclaude-layer",
	})
	require.Equal(t, http.StatusUnprocessableEntity, resp.Code,
		"a host that cannot run the layer must refuse the spawn; body=%s", resp.Raw)
	failure := decodeFailure(t, resp.Raw)
	assert.Equal(t, "sandbox_implementation_unavailable", failure.Code,
		"the refusal kind must distinguish host-unavailable from an inapplicable value")
	assert.Contains(t, failure.Error, errNoBwrap.Error(),
		"the refusal must name the concrete missing capability")
	assert.Contains(t, failure.Error, "rather than falling back",
		"the refusal must say it is not degrading to harness-builtin")
}

// TestSpawn_TclaudeLayerFromProfileStillRefusedWhenHostLacksCapability: the
// case the design note singles out. The value arrived from a group default
// profile rather than from this request, and it must STILL refuse. If host
// availability rode the per-harness validator instead of its own gate, this is
// the spawn that would have silently resolved to harness-builtin.
func TestSpawn_TclaudeLayerFromProfileStillRefusedWhenHostLacksCapability(t *testing.T) {
	f := newFlow(t)
	unavailableHost(t, errNoBwrap)
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
	assert.Contains(t, failure.Error, errNoBwrap.Error(),
		"the refusal must name the concrete missing capability")
}

// TestSpawn_ResolvedLaunchEchoesSandboxImplementationSource: an agent whose wall
// was chosen by a profile it never named must be able to see that. The echo
// names the tier, not just the value.
func TestSpawn_ResolvedLaunchEchoesSandboxImplementationSource(t *testing.T) {
	f := newFlow(t)
	availableHost(t)
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
	unavailableHost(t, errNoBwrap)

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
}

// TestTaskForceDeploy_TclaudeLayerDeploysWhenHostSupportsIt is the positive
// half: the same template on a capable host reaches the launch with the layer
// intact, so the gate above is refusing for the right reason rather than
// blocking template deploys outright.
func TestTaskForceDeploy_TclaudeLayerDeploysWhenHostSupportsIt(t *testing.T) {
	f := newFlow(t)
	availableHost(t)

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
}
