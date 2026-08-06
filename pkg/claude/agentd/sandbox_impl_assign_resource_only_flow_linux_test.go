//go:build linux

// resource-only is Linux-only: sandboxImplementationHostFailure refuses it off
// Linux rather than degrading to `off`, so the assignment scenarios that drive
// it live here alongside the resource-only spawn flows.

package agentd_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// stubResourceCgroup swaps the boundary a flow test cannot create for real. The
// assignment probes it before recording a posture that needs one, so without
// this every scenario here would depend on the CI host's cgroup delegation.
func stubResourceCgroup(t *testing.T, err error) {
	t.Helper()
	t.Cleanup(agentd.SetPrepareResourceCgroupForTest(
		func(string, sandboxpolicy.ResourceLimits) (string, func(), error) {
			if err != nil {
				return "", func() {}, err
			}
			return t.TempDir(), func() {}, nil
		}))
}

// The motivating case: an agent spawned before `resource-only` existed is moved
// onto it, and its next launch runs under the implementation whose only
// enforcement is the per-agent cgroup.
func TestSandboxImplAssign_ResourceOnlyReachesAPreexistingAgentsRelaunch(t *testing.T) {
	f := newFlow(t)
	stubResourceCgroup(t, nil)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":    "predates-cgroups",
		"sandbox": harness.ClaudeSandboxOn,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	wire, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationResourceOnly)})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin), wire.Previous)
	assert.Equal(t, string(sandboxpolicy.ImplementationResourceOnly), wire.Implementation)
	assert.True(t, wire.ResourceCgroup,
		"the implementation asks for the boundary even with no ceiling authored")
	assert.Equal(t, harness.ClaudeSandboxOff, wire.Sandbox,
		"resource-only stands the harness's own wall down, so the recorded mode "+
			"must move with it rather than keep claiming `on`")

	f.AsHuman().Resume(spawn.ConvID)
	relaunched, ok := f.World.SpawnSandboxImplementation(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, string(sandboxpolicy.ImplementationResourceOnly), relaunched)

	row, err := db.FindSessionByConvID(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, string(sandboxpolicy.ImplementationResourceOnly), row.SandboxImplementation)
}

// A ceiling already authored in the agent's chain becomes enforceable the moment
// the implementation that carries it is assigned, so the assignment must arrive
// with it rather than leaving the limits inert.
func TestSandboxImplAssign_ResourceOnlyCarriesAnAlreadyAuthoredCeiling(t *testing.T) {
	f := newFlow(t)
	stubResourceCgroup(t, nil)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:           "assigned-limits",
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "8GiB"},
	})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":            "gets-a-ceiling",
		"sandbox_profile": "assigned-limits",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	_, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationResourceOnly)})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)

	f.AsHuman().Resume(spawn.ConvID)
	policy, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, policy)
	assert.Equal(t, "8GiB", policy.Effective.ResourceLimits.Memory,
		"the relaunch must carry the ceiling the assigned implementation enforces")
}

// The refusal that has to happen HERE. A launch refuses a cgroup it cannot
// create only when the operator is choosing the posture right then; the launch
// this assignment takes effect on is a relaunch, which deliberately degrades to
// a notice instead. Without this probe the operator would record a boundary,
// wake the agent, and get nothing but a dashboard notice.
func TestSandboxImplAssign_RefusesWhenTheHostCannotCreateTheCgroup(t *testing.T) {
	f := newFlow(t)
	stubResourceCgroup(t, errors.New("no delegated cgroup v2 subtree"))
	f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "undelegated-host")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	_, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationResourceOnly)})
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, body, "no delegated cgroup v2 subtree",
		"the refusal must name what the host is missing, not just that it failed")

	assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin),
		showSandboxImpl(t, f, spawn.ConvID).Implementation,
		"a boundary that cannot be created must not be recorded as if it could")
}

// An implementation with no cgroup in it must not pay for the probe, or a host
// with no delegation could not use the endpoint at all.
func TestSandboxImplAssign_SkipsTheCgroupProbeWhenNoBoundaryIsAsked(t *testing.T) {
	f := newFlow(t)
	stubResourceCgroup(t, errors.New("this host has no delegation at all"))
	f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "plain-agent")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	wire, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationOff)})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.False(t, wire.ResourceCgroup)
}
