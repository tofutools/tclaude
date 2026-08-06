package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestReplaceAccessDegradationNoticesPreservesResourceOverride(t *testing.T) {
	resourceOverride := sandboxpolicy.AccessNotice{
		Class:  sandboxpolicy.AccessNoticeClassDegradation,
		Axis:   "resource_limits",
		Reason: sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
		Effect: sandboxpolicy.AccessNoticeEffectNotEnforced,
		Detail: "current launch cannot create its cgroup",
	}
	staleNetwork := sandboxpolicy.AccessNotice{
		Class:  sandboxpolicy.AccessNoticeClassDegradation,
		Axis:   "network",
		Detail: "stale target verdict",
	}
	currentNetwork := sandboxpolicy.AccessNotice{
		Class:  sandboxpolicy.AccessNoticeClassDegradation,
		Axis:   "network",
		Detail: "current target verdict",
	}

	got := replaceAccessDegradationNotices(
		[]sandboxpolicy.AccessNotice{resourceOverride, staleNetwork}, currentNetwork)
	assert.Equal(t, []sandboxpolicy.AccessNotice{currentNetwork, resourceOverride}, got)
	assert.True(t, resourceLimitsAlreadyOverridden(got))
}

func TestResourceCgroupFailureActionKeepsFreshLaunchesLoud(t *testing.T) {
	accounting := sandboxpolicy.ResourceLimits{}
	ceiling := sandboxpolicy.ResourceLimits{Memory: "1GiB"}

	assert.Equal(t, RefuseResourceCgroupFailure,
		ResourceCgroupFailureAction(accounting, false, false),
		"a fresh launch that asked for a boundary must be told the host cannot create one")
	assert.Equal(t, DiscloseMissingResourceAccounting,
		ResourceCgroupFailureAction(accounting, true, false),
		"a continuation has no override, so refusing over counters would strand the agent")
	assert.Equal(t, DiscloseMissingResourceAccounting,
		ResourceCgroupFailureAction(accounting, false, true),
		"with no ceiling the override has nothing to authorize; claiming it did would "+
			"record the wrong reason and suppress the probe on every later launch")
	assert.Equal(t, RefuseResourceCgroupFailure,
		ResourceCgroupFailureAction(ceiling, true, false),
		"a ceiling fails closed on every path, resume included")
	assert.Equal(t, DiscloseUnenforcedResourceOverride,
		ResourceCgroupFailureAction(ceiling, true, true))
}

func TestLaunchIsSandboxContinuationCoversForksWithoutResume(t *testing.T) {
	assert.False(t, launchIsSandboxContinuation(&NewParams{}),
		"a fresh launch is an operator choice and must stay refusable")
	assert.True(t, launchIsSandboxContinuation(&NewParams{Resume: "conv_123"}))
	assert.True(t, launchIsSandboxContinuation(&NewParams{SandboxContinuation: true}),
		"a reincarnated successor and a no-copy clone fork session new without -r, "+
			"and no operator control reaches either")
}
