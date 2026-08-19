package agentd

import (
	"fmt"
	"log/slog"
	"runtime"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

var prepareResourceCgroup = session.PrepareResourceCgroup

// resourceCgroupRequested answers the per-agent-cgroup question for the host
// agentd is running on. Every daemon-side seam — the launch seams and the reads
// that report the posture back — has to answer it identically, and the host is
// the same one for all of them.
func resourceCgroupRequested(
	limits sandboxpolicy.ResourceLimits,
	implementation sandboxpolicy.Implementation,
) bool {
	return sandboxpolicy.ResourceCgroupRequested(limits, implementation, runtime.GOOS)
}

func prepareManagedServerResourceCgroup(
	sessionID string,
	snapshot *sandboxpolicy.Snapshot,
	rawImplementation string,
	allowUnenforced bool,
	resuming bool,
) (string, func(), error) {
	noop := func() {}
	// resource-only and tclaude-layer ask for the cgroup through the
	// implementation alone, so a conversation with no recorded effective-sandbox
	// row still gets its boundary.
	var limits sandboxpolicy.ResourceLimits
	if snapshot != nil {
		if hasResourceLimitOverride(snapshot.Effective.AccessNotices) {
			return "", noop, nil
		}
		limits = snapshot.Effective.ResourceLimits
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(rawImplementation)
	if err != nil {
		return "", noop, fmt.Errorf("managed server sandbox implementation: %w", err)
	}
	if !resourceCgroupRequested(limits, implementation) {
		return "", noop, nil
	}
	if existing, lookupErr := db.GetOpenCodeRuntime(sessionID); lookupErr != nil {
		return "", noop, lookupErr
	} else if existing != nil && existing.ResourceCgroupDir != "" {
		if validateErr := session.ValidatePreparedResourceCgroup(
			existing.ResourceCgroupDir, limits); validateErr == nil {
			return existing.ResourceCgroupDir, noop, nil
		}
		if stopErr := stopOpenCodeRuntime(sessionID); stopErr != nil {
			return "", noop, fmt.Errorf("replace changed OpenCode resource cgroup: %w", stopErr)
		}
	}
	dir, cleanup, err := prepareResourceCgroup(sessionID, limits)
	if err == nil {
		return dir, cleanup, nil
	}
	switch session.ResourceCgroupFailureAction(limits, implementation, resuming, allowUnenforced) {
	case session.DiscloseMissingResourceAccounting:
		// Refusing a relaunch over counters would strand the conversation: the
		// dashboard override is a fresh-spawn control with no resume equivalent.
		slog.Warn("resource cgroup unavailable; resuming managed server without per-agent accounting",
			"session_id", sessionID, "error", err)
		appendManagedServerAccountingUnavailable(snapshot, err)
		return "", noop, nil
	case session.DiscloseUnenforcedResourceOverride:
		appendManagedServerResourceOverride(snapshot, err)
		return "", noop, nil
	}
	// RefuseResourceCgroupFailure, and any policy value this seam does not know:
	// fail closed rather than start a server without the boundary it asked for.
	return "", noop, err
}

// degradeManagedServerResourceCgroup decides what a server that could not be
// placed in its prepared boundary does next, and records the disclosure that
// goes with the answer. It reports true when the launch may retry outside the
// boundary, and false when the boundary has to hold and the failure stands.
//
// Placement is the other way the boundary can be lost: the cgroup was created,
// then the server could not be forked into it. That is the same loss as a
// creation failure and it takes the same answer, so this defers to the same
// ResourceCgroupFailureAction prepareManagedServerResourceCgroup consults —
// including the resuming distinction, without which a relaunch could refuse
// over counters that no fresh-spawn override can rescue.
//
// The two disclosures are not interchangeable. Where no ceiling is at stake the
// operator override has nothing to authorize, and recording it anyway would
// name a decision they did not make — and it is sticky: an override notice
// suppresses the boundary on every later launch of this conversation.
func degradeManagedServerResourceCgroup(
	snapshot *sandboxpolicy.Snapshot,
	rawImplementation string,
	allowUnenforced bool,
	resuming bool,
	cause error,
) bool {
	var limits sandboxpolicy.ResourceLimits
	if snapshot != nil {
		limits = snapshot.Effective.ResourceLimits
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(rawImplementation)
	if err != nil {
		// Unreachable from a launch that got this far — the preparation seam
		// normalized the same string — so fail closed rather than invent a
		// posture for a value this daemon cannot read.
		return false
	}
	switch session.ResourceCgroupFailureAction(limits, implementation, resuming, allowUnenforced) {
	case session.DiscloseMissingResourceAccounting:
		appendManagedServerAccountingUnavailable(snapshot, cause)
		return true
	case session.DiscloseUnenforcedResourceOverride:
		appendManagedServerResourceOverride(snapshot, cause)
		return true
	}
	// RefuseResourceCgroupFailure, and any policy value this seam does not know.
	return false
}

// appendManagedServerAccountingUnavailable mirrors the pane-side disclosure for
// a managed server that asked for a limitless boundary and did not get one.
func appendManagedServerAccountingUnavailable(snapshot *sandboxpolicy.Snapshot, err error) {
	if snapshot == nil {
		return
	}
	snapshot.Effective.AccessNotices = append(snapshot.Effective.AccessNotices,
		session.ResourceCgroupUnavailableNotice(err))
}

func appendManagedServerResourceOverride(snapshot *sandboxpolicy.Snapshot, err error) {
	if snapshot == nil {
		return
	}
	snapshot.Effective.AccessNotices = append(snapshot.Effective.AccessNotices,
		sandboxpolicy.AccessNotice{
			Class: sandboxpolicy.AccessNoticeClassDegradation, Axis: "resource_limits",
			Reason: sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
			Effect: sandboxpolicy.AccessNoticeEffectNotEnforced,
			Detail: fmt.Sprintf("the human operator used the dashboard launch override; configured CPU and memory limits are not enforced: %v", err),
		})
}

func hasResourceLimitOverride(notices []sandboxpolicy.AccessNotice) bool {
	for _, notice := range notices {
		if notice.Axis == "resource_limits" &&
			notice.Reason == sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride &&
			notice.Effect == sandboxpolicy.AccessNoticeEffectNotEnforced {
			return true
		}
	}
	return false
}
