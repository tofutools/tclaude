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
		string(sandboxpolicy.ImplementationTclaudeLayer), false, cause))
	require.Len(t, opportunistic.Effective.AccessNotices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonResourceCgroupUnavailable,
		opportunistic.Effective.AccessNotices[0].Reason,
		"an override notice here would name a decision the operator never made, and it is "+
			"sticky: it suppresses the boundary on every later launch")

	capped := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		ResourceLimits: sandboxpolicy.ResourceLimits{Memory: "512MB"},
	}}
	assert.False(t, degradeManagedServerResourceCgroup(capped,
		string(sandboxpolicy.ImplementationTclaudeLayer), false, cause),
		"a ceiling the server would run without must fail the launch")
	assert.Empty(t, capped.Effective.AccessNotices)

	assert.True(t, degradeManagedServerResourceCgroup(capped,
		string(sandboxpolicy.ImplementationTclaudeLayer), true, cause),
		"the dashboard override is what authorizes running a ceiling unenforced")
	require.Len(t, capped.Effective.AccessNotices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
		capped.Effective.AccessNotices[0].Reason)

	assert.False(t, degradeManagedServerResourceCgroup(&sandboxpolicy.Snapshot{},
		"not-an-implementation", false, cause),
		"an implementation this daemon cannot parse never reached a launch that asked "+
			"for a boundary, so nothing here excuses the failure")
}
