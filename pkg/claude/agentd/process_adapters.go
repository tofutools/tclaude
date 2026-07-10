package agentd

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	processexec "github.com/tofutools/tclaude/pkg/claude/process/exec"
	"github.com/tofutools/tclaude/pkg/claude/process/model"
	"github.com/tofutools/tclaude/pkg/claude/process/state"
)

var processCommandIDPattern = regexp.MustCompile(`^cmd_[a-f0-9]{24}$`)

type processAgentAdapter struct{}

func (processAgentAdapter) Validate(request processexec.Request) error {
	if request.Performer.Kind != model.PerformerAgent {
		return fmt.Errorf("agent adapter received performer kind %q", request.Performer.Kind)
	}
	if !processCommandIDPattern.MatchString(request.Command.ID) {
		return fmt.Errorf("invalid process command id %q", request.Command.ID)
	}
	if strings.TrimSpace(request.Performer.Profile) == "" {
		return fmt.Errorf("agent performer requires a spawn profile")
	}
	if strings.TrimSpace(request.Performer.Prompt) == "" {
		return fmt.Errorf("agent performer requires a prompt")
	}
	return nil
}

func (processAgentAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	return processexec.Observation{}, fmt.Errorf("agent performers are asynchronous")
}

func (a processAgentAdapter) Dispatch(_ context.Context, request processexec.Request) (processexec.DispatchResult, error) {
	if err := a.Validate(request); err != nil {
		return processexec.DispatchResult{}, err
	}
	if existing, err := db.AgentForProcessCommand(request.Command.ID); err != nil {
		return processexec.DispatchResult{}, err
	} else if existing != nil {
		return agentDispatchResult(existing.AgentID, request), nil
	}
	profile, err := db.GetSpawnProfile(strings.TrimSpace(request.Performer.Profile))
	if err != nil {
		return processexec.DispatchResult{}, err
	}
	if profile == nil {
		return processexec.DispatchResult{}, fmt.Errorf("spawn profile %q not found", request.Performer.Profile)
	}
	p, err := processSpawnParams(profile, request)
	if err != nil {
		return processexec.DispatchResult{}, err
	}
	outcome, fail := executeSpawn(nil, p)
	if fail != nil {
		return processexec.DispatchResult{}, fmt.Errorf("%s", fail.Msg)
	}
	agentID, err := db.AgentIDForConv(outcome.ConvID)
	if err != nil {
		return processexec.DispatchResult{}, err
	}
	if agentID == "" {
		return processexec.DispatchResult{}, fmt.Errorf("spawned process agent has no stable identity")
	}
	return agentDispatchResult(agentID, request), nil
}

func agentDispatchResult(agentID string, request processexec.Request) processexec.DispatchResult {
	return processexec.DispatchResult{
		ExternalRef:      agentID,
		Assignee:         "agent:" + agentID,
		Summary:          "Agent work for " + request.Input.NodeID,
		AvailableActions: []string{"pass", "fail", "ask-changes"},
		CreateObligation: true,
	}
}

func processSpawnParams(profile *db.SpawnProfile, request processexec.Request) (spawnParams, error) {
	h, err := resolveSpawnHarness(profile.Harness)
	if err != nil {
		return spawnParams{}, err
	}
	tiers := []launchProfileTier{{profile: profile, source: profileSource(profile, agent.ProvCLIProfileSource)}}
	modelName, _, _, fail := resolveStringLaunchField("model", request.Performer.Model, h.Name, tiers,
		func(p *db.SpawnProfile) string { return p.Model }, h.Models.ValidateModel)
	if fail != nil {
		return spawnParams{}, fmt.Errorf("%s", fail.Msg)
	}
	effort, _, _, fail := resolveStringLaunchField("effort", request.Performer.Effort, h.Name, tiers,
		func(p *db.SpawnProfile) string { return p.Effort }, h.Models.ValidateEffort)
	if fail != nil {
		return spawnParams{}, fmt.Errorf("%s", fail.Msg)
	}
	sandbox, err := harness.ResolveSandboxMode(h, profile.Sandbox)
	if err != nil {
		return spawnParams{}, err
	}
	approval, err := harness.ResolveApprovalPolicy(h, profile.Approval)
	if err != nil {
		return spawnParams{}, err
	}
	askTimeout, err := harness.ResolveAskTimeoutMode(h, profile.AskUserQuestionTimeout)
	if err != nil {
		return spawnParams{}, err
	}
	autoReview := profile.AutoReview != nil && *profile.AutoReview
	if autoReview, err = harness.ResolveAutoReview(h, autoReview); err != nil {
		return spawnParams{}, err
	}
	trustDir := profile.TrustDir != nil && *profile.TrustDir
	if trustDir, err = harness.ResolveTrustDir(h, trustDir); err != nil {
		return spawnParams{}, err
	}
	remoteControl := profile.RemoteControl != nil && *profile.RemoteControl
	if remoteControl, err = harness.ResolveRemoteControl(h, remoteControl); err != nil {
		return spawnParams{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return spawnParams{}, err
	}
	brief := processAgentBrief(request)
	if profile.InitialMessage != "" {
		brief = strings.TrimSpace(profile.InitialMessage) + "\n\n" + brief
	}
	return spawnParams{
		Name:                   "process-" + strings.TrimPrefix(request.Command.ID, "cmd_")[:12],
		Role:                   profile.Role,
		Descr:                  "process performer for " + request.Input.RunID + "/" + request.Input.NodeID,
		InitialMessage:         brief,
		Cwd:                    cwd,
		Effort:                 effort,
		Model:                  modelName,
		Harness:                profile.Harness,
		SandboxMode:            sandbox,
		ApprovalPolicy:         approval,
		AskUserQuestionTimeout: askTimeout,
		AutoReview:             autoReview,
		AutoReviewSet:          profile.AutoReview != nil,
		TrustDir:               trustDir,
		TrustDirSet:            profile.TrustDir != nil,
		RemoteControl:          remoteControl,
		PermissionOverrides:    profile.PermissionOverrides,
		Timeout:                30 * time.Second,
		ProcessCommandID:       request.Command.ID,
	}, nil
}

func processAgentBrief(request processexec.Request) string {
	return fmt.Sprintf(`You are performing a tclaude process slot.

Run: %s
Node: %s
Attempt: %d
Command: %s

Task:
%s

When the task is complete, report a structured result through the daemon:
tclaude process report %s %s --command %s --verdict <action> --evidence <ref> [--feedback <text>]

Use pass/fail for task work. For a decision, use the selected decision-edge name. Unknown actions are rejected. A result without an evidence ref is not complete. Do not claim another actor identity; agentd derives your stable identity from the calling pane.`,
		request.Input.RunID, request.Input.NodeID, request.Input.Attempt, request.Command.ID,
		request.Performer.Prompt, request.Input.RunID, request.Input.NodeID, request.Command.ID)
}

func (processAgentAdapter) ReconcileDeferred(_ context.Context, request processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	agent, err := db.AgentForProcessCommand(request.Command.ID)
	if err != nil {
		return processexec.Observation{}, processexec.DeferredMissing, err
	}
	if agent == nil {
		return processexec.Observation{}, processexec.DeferredMissing, nil
	}
	return processexec.Observation{}, processexec.DeferredInFlight, nil
}

func (processAgentAdapter) Contact(_ context.Context, request processexec.Request, escalation bool) error {
	agent, err := db.AgentForProcessCommand(request.Command.ID)
	if err != nil {
		return err
	}
	if escalation {
		_, _, target, scheduleErr := processexec.ContactScheduleFor(request.Performer)
		if scheduleErr != nil {
			return scheduleErr
		}
		_, err = recordHumanMessageWithProcess("", "Process performer escalation",
			fmt.Sprintf("Process %s node %s exhausted its agent nudge budget. Command: %s. Escalation target: %s", request.Input.RunID, request.Input.NodeID, request.Command.ID, target),
			request.Input.RunID, request.Input.NodeID, request.Command.ID)
		return err
	}
	if agent == nil {
		return fmt.Errorf("process agent for command %s is missing", request.Command.ID)
	}
	convID := agent.CurrentConvID
	_, err = db.InsertAgentMessage(&db.AgentMessage{
		GroupID:      0,
		FromConv:     "",
		ToConv:       convID,
		Subject:      "Process nudge",
		Body:         fmt.Sprintf("Please continue process %s node %s. Report with command %s when complete.", request.Input.RunID, request.Input.NodeID, request.Command.ID),
		ToRecipients: []string{convID},
	})
	if err == nil {
		enqueueDeliveryForConv(convID)
	}
	return err
}

func (processAgentAdapter) Activity(_ context.Context, request processexec.Request, since time.Time) (processexec.Activity, error) {
	agent, err := db.AgentForProcessCommand(request.Command.ID)
	if err != nil || agent == nil {
		return processexec.Activity{}, err
	}
	sessionRow, err := db.FindSessionByConvID(agent.CurrentConvID)
	if err != nil || sessionRow == nil || !sessionRow.LastHook.After(since) {
		return processexec.Activity{}, err
	}
	if sessionRow.StatusDetail != "UserPromptSubmit" {
		return processexec.Activity{Recovered: true, At: sessionRow.LastHook}, nil
	}
	automated, err := processAutomatedDeliveryNear(agent, sessionRow.LastHook)
	if err != nil {
		return processexec.Activity{}, err
	}
	if automated {
		return processexec.Activity{AutomatedDelivery: true, At: sessionRow.LastHook}, nil
	}
	return processexec.Activity{HumanInteracted: true, At: sessionRow.LastHook}, nil
}

const processDeliveryCorrelationWindow = 10 * time.Second

// processAutomatedDeliveryNear correlates a UserPromptSubmit hook with the
// daemon's durable inbox delivery timestamp. Hooks do not carry prompt origin,
// so an ambiguous hook is preemptive only when it is not near a known tclaude
// delivery. This deliberately favors a missed preemption over pausing the
// automation because of its own nudge.
func processAutomatedDeliveryNear(agent *db.Agent, at time.Time) (bool, error) {
	messages, err := db.ListInboxForActor(agent.CurrentConvID, agent.AgentID, 32)
	if err != nil {
		return false, err
	}
	for _, message := range messages {
		if message.DeliveredAt.IsZero() {
			continue
		}
		delta := message.DeliveredAt.Sub(at)
		if delta < 0 {
			delta = -delta
		}
		if delta <= processDeliveryCorrelationWindow {
			return true, nil
		}
	}
	return false, nil
}

type processHumanAdapter struct{}

func (processHumanAdapter) Validate(request processexec.Request) error {
	if request.Performer.Kind != model.PerformerHuman {
		return fmt.Errorf("human adapter received performer kind %q", request.Performer.Kind)
	}
	if !processCommandIDPattern.MatchString(request.Command.ID) {
		return fmt.Errorf("invalid process command id %q", request.Command.ID)
	}
	if strings.TrimSpace(request.Performer.Ask) == "" && strings.TrimSpace(request.Performer.Prompt) == "" {
		return fmt.Errorf("human performer requires instructions")
	}
	if err := model.ValidateChoiceRouting(request.Performer, request.Command.Kind == state.CommandKindRecordDecision); err != nil {
		return fmt.Errorf("human performer choice routing: %w", err)
	}
	return nil
}

func (processHumanAdapter) Perform(context.Context, processexec.Request) (processexec.Observation, error) {
	return processexec.Observation{}, fmt.Errorf("human performers are asynchronous")
}

func (a processHumanAdapter) Dispatch(_ context.Context, request processexec.Request) (processexec.DispatchResult, error) {
	if err := a.Validate(request); err != nil {
		return processexec.DispatchResult{}, err
	}
	assignee := strings.TrimSpace(request.Performer.Assignee)
	if assignee == "" {
		assignee = strings.TrimSpace(request.Performer.Profile)
	}
	if assignee == "" {
		assignee = "human:operator"
	} else if !strings.HasPrefix(assignee, "human:") && !strings.HasPrefix(assignee, "role:") {
		assignee = "human:" + assignee
	}
	summary := strings.TrimSpace(request.Performer.Ask)
	if summary == "" {
		summary = strings.TrimSpace(request.Performer.Prompt)
	}
	existing, err := db.FindHumanMessageForProcessCommand(request.Command.ID, "Process obligation")
	if err != nil {
		return processexec.DispatchResult{}, err
	}
	if existing == nil {
		_, err = recordHumanMessageWithProcess("", "Process obligation",
			fmt.Sprintf("%s\n\nRun: %s\nNode: %s\nAttempt: %d\nResolve with: tclaude process resolve %s %s --verdict <verdict> --actor %s", summary, request.Input.RunID, request.Input.NodeID, request.Input.Attempt, request.Input.RunID, request.Input.NodeID, assignee),
			request.Input.RunID, request.Input.NodeID, request.Command.ID)
		if err != nil {
			return processexec.DispatchResult{}, err
		}
	}
	actions := []string{"approve", "reject", "ask-changes"}
	if request.Command.Kind != state.CommandKindRecordDecision && len(request.Performer.Choices) > 0 {
		actions = make([]string, 0, len(request.Performer.Choices))
		for _, choice := range request.Performer.Choices {
			actions = append(actions, strings.TrimSpace(choice))
		}
	}
	return processexec.DispatchResult{
		ExternalRef:      "obligation:" + request.Command.ID,
		Assignee:         assignee,
		Summary:          summary,
		AvailableActions: actions,
		CreateObligation: true,
	}, nil
}

func (a processHumanAdapter) ReconcileDeferred(_ context.Context, request processexec.Request) (processexec.Observation, processexec.DeferredStatus, error) {
	if err := a.Validate(request); err != nil {
		return processexec.Observation{}, processexec.DeferredMissing, err
	}
	return processexec.Observation{}, processexec.DeferredInFlight, nil
}

func (processHumanAdapter) Contact(_ context.Context, request processexec.Request, escalation bool) error {
	subject := "Process reminder"
	body := fmt.Sprintf("Waiting on process %s node %s (command %s).", request.Input.RunID, request.Input.NodeID, request.Command.ID)
	if escalation {
		subject = "Process obligation escalation"
		_, _, target, err := processexec.ContactScheduleFor(request.Performer)
		if err != nil {
			return err
		}
		body += " Escalation target: " + target + "."
	}
	_, err := recordHumanMessageWithProcess("", subject, body, request.Input.RunID, request.Input.NodeID, request.Command.ID)
	return err
}

func (processHumanAdapter) Activity(context.Context, processexec.Request, time.Time) (processexec.Activity, error) {
	return processexec.Activity{}, nil
}

var _ processexec.DeferredAdapter = processAgentAdapter{}
var _ processexec.ContactAdapter = processAgentAdapter{}
var _ processexec.DeferredAdapter = processHumanAdapter{}
var _ processexec.ContactAdapter = processHumanAdapter{}
