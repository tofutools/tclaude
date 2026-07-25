package agentd

import (
	"fmt"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// durableRelaunchConfig is the validated composition of stable agent intent
// and conversation-owned resume facts. No field is sourced from a predecessor
// session row.
type durableRelaunchConfig struct {
	Harness                string
	Cwd                    string
	ResumeProvenance       string
	Sandbox                string
	Approval               string
	ToolGovernance         string
	AutoReview             bool
	Model                  string
	Effort                 string
	AskUserQuestionTimeout string
	RemoteControl          bool
	AutoMemory             bool
	ContextFeatures        map[string]string
	AutoCompactWindow      string
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
	approvalPolicy := p.ApprovalPolicy
	toolGovernance := p.ToolGovernance
	autoReview := p.AutoReview
	effort := p.Effort
	askTimeout := p.AskUserQuestionTimeout
	remoteControl := p.RemoteControl
	autoMemory := p.AutoMemory
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
		Version:                db.RelaunchProfileVersion,
		SandboxMode:            &sandboxMode,
		ApprovalPolicy:         &approvalPolicy,
		ToolGovernance:         &toolGovernance,
		ApprovalAutoReview:     &autoReview,
		ModelID:                &model,
		Effort:                 &effort,
		ContextWindowSize:      &contextWindowSize,
		AskUserQuestionTimeout: &askTimeout,
		RemoteControl:          &remoteControl,
		AutoMemory:             &autoMemory,
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

	if agentProfile.ApprovalPolicy == nil {
		return nil, fmt.Errorf("durable agent relaunch profile has unknown approval policy")
	}
	approval := strings.TrimSpace(*agentProfile.ApprovalPolicy)
	if approval == "" {
		approval = approvalForHarness(h.Name)
	} else {
		approval, err = harness.ValidateApprovalPolicy(h, approval)
		if err != nil {
			return nil, fmt.Errorf("invalid durable approval policy: %w", err)
		}
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

	return &durableRelaunchConfig{
		Harness:                h.Name,
		Cwd:                    strings.TrimSpace(conversation.Cwd),
		ResumeProvenance:       conversation.ResumeProvenance,
		Sandbox:                sandboxMode,
		Approval:               approval,
		ToolGovernance:         toolGovernance,
		AutoReview:             autoReview,
		Model:                  model,
		Effort:                 effort,
		AskUserQuestionTimeout: askTimeout,
		RemoteControl:          remoteControl,
		AutoMemory:             autoMemory,
		ContextFeatures:        contextFeatures,
		AutoCompactWindow:      autoCompactWindow,
	}, nil
}
