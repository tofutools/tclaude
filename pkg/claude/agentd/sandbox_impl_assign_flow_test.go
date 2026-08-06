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

// Restoring `harness-builtin` must not quietly land on a posture with no
// confinement. The mode recorded for a no-confinement implementation is one that
// implementation FORCED, so carrying it into harness-builtin would pair the
// implementation that means "the harness confines this agent" with the mode that
// means "it does not" — under the command an operator issues to undo their
// change.
func TestSandboxImplAssign_RestoringHarnessBuiltinDoesNotInheritTheForcedOffMode(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":    "round-trip",
		"sandbox": harness.ClaudeSandboxOn,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	unconfined, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationOff)})
	require.Equalf(t, http.StatusOK, code, "assign off body=%s", body)
	require.Equal(t, harness.ClaudeSandboxOff, unconfined.Sandbox)

	restored, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationHarnessBuiltin)})
	require.Equalf(t, http.StatusOK, code, "assign harness-builtin body=%s", body)
	assert.Equal(t, harness.ClaudeSandboxInherit, restored.Sandbox,
		"with no mode ever chosen for this posture, the harness default is the "+
			"honest answer — not the off mode the replaced implementation forced")
	assert.NotEqual(t, harness.ClaudeSandboxOff, restored.Sandbox)

	// An explicit mode is still the operator's to pick.
	pinned, code, body := assignSandboxImpl(t, f, spawn.ConvID, map[string]any{
		"implementation": string(sandboxpolicy.ImplementationHarnessBuiltin),
		"sandbox":        harness.ClaudeSandboxOn,
	})
	require.Equalf(t, http.StatusOK, code, "assign pinned body=%s", body)
	assert.Equal(t, harness.ClaudeSandboxOn, pinned.Sandbox)
}

// The dashboard picker reaches the same operation over the cookie-authenticated
// route. Its GET is what the dialog renders from, and it must report the DURABLE
// posture: the row it is opened from carries the last launch's implementation,
// which is a different question and diverges the moment an assignment lands.
func TestSandboxImplAssign_DashboardRouteReadsAndAssigns(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":    "dash-target",
		"sandbox": harness.ClaudeSandboxOn,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	dash := agentd.BuildDashboardHandlerForTest()
	path := "/api/agents/" + spawn.ConvID + "/sandbox-impl"

	read := testharness.Serve(dash, dashReq(t, http.MethodGet, path, nil))
	require.Equalf(t, http.StatusOK, read.Code, "GET body=%s", read.Body.String())
	var before sandboxImplWire
	testharness.DecodeJSON(t, read, &before)
	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin), before.Implementation)
	assert.Equal(t, harness.ClaudeSandboxOn, before.Sandbox)
	assert.False(t, before.Online)

	write := testharness.Serve(dash, dashReq(t, http.MethodPost, path,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationOff)}))
	require.Equalf(t, http.StatusOK, write.Code, "POST body=%s", write.Body.String())
	var after sandboxImplWire
	testharness.DecodeJSON(t, write, &after)
	assert.Equal(t, string(sandboxpolicy.ImplementationOff), after.Implementation)
	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin), after.Previous)

	// The recorded session row still describes the OLD launch, which is exactly
	// why the dialog reads this endpoint rather than the row.
	row, err := db.FindSessionByConvID(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin), row.SandboxImplementation)
	assert.Equal(t, string(sandboxpolicy.ImplementationOff),
		showSandboxImpl(t, f, spawn.ConvID).Implementation)

	f.AsHuman().Resume(spawn.ConvID)
	relaunched, ok := f.World.SpawnSandboxImplementation(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, string(sandboxpolicy.ImplementationOff), relaunched)
}

// The dashboard route refuses a running agent for the same reason the CLI does —
// the button is disabled in the UI, but a stale page must not be able to record a
// posture the live process does not have.
func TestSandboxImplAssign_DashboardRouteRefusesWhileOnline(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "dash-live")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		dashReq(t, http.MethodPost, "/api/agents/"+spawn.ConvID+"/sandbox-impl",
			map[string]any{"implementation": string(sandboxpolicy.ImplementationOff)}))
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "agent_online")
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
