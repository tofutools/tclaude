package app

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Service) DeployTeam(ctx context.Context, req DeployTeamRequest) (TeamDeploymentResult, error) {
	if err := validateEffectContext(req.Context); err != nil {
		return TeamDeploymentResult{}, err
	}
	if err := requireOperator(req.Context.Principal); err != nil {
		return TeamDeploymentResult{}, err
	}
	if err := req.DeploymentID.Validate(); err != nil {
		return TeamDeploymentResult{}, fail(ErrInvalid, "%v", err)
	}
	if err := req.Instantiation.GroupID.Validate(); err != nil {
		return TeamDeploymentResult{}, fail(ErrInvalid, "%v", err)
	}
	ref := req.Instantiation.Definition
	revision, err := s.store.DefinitionRevision(ctx, ref.RevisionID)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	if ref.Kind != model.DefinitionTeam || revision.Team == nil || revision.DefinitionID != ref.DefinitionID || revision.ContentHash != ref.ContentHash {
		return TeamDeploymentResult{}, fail(ErrConflict, "team definition is not the pinned revision")
	}
	if err = validateParameterValues(revision.Parameters, req.Instantiation.Parameters); err != nil {
		return TeamDeploymentResult{}, err
	}
	now := s.now().UTC()
	members := make(map[string]model.AgentID, len(revision.Team.Members))
	agents := make([]model.Agent, 0, len(revision.Team.Members))
	group := model.Group{ID: req.Instantiation.GroupID, Name: "team " + string(req.DeploymentID), Revision: 1, CreatedAt: now, UpdatedAt: now}
	for _, spec := range revision.Team.Members {
		if err = validateDesired(spec.Desired); err != nil {
			return TeamDeploymentResult{}, fail(ErrInvalid, "member %s: %v", spec.Key, err)
		}
		id := model.AgentID(deterministicOrchestrationID("agent_", string(req.DeploymentID)+":"+spec.Key))
		members[spec.Key] = id
		agents = append(agents, model.Agent{ID: id, Name: spec.Name, Desired: spec.Desired, Revision: 1, CreatedAt: now, UpdatedAt: now})
		group.Members = append(group.Members, id)
		if spec.Owner {
			group.OwnerAgentID = id
		}
	}
	workRunID := model.WorkRunID(deterministicOrchestrationID("work_", string(req.DeploymentID)))
	var automationIDs []model.AutomationRuleID
	for _, automation := range revision.Team.Automation {
		automationIDs = append(automationIDs, automation.RuleID)
	}
	deployment := model.TeamDeployment{ID: req.DeploymentID, Definition: ref, DependencyClosure: append([]model.DefinitionRef(nil), revision.Dependencies...), Mission: strings.TrimSpace(req.Instantiation.Mission), Parameters: cloneRawMap(req.Instantiation.Parameters), GroupID: group.ID, Members: members, AutomationRuleIDs: automationIDs, WorkRunID: workRunID, State: model.DeploymentDeploying, Revision: 1, CreatedAt: now, UpdatedAt: now}
	stored, _, err := s.store.CreateTeamDeployment(ctx, deployment, group, agents)
	if err != nil {
		return TeamDeploymentResult{}, err
	}
	graph := teamDeploymentGraph(*revision.Team, stored)
	_, err = s.StartProcess(ctx, StartProcessRequest{Context: RequestContext{Principal: req.Context.Principal, RequestID: model.RequestID(deterministicOrchestrationID("request_", string(req.DeploymentID)))}, ID: workRunID, Start: model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{GroupID: group.ID, DeploymentID: deployment.ID}, Deadline: now.Add(admittedEffectTimeout)}})
	if err != nil {
		partial, updateErr := s.store.UpdateTeamDeployment(ctx, stored.ID, stored.Revision, model.DeploymentPartial, stored.AdvisoryPhase, s.now().UTC())
		if updateErr == nil {
			stored = partial
		}
		return TeamDeploymentResult{Deployment: stored}, err
	}
	return TeamDeploymentResult{Deployment: stored}, nil
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
	return TeamDeploymentResult{Deployment: deployment}, nil
}

func (s *Service) reconcileTeamDeployments(ctx context.Context) error {
	deployments, err := s.store.PendingTeamDeployments(ctx)
	if err != nil {
		return err
	}
	for _, deployment := range deployments {
		run, readErr := s.store.WorkRun(ctx, deployment.WorkRunID)
		if readErr != nil {
			continue
		}
		state := deployment.State
		switch run.Run.State {
		case model.WorkRunSucceeded:
			state = model.DeploymentReady
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
	briefs := make(map[string]model.TeamBriefing, len(team.Briefings))
	for _, brief := range team.Briefings {
		briefs[brief.ID] = brief
	}
	memberSpecs := make(map[string]model.TeamMemberSpec, len(team.Members))
	for _, member := range team.Members {
		memberSpecs[member.Key] = member
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
			spec := memberSpecs[key]
			brief := deployment.Mission
			for _, id := range spec.BriefingIDs {
				item := briefs[id]
				if item.Timing == model.BriefingBeforeFirstWork {
					brief = strings.TrimSpace(brief + "\n\n" + item.Body)
				}
			}
			nodeID := model.WorkNodeID("member_" + key)
			nodes = append(nodes, model.WorkNode{ID: nodeID, Kind: model.WorkNodeTask, Input: map[string]json.RawMessage{"deployment_launch": json.RawMessage("true")}, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: deployment.Members[key], ContextPolicy: model.AgentContextFresh, Brief: brief}}})
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
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return model.WorkGraph{CompilerVersion: orchestrationCompilerVersion, EntryNodeID: entryID, Nodes: nodes, Edges: edges, Outcome: model.WorkGraphOutcomePolicy{ArtifactRevision: deployment.Definition.ContentHash}}
}
