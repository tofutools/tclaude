package db

import (
	"maps"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// ComposeAgentRelaunchProfile overlays one relaunch profile onto another, FIELD
// BY FIELD. A nil field in the overlay means "unknown", so it retains base's
// value instead of erasing it — the tri-state rule the whole relaunch contract
// rests on (nil = unknown, a pointer to the zero value = asserted intent).
//
// Adding a field to AgentRelaunchProfile without adding it here silently makes
// that field un-overlayable; TestComposeAgentRelaunchProfileCoversEveryField
// fails when that happens.
func ComposeAgentRelaunchProfile(base, overlay *AgentRelaunchProfile) *AgentRelaunchProfile {
	if overlay == nil {
		return base
	}
	if base == nil {
		return overlay
	}
	merged := *base
	merged.Version = overlay.Version
	if overlay.HarnessBuiltinMode != nil {
		merged.HarnessBuiltinMode = overlay.HarnessBuiltinMode
	}
	if overlay.SandboxImplementation != nil {
		merged.SandboxImplementation = overlay.SandboxImplementation
	}
	if overlay.HarnessBuiltinModeSource != nil {
		merged.HarnessBuiltinModeSource = overlay.HarnessBuiltinModeSource
	}
	if overlay.TemporaryHarnessBuiltinMode != nil {
		merged.TemporaryHarnessBuiltinMode = overlay.TemporaryHarnessBuiltinMode
	}
	if overlay.ApprovalPolicy != nil {
		merged.ApprovalPolicy = overlay.ApprovalPolicy
	}
	if overlay.ToolGovernance != nil {
		merged.ToolGovernance = overlay.ToolGovernance
	}
	if overlay.ApprovalAutoReview != nil {
		merged.ApprovalAutoReview = overlay.ApprovalAutoReview
	}
	if overlay.ModelID != nil {
		merged.ModelID = overlay.ModelID
	}
	if overlay.Effort != nil {
		merged.Effort = overlay.Effort
	}
	if overlay.ContextWindowSize != nil {
		merged.ContextWindowSize = overlay.ContextWindowSize
	}
	if overlay.ConfiguredContextWindowMax != nil {
		merged.ConfiguredContextWindowMax = overlay.ConfiguredContextWindowMax
	}
	if overlay.CopilotAPI != nil {
		merged.CopilotAPI = overlay.CopilotAPI
	}
	if overlay.CopilotAPISource != nil {
		merged.CopilotAPISource = overlay.CopilotAPISource
	}
	if overlay.CodexAppServer != nil {
		merged.CodexAppServer = overlay.CodexAppServer
	}
	if overlay.CodexAppServerSource != nil {
		merged.CodexAppServerSource = overlay.CodexAppServerSource
	}
	if overlay.CodexStateRoot != nil {
		merged.CodexStateRoot = overlay.CodexStateRoot
	}
	if overlay.CodexStateRootSource != nil {
		merged.CodexStateRootSource = overlay.CodexStateRootSource
	}
	if overlay.FastMode != nil {
		merged.FastMode = overlay.FastMode
	}
	if overlay.AskUserQuestionTimeout != nil {
		merged.AskUserQuestionTimeout = overlay.AskUserQuestionTimeout
	}
	if overlay.RemoteControl != nil {
		merged.RemoteControl = overlay.RemoteControl
	}
	if overlay.AutoMemory != nil {
		merged.AutoMemory = overlay.AutoMemory
	}
	if overlay.SSHWorkaround != nil {
		merged.SSHWorkaround = overlay.SSHWorkaround
	}
	if overlay.SSHWorkaroundSource != nil {
		merged.SSHWorkaroundSource = overlay.SSHWorkaroundSource
	}
	if overlay.AutoCompactWindow != nil {
		merged.AutoCompactWindow = overlay.AutoCompactWindow
	}
	if overlay.ContextFeatures != nil {
		merged.ContextFeatures = overlay.ContextFeatures
	}
	return &merged
}

// RecordedLaunchPostureForConv returns everything tclaude knows about the
// posture a conversation was last launched with, as a TRI-STATE profile: nil
// means the conversation has no recorded posture at all, and a nil FIELD means
// nothing was ever recorded for that field — never "recorded as empty".
//
// That distinction is the point. A relaunch that collapses unknown into a known
// zero writes the zero onto its fresh session row, the durable projection then
// asserts it as intent, and the original posture is gone for good — the erasure
// TCL-730 describes. Callers preserve on nil and only override on an explicit
// caller value.
//
// The tiers are the ones the per-field *ForConv helpers below have always
// consulted, in the same order:
//
//  1. the stable agent's durable intent (agents.relaunch_profile),
//  2. the conversation-owned fallback (conversation_resume_profiles), which is
//     what keeps an ordinary non-agent conversation resumable,
//  3. the newest session row, for records predating the v145 projection.
//
// Tier 3 reads plain columns, which cannot express "unknown", so a ZERO column
// there is reported as unknown rather than as intent. That is the safe reading
// twice over: a legacy row never asserted anything, and every zero in that tier
// is also the launch default, so preserving nothing and preserving the zero
// produce the same launch.
func RecordedLaunchPostureForConv(convID string) (*AgentRelaunchProfile, error) {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return nil, nil
	}
	var posture *AgentRelaunchProfile
	conversation, err := ConversationResumeProfileForConv(convID)
	if err != nil {
		return nil, err
	}
	if conversation != nil {
		posture = conversation.FallbackRelaunch
	}
	agent, err := AgentRelaunchProfileForConv(convID)
	if err != nil {
		return nil, err
	}
	posture = ComposeAgentRelaunchProfile(posture, agent)
	if posture != nil && recordedPostureIsComplete(posture) {
		return activeRecordedLaunchPostureForConv(convID, posture)
	}
	// Only pay for the session lookup when something is still unknown; the
	// legacy row is the weakest tier, so it goes underneath what we already have.
	legacy, err := legacySessionLaunchPosture(convID)
	if err != nil || legacy == nil {
		if err != nil {
			return posture, err
		}
		return activeRecordedLaunchPostureForConv(convID, posture)
	}
	return activeRecordedLaunchPostureForConv(
		convID, ComposeAgentRelaunchProfile(legacy, posture),
	)
}

// NormalSandboxImplementationForConv resolves the durable implementation
// independently of a process-only temporary override. The historical lookup
// repairs the specific projection bug fingerprint while leaving an intentional
// harness-builtin transition alone.
func NormalSandboxImplementationForConv(
	convID string,
	posture *AgentRelaunchProfile,
) (sandboxpolicy.Implementation, error) {
	implementation := sandboxpolicy.ImplementationHarnessBuiltin
	if posture != nil && posture.SandboxImplementation != nil {
		var err error
		implementation, err = sandboxpolicy.NormalizeImplementation(
			*posture.SandboxImplementation,
		)
		if err != nil {
			return "", err
		}
	}
	if implementation != sandboxpolicy.ImplementationHarnessBuiltin {
		return implementation, nil
	}
	// An operator assignment is a deliberate later choice, so there is nothing to
	// repair: the historical scan below exists to undo a projection bug, and its
	// own fingerprint requires the damaged temporary-unlock tail. That tail
	// survives a restore whose relaunch failed, which is exactly when an operator
	// reassigns the agent to harness-builtin — and resurrecting the older layered
	// value there would undo the assignment they just made.
	//
	// The marker is read from the MODE attribution because that is where an
	// assignment records itself; the two move together only because
	// AssignAgentSandboxImplementation writes them together. A future edit that
	// stamps this source while changing the mode ALONE would disable this repair
	// as a side effect, so keep that constant to writes of the whole posture.
	if posture != nil && posture.HarnessBuiltinModeSource != nil &&
		strings.TrimSpace(*posture.HarnessBuiltinModeSource) == AssignedSandboxImplementationSource {
		return implementation, nil
	}
	historical, err := PreTemporaryUnlockSandboxImplementationForConv(convID)
	if err != nil || strings.TrimSpace(historical) == "" {
		return implementation, err
	}
	return sandboxpolicy.NormalizeImplementation(historical)
}

// activeRecordedLaunchPostureForConv returns a copy whose implementation is
// ready for a process launch. Temporary off always means harness-builtin plus
// the harness's off mode: replaying the durable TClaude value would silently
// turn the outer wall back on. The normal value remains stored on the agent for
// exact restore and clone semantics.
func activeRecordedLaunchPostureForConv(
	convID string,
	posture *AgentRelaunchProfile,
) (*AgentRelaunchProfile, error) {
	if posture == nil {
		return nil, nil
	}
	// Preserve the tri-state contract for a genuinely unknown legacy field.
	if posture.SandboxImplementation == nil && posture.TemporaryHarnessBuiltinMode == nil {
		return posture, nil
	}
	implementation, err := NormalSandboxImplementationForConv(convID, posture)
	if err != nil {
		return nil, err
	}
	if posture.TemporaryHarnessBuiltinMode != nil {
		implementation = sandboxpolicy.ImplementationHarnessBuiltin
	}
	effective := *posture
	value := string(implementation)
	effective.SandboxImplementation = &value
	return &effective, nil
}

// recordedPostureIsComplete reports whether every field the legacy session tier
// could contribute is already known, so that tier can be skipped.
func recordedPostureIsComplete(p *AgentRelaunchProfile) bool {
	return p.HarnessBuiltinMode != nil && p.SandboxImplementation != nil &&
		p.ApprovalPolicy != nil && p.ApprovalAutoReview != nil &&
		p.AskUserQuestionTimeout != nil && p.RemoteControl != nil && p.AutoMemory != nil &&
		p.ContextFeatures != nil && p.AutoCompactWindow != nil
}

// legacySessionLaunchPosture reconstructs what a pre-v145 record can still
// prove from its newest session row. Zero columns stay nil (see the tier-3 note
// on RecordedLaunchPostureForConv).
func legacySessionLaunchPosture(convID string) (*AgentRelaunchProfile, error) {
	s, err := FindSessionByConvID(convID)
	if err != nil || s == nil {
		return nil, err
	}
	p := &AgentRelaunchProfile{Version: RelaunchProfileVersion}
	if mode := strings.TrimSpace(s.HarnessBuiltinMode); mode != "" {
		p.HarnessBuiltinMode = stringPtr(mode)
	}
	if implementation := strings.TrimSpace(s.SandboxImplementation); implementation != "" {
		p.SandboxImplementation = stringPtr(implementation)
	}
	if policy := strings.TrimSpace(s.ApprovalPolicy); policy != "" {
		p.ApprovalPolicy = stringPtr(policy)
		// auto-review is only meaningful alongside a recorded policy; on its own
		// a false column is indistinguishable from a row that never carried one.
		p.ApprovalAutoReview = boolPtr(s.ApprovalAutoReview)
	}
	if timeout := strings.TrimSpace(s.AskUserQuestionTimeout); timeout != "" {
		p.AskUserQuestionTimeout = stringPtr(timeout)
	}
	if s.RemoteControl {
		p.RemoteControl = boolPtr(true)
	}
	if s.AutoMemory {
		p.AutoMemory = boolPtr(true)
	}
	if len(s.ContextFeatures) > 0 {
		features := make(map[string]string, len(s.ContextFeatures))
		maps.Copy(features, s.ContextFeatures)
		p.ContextFeatures = &features
	}
	if window := strings.TrimSpace(s.AutoCompactWindow); window != "" {
		p.AutoCompactWindow = stringPtr(window)
	}
	return p, nil
}
