package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The sandbox IMPLEMENTATION used to be frozen at birth, so an agent spawned
// before an implementation existed could never reach it. These scenarios drive
// the assignment through the daemon mux, because the value only matters if it
// survives into the LAUNCH the operator is trying to change — the durable write
// on its own proves nothing.

// sandboxImplWire mirrors the daemon's sandbox-impl payload.
type sandboxImplWire struct {
	ConvID           string `json:"conv_id"`
	AgentID          string `json:"agent_id"`
	Harness          string `json:"harness"`
	Implementation   string `json:"sandbox_implementation"`
	Previous         string `json:"previous_sandbox_implementation"`
	Sandbox          string `json:"sandbox"`
	Source           string `json:"sandbox_source"`
	TemporarySandbox bool   `json:"temporary_sandbox_active"`
	Online           bool   `json:"online"`
	ResourceCgroup   bool   `json:"resource_cgroup"`
}

func assignSandboxImpl(
	t *testing.T, f *testharness.Flow, convID string, body map[string]any,
) (sandboxImplWire, int, string) {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodPost,
			"/v1/agent/"+convID+"/sandbox-impl", body)))
	var wire sandboxImplWire
	if rec.Code == http.StatusOK {
		testharness.DecodeJSON(t, rec, &wire)
	}
	return wire, rec.Code, rec.Body.String()
}

func showSandboxImpl(t *testing.T, f *testharness.Flow, convID string) sandboxImplWire {
	t.Helper()
	rec := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodGet,
			"/v1/agent/"+convID+"/sandbox-impl", nil)))
	require.Equalf(t, http.StatusOK, rec.Code, "show body=%s", rec.Body.String())
	var wire sandboxImplWire
	testharness.DecodeJSON(t, rec, &wire)
	return wire
}

// The core claim: an assignment made while the agent is stopped is the posture
// its next launch runs under. Anything less and the feature does nothing.
func TestSandboxImplAssign_OfflineAgentRelaunchesUnderTheNewImplementation(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "legacy-agent")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	require.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin),
		showSandboxImpl(t, f, spawn.ConvID).Implementation,
		"an ordinary spawn is born on the compatibility default")

	f.AsHuman().Stop(spawn.ConvID, false)
	wire, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationOff)})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin), wire.Previous)
	assert.Equal(t, string(sandboxpolicy.ImplementationOff), wire.Implementation)
	assert.Equal(t, db.AssignedSandboxImplementationSource, wire.Source,
		"the recorded mode is no longer the one any spawn tier chose")
	assert.Equal(t, harness.ClaudeSandboxOff, wire.Sandbox,
		"an implementation that omits OS confinement carries the harness's own off mode")

	f.AsHuman().Resume(spawn.ConvID)
	relaunched, ok := f.World.SpawnSandboxImplementation(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, string(sandboxpolicy.ImplementationOff), relaunched,
		"the assignment is only real if the relaunch carries it")

	row, err := db.FindSessionByConvID(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, string(sandboxpolicy.ImplementationOff), row.SandboxImplementation,
		"the relaunched row records the assigned implementation for the launch after it")
}

// A live pane cannot be moved onto a new implementation, and recording one
// anyway would leave the durable posture asserting containment the running
// process does not have.
func TestSandboxImplAssign_RefusesWhileTheAgentIsRunning(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "running-agent")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	_, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationOff)})
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, body, "agent_online")

	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin),
		showSandboxImpl(t, f, spawn.ConvID).Implementation,
		"the refused assignment must not have written anything")
}

// The temporary dashboard unlock preserves the normal posture underneath so
// restore can put it back exactly. Writing that normal posture from under an
// active override would silently change what restore restores.
func TestSandboxImplAssign_RefusesUnderTheTemporarySandboxUnlock(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":    "unlocked-agent",
		"sandbox": harness.ClaudeSandboxOn,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.SetSessionStatus(spawn.ConvID, "idle")
	unlock := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		dashReq(t, http.MethodPost,
			"/api/agents/"+spawn.ConvID+"/sandbox-restart",
			map[string]any{"action": "unlock"}))
	require.Equalf(t, http.StatusOK, unlock.Code, "unlock body=%s", unlock.Body.String())

	f.AsHuman().Stop(spawn.ConvID, false)
	_, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationOff)})
	assert.Equal(t, http.StatusConflict, code)
	assert.Contains(t, body, "temporary_sandbox_override")
}

// An unknown value is a bad request, not a silent fallback to the default
// implementation — the whole point of the endpoint is that the recorded value
// is the one the operator chose.
func TestSandboxImplAssign_RefusesAnUnknownImplementation(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "typo-agent")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	_, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": "resource_only"})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, body, "invalid_sandbox_implementation")

	_, emptyCode, emptyBody := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": "  "})
	assert.Equal(t, http.StatusBadRequest, emptyCode)
	assert.Contains(t, emptyBody, "implementation is required")
}

// The slug is not owner-implied on purpose: the assignment can move an agent
// onto an implementation with no access confinement at all, which is the
// boundary the sandbox-lineage guard protects. Owning the group the target
// belongs to must not be enough.
func TestSandboxImplAssign_GroupOwnershipDoesNotConferTheSlug(t *testing.T) {
	f := newFlow(t)
	g := f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "worker-agent")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	const lead = "lead-aaaa-bbbb-cccc-dddd"
	f.HaveConvWithTitle(lead, "lead")
	f.HaveMember("crew", lead)
	require.NoError(t, db.AddAgentGroupOwner(g.ID, lead, "test"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost,
			"/v1/agent/"+spawn.ConvID+"/sandbox-impl",
			map[string]any{"implementation": string(sandboxpolicy.ImplementationOff)}), lead))
	assert.Equal(t, http.StatusForbidden, rec.Code,
		"an owner is not automatically an operator of the host's sandbox policy")
	assert.Contains(t, rec.Body.String(), agentd.PermAgentSandboxImplementation)

	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin),
		showSandboxImpl(t, f, spawn.ConvID).Implementation)
}
