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
