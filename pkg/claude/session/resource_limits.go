package session

import (
	"fmt"
	"strings"

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

// ResourceCgroupFailurePolicy is what a launch does when the per-agent cgroup it
// asked for could not be created.
type ResourceCgroupFailurePolicy int

const (
	// RefuseResourceCgroupFailure fails the launch. This is the answer whenever
	// the operator can act on it: a fresh launch is a live decision, and its
	// error names the missing delegation and how to fix it.
	RefuseResourceCgroupFailure ResourceCgroupFailurePolicy = iota
	// DiscloseUnenforcedResourceOverride launches anyway because the operator
	// authorized that for this spawn through the dashboard.
	DiscloseUnenforcedResourceOverride
	// DiscloseMissingResourceAccounting launches anyway because no ceiling was
	// authored and refusing would strand the agent rather than inform anyone. It
	// covers every continuation of an existing agent — a resume, a reincarnation,
	// a no-copy clone — none of which carries the dashboard's fresh-spawn
	// override, so a refusal there would make an agent already recorded as
	// resource-only permanently unlaunchable. A ceiling still fails closed.
	DiscloseMissingResourceAccounting
)

// ResourceCgroupFailureAction picks between refusing and disclosing. It is the
// one place that decides, because the pane seam and the managed-server seam must
// answer identically for the same launch.
//
// continuation means this launch carries a sandbox posture recorded earlier
// rather than one the operator is choosing right now. That is the whole
// distinction: a refusal is worth raising only where someone can act on it.
func ResourceCgroupFailureAction(
	limits sandboxpolicy.ResourceLimits,
	continuation bool,
	allowUnenforced bool,
) ResourceCgroupFailurePolicy {
	if !limits.Enabled() {
		// Nothing authored is going unenforced, so the override has nothing to
		// authorize here — and claiming it did would record the wrong reason AND
		// make it sticky, since an override notice suppresses the probe on every
		// later launch. Disclose the missing counters instead.
		if continuation || allowUnenforced {
			return DiscloseMissingResourceAccounting
		}
		return RefuseResourceCgroupFailure
	}
	if allowUnenforced {
		return DiscloseUnenforcedResourceOverride
	}
	return RefuseResourceCgroupFailure
}

// launchIsSandboxContinuation reports whether this launch carries a sandbox
// posture recorded earlier rather than one an operator is choosing now. A resume
// says so with -r; a reincarnated successor and a no-copy clone fork
// `session new` without it and carry the explicit marker instead.
func launchIsSandboxContinuation(params *NewParams) bool {
	return strings.TrimSpace(params.Resume) != "" || params.SandboxContinuation
}

// ResourceCgroupUnavailableNotice discloses a limitless launch that could not get
// its accounting boundary. No ceiling was authored, so nothing the operator asked
// to enforce is being widened — but the counters, the OOM attribution and the
// kill handle they selected the implementation for are absent, and only a notice
// can say so.
func ResourceCgroupUnavailableNotice(err error) sandboxpolicy.AccessNotice {
	return sandboxpolicy.AccessNotice{
		Class:  sandboxpolicy.AccessNoticeClassDegradation,
		Axis:   "resource_limits",
		Reason: sandboxpolicy.AccessNoticeReasonResourceCgroupUnavailable,
		Effect: sandboxpolicy.AccessNoticeEffectNotEnforced,
		Detail: fmt.Sprintf(
			"no ceiling was authored and this host has no delegated cgroup for the per-agent boundary, so accounting, OOM attribution and the workload kill handle are unavailable: %v",
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
