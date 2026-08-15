package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestAccessEnforcementOptionsFromLaunchNoticesKeepsOverridesNarrow(t *testing.T) {
	notice := func(reason string) sandboxpolicy.AccessNotice {
		return sandboxpolicy.AccessNotice{
			Class:  sandboxpolicy.AccessNoticeClassDegradation,
			Axis:   "network",
			Reason: reason,
			Effect: sandboxpolicy.AccessNoticeEffectNotEnforced,
		}
	}

	closed := accessEnforcementOptionsFromLaunchNotices([]sandboxpolicy.AccessNotice{
		notice(sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride),
	})
	assert.True(t, closed.AllowUnenforcedNetworkClosed)
	assert.False(t, closed.AllowReducedNetworkDeny)

	reducedDeny := accessEnforcementOptionsFromLaunchNotices([]sandboxpolicy.AccessNotice{
		notice(sandboxpolicy.AccessNoticeReasonOperatorReducedNetworkDenyOverride),
	})
	assert.False(t, reducedDeny.AllowUnenforcedNetworkClosed)
	assert.True(t, reducedDeny.AllowReducedNetworkDeny)

	wrongShape := accessEnforcementOptionsFromLaunchNotices([]sandboxpolicy.AccessNotice{{
		Class:  sandboxpolicy.AccessNoticeClassComposition,
		Axis:   "network",
		Reason: sandboxpolicy.AccessNoticeReasonOperatorReducedNetworkDenyOverride,
		Effect: sandboxpolicy.AccessNoticeEffectNotEnforced,
	}})
	assert.False(t, wrongShape.AllowReducedNetworkDeny)
}
