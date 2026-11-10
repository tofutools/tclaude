package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Planning only runs for a confining launch whose profile declares network,
// socket or filesystem-root rules. Every other launch leaves the sandbox
// state and both snapshots exactly as they were.
func TestPlanLaunchAccessEnforcementLeavesALaunchWithNothingToPlanAlone(t *testing.T) {
	claude := harness.MustGet(harness.DefaultName)
	before := launchAccessState{
		posture:   sandboxpolicy.NetworkFiltered,
		root:      sandboxpolicy.RootConstructed,
		osSandbox: harness.LaunchOSSandbox{State: "on", Source: "test"},
	}
	ruled := func() *sandboxpolicy.Snapshot {
		s := &sandboxpolicy.Snapshot{}
		s.Effective.FilesystemRoot = sandboxpolicy.FilesystemRootSeparate
		return s
	}
	cases := map[string]launchAccessPlanInput{
		"no launch sandbox": {},
		"no rules declared": {launchSandbox: &sandboxpolicy.Snapshot{}},
		"enforces nothing":  {unconfined: true, launchSandbox: ruled(), effectiveSandbox: ruled()},
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			in.h, in.params = claude, &NewParams{}
			state := before
			require.NoError(t, planLaunchAccessEnforcement(in, &state))
			assert.Equal(t, before, state)
			for _, snap := range []*sandboxpolicy.Snapshot{in.launchSandbox, in.effectiveSandbox} {
				if snap != nil {
					assert.Empty(t, snap.Effective.AccessNotices, "nothing planned, nothing noticed")
				}
			}
		})
	}
}

// A launch that declares access rules reaches the planner, and one whose OS
// sandbox cannot enforce them is refused before anything is recorded.
func TestPlanLaunchAccessEnforcementRefusesRulesNoSandboxCanEnforce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	claude := harness.MustGet(harness.DefaultName)
	launch := &sandboxpolicy.Snapshot{}
	launch.Effective.FilesystemRoot = sandboxpolicy.FilesystemRootSeparate
	state := launchAccessState{posture: sandboxpolicy.NetworkHostOpen}
	err := planLaunchAccessEnforcement(launchAccessPlanInput{
		h: claude, params: &NewParams{},
		sandboxImplementation: sandboxpolicy.ImplementationHarnessBuiltin,
		launchSandbox:         launch,
	}, &state)
	require.ErrorContains(t, err, "access enforcement requires a functioning OS sandbox")
	assert.Empty(t, launch.Effective.AccessNotices, "a refused plan records nothing")
}
