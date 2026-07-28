package agentd_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// errNoBwrap stands in for the real host-capability failure. The production
// text comes from resolveBwrapBinary and names the missing capability; what the
// tests assert is that whatever it says is carried through to the operator
// verbatim rather than collapsed into a generic "unavailable".
var errNoBwrap = errors.New("tclaude-layer requires bubblewrap (`bwrap`) on PATH: exec: \"bwrap\": not found")

// Scenario (TCL-769): the profile side of the spawn-implementation surface —
// saving, reading back, and moving a pinned implementation between machines.
//
// The rule that governs every test here: a profile records AUTHORING INTENT,
// not a launch. So the save boundary validates the value and the harness, but
// deliberately does NOT probe the host — pinning tclaude-layer on a laptop
// where bwrap is not installed yet is legitimate, and the launch is what
// refuses.

// TestSpawnProfile_SandboxImplementationRoundTrips: the basic CRUD contract.
func TestSpawnProfile_SandboxImplementationRoundTrips(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name": "layered", "harness": "claude", "sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	got := readProfile(t, f, "layered")
	assert.Equal(t, "tclaude-layer", got["sandbox_implementation"])

	// Update clears it back to unset. The key disappears entirely (omitempty),
	// which is what "this profile pins nothing" looks like on the wire.
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPatch, "/v1/spawn-profiles/layered",
		map[string]any{"name": "layered", "harness": "claude", "sandbox_implementation": ""}))
	rec = testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusOK, rec.Code, "update profile body=%s", rec.Body.String())

	got = readProfile(t, f, "layered")
	_, present := got["sandbox_implementation"]
	assert.False(t, present, "a cleared implementation must read back as absent, not as harness-builtin")
}

// TestSpawnProfile_UnsetSandboxImplementationIsNotAPin: the invariant the
// migration is built around. A profile that never mentions the field must stay
// silent so lower precedence tiers still speak; if saving normalized "" to
// harness-builtin, every profile in existence would quietly become an override.
func TestSpawnProfile_UnsetSandboxImplementationIsNotAPin(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{"name": "plain", "harness": "claude"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	got := readProfile(t, f, "plain")
	_, present := got["sandbox_implementation"]
	assert.False(t, present,
		"a profile that never mentioned the field must not read back as pinning harness-builtin")
}

// TestSpawnProfile_RejectsUnknownSandboxImplementation: a typo is caught at
// save, where the operator is looking, rather than at some later spawn.
func TestSpawnProfile_RejectsUnknownSandboxImplementation(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name": "typo", "harness": "claude", "sandbox_implementation": "bubblewrap",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"an unknown implementation must be refused at save; body=%s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid sandbox implementation")
}

func TestSpawnProfile_RejectsOpenCodeHarnessBuiltinAtAuthoring(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name": "false-wall", "harness": "opencode",
		"sandbox_implementation": "harness-builtin",
	})
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code,
		"the inapplicable pair must fail at profile save; body=%s", rec.Body.String())
	failure := decodeFailure(t, rec.Body.Bytes())
	assert.Equal(t, "invalid_sandbox_implementation", failure.Code)
	assert.Contains(t, failure.Error,
		"OpenCode has no built-in OS sandbox; its access-control mode is a command filter, not confinement")
}

func TestSpawnProfile_StackedRoundTripsAndOpenCodeRefuses(t *testing.T) {
	f := newFlow(t)
	for _, harnessName := range []string{"claude", "codex"} {
		name := harnessName + "-stacked"
		rec := createProfile(t, f, map[string]any{
			"name": name, "harness": harnessName, "sandbox_implementation": "stacked",
		})
		require.Equalf(t, http.StatusCreated, rec.Code,
			"%s stacked profile body=%s", harnessName, rec.Body.String())
		assert.Equal(t, "stacked", readProfile(t, f, name)["sandbox_implementation"])
	}
	rec := createProfile(t, f, map[string]any{
		"name": "opencode-stacked", "harness": "opencode", "sandbox_implementation": "stacked",
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "missing capability stacked_inner_harness_sandbox")
	assert.Contains(t, rec.Body.String(), "refusing rather than falling back")
}

// OpenCode's authoritative server is now the process inside the outer layer,
// so profile authoring accepts the same implementation pin as the pane-owned
// harnesses. Host capability remains a launch-time concern.
func TestSpawnProfile_AcceptsTclaudeLayerForOpenCodeExecutor(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name": "oc-layered", "harness": "opencode", "sandbox_implementation": "tclaude-layer",
	})
	require.Equal(t, http.StatusCreated, rec.Code,
		"OpenCode executor-layer intent must be accepted at save; body=%s", rec.Body.String())
	got := readProfile(t, f, "oc-layered")
	assert.Equal(t, "tclaude-layer", got["sandbox_implementation"])
}

// TestSpawnProfile_SandboxImplementationSavesOnHostWithoutCapability: the
// deliberate asymmetry. Authoring is not launching, so a host that cannot run
// the layer must still be able to author a profile that pins it — for a
// different machine, or for after bwrap is installed. The save boundary
// therefore runs no host probe at all; a failing one here would be a bug.
func TestSpawnProfile_SandboxImplementationSavesOnHostWithoutCapability(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return errNoBwrap }))

	rec := createProfile(t, f, map[string]any{
		"name": "for-the-other-box", "harness": "claude", "sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusCreated, rec.Code,
		"authoring a profile must not require the host to be able to run the layer; body=%s", rec.Body.String())

	got := readProfile(t, f, "for-the-other-box")
	assert.Equal(t, "tclaude-layer", got["sandbox_implementation"])
}

// TestSpawnProfile_SandboxImplementationSurvivesExportImport: the field rides
// the export envelope, so a pinned implementation moves between machines with
// the rest of the profile instead of being silently dropped on import.
func TestSpawnProfile_SandboxImplementationSurvivesExportImport(t *testing.T) {
	f := newFlow(t)

	rec := createProfile(t, f, map[string]any{
		"name": "layered", "harness": "claude", "sandbox_implementation": "tclaude-layer",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodGet, "/v1/spawn-profiles/export?name=layered", nil))
	rec = testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusOK, rec.Code, "export body=%s", rec.Body.String())

	var envelope map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	profiles, _ := envelope["profiles"].([]any)
	require.Len(t, profiles, 1, "export should carry the one requested profile")
	exported, _ := profiles[0].(map[string]any)
	require.Equal(t, "tclaude-layer", exported["sandbox_implementation"],
		"the export must carry the pinned implementation")

	// Re-import under a new name and confirm the pin survived the round trip.
	exported["name"] = "layered-copy"
	envelope["profiles"] = []any{exported}
	r = agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/spawn-profiles/import", envelope))
	rec = testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusOK, rec.Code, "import body=%s", rec.Body.String())

	got := readProfile(t, f, "layered-copy")
	assert.Equal(t, "tclaude-layer", got["sandbox_implementation"],
		"an imported profile must keep the implementation it was exported with")
}

// TestDashboardSnapshot_SandboxImplCatalogDisclosesHostAvailability: the dialog
// gets its answer from the snapshot, and the answer has to be honest in both
// directions — including naming the concrete missing capability, so the
// operator can act on it rather than guess.
func TestDashboardSnapshot_SandboxImplCatalogDisclosesHostAvailability(t *testing.T) {
	t.Run("unavailable", func(t *testing.T) {
		f := newFlow(t)
		t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return errNoBwrap }))

		catalog := snapshotSandboxImpl(t, f)
		assert.Equal(t, false, catalog["host_available"])
		assert.Equal(t, errNoBwrap.Error(), catalog["host_unavailable_reason"],
			"the disclosure must name the concrete missing capability")
		assert.Equal(t, false, catalog["server_host_available"])
		assert.Equal(t, errNoBwrap.Error(), catalog["server_host_unavailable_reason"])
	})

	t.Run("available", func(t *testing.T) {
		f := newFlow(t)
		t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return nil }))

		catalog := snapshotSandboxImpl(t, f)
		assert.Equal(t, true, catalog["host_available"])
		assert.Equal(t, true, catalog["server_host_available"])
		_, present := catalog["host_unavailable_reason"]
		assert.False(t, present, "an available host must carry no reason")
		_, present = catalog["server_host_unavailable_reason"]
		assert.False(t, present, "an available server boundary must carry no reason")
	})

	t.Run("server boundary is disclosed independently", func(t *testing.T) {
		f := newFlow(t)
		errPidfd := errors.New("pidfd unavailable")
		t.Cleanup(agentd.SetTclaudeLayerHostAvailabilitiesForTest(
			func() error { return errPidfd },
			func() error { return nil },
		))

		catalog := snapshotSandboxImpl(t, f)
		assert.Equal(t, false, catalog["host_available"])
		assert.Equal(t, errPidfd.Error(), catalog["host_unavailable_reason"])
		assert.Equal(t, true, catalog["server_host_available"])
		_, present := catalog["server_host_unavailable_reason"]
		assert.False(t, present)
	})

	// The AppArmor hint is the one disclosure that must fire while everything
	// else looks healthy: per-harness stacked availability resolves the engine
	// and reports available on exactly the hosts whose policy denies the nested
	// wall. So the flag is asserted independently of availability, in both
	// directions, and stays absent when the shape is not there.
	t.Run("a likely AppArmor nested-bwrap block is disclosed", func(t *testing.T) {
		f := newFlow(t)
		t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return nil }))
		t.Cleanup(agentd.SetStackedAppArmorLikelyForTest(true))

		catalog := snapshotSandboxImpl(t, f)
		assert.Equal(t, true, catalog["host_available"],
			"the heuristic must not be confused with an unavailable host")
		assert.Equal(t, true, catalog["stacked_apparmor_nested_bwrap_likely"])
	})

	t.Run("no AppArmor claim on a host without the policy", func(t *testing.T) {
		f := newFlow(t)
		t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return nil }))
		t.Cleanup(agentd.SetStackedAppArmorLikelyForTest(false))

		catalog := snapshotSandboxImpl(t, f)
		_, present := catalog["stacked_apparmor_nested_bwrap_likely"]
		assert.False(t, present, "silence, rather than a false claim, is the default")
	})

	t.Run("options label the experimental layer", func(t *testing.T) {
		f := newFlow(t)
		t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return nil }))

		catalog := snapshotSandboxImpl(t, f)
		assert.Equal(t, "harness-builtin", catalog["default"],
			"the default must remain the legacy implementation")
		options, _ := catalog["options"].([]any)
		require.Len(t, options, 3)
		var sawExperimental, sawStacked, sawBuiltin bool
		for _, raw := range options {
			option, _ := raw.(map[string]any)
			if option["value"] == "harness-builtin" {
				sawBuiltin = true
				// The renderer fills {harness} with the selected harness's
				// display name, so the option reads "Claude Code built-in"
				// rather than a generic "harness" the operator could mistake
				// for tclaude itself.
				assert.Equal(t, "{harness} built-in", option["label"])
				assert.Contains(t, option["descr"], "{harness} owns OS-level containment")
				continue
			}
			if option["value"] == "stacked" {
				sawStacked = true
				assert.Equal(t, "Stacked: tclaude + {harness} (experimental)", option["label"])
				assert.Equal(t, true, option["experimental"])
				continue
			}
			if option["value"] != "tclaude-layer" {
				continue
			}
			sawExperimental = true
			assert.Equal(t, true, option["experimental"])
			assert.Contains(t, option["label"], "experimental",
				"the label itself must carry the caveat, not only the flag")
			assert.Contains(t, option["descr"], "Linux only",
				"the platform caveat must be stated, not implied")
		}
		assert.True(t, sawExperimental, "the catalog must offer the tclaude layer")
		assert.True(t, sawStacked, "the catalog must always offer stacked")
		assert.True(t, sawBuiltin, "the catalog must always offer the harness-owned option")
	})
}

// snapshotSandboxImpl reads the catalog off the real dashboard snapshot. /api/*
// lives on the dashboard mux, not the /v1 Unix-socket mux.
func snapshotSandboxImpl(t *testing.T, _ *testharness.Flow) map[string]any {
	t.Helper()
	dash := agentd.BuildDashboardHandlerForTest()
	rec := testharness.Serve(dash, dashReq(t, http.MethodGet, "/api/snapshot", nil))
	require.Equalf(t, http.StatusOK, rec.Code, "snapshot body=%s", rec.Body.String())
	var snapshot map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snapshot))
	catalog, ok := snapshot["sandbox_impl"].(map[string]any)
	require.True(t, ok, "the snapshot must carry the sandbox-implementation catalog")
	return catalog
}
