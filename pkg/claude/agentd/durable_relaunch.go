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
	AutoReview             bool
	Model                  string
	Effort                 string
	AskUserQuestionTimeout string
	RemoteControl          bool
	AutoMemory             bool
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
	autoReview := p.AutoReview
	effort := p.Effort
	askTimeout := p.AskUserQuestionTimeout
	remoteControl := p.RemoteControl
	autoMemory := p.AutoMemory
	return db.AgentRelaunchProfile{
		Version:                db.RelaunchProfileVersion,
		SandboxMode:            &sandboxMode,
		ApprovalPolicy:         &approvalPolicy,
		ApprovalAutoReview:     &autoReview,
		ModelID:                &model,
		Effort:                 &effort,
		ContextWindowSize:      &contextWindowSize,
		AskUserQuestionTimeout: &askTimeout,
		RemoteControl:          &remoteControl,
		AutoMemory:             &autoMemory,
	}
}

// composeAgentRelaunchProfile overlays stable agent intent onto the
// conversation fallback one field at a time. This matters for migrated agents
// whose historical birth request captured only explicit overrides: nil means
// unknown, not an instruction to discard the last proven conversation value.
func composeAgentRelaunchProfile(fallback, agent *db.AgentRelaunchProfile) *db.AgentRelaunchProfile {
	if agent == nil {
		return fallback
	}
	if fallback == nil {
		return agent
	}
	merged := *fallback
	merged.Version = agent.Version
	if agent.SandboxMode != nil {
		merged.SandboxMode = agent.SandboxMode
	}
	if agent.ApprovalPolicy != nil {
		merged.ApprovalPolicy = agent.ApprovalPolicy
	}
	if agent.ApprovalAutoReview != nil {
		merged.ApprovalAutoReview = agent.ApprovalAutoReview
	}
	if agent.ModelID != nil {
		merged.ModelID = agent.ModelID
	}
	if agent.Effort != nil {
		merged.Effort = agent.Effort
	}
	if agent.ContextWindowSize != nil {
		merged.ContextWindowSize = agent.ContextWindowSize
	}
	if agent.AskUserQuestionTimeout != nil {
		merged.AskUserQuestionTimeout = agent.AskUserQuestionTimeout
	}
	if agent.RemoteControl != nil {
		merged.RemoteControl = agent.RemoteControl
	}
	if agent.AutoMemory != nil {
		merged.AutoMemory = agent.AutoMemory
	}
	return &merged
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

	return &durableRelaunchConfig{
		Harness:                h.Name,
		Cwd:                    strings.TrimSpace(conversation.Cwd),
		ResumeProvenance:       conversation.ResumeProvenance,
		Sandbox:                sandboxMode,
		Approval:               approval,
		AutoReview:             autoReview,
		Model:                  model,
		Effort:                 effort,
		AskUserQuestionTimeout: askTimeout,
		RemoteControl:          remoteControl,
		AutoMemory:             autoMemory,
	}, nil
}
