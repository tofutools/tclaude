package app

import (
	"context"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Service) validateTeamDirectoryTrust(ctx context.Context, team model.TeamDefinition) error {
	return s.validateTeamNativeBoolean(ctx, team, func(d model.DesiredConfiguration) bool { return d.TrustDirectory }, func(o *model.TeamProfileOverrides) *bool { return o.TrustDirectory }, model.ValidateDirectoryTrust)
}

func (s *Service) prepareTeamDirectoryTrust(ctx context.Context, req DeployTeamRequest, group model.GroupID, agents []model.Agent) (map[model.AgentID]model.DirectoryTrustAdmission, []string, error) {
	if OperatorProfileCaller(req.Context.Principal) {
		return nil, nil, nil
	}
	admitted := make(map[model.AgentID]model.DirectoryTrustAdmission)
	var paths []string
	for _, agent := range agents {
		desired := agent.Desired
		trust, err := s.resolveDirectoryTrust(ctx, req.Context, desired)
		if err != nil {
			return nil, nil, err
		}
		if len(trust.proven) != 0 {
			if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: req.Context.Principal, Action: model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceGroupPeers, GroupID: group}, RequestedConfiguration: &desired}, s.now().UTC()); err != nil {
				return nil, nil, err
			}
		}
		admitted[agent.ID] = model.DirectoryTrustAdmission{AgentID: agent.ID, Harness: desired.Harness, RequestedTrust: desired.TrustDirectory, RequestedDirectory: desired.WorkingDirectory, PhysicalDirectory: trust.path, Enabled: trust.enabled, ProvenDirectories: trust.proven}
		paths = append(paths, trust.proven...)
	}
	if len(paths) == 0 {
		return admitted, nil, nil
	}
	configurations := make(map[model.AgentID]model.DesiredConfiguration, len(agents))
	for _, agent := range agents {
		configurations[agent.ID] = agent.Desired
	}
	proven, err := s.directoryChallenges.verify(ctx, s.directoryProof, req.Context.Principal, struct {
		Request DeployTeamRequest
		Agents  map[model.AgentID]model.DesiredConfiguration
	}{req, configurations}, req.Context.WriteProofToken, paths, s.now().UTC())
	if err != nil {
		return nil, nil, err
	}
	return admitted, proven, nil
}
