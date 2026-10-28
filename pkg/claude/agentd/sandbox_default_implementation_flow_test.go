package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func setSandboxImplDefault(t *testing.T, f *testharness.Flow, value string) {
	t.Helper()
	rec := profileReq(t, f, http.MethodPut, "/v1/sandbox-impl-default", map[string]any{"value": value})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// The install default is the lowest tier: below every spawn profile, above the
// harness default. Undecided and an explicit "harness" both leave the harness in
// charge, so an upgraded install resolves exactly as before.
func TestSandboxImplInstallDefaultIsTheLowestTier(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateAgentGroup("crew", "")
	require.NoError(t, err)

	undecided := getLaunchDefaults(t, f, "?group=crew")
	assert.Equal(t, "harness-builtin", undecided.Implementation)
	assert.Equal(t, "harness default", undecided.ResolvedBy)

	setSandboxImplDefault(t, f, "tclaude-layer")
	layered := getLaunchDefaults(t, f, "?group=crew")
	assert.Equal(t, "tclaude-layer", layered.Implementation)
	assert.Contains(t, layered.ResolvedBy, "install default")

	// Every registered harness can host the layer, so the default reaches them
	// all — including OpenCode, whose unset posture is only a command filter.
	opencode := getLaunchDefaults(t, f, "?group=crew&harness=opencode")
	assert.Equal(t, "tclaude-layer", opencode.Implementation)

	// Any profile tier that sets the field outranks the install default.
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{
		Name: "house", Harness: "claude", SandboxImplementation: "harness-builtin",
	})
	require.NoError(t, err)
	rec := profileReq(t, f, http.MethodPut, "/v1/spawn-profile-default", map[string]any{"name": "house"})
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	profiled := getLaunchDefaults(t, f, "?group=crew")
	assert.Equal(t, "harness-builtin", profiled.Implementation)
	assert.Contains(t, profiled.ResolvedBy, `global default profile "house"`)
	rec = profileReq(t, f, http.MethodDelete, "/v1/spawn-profile-default", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	setSandboxImplDefault(t, f, "harness")
	kept := getLaunchDefaults(t, f, "?group=crew")
	assert.Equal(t, "harness-builtin", kept.Implementation)
	assert.Equal(t, "harness default", kept.ResolvedBy)
}

func TestSandboxImplInstallDefaultEndpoint(t *testing.T) {
	f := newFlow(t)
	read := func() map[string]any {
		rec := profileReq(t, f, http.MethodGet, "/v1/sandbox-impl-default", nil)
		require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		var got map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got
	}
	assert.Equal(t, map[string]any{"value": "", "decided": false}, read())

	rec := profileReq(t, f, http.MethodPut, "/v1/sandbox-impl-default", map[string]any{"value": "off"})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "only tclaude-layer and harness are install defaults")

	setSandboxImplDefault(t, f, "harness-builtin")
	assert.Equal(t, map[string]any{"value": "harness", "decided": true}, read(),
		"harness-builtin is the operator's word for harness, stored as such")

	rec = profileReq(t, f, http.MethodDelete, "/v1/sandbox-impl-default", nil)
	require.Equalf(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, false, read()["decided"])
}

// Nobody typed the install default at this spawn, so when it is what put the
// layer on a host that cannot run it, the refusal stands (no silent downgrade)
// but must say where the value came from and how to get out.
func TestSpawn_InstallDefaultRefusalNamesTheWayOut(t *testing.T) {
	f := newFlow(t)
	f.World.SandboxLayer.SetAvailability(testharness.SandboxLayerInteractive, errNoBwrap)
	f.HaveGroup("crew")
	setSandboxImplDefault(t, f, "tclaude-layer")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{"name": "doomed"})
	require.Equalf(t, http.StatusUnprocessableEntity, resp.Code, "body=%s", resp.Raw)
	failure := decodeFailure(t, resp.Raw)
	assert.Equal(t, "sandbox_implementation_unavailable", failure.Code)
	assert.Contains(t, failure.Error, sandboxLayerUnavailableMessage(errNoBwrap))
	assert.Contains(t, failure.Error, "tclaude agent sandbox-impl default set harness")
}

func TestSpawn_InstallDefaultConfinesANewAgent(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetTclaudeLayerHostAvailabilityForTest(func() error { return nil }))
	f.HaveGroup("crew")
	setSandboxImplDefault(t, f, "tclaude-layer")

	resp := f.AsHuman().SpawnWith("crew", map[string]any{"name": "walled"})
	require.Equalf(t, http.StatusOK, resp.Code, "body=%s", resp.Raw)
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
	assert.Equal(t, "install default", parsed.Resolved.SandboxImpl.Source)
	got, ok := f.World.SpawnSandboxImplementation(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "tclaude-layer", got)
}

// The default decides how every future agent is confined, so an agent without
// sandbox-profiles.manage — group owners included — can read it but not change it.
func TestSandboxImplInstallDefaultWritesNeedTheManageSlug(t *testing.T) {
	f := newFlow(t)
	const conv = "0f0e0d0c-0b0a-4908-8706-0504030201d1"
	f.HaveGroup("crew")
	f.HaveAliveSession(conv, "worker", "worker-pane", t.TempDir())
	f.HaveMember("crew", conv)

	asAgent := func(method string, body any) int {
		return testharness.Serve(f.Mux, agentd.AsAgentPeer(
			testharness.JSONRequest(t, method, "/v1/sandbox-impl-default", body), conv)).Code
	}
	assert.Equal(t, http.StatusOK, asAgent(http.MethodGet, nil))
	assert.Equal(t, http.StatusForbidden, asAgent(http.MethodPut, map[string]any{"value": "harness"}))
	assert.Equal(t, http.StatusForbidden, asAgent(http.MethodDelete, nil))
	_, decided, err := db.GetSandboxDefaultImplementation()
	require.NoError(t, err)
	assert.False(t, decided, "a refused write must not record a decision")
}
