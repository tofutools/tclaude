package app

import (
	"context"
	"slices"

	"github.com/tofutools/tclaude/internal/backend/model"
)

func (s *Service) prepareWorkDirectoryTrust(ctx context.Context, request StartProcessRequest, graph model.WorkGraph, digest string) (*model.WorkDirectoryTrust, []string, error) {
	trust := &model.WorkDirectoryTrust{RequestDigest: digest}
	if OperatorProfileCaller(request.Context.Principal) {
		return trust, nil, nil
	}
	trust.Nodes = make(map[model.WorkNodeID]model.DirectoryTrustAdmission)
	trust.AgentRevisions = make(map[model.AgentID]model.Revision)
	trust.WorkspaceRevisions = make(map[model.WorkspaceID]model.Revision)
	var paths []string
	for _, node := range graph.Nodes {
		if node.Performer == nil || node.Performer.Agent == nil || node.Performer.Agent.AgentID == "" {
			continue
		}
		performer := node.Performer.Agent
		agent, err := s.store.Agent(ctx, performer.AgentID)
		if err != nil {
			return nil, nil, err
		}
		desired := agent.Desired
		trust.AgentRevisions[agent.ID] = agent.Revision
		if performer.WorkspaceID != "" {
			workspace, err := s.store.Workspace(ctx, performer.WorkspaceID)
			if err != nil {
				return nil, nil, err
			}
			if workspace.State != model.WorkspaceAvailable {
				return nil, nil, ErrConflict
			}
			if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: request.Context.Principal, Action: model.ActionInspectWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: workspace.ID}}, s.now().UTC()); err != nil {
				return nil, nil, err
			}
			desired.WorkingDirectory = workspace.Observation.ActualPath
			trust.WorkspaceRevisions[workspace.ID] = workspace.Revision
		}
		if performer.ContextPolicy == model.AgentContextReuse && agent.PrimaryExecutionID != "" {
			continue
		}
		if inherited, ok := request.directoryTrust[agent.ID]; ok {
			admission, err := s.retainedDirectoryTrust(ctx, inherited, agent.ID, desired)
			if err != nil {
				return nil, nil, err
			}
			inherited.ProvenDirectories = slices.Clone(admission.proven)
			trust.Nodes[node.ID] = inherited
			continue
		}
		admission, err := s.resolveDirectoryTrust(ctx, request.Context, desired)
		if err != nil {
			return nil, nil, err
		}
		if len(admission.proven) != 0 {
			if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: request.Context.Principal, Action: model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: agent.ID}, RequestedConfiguration: &desired}, s.now().UTC()); err != nil {
				return nil, nil, err
			}
		}

		trust.Nodes[node.ID] = model.DirectoryTrustAdmission{AgentID: agent.ID, Harness: desired.Harness, RequestedTrust: desired.TrustDirectory, RequestedDirectory: desired.WorkingDirectory, PhysicalDirectory: admission.path, Enabled: admission.enabled, ProvenDirectories: slices.Clone(admission.proven)}
		paths = append(paths, admission.proven...)
	}
	if len(paths) == 0 {
		return trust, nil, nil
	}
	proven, err := s.directoryChallenges.verify(ctx, s.directoryProof, request.Context.Principal, struct {
		Request StartProcessRequest
		Graph   model.WorkGraph
		Trust   *model.WorkDirectoryTrust
	}{request, graph, trust}, request.Context.WriteProofToken, paths, s.now().UTC())
	if err != nil {
		return nil, nil, err
	}
	return trust, proven, nil
}

func (s *Service) deferredDirectoryTrust(ctx context.Context, run model.WorkRun, node model.WorkNodeID, agent model.AgentID, desired model.DesiredConfiguration) (directoryTrustAdmission, error) {
	if OperatorProfileCaller(run.Requester) {
		return s.prepareDirectoryTrust(ctx, RequestContext{Principal: run.Requester}, desired, nil)
	}
	if run.DirectoryTrust != nil {
		if stored, ok := run.DirectoryTrust.Nodes[node]; ok {
			return s.retainedDirectoryTrust(ctx, stored, agent, desired)
		}
	}
	// Earlier runs have no retained proof. Ordinary untrusted launches remain
	// valid; a launch needing caller proof must be admitted by a new request.
	admission, err := s.resolveDirectoryTrust(ctx, RequestContext{Principal: run.Requester}, desired)
	if err != nil {
		return admission, err
	}
	if len(admission.proven) != 0 {
		return directoryTrustAdmission{}, fail(ErrUnauthorized, "process launch requires caller directory write proof at admission")
	}
	return admission, nil
}

func (s *Service) retainedDirectoryTrust(ctx context.Context, stored model.DirectoryTrustAdmission, agent model.AgentID, desired model.DesiredConfiguration) (directoryTrustAdmission, error) {
	if stored.AgentID != agent || stored.Harness != desired.Harness || stored.RequestedDirectory != desired.WorkingDirectory || stored.RequestedTrust != desired.TrustDirectory {
		return directoryTrustAdmission{}, fail(ErrConflict, "agent launch directory trust changed after admission")
	}
	admission := directoryTrustAdmission{enabled: stored.Enabled, path: stored.PhysicalDirectory, proven: slices.Clone(stored.ProvenDirectories)}
	if err := s.reassertDirectoryTrust(ctx, admission); err != nil {
		return directoryTrustAdmission{}, err
	}
	return admission, nil
}
