package app

import (
	"context"
	"errors"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (s *Service) ListTeamDeployments(ctx context.Context, req ListTeamDeploymentsRequest) ([]TeamDeploymentResult, error) {
	deployments, err := s.store.ListTeamDeployments(ctx, req.GroupID)
	if err != nil {
		return nil, err
	}
	results := make([]TeamDeploymentResult, 0, len(deployments))
	for _, deployment := range deployments {
		if req.Principal.Kind != model.PrincipalOperator {
			err = s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadStatus, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: deployment.GroupID}}, s.now().UTC())
			if err != nil {
				continue
			}
		}
		results = append(results, TeamDeploymentResult{Deployment: deployment})
	}
	return results, nil
}

func (s *Service) RebriefDeployment(ctx context.Context, req RebriefDeploymentRequest) (TeamDeploymentResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return TeamDeploymentResult{}, err
	}
	if req.ExpectedRevision == 0 || req.Definition.Kind != model.DefinitionTeam {
		return TeamDeploymentResult{}, fail(ErrInvalid, "expected deployment revision and team definition are required")
	}
	digest := contentHash(struct {
		Deployment model.DeploymentID
		Expected   model.Revision
		Definition model.DefinitionRef
	}{req.DeploymentID, req.ExpectedRevision, req.Definition})
	deployment, err := s.store.TeamDeployment(ctx, req.DeploymentID)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	revision, err := s.store.DefinitionRevision(ctx, req.Definition.RevisionID)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	if revision.DefinitionID != req.Definition.DefinitionID || revision.ContentHash != req.Definition.ContentHash || revision.Team == nil {
		return TeamDeploymentResult{}, fail(ErrConflict, "rebrief definition is not the pinned revision")
	}
	if err = compatibleRebriefRoster(deployment, *revision.Team); err != nil {
		return TeamDeploymentResult{}, err
	}
	// BeginTeamRebrief returns an exact prior admission before comparing the
	// current deployment revision or current authority.
	deployment, admitted, repeated, err := s.store.BeginTeamRebrief(ctx, req.DeploymentID, req.ExpectedRevision, req.Definition, req.Context.Principal, req.Context.RequestID, digest, s.now().UTC())
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	if repeated && admitted.State == model.TeamRebriefCompleted {
		return TeamDeploymentResult{Deployment: deployment}, nil
	}
	operations := make(map[string][]model.OperationID, len(deployment.Members))
	keys := sortedMemberKeys(deployment.Members)
	for _, key := range keys {
		body := rebriefBody(key, *revision.Team)
		if body == "" {
			continue
		}
		result, sendErr := s.SendMessage(ctx, SendMessageRequest{RequestContext: RequestContext{Principal: req.Context.Principal, RequestID: model.RequestID(deterministicOrchestrationID("request_", string(req.Context.RequestID)+":rebrief:"+key))}, Subject: "Team rebrief", To: model.MessageAudience{AgentIDs: []model.AgentID{deployment.Members[key]}}, Body: body})
		if sendErr != nil {
			return TeamDeploymentResult{Deployment: deployment}, sendErr
		}
		operations[key] = append(operations[key], result.Operation.ID)
	}
	deployment, err = s.store.CompleteTeamRebrief(ctx, deployment.ID, req.Context.Principal, req.Context.RequestID, operations, s.now().UTC())
	return TeamDeploymentResult{Deployment: deployment}, err
}

func (s *Service) AdvanceAdvisoryPhase(ctx context.Context, req AdvanceAdvisoryPhaseRequest) (TeamDeploymentResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return TeamDeploymentResult{}, err
	}
	if req.ExpectedRevision == 0 {
		return TeamDeploymentResult{}, fail(ErrInvalid, "expected deployment revision is required")
	}
	deployment, err := s.store.TeamDeployment(ctx, req.DeploymentID)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	revision, err := s.store.DefinitionRevision(ctx, deployment.Definition.RevisionID)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	if deployment.Revision == req.ExpectedRevision && (revision.Team == nil || int(deployment.AdvisoryPhase)+1 >= len(revision.Team.AdvisoryPhases)) {
		return TeamDeploymentResult{}, fail(ErrConflict, "deployment has no next advisory phase")
	}
	digest := contentHash(struct {
		Deployment model.DeploymentID
		Expected   model.Revision
	}{req.DeploymentID, req.ExpectedRevision})
	deployment, err = s.store.AdvanceTeamAdvisoryPhase(ctx, req.DeploymentID, req.ExpectedRevision, req.Context.Principal, req.Context.RequestID, digest, s.now().UTC())
	return TeamDeploymentResult{Deployment: deployment}, err
}

func (s *Service) StandDownDeployment(ctx context.Context, req StandDownDeploymentRequest) (TeamDeploymentResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return TeamDeploymentResult{}, err
	}
	if req.ExpectedRevision == 0 || strings.TrimSpace(req.Reason) == "" {
		return TeamDeploymentResult{}, fail(ErrInvalid, "expected deployment revision and reason are required")
	}
	digest := contentHash(struct {
		Deployment model.DeploymentID
		Expected   model.Revision
		Reason     string
	}{req.DeploymentID, req.ExpectedRevision, strings.TrimSpace(req.Reason)})
	deployment, err := s.store.BeginTeamStandDown(ctx, req.DeploymentID, req.ExpectedRevision, req.Context.Principal, req.Context.RequestID, digest, s.now().UTC())
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	deployment, err = s.continueTeamStandDown(ctx, deployment, req.Context.Principal, strings.TrimSpace(req.Reason))
	return TeamDeploymentResult{Deployment: deployment}, err
}

func compatibleRebriefRoster(deployment model.TeamDeployment, team model.TeamDefinition) error {
	if len(deployment.Members) != len(team.Members) {
		return fail(ErrConflict, "rebrief definition changes the stable member roster")
	}
	for _, member := range team.Members {
		if deployment.Members[member.Key] == "" {
			return fail(ErrConflict, "rebrief definition changes stable member key %s", member.Key)
		}
	}
	return nil
}

func rebriefBody(memberKey string, team model.TeamDefinition) string {
	var bodies []string
	for _, brief := range team.Briefings {
		for _, key := range brief.MemberKeys {
			if key == memberKey {
				bodies = append(bodies, brief.Body)
				break
			}
		}
	}
	return strings.TrimSpace(strings.Join(bodies, "\n\n"))
}

func sortedMemberKeys(members map[string]model.AgentID) []string {
	keys := make([]string, 0, len(members))
	for key := range members {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s *Service) continueTeamStandDown(ctx context.Context, deployment model.TeamDeployment, principal model.Principal, reason string) (model.TeamDeployment, error) {
	for _, ruleID := range deployment.OwnedAutomationRuleIDs {
		rule, err := s.store.AutomationRule(ctx, ruleID)
		if err != nil {
			return deployment, err
		}
		if rule.Rule.Enabled {
			if _, err = s.store.SetAutomationRuleEnabled(ctx, ruleID, rule.Rule.Revision, false, principal, s.now().UTC()); err != nil {
				return deployment, err
			}
		}
	}
	if run, err := s.store.WorkRun(ctx, deployment.WorkRunID); err == nil && run.Run.State != model.WorkRunCancelled && run.Run.State != model.WorkRunFailed && run.Run.State != model.WorkRunSucceeded {
		_, err = s.CancelWork(ctx, CancelWorkRequest{Context: RequestContext{Principal: principal, RequestID: model.RequestID(deterministicOrchestrationID("request_", string(deployment.ID)+":standdown:cancel"))}, WorkRunID: deployment.WorkRunID, ExpectedRunRevision: run.Run.Revision, Reason: reason})
		if err != nil && !errors.Is(err, ErrConflict) {
			return deployment, err
		}
	}
	settled := true
	for _, key := range sortedMemberKeys(deployment.Members) {
		agent, err := s.store.Agent(ctx, deployment.Members[key])
		if err != nil {
			return deployment, err
		}
		if agent.PrimaryExecutionID != "" {
			execution, readErr := s.store.Execution(ctx, agent.PrimaryExecutionID)
			if readErr != nil {
				return deployment, readErr
			}
			if execution.State != model.ExecutionExited && execution.State != model.ExecutionFailed {
				_, stopErr := s.Stop(ctx, StopRequest{RequestContext: RequestContext{Principal: principal, RequestID: model.RequestID(deterministicOrchestrationID("request_", string(deployment.ID)+":standdown:stop:"+key))}, ExecutionID: execution.ID})
				if stopErr != nil {
					return deployment, stopErr
				}
				execution, readErr = s.store.Execution(ctx, execution.ID)
				if readErr != nil || (execution.State != model.ExecutionExited && execution.State != model.ExecutionFailed) {
					settled = false
					continue
				}
			}
			if use, useErr := s.store.WorkspaceUseForExecution(ctx, execution.ID); useErr == nil && use.ReleasedAt == nil {
				if releaseErr := s.store.ReleaseWorkspaceUse(ctx, use.ID, execution.ID, s.now().UTC()); releaseErr != nil && !errors.Is(releaseErr, ErrConflict) {
					return deployment, releaseErr
				}
			}
		}
		if agent.Lifecycle == model.AgentActive {
			if _, err = s.RetireAgent(ctx, RetireAgentRequest{Context: principal, ID: agent.ID, ExpectedRevision: agent.Revision, Reason: reason}); err != nil {
				return deployment, err
			}
		}
	}
	if !settled {
		return s.store.TeamDeployment(ctx, deployment.ID)
	}
	current, err := s.store.TeamDeployment(ctx, deployment.ID)
	if err != nil {
		return deployment, err
	}
	if current.State == model.DeploymentStopped {
		return current, nil
	}
	return s.store.UpdateTeamDeployment(ctx, current.ID, current.Revision, model.DeploymentStopped, current.AdvisoryPhase, s.now().UTC())
}

func (s *Service) reconcileDeploymentMember(ctx context.Context, record WorkRunRecord, attempt model.WorkNodeAttempt, deployment model.TeamDeployment) (WorkRunRecord, error) {
	execution, err := s.store.Execution(ctx, attempt.ExecutionID)
	if err != nil {
		return record, err
	}
	memberKey := ""
	for key, id := range deployment.Members {
		if id == attempt.Performer.Agent.AgentID {
			memberKey = key
			break
		}
	}
	revision, err := s.store.DefinitionRevision(ctx, deployment.Definition.RevisionID)
	if err != nil || revision.Team == nil {
		return record, err
	}
	requiredReady, requiredBriefs := teamMemberGate(*revision.Team, memberKey)
	hasAfterReady := memberHasAfterReadyBrief(*revision.Team, memberKey)
	if execution.ContextReadiness != model.ContextReadinessReady && (requiredReady || hasAfterReady) {
		if runtime, runtimeErr := s.runtimeFor(ctx, execution); runtimeErr == nil {
			if observation, observeErr := runtime.Observe(ctx); observeErr == nil && observation.Context == ports.ContextReady {
				execution, err = s.store.RecordExecutionReadiness(ctx, execution.ID, execution.Attempt, model.ContextReadinessReady, s.now().UTC())
				if err != nil {
					return record, err
				}
			}
		}
	}
	if requiredReady && execution.ContextReadiness != model.ContextReadinessReady {
		return record, nil
	}
	if execution.ContextReadiness == model.ContextReadinessReady && hasAfterReady {
		deployment, err = s.deliverAfterReadyBriefings(ctx, deployment, memberKey, *revision.Team)
		if err != nil {
			return record, err
		}
	}
	if requiredBriefs && !requiredTeamBriefingsAdmitted(deployment, memberKey, *revision.Team) {
		return record, nil
	}
	return s.store.ApplyGraphTransition(ctx, s.graphOutcomeTransition(record, attempt, model.WorkOutcomeVerified, "member ready with required briefings admitted"))
}

func teamMemberGate(team model.TeamDefinition, memberKey string) (bool, bool) {
	for _, wave := range team.Waves {
		for _, key := range wave.MemberKeys {
			if key == memberKey {
				return wave.RequiredReady, wave.RequiredBriefs
			}
		}
	}
	return false, false
}

func memberHasAfterReadyBrief(team model.TeamDefinition, memberKey string) bool {
	for _, brief := range team.Briefings {
		if brief.Timing != model.BriefingAfterReady {
			continue
		}
		for _, key := range brief.MemberKeys {
			if key == memberKey {
				return true
			}
		}
	}
	return false
}

func (s *Service) deliverAfterReadyBriefings(ctx context.Context, deployment model.TeamDeployment, memberKey string, team model.TeamDefinition) (model.TeamDeployment, error) {
	if len(deployment.BriefingOperationIDs[memberKey]) != 0 {
		return deployment, nil
	}
	principal, err := s.store.TeamDeploymentRequester(ctx, deployment.ID)
	if err != nil {
		return deployment, err
	}
	var operations []model.OperationID
	for _, brief := range team.Briefings {
		if brief.Timing != model.BriefingAfterReady {
			continue
		}
		selected := false
		for _, key := range brief.MemberKeys {
			selected = selected || key == memberKey
		}
		if !selected {
			continue
		}
		result, sendErr := s.SendMessage(ctx, SendMessageRequest{RequestContext: RequestContext{Principal: principal, RequestID: model.RequestID(deterministicOrchestrationID("request_", string(deployment.ID)+":brief:"+memberKey+":"+brief.ID))}, Subject: "Team briefing", To: model.MessageAudience{AgentIDs: []model.AgentID{deployment.Members[memberKey]}}, Body: brief.Body})
		if sendErr != nil {
			if brief.Required {
				return deployment, sendErr
			}
			continue
		}
		operations = append(operations, result.Operation.ID)
	}
	return s.store.RecordTeamBriefingOperations(ctx, deployment.ID, deployment.Revision, memberKey, operations, s.now().UTC())
}

func requiredTeamBriefingsAdmitted(deployment model.TeamDeployment, memberKey string, team model.TeamDefinition) bool {
	required := 0
	for _, brief := range team.Briefings {
		if brief.Timing != model.BriefingAfterReady || !brief.Required {
			continue
		}
		for _, key := range brief.MemberKeys {
			if key == memberKey {
				required++
				break
			}
		}
	}
	return len(deployment.BriefingOperationIDs[memberKey]) >= required
}
