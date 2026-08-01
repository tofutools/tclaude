package session

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func resourceLimitOverrideNotice(err error) sandboxpolicy.AccessNotice {
	return sandboxpolicy.AccessNotice{
		Class:  sandboxpolicy.AccessNoticeClassDegradation,
		Axis:   "resource_limits",
		Reason: sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
		Effect: sandboxpolicy.AccessNoticeEffectNotEnforced,
		Detail: fmt.Sprintf(
			"the human operator used the dashboard launch override; configured CPU and memory limits are not enforced: %v",
			err,
		),
	}
}
