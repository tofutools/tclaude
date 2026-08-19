//go:build linux

// The opportunistic boundary is Linux-only: off Linux a tclaude-layer launch is
// Seatbelt with no cgroup v2 beneath it, and ResourceCgroupRequested says so.
// These drive the managed-server seam, where the same question is answered for
// the agentd-owned OpenCode server rather than for a pane.

package agentd

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// tclaude already owns this workload's outer wall and forks the server itself,
// so the accounting a limitless resource-only server is selected for is there
// for the taking. A server that asked for nothing but confinement still gets
// the counters.
func TestManagedServerPreparesOpportunisticCgroupForTclaudeLayer(t *testing.T) {
	setupTestDB(t)
	t.Setenv("TMUX", "")
	boundary := "/sys/fs/cgroup/system.slice/tclaude-agentd.service/tclaude-layer"
	previousPrepare := prepareResourceCgroup
	prepareResourceCgroup = func(_ string, limits sandboxpolicy.ResourceLimits) (string, func(), error) {
		assert.False(t, limits.Enabled(), "no ceiling was authored; the cgroup itself is the request")
		return boundary, func() {}, nil
	}
	t.Cleanup(func() { prepareResourceCgroup = previousPrepare })

	dir, _, err := prepareManagedServerResourceCgroup("managed-layer", &sandboxpolicy.Snapshot{},
		string(sandboxpolicy.ImplementationTclaudeLayer), false, false)
	require.NoError(t, err)
	assert.Equal(t, boundary, dir)
}

// The whole difference from resource-only. The layer was chosen for its
// confinement, which the cgroup has no part in, so a host that cannot provide
// the boundary must still start the server — even on a fresh spawn, where a
// resource-only launch is refused by name.
func TestManagedServerTclaudeLayerStartsWithoutTheCgroupItCouldNotGet(t *testing.T) {
	setupTestDB(t)
	t.Setenv("TMUX", "")
	previousPrepare := prepareResourceCgroup
	prepareResourceCgroup = func(string, sandboxpolicy.ResourceLimits) (string, func(), error) {
		return "", func() {}, errors.New("a per-agent cgroup requires a delegated cgroup v2 service subtree")
	}
	t.Cleanup(func() { prepareResourceCgroup = previousPrepare })
	snapshot := &sandboxpolicy.Snapshot{}

	dir, _, err := prepareManagedServerResourceCgroup("managed-layer-nodeleg", snapshot,
		string(sandboxpolicy.ImplementationTclaudeLayer), false, false)
	require.NoError(t, err,
		"refusing here would deny the operator the confinement they chose the layer for")
	assert.Empty(t, dir)

	var disclosed bool
	for _, notice := range snapshot.Effective.AccessNotices {
		if notice.Reason == sandboxpolicy.AccessNoticeReasonResourceCgroupUnavailable {
			disclosed = true
			assert.Equal(t, sandboxpolicy.AccessNoticeEffectNotEnforced, notice.Effect)
		}
		assert.NotEqual(t, sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
			notice.Reason, "the operator authorized nothing here and must not be recorded as having")
	}
	assert.True(t, disclosed, "counters the launch asked for and did not get must be disclosed")

	// A ceiling under the same implementation is a different question, and it
	// still fails closed.
	capped := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "512MB"},
	}}
	_, _, cappedErr := prepareManagedServerResourceCgroup("managed-layer-capped", capped,
		string(sandboxpolicy.ImplementationTclaudeLayer), false, false)
	assert.Error(t, cappedErr)
}

// Placement is the other way the boundary can fail: prepared, then rejected
// when the server is forked into it. The answer has to match the preparation
// seam's, or a launch that survived a missing delegation would die of the same
// boundary one step later.
func TestDegradeManagedServerResourceCgroupSeparatesBonusFromCeiling(t *testing.T) {
	cause := errors.New("clone3 refused the cgroup fd")

	opportunistic := &sandboxpolicy.Snapshot{}
	assert.True(t, degradeManagedServerResourceCgroup(opportunistic,
		string(sandboxpolicy.ImplementationTclaudeLayer), false, false, cause))
	require.Len(t, opportunistic.Effective.AccessNotices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonResourceCgroupUnavailable,
		opportunistic.Effective.AccessNotices[0].Reason,
		"an override notice here would name a decision the operator never made, and it is "+
			"sticky: it suppresses the boundary on every later launch")

	capped := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "512MB"},
	}}
	assert.False(t, degradeManagedServerResourceCgroup(capped,
		string(sandboxpolicy.ImplementationTclaudeLayer), false, false, cause),
		"a ceiling the server would run without must fail the launch")
	assert.Empty(t, capped.Effective.AccessNotices)

	assert.True(t, degradeManagedServerResourceCgroup(capped,
		string(sandboxpolicy.ImplementationTclaudeLayer), true, false, cause),
		"the dashboard override is what authorizes running a ceiling unenforced")
	require.Len(t, capped.Effective.AccessNotices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
		capped.Effective.AccessNotices[0].Reason)

	assert.False(t, degradeManagedServerResourceCgroup(&sandboxpolicy.Snapshot{},
		"not-an-implementation", false, false, cause),
		"an implementation this daemon cannot parse never reached a launch that asked "+
			"for a boundary, so nothing here excuses the failure")
}

// The placement seam has to answer what the preparation seam one function above
// answers, for the same launch — a limitless `resource-only` server included,
// which is the case where the two used to disagree.
func TestDegradeManagedServerResourceCgroupMatchesThePreparationPolicy(t *testing.T) {
	cause := errors.New("clone3 refused the cgroup fd")

	// A fresh limitless resource-only spawn fails closed here exactly as
	// PrepareResourceCgroup's failure does: the operator is choosing the posture
	// right now and can act on the refusal.
	assert.False(t, degradeManagedServerResourceCgroup(&sandboxpolicy.Snapshot{},
		string(sandboxpolicy.ImplementationResourceOnly), false, false, cause))

	// Its resume does not, because a resume has no override to rescue it and a
	// refusal would strand an agent already recorded as resource-only.
	resumed := &sandboxpolicy.Snapshot{}
	assert.True(t, degradeManagedServerResourceCgroup(resumed,
		string(sandboxpolicy.ImplementationResourceOnly), false, true, cause),
		"refusing a relaunch over counters would leave the agent unwakeable")
	require.Len(t, resumed.Effective.AccessNotices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonResourceCgroupUnavailable,
		resumed.Effective.AccessNotices[0].Reason)

	// And where no ceiling exists the override has nothing to authorize, so
	// ticking the dashboard box must not buy the sticky notice that would
	// suppress this conversation's boundary from here on.
	authorized := &sandboxpolicy.Snapshot{}
	assert.True(t, degradeManagedServerResourceCgroup(authorized,
		string(sandboxpolicy.ImplementationResourceOnly), true, false, cause))
	require.Len(t, authorized.Effective.AccessNotices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonResourceCgroupUnavailable,
		authorized.Effective.AccessNotices[0].Reason,
		"an override notice here would retire the accounting on every later launch")
}
