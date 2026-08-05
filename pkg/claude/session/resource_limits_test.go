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
	assert.Equal(t, DiscloseUnenforcedResourceOverride,
		ResourceCgroupFailureAction(accounting, false, true),
		"the operator authorized this spawn without enforcement")
	assert.Equal(t, DiscloseMissingResourceAccounting,
		ResourceCgroupFailureAction(accounting, true, false),
		"a resume has no override, so refusing over counters would strand the agent")
	assert.Equal(t, RefuseResourceCgroupFailure,
		ResourceCgroupFailureAction(ceiling, true, false),
		"a ceiling fails closed on every path, resume included")
	assert.Equal(t, DiscloseUnenforcedResourceOverride,
		ResourceCgroupFailureAction(ceiling, true, true))
}
