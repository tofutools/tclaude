package agentd

import (
	"fmt"
	"log/slog"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

var prepareResourceCgroup = session.PrepareResourceCgroup

func prepareManagedServerResourceCgroup(
	sessionID string,
	snapshot *sandboxpolicy.Snapshot,
	rawImplementation string,
	allowUnenforced bool,
) (string, func(), error) {
	noop := func() {}
	// resource-only asks for the cgroup through the implementation alone, so a
	// conversation with no recorded effective-sandbox row still gets its boundary.
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
	if !sandboxpolicy.ResourceCgroupRequested(limits, implementation) {
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
	if !limits.Enabled() {
		// An accounting boundary is observability: a host that cannot provide one
		// must not make the server unstartable, on a resume least of all.
		slog.Warn("resource cgroup unavailable; starting managed server without per-agent accounting",
			"session_id", sessionID, "error", err)
		appendManagedServerAccountingUnavailable(snapshot, err)
		return "", noop, nil
	}
	if !allowUnenforced {
		return "", noop, err
	}
	appendManagedServerResourceOverride(snapshot, err)
	return "", noop, nil
}

// appendManagedServerAccountingUnavailable mirrors the pane-side disclosure for
// a managed server that asked for a limitless boundary and did not get one.
func appendManagedServerAccountingUnavailable(snapshot *sandboxpolicy.Snapshot, err error) {
	if snapshot == nil {
		return
	}
	snapshot.Effective.AccessNotices = append(snapshot.Effective.AccessNotices,
		sandboxpolicy.AccessNotice{
			Class: sandboxpolicy.AccessNoticeClassDegradation, Axis: "resource_limits",
			Reason: sandboxpolicy.AccessNoticeReasonResourceCgroupUnavailable,
			Effect: sandboxpolicy.AccessNoticeEffectNotEnforced,
			Detail: fmt.Sprintf("no ceiling was authored and this host has no delegated cgroup for the per-agent boundary, so accounting, OOM attribution and the workload kill handle are unavailable: %v", err),
		})
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
