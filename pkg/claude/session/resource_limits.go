package session

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
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

func recordResourceLimitRuntimeOverride(sessionID string, cause error) error {
	return db.AppendSessionSandboxAccessNotice(sessionID, resourceLimitOverrideNotice(cause))
}

var recordResourceLimitRuntimeOverrideForExec = recordResourceLimitRuntimeOverride

const ResourceLimitOOMExitReason = "resource_limit_oom"

var recordResourceLimitOOMForExec = func(sessionID string) error {
	return db.SetSessionExitReason(sessionID, ResourceLimitOOMExitReason)
}

func resourceLimitsAlreadyOverridden(notices []sandboxpolicy.AccessNotice) bool {
	for _, notice := range notices {
		if notice.Axis == "resource_limits" &&
			notice.Reason == sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride &&
			notice.Effect == sandboxpolicy.AccessNoticeEffectNotEnforced {
			return true
		}
	}
	return false
}

// replaceAccessDegradationNotices preserves the dashboard-authorized resource
// fallback recorded by agentd for this launch. The generic replacement still
// discards stale target-derived network/socket degradation notices.
func replaceAccessDegradationNotices(
	existing []sandboxpolicy.AccessNotice,
	current ...sandboxpolicy.AccessNotice,
) []sandboxpolicy.AccessNotice {
	for _, notice := range existing {
		if notice.Axis == "resource_limits" &&
			notice.Reason == sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride &&
			notice.Effect == sandboxpolicy.AccessNoticeEffectNotEnforced {
			current = append(current, notice)
		}
	}
	return sandboxpolicy.ReplaceAccessDegradationNotices(existing, current...)
}
