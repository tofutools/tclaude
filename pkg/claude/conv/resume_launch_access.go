package conv

import (
	"fmt"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// reassertResumeAccessNotices is the resume's access-notice step. An
// unconfined resume discloses the rules it will not enforce; a resume under
// the harness's own sandbox re-plans the persisted network and socket intent
// against what the harness can enforce today. Either way the notices land in
// both effectiveProfile and the snapshot the caller discloses from.
func reassertResumeAccessNotices(
	h *harness.Harness,
	implementation sandboxpolicy.Implementation,
	harnessBuiltinMode, convID, resumeCwd string,
	outerLayer, unconfined bool,
	effectiveSandbox *sandboxpolicy.Snapshot,
	effectiveProfile *sandboxpolicy.EffectiveProfile,
) error {
	// An implementation that confines nothing has nothing to reassert. The block
	// below plans enforcement, and PlanAccessEnforcement correctly refuses a
	// closed network that no mechanism can hold — which is the right answer for
	// every caller that expected a wall, and the wrong question for one that
	// declared it has none. Without this, resuming a resource-only conversation
	// whose chain carries a modern deny-baseline `network:` block fails with
	// sandbox_profile_changed, on both `tclaude conv resume` and watch's
	// auto-resume, even though the same chain spawns fine.
	//
	// The rules are still disclosed, in the same words a fresh launch uses, so
	// a resumed pane does not look quieter than the one it replaced.
	if effectiveSandbox != nil && unconfined {
		if notice, ok := sandboxpolicy.UnconfinedAccessRulesNotice(
			implementation, *effectiveProfile,
		); ok {
			effectiveProfile.AccessNotices = sandboxpolicy.ReplaceAccessDegradationNotices(
				effectiveProfile.AccessNotices, notice,
			)
			effectiveSandbox.Effective.AccessNotices = append(
				[]sandboxpolicy.AccessNotice(nil), effectiveProfile.AccessNotices...)
		}
	}
	if effectiveSandbox != nil && !outerLayer && !unconfined &&
		(effectiveProfile.Network != nil || effectiveProfile.UnixSockets != nil) {
		// Standalone conversation/watch resume does not pass through agentd's
		// lifecycle planner. Reassert the persisted access intent here with no
		// fresh-spawn authorization: a degradation notice records what the
		// predecessor ran under, but is never itself permission to widen the
		// successor's network boundary.
		axes, axesErr := sandboxpolicy.EffectiveAccessAxes(*effectiveProfile)
		if axesErr != nil {
			return fmt.Errorf("sandbox_profile_changed: %w", axesErr)
		}
		launchOSSandbox := harness.ResolveLaunchOSSandbox(
			h, harnessBuiltinMode, resumeSandboxChosenBy(convID), resumeCwd)
		caps, capsErr := harness.ResolveAccessEnforcement(
			h, implementation, axes, launchOSSandbox, harnessBuiltinMode)
		if capsErr != nil {
			return fmt.Errorf("sandbox_profile_changed: %w", capsErr)
		}
		_, notices, planErr := harness.PlanAccessEnforcement(axes, caps)
		if planErr != nil {
			return fmt.Errorf("sandbox_profile_changed: %w", planErr)
		}
		effectiveProfile.AccessNotices = sandboxpolicy.ReplaceAccessDegradationNotices(
			effectiveProfile.AccessNotices, notices...,
		)
		effectiveSandbox.Effective.AccessNotices = append(
			[]sandboxpolicy.AccessNotice(nil), effectiveProfile.AccessNotices...)
	}
	return nil
}
