//go:build linux

// The cgroup a tclaude-layer launch asks for is Linux-only — off Linux the
// layer is Seatbelt with no cgroup v2 beneath it — so the assignment and
// dashboard scenarios that turn on it live here. stubResourceCgroup is shared
// with the resource-only assignment flows in this package.

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
)

// The counterpart of TestSandboxImplAssign_RefusesWhenTheHostCannotCreateTheCgroup.
// resource-only is refused there because a relaunch with no cgroup is the whole
// implementation gone; tclaude-layer relaunches with its wall intact and loses
// only counters, so refusing the assignment would deny the operator the
// confinement they came for over a bonus.
func TestSandboxImplAssign_TclaudeLayerSurvivesAHostWithNoCgroupDelegation(t *testing.T) {
	f := newFlow(t)
	stubResourceCgroup(t, errors.New("no delegated cgroup v2 subtree"))
	f.HaveGroup("crew")

	spawn := f.AsHuman().Spawn("crew", "undelegated-layer-host")
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	wire, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationTclaudeLayer)})
	require.Equalf(t, http.StatusOK, code, "assign body=%s", body)
	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer), wire.Implementation)
	assert.True(t, wire.ResourceCgroup,
		"the launch will try for the boundary; whether this host can provide one is "+
			"the launch's disclosure to make, not a reason to withhold the posture")

	assert.Equal(t, string(sandboxpolicy.ImplementationTclaudeLayer),
		showSandboxImpl(t, f, spawn.ConvID).Implementation)
}

// A ceiling in the chain is a different matter under the same implementation:
// the relaunch fails closed on it, and the dashboard's allow-unenforced control
// is a fresh-spawn control the operator could not reach afterwards. That
// refusal has to survive the widened gate.
func TestSandboxImplAssign_TclaudeLayerStillRefusesAnUncreatableCeiling(t *testing.T) {
	f := newFlow(t)
	stubResourceCgroup(t, errors.New("no delegated cgroup v2 subtree"))
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:           "layer-ceiling",
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "8GiB"},
	})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":            "layer-with-ceiling",
		"sandbox_profile": "layer-ceiling",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	f.AsHuman().Stop(spawn.ConvID, false)

	_, code, body := assignSandboxImpl(t, f, spawn.ConvID,
		map[string]any{"implementation": string(sandboxpolicy.ImplementationTclaudeLayer)})
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, body, "no delegated cgroup v2 subtree")
}

// The dashboard read has to agree with the launch seams, because the tooltip's
// resource block appears exactly when there is a boundary to describe. A
// tclaude-layer agent with nothing authored now has one, so a read still
// reporting "no cgroup" would hide the counters the launch created — and, when
// the host could not create them, the `not enforced` line that says so.
func TestDashboardSnapshot_TclaudeLayerReportsItsOpportunisticCgroup(t *testing.T) {
	const convID = "sbxi-1111-2222-3333-4444"

	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	f := newFlow(t)
	f.HaveGroup("layeraccounted")
	f.HaveAliveSession(convID, "spwn-sbxi", "tmux-sbxi", f.TestCwd("sbxi"))
	snapshot := sandboxpolicy.EmptySnapshot()
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "spwn-sbxi", TmuxSession: "tmux-sbxi", ConvID: convID, Cwd: f.TestCwd("sbxi"),
		Status: "running", Harness: "claude",
		SandboxImplementation: string(sandboxpolicy.ImplementationTclaudeLayer),
		OSSandboxState:        "on",
		EffectiveSandbox:      &snapshot,
	}), "stamp a tclaude-layer launch with no resource budget at all")
	f.HaveMember("layeraccounted", convID)

	state := requireDashMemberState(t,
		fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest()), "layeraccounted", convID)
	assert.True(t, state.ResourceCgroup, "the implementation alone asks for the boundary")
	assert.Empty(t, state.ResourceMemoryLimit, "no memory ceiling was authored")
	assert.Nil(t, state.ResourceCPULimit, "no CPU ceiling was authored")
}
