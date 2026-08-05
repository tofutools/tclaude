package agentd

import (
	"fmt"

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
	if snapshot == nil || hasResourceLimitOverride(snapshot.Effective.AccessNotices) {
		return "", noop, nil
	}
	implementation, err := sandboxpolicy.NormalizeImplementation(rawImplementation)
	if err != nil {
		return "", noop, fmt.Errorf("managed server sandbox implementation: %w", err)
	}
	if !sandboxpolicy.ResourceCgroupRequested(snapshot.Effective.ResourceLimits, implementation) {
		return "", noop, nil
	}
	if existing, lookupErr := db.GetOpenCodeRuntime(sessionID); lookupErr != nil {
		return "", noop, lookupErr
	} else if existing != nil && existing.ResourceCgroupDir != "" {
		if validateErr := session.ValidatePreparedResourceCgroup(
			existing.ResourceCgroupDir, snapshot.Effective.ResourceLimits); validateErr == nil {
			return existing.ResourceCgroupDir, noop, nil
		}
		if stopErr := stopOpenCodeRuntime(sessionID); stopErr != nil {
			return "", noop, fmt.Errorf("replace changed OpenCode resource cgroup: %w", stopErr)
		}
	}
	dir, cleanup, err := prepareResourceCgroup(
		sessionID, snapshot.Effective.ResourceLimits)
	if err == nil {
		return dir, cleanup, nil
	}
	if !allowUnenforced {
		return "", noop, err
	}
	appendManagedServerResourceOverride(snapshot, err)
	return "", noop, nil
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
