package app

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

func (s *Service) DeployTeam(ctx context.Context, req DeployTeamRequest) (TeamDeploymentResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return TeamDeploymentResult{}, err
	}
	if err := req.DeploymentID.Validate(); err != nil {
		return TeamDeploymentResult{}, fail(ErrInvalid, "%v", err)
	}
	target, err := normalizeTeamTarget(req.Instantiation)
	if err != nil {
		return TeamDeploymentResult{}, fail(ErrInvalid, "%v", err)
	}
	req.Instantiation.Target, req.Instantiation.GroupID = target, ""
	requestDigest := contentHash(struct {
		DeploymentID  model.DeploymentID
		Instantiation model.TeamInstantiation
	}{req.DeploymentID, req.Instantiation})
	// Exact retries precede occurrence, definition, group, role, workspace, and
	// authority reads. Changed payload under one request identity conflicts.
	if prior, ok, readErr := s.store.TeamDeploymentByRequest(ctx, req.Context.Principal, req.Context.RequestID, requestDigest); readErr != nil {
		return TeamDeploymentResult{}, readErr
	} else if ok {
		if linkErr := s.attributeDeploymentOccurrence(ctx, prior, req.Context.Principal); linkErr != nil {
			return TeamDeploymentResult{Deployment: prior}, linkErr
		}
		return TeamDeploymentResult{Deployment: prior}, nil
	}
	var automationRuleID model.AutomationRuleID
	if req.Context.Principal.Kind != model.PrincipalOperator {
		if req.Context.Principal.Kind != model.PrincipalAutomation {
			return TeamDeploymentResult{}, ErrUnauthorized
		}
		occurrence, occurrenceErr := s.store.Occurrence(ctx, model.OccurrenceID(req.Context.Principal.AutomationRun))
		if occurrenceErr != nil {
			return TeamDeploymentResult{}, occurrenceErr
		}
		automationRuleID = occurrence.Occurrence.RuleID
		if err = s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionRunAutomation, Resource: model.ResourceSelector{Kind: model.ResourceAutomationRule, AutomationRuleID: occurrence.Occurrence.RuleID}}, s.now().UTC()); err != nil {
			return TeamDeploymentResult{}, err
		}
	}
	ref := req.Instantiation.Definition
	revision, err := s.store.DefinitionRevision(ctx, ref.RevisionID)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	if ref.Kind != model.DefinitionTeam || revision.Team == nil || revision.DefinitionID != ref.DefinitionID || revision.ContentHash != ref.ContentHash {
		return TeamDeploymentResult{}, fail(ErrConflict, "team definition is not the pinned revision")
	}
	if _, err := resolveTeamMissionBriefings(*revision.Team, strings.TrimSpace(req.Instantiation.Mission), true); err != nil {
		return TeamDeploymentResult{}, err
	}
	parameters := materializeParameterValues(revision.Parameters, req.Instantiation.Parameters)
	if err = validateParameterValues(revision.Parameters, parameters); err != nil {
		return TeamDeploymentResult{}, err
	}
	now := s.now().UTC()
	members := make(map[string]model.AgentID, len(revision.Team.Members))
	roleMembers := make(map[model.RoleID][]model.AgentID)
	agents := make([]model.Agent, 0, len(revision.Team.Members))
	group := model.Group{ID: target.GroupID, Name: "team " + string(req.DeploymentID), Revision: 1, CreatedAt: now, UpdatedAt: now}
	if target.Kind == model.TeamTargetExistingGroup {
		group, err = s.store.Group(ctx, target.GroupID)
		if err != nil {
			return TeamDeploymentResult{}, err
		}
		if !OperatorProfileCaller(req.Context.Principal) {
			configurationStore, ok := s.store.(GroupConfigurationStore)
			if !ok {
				return TeamDeploymentResult{}, ErrUnsupported
			}
			configuration, readErr := configurationStore.GroupConfiguration(ctx, target.GroupID)
			if readErr != nil {
				return TeamDeploymentResult{}, readErr
			}
			if configuration.Profile != nil {
				profile, readErr := s.store.ConfigurationProfile(ctx, configuration.Profile.ProfileID, "")
				if readErr != nil {
					return TeamDeploymentResult{}, readErr
				}
				if err := s.requireProfileCreation(ctx, req.Context.Principal, profile.Profile); err != nil {
					return TeamDeploymentResult{}, err
				}
			}
		}
	}
	if err = s.requireAvailableGroupCapacity(ctx, group, len(revision.Team.Members)); err != nil {
		return TeamDeploymentResult{}, err
	}
	if err := s.requireProfileCreation(ctx, req.Context.Principal, model.ConfigurationProfile{}); err != nil {
		return TeamDeploymentResult{}, err
	}
	memberStartups := make(map[string]model.ProfileStartup)
	var configurationSources []model.TeamConfigurationSources
	for _, spec := range revision.Team.Members {
		desired := spec.Desired
		var profileRef *model.ConfigurationProfileRef
		if spec.Options != nil {
			var defaultGroup model.GroupID
			if target.Kind == model.TeamTargetExistingGroup {
				defaultGroup = target.GroupID
			}
			resolved, sources, resolveErr := s.resolveInlineConfiguration(ctx, *spec.Options, defaultGroup)
			desired, err = resolved.Desired, resolveErr
			configurationSources = append(configurationSources, sources)
			if err != nil {
				return TeamDeploymentResult{}, fail(ErrInvalid, "member %s: %v", spec.Key, err)
			}
		}
		if spec.ProfileID != "" {
			if !desired.Equal(model.DesiredConfiguration{}) {
				return TeamDeploymentResult{}, fail(ErrInvalid, "member %s selects a profile or custom settings", spec.Key)
			}
			profile, readErr := s.store.ConfigurationProfile(ctx, spec.ProfileID, "")
			if readErr != nil {
				return TeamDeploymentResult{}, readErr
			}
			if err := s.requireProfileCreation(ctx, req.Context.Principal, profile.Profile); err != nil {
				return TeamDeploymentResult{}, err
			}
			if err := ConfigurationProfileEnabled(profile.Profile); err != nil {
				return TeamDeploymentResult{}, err
			}
			if profile.Profile.Archived {
				return TeamDeploymentResult{}, fail(ErrConflict, "member %s profile is archived", spec.Key)
			}
			if profile.Revision.Options != nil {
				var overrides *model.ConfigurationOptions
				if o := spec.Overrides; o != nil {
					overrides = &model.ConfigurationOptions{Harness: o.Harness, Model: o.Model, Effort: o.Effort, Approval: o.Approval, Sandbox: o.Sandbox, AutoReview: o.AutoReview, AutoMemory: o.AutoMemory, FastMode: o.FastMode, ToolGovernance: o.ToolGovernance}
				}
				resolved, resolveErr := s.resolveProfileConfigurationWithOverrides(ctx, profile, overrides)
				if resolveErr != nil {
					return TeamDeploymentResult{}, resolveErr
				}
				desired = resolved.Desired
				configurationSources = append(configurationSources, model.TeamConfigurationSources{DefaultsRevision: resolved.DefaultsRevision, GlobalProfile: resolved.GlobalProfile, Selected: &resolved.Selected})
			} else {
				desired, err = s.resolveTeamProfile(spec, profile.Revision.Desired)
				if err != nil {
					return TeamDeploymentResult{}, err
				}
			}
			ref := profile.Revision.Ref
			profileRef = &ref
			if profile.Revision.Startup != nil {
				startup := *profile.Revision.Startup
				if authored := profile.Revision.AuthoredHarness(); authored != "" && desired.Harness != authored {
					startup.Context = ""
				}
				memberStartups[spec.Key] = startup
			}
		}
		if err = validateLaunchConfiguration(desired); err != nil {
			return TeamDeploymentResult{}, fail(ErrInvalid, "member %s: %v", spec.Key, err)
		}
		id := model.AgentID(deterministicOrchestrationID("agent_", string(req.DeploymentID)+":"+spec.Key))
		members[spec.Key] = id
		desired.HostSandbox = model.SandboxInGroup(desired.HostSandbox, group.ID)
		agents = append(agents, model.Agent{ID: id, Name: spec.Name, Labels: model.AgentLabels{Groups: map[model.GroupID]model.AgentDisplayLabels{group.ID: {Role: spec.Labels.Role, Description: spec.Labels.Description}}}, Desired: desired, ConfigurationProfile: profileRef, Revision: 1, CreatedAt: now, UpdatedAt: now})
		group.Members = append(group.Members, id)
		for _, roleID := range spec.Roles {
			roleMembers[roleID] = append(roleMembers[roleID], id)
		}
		if spec.Owner {
			if !slices.Contains(spec.Roles, model.GroupOwnerRole) {
				roleMembers[model.GroupOwnerRole] = append(roleMembers[model.GroupOwnerRole], id)
			}
			if group.OwnerAgentID == "" {
				group.OwnerAgentID = id
			}
		}
	}
	if len(revision.Team.Rhythms) > 0 {
		if _, err = teamRhythmDelegation(req.Context.Principal, group.ID); err != nil {
			return TeamDeploymentResult{}, err
		}
	}
	assignments, pins, roleErr := s.teamRoleAdmissions(ctx, req.Context.Principal, group.ID, roleMembers, now)
	if roleErr != nil {
		return TeamDeploymentResult{}, roleErr
	}
	workspaceBindings, ownedWorkspaceIDs, err := s.prepareTeamWorkspaces(ctx, req, *revision.Team)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	// Persist the same directory that the initial execution uses, so a later
	// ordinary agent restart does not return to the template author's machine.
	for i, spec := range revision.Team.Members {
		binding := workspaceBindings[spec.Key]
		workspace, readErr := s.store.Workspace(ctx, binding.WorkspaceID)
		if readErr != nil {
			return TeamDeploymentResult{}, readErr
		}
		if workspace.Revision != binding.SelectedRevision {
			return TeamDeploymentResult{}, ErrConflict
		}
		agents[i].Desired.WorkingDirectory = workspace.Observation.ActualPath
	}
	workRunID := model.WorkRunID(deterministicOrchestrationID("work_", string(req.DeploymentID)))
	var automationIDs, ownedAutomationIDs []model.AutomationRuleID
	for _, automation := range revision.Team.Automation {
		automationIDs = append(automationIDs, automation.RuleID)
		ownedAutomationIDs = append(ownedAutomationIDs, model.AutomationRuleID(deterministicOrchestrationID("rule_", string(req.DeploymentID)+":"+string(automation.RuleID))))
	}
	for i := range revision.Team.Rhythms {
		ownedAutomationIDs = append(ownedAutomationIDs, teamRhythmID(req.DeploymentID, i))
	}
	deployment := model.TeamDeployment{ConfigurationSources: configurationSources, ID: req.DeploymentID, Definition: ref, DependencyClosure: append([]model.DefinitionRef(nil), revision.Dependencies...), Mission: strings.TrimSpace(req.Instantiation.Mission), Parameters: parameters, GroupID: group.ID, TargetKind: target.Kind, Members: members, AutomationRuleIDs: automationIDs, OwnedAutomationRuleIDs: ownedAutomationIDs, Workspaces: workspaceBindings, OwnedWorkspaceIDs: ownedWorkspaceIDs, BriefingOperationIDs: map[string][]model.OperationID{}, WorkRunID: workRunID, State: model.DeploymentDeploying, Revision: 1, CreatedAt: now, UpdatedAt: now}
	deployment.MemberStartups = memberStartups
	deployment.RolePins = pins
	stored, _, err := s.store.CreateTeamDeployment(ctx, deployment, group, agents, assignments, req.Context.Principal, req.Context.RequestID, requestDigest, now)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	if err = s.attributeDeploymentOccurrence(ctx, stored, req.Context.Principal); err != nil {
		return TeamDeploymentResult{Deployment: stored}, err
	}
	if err = s.materializeDeploymentRhythms(ctx, stored, *revision.Team, req.Context); err != nil {
		return s.markDeploymentPartial(ctx, stored, err)
	}
	err = s.startTeamDeploymentProcess(ctx, stored, *revision.Team, req.Context.Principal, automationRuleID)
	if err != nil {
		return s.markDeploymentPartial(ctx, stored, err)
	}
	return TeamDeploymentResult{Deployment: stored}, nil
}

func (s *Service) attributeDeploymentOccurrence(ctx context.Context, deployment model.TeamDeployment, principal model.Principal) error {
	if principal.Kind != model.PrincipalAutomation {
		return nil
	}
	occurrence, err := s.store.Occurrence(ctx, model.OccurrenceID(principal.AutomationRun))
	if err != nil {
		return err
	}
	if occurrence.Occurrence.DeploymentID == deployment.ID {
		return nil
	}
	if occurrence.Occurrence.DeploymentID != "" {
		return ErrConflict
	}
	_, err = s.store.UpdateOccurrence(ctx, occurrence.Occurrence.ID, occurrence.Occurrence.Revision, model.OccurrenceAdmitted, occurrence.Occurrence.OperationID, occurrence.Occurrence.WorkRunID, deployment.ID, occurrence.Occurrence.Recipients, s.now().UTC())
	return err
}

func (s *Service) markDeploymentPartial(ctx context.Context, deployment model.TeamDeployment, cause error) (TeamDeploymentResult, error) {
	partial, updateErr := s.store.UpdateTeamDeployment(ctx, deployment.ID, deployment.Revision, model.DeploymentPartial, deployment.AdvisoryPhase, s.now().UTC())
	if updateErr == nil {
		deployment = partial
	}
	return TeamDeploymentResult{Deployment: deployment}, cause
}

func (s *Service) teamRoleAdmissions(ctx context.Context, principal model.Principal, groupID model.GroupID, members map[model.RoleID][]model.AgentID, now time.Time) ([]model.RoleAssignment, []model.TeamRolePin, error) {
	if len(members) == 0 {
		return nil, nil, nil
	}
	if err := requireOperator(principal); err != nil {
		return nil, nil, fail(ErrUnauthorized, "team role activation requires operator authority")
	}
	state, err := s.store.AuthorityState(ctx)
	if err != nil {
		return nil, nil, err
	}
	roles := make(map[model.RoleID]model.Role, len(state.Roles))
	for _, role := range state.Roles {
		roles[role.ID] = role
	}
	ids := make([]model.RoleID, 0, len(members))
	for id := range members {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var assignments []model.RoleAssignment
	pins := make([]model.TeamRolePin, 0, len(ids))
	for _, id := range ids {
		if err := id.Validate(); err != nil {
			return nil, nil, fail(ErrInvalid, "%v", err)
		}
		role, ok := roles[id]
		if !ok {
			return nil, nil, fail(ErrInvalid, "team role %s does not exist", id)
		}
		pins = append(pins, model.TeamRolePin{Brief: role.Brief, RoleID: id, Revision: role.Revision, Actions: append([]model.Action(nil), role.Actions...)})
		agents := append([]model.AgentID(nil), members[id]...)
		sort.Slice(agents, func(i, j int) bool { return agents[i] < agents[j] })
		for i, agentID := range agents {
			if i > 0 && agentID == agents[i-1] {
				continue
			}
			assignments = append(assignments, model.RoleAssignment{RoleID: id, Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: agentID}, Resource: model.ResourceSelector{Kind: model.ResourceGroupPeers, GroupID: groupID}, Bounds: model.ConfigurationBounds{}, Revision: 1, CreatedAt: now, UpdatedAt: now})
		}
	}
	return assignments, pins, nil
}

func (s *Service) startTeamDeploymentProcess(ctx context.Context, deployment model.TeamDeployment, team model.TeamDefinition, principal model.Principal, ruleID model.AutomationRuleID) error {
	team, err := resolveTeamMissionBriefings(team, deployment.Mission, true)
	if err != nil {
		return err
	}
	budget, err := teamWaveRunBudget(team)
	if err != nil {
		return err
	}
	graph := teamDeploymentGraph(team, deployment)
	scope := model.WorkScope{GroupID: deployment.GroupID, DeploymentID: deployment.ID}
	if principal.Kind == model.PrincipalAutomation {
		scope.RuleID = ruleID
		scope.OccurrenceID = model.OccurrenceID(principal.AutomationRun)
	}
	_, err = s.StartProcess(ctx, StartProcessRequest{Context: RequestContext{Principal: principal, RequestID: model.RequestID(deterministicOrchestrationID("request_", string(deployment.ID)))}, ID: deployment.WorkRunID, Start: model.WorkStart{InlineGraph: &graph, Scope: scope, Deadline: s.now().UTC().Add(budget)}})
	return err
}

func (s *Service) GetTeamDeployment(ctx context.Context, req GetTeamDeploymentRequest) (TeamDeploymentResult, error) {
	deployment, err := s.store.TeamDeployment(ctx, req.DeploymentID)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	if req.Principal.Kind != model.PrincipalOperator {
		if err = s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Principal, Action: model.ActionReadStatus, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: deployment.GroupID}}, s.now().UTC()); err != nil {
			return TeamDeploymentResult{}, err
		}
	}
	return s.teamDeploymentView(ctx, deployment)
}

func (s *Service) reconcileTeamDeployments(ctx context.Context) error {
	requests, err := s.store.PendingTeamRebriefs(ctx)
	if err != nil {
		return err
	}
	for _, request := range requests {
		if _, err = s.RebriefDeployment(ctx, request); err != nil {
			return err
		}
	}

	deployments, err := s.store.PendingTeamDeployments(ctx)
	if err != nil {
		return err
	}
	for _, deployment := range deployments {
		if deployment.State == model.DeploymentStandingDown {
			request, principalErr := s.store.TeamStandDownRequest(ctx, deployment.ID)
			if principalErr != nil {
				return principalErr
			}
			if _, standDownErr := s.continueTeamStandDown(ctx, deployment, request.Context.Principal, request.Reason); standDownErr != nil {
				return standDownErr
			}
			continue
		}
		if deployment.State == model.DeploymentReady {
			if err = s.reconcileDeferredTeamBriefings(ctx, deployment); err != nil {
				return err
			}
			continue
		}
		run, readErr := s.store.WorkRun(ctx, deployment.WorkRunID)
		if errors.Is(readErr, ErrNotFound) {
			definition, definitionErr := s.store.DefinitionRevision(ctx, deployment.Definition.RevisionID)
			if definitionErr != nil || definition.Team == nil || definition.DefinitionID != deployment.Definition.DefinitionID || definition.ContentHash != deployment.Definition.ContentHash {
				if definitionErr == nil {
					definitionErr = ErrConflict
				}
				return definitionErr
			}
			principal, principalErr := s.store.TeamDeploymentRequester(ctx, deployment.ID)
			if principalErr != nil {
				return principalErr
			}
			if materializeErr := s.materializeDeploymentRhythms(ctx, deployment, *definition.Team, RequestContext{Principal: principal}); materializeErr != nil {
				if deployment.State == model.DeploymentDeploying {
					_, _ = s.store.UpdateTeamDeployment(ctx, deployment.ID, deployment.Revision, model.DeploymentPartial, deployment.AdvisoryPhase, s.now().UTC())
				}
				return materializeErr
			}
			var ruleID model.AutomationRuleID
			if principal.Kind == model.PrincipalAutomation {
				occurrence, occurrenceErr := s.store.Occurrence(ctx, model.OccurrenceID(principal.AutomationRun))
				if occurrenceErr != nil {
					return occurrenceErr
				}
				ruleID = occurrence.Occurrence.RuleID
				if occurrenceErr = s.attributeDeploymentOccurrence(ctx, deployment, principal); occurrenceErr != nil {
					return occurrenceErr
				}
			}
			if startErr := s.startTeamDeploymentProcess(ctx, deployment, *definition.Team, principal, ruleID); startErr != nil {
				if deployment.State == model.DeploymentDeploying {
					_, _ = s.store.UpdateTeamDeployment(ctx, deployment.ID, deployment.Revision, model.DeploymentPartial, deployment.AdvisoryPhase, s.now().UTC())
				}
				return startErr
			}
			run, readErr = s.store.WorkRun(ctx, deployment.WorkRunID)
		}
		if readErr != nil {
			return readErr
		}
		state := deployment.State
		switch run.Run.State {
		case model.WorkRunSucceeded:
			if enableErr := s.enableDeploymentRhythms(ctx, deployment); enableErr != nil {
				state = model.DeploymentPartial
			} else {
				state = model.DeploymentReady
			}
		case model.WorkRunFailed, model.WorkRunCancelled, model.WorkRunUncertain:
			state = model.DeploymentPartial
		}
		if state != deployment.State {
			if _, err = s.store.UpdateTeamDeployment(ctx, deployment.ID, deployment.Revision, state, deployment.AdvisoryPhase, s.now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) enableDeploymentRhythms(ctx context.Context, deployment model.TeamDeployment) error {
	principal, err := s.store.TeamDeploymentRequester(ctx, deployment.ID)
	if err != nil {
		return err
	}
	for _, id := range deployment.OwnedAutomationRuleIDs {
		record, readErr := s.store.AutomationRule(ctx, id)
		if readErr != nil {
			return readErr
		}
		if record.Rule.DeploymentID != deployment.ID {
			return ErrConflict
		}
		if !record.Rule.Enabled && !record.Rule.Tombstoned {
			if _, err = s.store.SetAutomationRuleEnabled(ctx, id, record.Rule.Revision, true, principal, s.now().UTC()); err != nil {
				return err
			}
		}
	}
	return nil
}

func teamDeploymentGraph(team model.TeamDefinition, deployment model.TeamDeployment) model.WorkGraph {
	var nodes []model.WorkNode
	var edges []model.WorkEdge
	completion := make(map[string]model.WorkNodeID, len(team.Waves))
	for _, wave := range team.Waves {
		if len(wave.MemberKeys) == 1 {
			completion[wave.ID] = model.WorkNodeID("member_" + wave.MemberKeys[0])
		} else {
			completion[wave.ID] = model.WorkNodeID("wave_done_" + wave.ID)
		}
	}
	for _, wave := range team.Waves {
		var predecessor model.WorkNodeID
		if len(wave.DependsOn) == 1 {
			predecessor = completion[wave.DependsOn[0]]
		} else if len(wave.DependsOn) > 1 {
			predecessor = model.WorkNodeID("wave_gate_" + wave.ID)
			nodes = append(nodes, model.WorkNode{ID: predecessor, Kind: model.WorkNodeJoin, Join: &model.JoinPolicy{Mode: model.JoinAll}})
			for _, dependency := range wave.DependsOn {
				edges = append(edges, model.WorkEdge{From: completion[dependency], To: predecessor})
			}
		}
		for _, key := range wave.MemberKeys {
			brief := deployment.Mission
			if startup, ok := deployment.MemberStartups[key]; ok && strings.TrimSpace(startup.Context) != "" {
				brief = strings.TrimSpace(brief + "\n\n" + startup.Context)
			}
			for _, item := range team.Briefings {
				if item.Timing == model.BriefingBeforeFirstWork && slices.Contains(teamBriefRecipients(team, item), key) {
					brief = strings.TrimSpace(brief + "\n\n" + item.Body)
				}
			}
			for _, member := range team.Members {
				if member.Key != key {
					continue
				}
				for _, id := range member.Roles {
					for _, role := range deployment.RolePins {
						if role.RoleID == id && strings.TrimSpace(role.Brief) != "" {
							brief = strings.TrimSpace(brief + "\n\n## Role\n\n" + strings.TrimSpace(role.Brief))
						}
					}
				}
			}
			if process := teamProcessBrief(team); process != "" {
				brief = strings.TrimSpace(brief + "\n\n" + process)
			}
			nodeID := model.WorkNodeID("member_" + key)
			nodes = append(nodes, model.WorkNode{ID: nodeID, Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: deployment.Members[key], WorkspaceID: deployment.Workspaces[key].WorkspaceID, ContextPolicy: model.AgentContextFresh, Brief: brief}}})
			if predecessor != "" {
				edges = append(edges, model.WorkEdge{From: predecessor, To: nodeID})
			}
			if len(wave.MemberKeys) > 1 {
				edges = append(edges, model.WorkEdge{From: nodeID, To: completion[wave.ID]})
			}
		}
		if len(wave.MemberKeys) > 1 {
			nodes = append(nodes, model.WorkNode{ID: completion[wave.ID], Kind: model.WorkNodeJoin, Join: &model.JoinPolicy{Mode: model.JoinAll}})
		}
	}
	var roots []model.WorkNodeID
	for _, wave := range team.Waves {
		if len(wave.DependsOn) == 0 {
			for _, key := range wave.MemberKeys {
				roots = append(roots, model.WorkNodeID("member_"+key))
			}
		}
	}
	entryID := roots[0]
	if len(roots) > 1 {
		entryID = "deployment_entry"
		nodes = append(nodes, model.WorkNode{ID: entryID, Kind: model.WorkNodeFork})
		for _, root := range roots {
			edges = append(edges, model.WorkEdge{From: entryID, To: root})
		}
	}
	endID := model.WorkNodeID("deployment_ready")
	nodes = append(nodes, model.WorkNode{ID: endID, Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}})
	dependent := make(map[string]bool)
	for _, wave := range team.Waves {
		for _, dependency := range wave.DependsOn {
			dependent[dependency] = true
		}
	}
	var leaves []model.WorkNodeID
	for _, wave := range team.Waves {
		if !dependent[wave.ID] {
			leaves = append(leaves, completion[wave.ID])
		}
	}
	if len(leaves) == 1 {
		edges = append(edges, model.WorkEdge{From: leaves[0], To: endID})
	} else {
		finalJoin := model.WorkNodeID("deployment_join")
		nodes = append(nodes, model.WorkNode{ID: finalJoin, Kind: model.WorkNodeJoin, Join: &model.JoinPolicy{Mode: model.JoinAll}})
		for _, leaf := range leaves {
			edges = append(edges, model.WorkEdge{From: leaf, To: finalJoin})
		}
		edges = append(edges, model.WorkEdge{From: finalJoin, To: endID})
	}
	for _, wave := range team.Waves {
		if !wave.WaitForIdle || !dependent[wave.ID] {
			continue
		}
		gateID := teamWaveGateID(wave.ID)
		for i := range edges {
			if edges[i].From == completion[wave.ID] {
				edges[i].From = gateID
			}
		}
		nodes = append(nodes, model.WorkNode{ID: gateID, Kind: model.WorkNodeWait, Wait: &model.WaitPolicy{Duration: teamWaveMaxWait(wave)}})
		edges = append(edges, model.WorkEdge{From: completion[wave.ID], To: gateID})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return model.WorkGraph{CompilerVersion: orchestrationCompilerVersion, EntryNodeID: entryID, Nodes: nodes, Edges: edges}
}

func (s *Service) reconcileDeferredTeamBriefings(ctx context.Context, deployment model.TeamDeployment) error {
	revision, err := s.store.DefinitionRevision(ctx, deployment.Definition.RevisionID)
	if err != nil {
		return err
	}
	if revision.Team == nil {
		return ErrConflict
	}
	for _, key := range sortedMemberKeys(deployment.Members) {
		if !memberHasAfterReadyBrief(*revision.Team, key) || len(deployment.BriefingOperationIDs[key]) >= teamAfterReadyBriefCount(*revision.Team, key) {
			continue
		}
		// The briefing belongs to this deployment's admitted execution, not
		// a later primary execution that happens to use the same agent identity.
		run, err := s.store.WorkRun(ctx, deployment.WorkRunID)
		if err != nil {
			return err
		}
		var executionID model.ExecutionID
		for _, attempt := range run.Run.NodeAttempts {
			if attempt.Performer != nil && attempt.Performer.Agent != nil && attempt.Performer.Agent.AgentID == deployment.Members[key] && attempt.ExecutionID != "" {
				executionID = attempt.ExecutionID
				break
			}
		}
		if executionID == "" {
			continue
		}
		execution, err := s.store.Execution(ctx, executionID)
		if err != nil {
			return err
		}
		if execution.State == model.ExecutionExited || execution.State == model.ExecutionFailed {
			continue
		}
		runtime, err := s.runtimeFor(ctx, execution)
		if err != nil {
			// The durable briefing stays pending until explicit runtime recovery.
			// It must not prevent independent deployments or work from advancing.
			continue
		}
		observation, err := runtime.Observe(ctx)
		if err != nil {
			continue
		}
		if observation.Context != ports.ContextReady {
			continue
		}
		deployment, err = s.deliverAfterReadyBriefings(ctx, deployment, key, *revision.Team)
		if err != nil {
			return err
		}
	}
	return nil
}

// Both authored targeting forms mean explicit recipients. Empty means none;
// clients that offer "all" materialize the exact roster in their revision.
func teamBriefRecipients(team model.TeamDefinition, brief model.TeamBriefing) []string {
	recipients := append([]string(nil), brief.MemberKeys...)
	for _, member := range team.Members {
		if slices.Contains(member.BriefingIDs, brief.ID) && !slices.Contains(recipients, member.Key) {
			recipients = append(recipients, member.Key)
		}
	}
	return recipients
}
