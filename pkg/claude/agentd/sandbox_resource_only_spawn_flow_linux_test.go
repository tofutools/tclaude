//go:build linux

// resource-only is a Linux-only implementation: sandboxImplementationHostFailure
// refuses it off Linux rather than degrading to `off`, so every test that drives
// a real resource-only launch or resume belongs here. The predicate, mode and
// enforcement-table tests stay cross-platform — they take an explicit platform
// argument and assert the same answer everywhere.

package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: `resource-only` is the implementation whose ONLY enforcement is a
// per-agent cgroup. Everything about it is therefore a claim about one value —
// the resolved ResourceLimits — surviving all the way to the launch. These
// drive real spawns through the daemon mux rather than calling the gate
// helpers, because every way this feature has broken so far broke BETWEEN the
// helpers: a snapshot replaced wholesale, or a capability gate refusing a
// chain the implementation never intended to enforce.

// The limits must reach the launch when the profile carries nothing else. If
// this fails, the implementation does nothing at all.
func TestSpawn_ResourceOnlyCarriesLimitsToLaunch(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:           "limits-only",
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "8GiB"},
	})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "capped",
		"sandbox_profile":        "limits-only",
		"sandbox_implementation": "resource-only",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	implementation, ok := f.World.SpawnSandboxImplementation(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, "resource-only", implementation)

	policy, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, policy)
	assert.Equal(t, "8GiB", policy.Effective.ResourceLimits.Memory,
		"the cgroup budget is the only thing this implementation enforces; "+
			"it must survive into the launch snapshot")
}

// resource-only with nothing authored used to be a silent no-op: no ceiling
// meant no cgroup, which made the implementation indistinguishable from `off`.
// It now means the accounting boundary, so the spawn must succeed and stay
// recorded as resource-only rather than being resolved away.
func TestSpawn_ResourceOnlyWithoutLimitsStillLaunches(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "accounted",
		"sandbox_implementation": "resource-only",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	implementation, ok := f.World.SpawnSandboxImplementation(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, "resource-only", implementation,
		"the launch keeps the implementation whose only boundary is the cgroup")

	row, err := db.FindSessionByConvID(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, string(sandboxpolicy.ImplementationResourceOnly), row.SandboxImplementation,
		"a flagless resume must reach the same accounting boundary")
}

// The regression the cold review caught. The resolved chain is global + group +
// explicit profile, and resource_limits travel in that same chain — so a
// resource-only launch must tolerate access rules it inherited rather than
// refusing, or an operator whose global profile carries any network rule
// cannot use the implementation at all. The rules are recorded and inert, and
// a notice has to say so.
func TestSpawn_ResourceOnlyToleratesInheritedAccessRulesAndDisclosesThem(t *testing.T) {
	for _, harnessName := range []string{"claude", "codex"} {
		t.Run(harnessName, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("crew")
			_, err := db.CreateSandboxProfile(&db.SandboxProfile{
				Name: "rules-and-limits",
				Filesystem: []db.SandboxFilesystemGrant{
					{Path: "/usr/share", Access: sandboxpolicy.AccessRead},
				},
				NetworkAccess:  sandboxpolicy.NetworkAccessNone,
				ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "4GiB"},
			})
			require.NoError(t, err)

			spawn := f.AsHuman().SpawnWith("crew", map[string]any{
				"name":                   "capped-" + harnessName,
				"harness":                harnessName,
				"sandbox_profile":        "rules-and-limits",
				"sandbox_implementation": "resource-only",
			})
			require.Equalf(t, http.StatusOK, spawn.Code,
				"a resource-only spawn must not be refused for rules it never claimed "+
					"to enforce; body=%s", spawn.Raw)

			policy, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
			require.True(t, ok)
			require.NotNil(t, policy)
			assert.Equal(t, "4GiB", policy.Effective.ResourceLimits.Memory,
				"inheriting access rules must not cost the launch its limits")

			var disclosed bool
			for _, notice := range policy.Effective.AccessNotices {
				if notice.Reason == sandboxpolicy.AccessNoticeReasonUnconfinedImplementation {
					disclosed = true
					assert.Equal(t, sandboxpolicy.AccessNoticeEffectNotEnforced, notice.Effect)
					assert.Contains(t, notice.Detail, "NOT enforced")
				}
			}
			assert.True(t, disclosed,
				"a resolved profile that shows up in the snapshot reads as policy in "+
					"force; the launch must state that its access rules are inert")
		})
	}
}

// A limits-only chain must NOT acquire the inert-rules warning: a notice that
// fires on every resource-only launch teaches the operator to ignore it.
func TestSpawn_ResourceOnlyStaysSilentWhenNoAccessRulesWereAuthored(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:           "quiet-limits",
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "2GiB"},
	})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "quiet",
		"sandbox_profile":        "quiet-limits",
		"sandbox_implementation": "resource-only",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	policy, ok := f.World.SpawnSandboxPolicy(spawn.ConvID)
	require.True(t, ok)
	require.NotNil(t, policy)
	for _, notice := range policy.Effective.AccessNotices {
		assert.NotEqual(t, sandboxpolicy.AccessNoticeReasonUnconfinedImplementation,
			notice.Reason, "nothing was authored, so nothing is inert")
	}
}

// "Restart without sandbox" means "give up access confinement". A
// resource-only agent has none, so the action's only effect would be to strip
// the CPU/memory ceiling — temporarySandboxLaunchSnapshot zeroes ResourceLimits
// — under a label that says sandbox. It must refuse rather than quietly lift
// the one budget the implementation exists to hold.
func TestSandboxRestartRefusesUnlockingAResourceOnlyAgent(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.CreateSandboxProfile(&db.SandboxProfile{
		Name:           "unlock-limits",
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "8GiB"},
	})
	require.NoError(t, err)

	spawn := f.AsHuman().SpawnWith("crew", map[string]any{
		"name":                   "capped-agent",
		"sandbox_profile":        "unlock-limits",
		"sandbox_implementation": "resource-only",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	f.SetSessionStatus(spawn.ConvID, "idle")
	unlock := testharness.Serve(agentd.BuildDashboardHandlerForTest(),
		dashReq(t, http.MethodPost,
			"/api/agents/"+spawn.ConvID+"/sandbox-restart",
			map[string]any{"action": "unlock"}))
	assert.Equal(t, http.StatusConflict, unlock.Code,
		"unlocking an implementation with no confinement must refuse")
	assert.Contains(t, unlock.Body.String(), "no OS-level access confinement to unlock")

	row, err := db.FindSessionByConvID(spawn.ConvID)
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.Equal(t, string(sandboxpolicy.ImplementationResourceOnly),
		row.SandboxImplementation,
		"the refused unlock must not have rewritten the live row")
}
