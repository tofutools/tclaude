package agentd

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// durableRelaunchConfig is the validated composition of stable agent intent
// and conversation-owned resume facts. No field is sourced from a predecessor
// session row.
type durableRelaunchConfig struct {
	Harness          string
	Cwd              string
	ResumeProvenance string
	Sandbox          string
	// SandboxImplementation is independent of the per-harness Sandbox mode:
	// it decides whether the durable launch is harness-owned or wrapped by
	// tclaude's outer layer.
	SandboxImplementation string
	// SandboxModeSource is the recorded attribution for Sandbox — which
	// resolution tier chose it at the ORIGINAL launch. Every relaunch replays it
	// alongside the mode; dropping it would let a crash recovery or a
	// reincarnation silently re-credit a group default's containment to "this
	// launch", and the durable projection would then assert that erasure.
	SandboxModeSource      string
	TemporarySandboxMode   bool
	NormalSandbox          string
	NormalSandboxSource    string
	NormalSSHWorkaround    bool
	Approval               string
	ToolGovernance         string
	AutoReview             bool
	Model                  string
	Effort                 string
	AskUserQuestionTimeout string
	RemoteControl          bool
	AutoMemory             bool
	SSHWorkaround          bool
	ContextFeatures        map[string]string
	AutoCompactWindow      string
}

// activeSandboxImplementation returns the implementation for this process
// launch. A temporary sandbox-off override must disable both containment
// layers: replaying tclaude-layer here would turn the outer wall back on even
// though the harness-native mode is off. The durable implementation remains
// unchanged on the config so restore and clone continue to use the preserved
// normal posture.
func (c *durableRelaunchConfig) activeSandboxImplementation() string {
	if c != nil && c.TemporarySandboxMode {
		return string(sandboxpolicy.ImplementationHarnessBuiltin)
	}
	if c == nil {
		return ""
	}
	return c.SandboxImplementation
}

// temporarySandboxLaunchSnapshot derives the process-only policy paired with a
// temporary sandbox-off mode. Codex's raw full-access mode cannot accept any
// profile values; Claude Code and OpenCode can still receive plain environment
// entries, but no filesystem, network, break-glass, or agent-directory policy
// is represented as confinement while their sandbox is disabled.
func temporarySandboxLaunchSnapshot(harnessName string, stable *sandboxpolicy.Snapshot) *sandboxpolicy.Snapshot {
	if harnessName == harness.CodexName {
		omitted := sandboxpolicy.OmittedProfilesSnapshot()
		return &omitted
	}
	if stable == nil {
		return nil
	}
	unconfined := sandboxpolicy.UnconfinedLaunchSnapshot(*stable)
	return &unconfined
}

// relaunchProfileForSpawn freezes the already-resolved launch posture at an
// agent's birth. executeSpawn calls applyDefaultProfile before enrollment, so
// these values are the exact flags handed to the harness rather than the raw
// request or a later process-row observation.
func relaunchProfileForSpawn(p spawnParams) db.AgentRelaunchProfile {
	model := strings.TrimSpace(p.Model)
	contextWindowSize := int64(0)
	if harnessOrDefault(p.Harness) == harness.DefaultName {
		if strings.HasSuffix(model, "[1m]") {
			model = strings.TrimSuffix(model, "[1m]")
			contextWindowSize = oneMillionContextWindow
		}
	}
	sandboxMode := p.SandboxMode
	sandboxImplementation := strings.TrimSpace(p.SandboxImplementation)
	if sandboxImplementation == "" {
		sandboxImplementation = string(sandboxpolicy.ImplementationHarnessBuiltin)
	}
	sandboxModeSource := p.SandboxModeSource
	approvalPolicy := p.ApprovalPolicy
	toolGovernance := p.ToolGovernance
	autoReview := p.AutoReview
	effort := p.Effort
	askTimeout := p.AskUserQuestionTimeout
	remoteControl := p.RemoteControl
	autoMemory := p.AutoMemory
	sshWorkaround := p.SSHWorkaround
	// Freeze the trim map as KNOWN intent even when it is empty: an agent
	// deliberately spawned untrimmed should stay untrimmed, and a nil here would
	// instead read as "unknown" and let a later profile edit change it.
	contextFeatures := map[string]string{}
	for slug, state := range p.ContextFeatures {
		contextFeatures[slug] = state
	}
	// Frozen as KNOWN intent for the same reason, including the empty string: an
	// agent deliberately spawned with no pinned window should keep the model's
	// own threshold across relaunches, not inherit one a later profile edit adds.
	autoCompactWindow := p.AutoCompactWindow
	return db.AgentRelaunchProfile{
		Version:               db.RelaunchProfileVersion,
		SandboxMode:           &sandboxMode,
		SandboxImplementation: &sandboxImplementation,
		// Frozen alongside the mode it explains: a relaunch replays both, so an
		// agent keeps naming the profile that chose its containment instead of
		// degrading to an anonymous "this launch" on its first restart.
		SandboxModeSource:      &sandboxModeSource,
		ApprovalPolicy:         &approvalPolicy,
		ToolGovernance:         &toolGovernance,
		ApprovalAutoReview:     &autoReview,
		ModelID:                &model,
		Effort:                 &effort,
		ContextWindowSize:      &contextWindowSize,
		AskUserQuestionTimeout: &askTimeout,
		RemoteControl:          &remoteControl,
		AutoMemory:             &autoMemory,
		SSHWorkaround:          &sshWorkaround,
		ContextFeatures:        &contextFeatures,
		AutoCompactWindow:      &autoCompactWindow,
	}
}

// composeAgentRelaunchProfile overlays stable agent intent onto the
// conversation fallback one field at a time. This matters for migrated agents
// whose historical birth request captured only explicit overrides: nil means
// unknown, not an instruction to discard the last proven conversation value.
//
// The field-by-field overlay itself lives in db, because the same rule governs
// the recorded-posture read that non-daemon relaunch paths use (TCL-730); two
// copies of the list is how a field ends up carried on one path and dropped on
// the other.
func composeAgentRelaunchProfile(fallback, agent *db.AgentRelaunchProfile) *db.AgentRelaunchProfile {
	return db.ComposeAgentRelaunchProfile(fallback, agent)
}

func durableRelaunchConfigForConv(convID string) (*durableRelaunchConfig, error) {
	conversation, err := db.ConversationResumeProfileForConv(convID)
	if err != nil {
		return nil, fmt.Errorf("load durable conversation resume profile: %w", err)
	}
	if conversation == nil {
		if err := db.BackfillDurableRelaunchProfilesFromLatestSession(convID); err != nil {
			return nil, fmt.Errorf("backfill durable relaunch profiles: %w", err)
		}
		conversation, err = db.ConversationResumeProfileForConv(convID)
		if err != nil {
			return nil, fmt.Errorf("reload durable conversation resume profile: %w", err)
		}
		if conversation == nil {
			return nil, fmt.Errorf("durable conversation resume profile is missing")
		}
	}
	h, err := harness.Resolve(strings.TrimSpace(conversation.Harness))
	if err != nil {
		return nil, fmt.Errorf("resolve durable conversation harness %q: %w", conversation.Harness, err)
	}
	agentProfile, err := db.AgentRelaunchProfileForConv(convID)
	if err != nil {
		return nil, fmt.Errorf("load durable agent relaunch profile: %w", err)
	}
	if agentProfile == nil {
		if err := db.BackfillDurableRelaunchProfilesFromLatestSession(convID); err != nil {
			return nil, fmt.Errorf("backfill durable agent relaunch profile: %w", err)
		}
		agentProfile, err = db.AgentRelaunchProfileForConv(convID)
		if err != nil {
			return nil, fmt.Errorf("reload durable agent relaunch profile: %w", err)
		}
	}
	var temporarySandboxMode *string
	if agentProfile != nil {
		temporarySandboxMode = agentProfile.TemporarySandboxMode
	}
	// A plain tclaude conversation has no stable agent row by design. Its
	// conversation-owned fallback keeps ordinary conv/session resume working
	// after process history is pruned. Managed intent wins field-by-field; a nil
	// migrated field means unknown and retains the last proven conversation
	// value rather than replacing it with today's defaults.
	agentProfile = composeAgentRelaunchProfile(conversation.FallbackRelaunch, agentProfile)
	if agentProfile == nil {
		return nil, fmt.Errorf("durable relaunch fallback is missing")
	}

	sandboxMode, err := relaunchSandboxForProfile(agentProfile, h.Name)
	if err != nil {
		return nil, err
	}
	sandboxImplementation, err :=
		db.NormalSandboxImplementationForConv(convID, agentProfile)
	if err != nil {
		return nil, fmt.Errorf("resolve normal sandbox implementation: %w", err)
	}
	// Attribution follows the mode it explains, and only while that mode is the
	// one that was recorded: a profile whose SandboxMode is blank falls through
	// to the harness default above, and crediting a tier with a mode it did not
	// supply is the same false attribution in the other direction.
	sandboxModeSource := ""
	if agentProfile.SandboxModeSource != nil && strings.TrimSpace(*agentProfile.SandboxMode) != "" {
		sandboxModeSource = harness.SanitizeSandboxChosenBy(*agentProfile.SandboxModeSource)
	}
	normalSandboxMode := sandboxMode
	normalSandboxModeSource := sandboxModeSource
	if temporarySandboxMode != nil {
		if strings.TrimSpace(*temporarySandboxMode) == "" {
			return nil, fmt.Errorf("invalid temporary sandbox override: mode is empty")
		}
		sandboxMode, err = harness.ValidateSandboxMode(h, *temporarySandboxMode)
		if err != nil {
			return nil, fmt.Errorf("invalid temporary sandbox override: %w", err)
		}
		sandboxModeSource = db.TemporarySandboxModeSource
	}

	if agentProfile.ApprovalPolicy == nil {
		return nil, fmt.Errorf("durable agent relaunch profile has unknown approval policy")
	}
	approval, err := reconstructApproval(h.Name, *agentProfile.ApprovalPolicy)
	if err != nil {
		return nil, fmt.Errorf("invalid durable approval policy: %w", err)
	}
	autoReview := false
	if agentProfile.ApprovalAutoReview != nil {
		autoReview, err = harness.ResolveAutoReview(h, *agentProfile.ApprovalAutoReview)
		if err != nil {
			return nil, fmt.Errorf("invalid durable auto-review posture: %w", err)
		}
	}
	toolGovernance := ""
	if agentProfile.ToolGovernance != nil {
		toolGovernance = strings.TrimSpace(*agentProfile.ToolGovernance)
	}
	toolGovernance, err = harness.ResolveToolGovernance(h, toolGovernance)
	if err != nil {
		return nil, fmt.Errorf("invalid durable tool-governance policy: %w", err)
	}

	model := ""
	if agentProfile.ModelID != nil {
		model = strings.TrimSpace(*agentProfile.ModelID)
		if h.Name == harness.DefaultName {
			model = strings.TrimSuffix(model, "[1m]")
			if model != "" && agentProfile.ContextWindowSize != nil && *agentProfile.ContextWindowSize == oneMillionContextWindow {
				model += "[1m]"
			}
		}
		if model != "" {
			model, err = h.Models.ValidateModel(model)
			if err != nil {
				// Model selection is not an authority boundary. A historical or
				// removed model must not permanently wedge the agent; omitting the
				// override delegates to the harness default without broadening
				// filesystem or approval privileges.
				model = ""
			}
		}
	}
	effort := ""
	if agentProfile.Effort != nil {
		effort = strings.TrimSpace(*agentProfile.Effort)
		if effort != "" {
			effort, err = h.Models.ValidateEffort(effort)
			if err != nil {
				effort = ""
			}
		}
	}

	askTimeout := ""
	if agentProfile.AskUserQuestionTimeout != nil {
		askTimeout, err = harness.ResolveAskTimeoutMode(h, *agentProfile.AskUserQuestionTimeout)
		if err != nil {
			return nil, fmt.Errorf("invalid durable AskUserQuestion timeout: %w", err)
		}
	}
	remoteControl := agentProfile.RemoteControl != nil && *agentProfile.RemoteControl
	if remoteControl && !h.CanRemoteControl() {
		remoteControl = false
	}
	autoMemory := agentProfile.AutoMemory != nil && *agentProfile.AutoMemory
	if autoMemory && !h.CanAutoMemory() {
		autoMemory = false
	}
	// Re-resolve the recorded trims against the harness this relaunch will
	// actually use. A harness change (or a catalog entry retired since the agent
	// was born) drops the trims rather than wedging the relaunch: losing a trim
	// costs context, never correctness, so failing closed here would be the worse
	// trade.
	contextFeatures := map[string]string{}
	if agentProfile.ContextFeatures != nil && h.CanContextFeatures() {
		for slug, state := range *agentProfile.ContextFeatures {
			if _, ok := harness.LookupContextFeature(slug); !ok {
				continue
			}
			if normalized, stateErr := harness.ValidateContextFeatureState(state); stateErr == nil && normalized != "" {
				contextFeatures[slug] = normalized
			}
		}
	}

	// Re-resolve the recorded window against the harness this relaunch will
	// actually use, on the same fail-soft terms as the trims above: a harness
	// change (or a value that no longer parses) drops the pin rather than wedging
	// the relaunch. Losing the pin costs an earlier compaction, never correctness.
	autoCompactWindow := ""
	if agentProfile.AutoCompactWindow != nil {
		if resolved, acwErr := harness.ResolveAutoCompactWindow(h, *agentProfile.AutoCompactWindow); acwErr == nil {
			autoCompactWindow = resolved
		}
	}

	sshWorkaround, err := harness.ResolveSSHWorkaround(h, agentProfile.SSHWorkaround)
	if err != nil {
		return nil, fmt.Errorf("invalid durable SSH workaround posture: %w", err)
	}
	normalSSHWorkaround := sshWorkaround && normalSandboxMode == harness.SandboxManagedProfile
	if sandboxMode != harness.SandboxManagedProfile {
		sshWorkaround = false
	}

	return &durableRelaunchConfig{
		Harness:                h.Name,
		Cwd:                    strings.TrimSpace(conversation.Cwd),
		ResumeProvenance:       conversation.ResumeProvenance,
		Sandbox:                sandboxMode,
		SandboxImplementation:  string(sandboxImplementation),
		SandboxModeSource:      sandboxModeSource,
		TemporarySandboxMode:   temporarySandboxMode != nil,
		NormalSandbox:          normalSandboxMode,
		NormalSandboxSource:    normalSandboxModeSource,
		NormalSSHWorkaround:    normalSSHWorkaround,
		Approval:               approval,
		ToolGovernance:         toolGovernance,
		AutoReview:             autoReview,
		Model:                  model,
		Effort:                 effort,
		AskUserQuestionTimeout: askTimeout,
		RemoteControl:          remoteControl,
		AutoMemory:             autoMemory,
		SSHWorkaround:          sshWorkaround,
		ContextFeatures:        contextFeatures,
		AutoCompactWindow:      autoCompactWindow,
	}, nil
}
