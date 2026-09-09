package app

import (
	"context"
	"github.com/tofutools/tclaude/internal/backend/model"
	"strings"
)

// Resolve the checkout into launch-local overrides without changing receipt
// identity or the selected reusable profile.
func (s *Service) groupMemberWorkspaceRequest(ctx context.Context, in CreateGroupMemberRequest) (CreateGroupMemberRequest, error) {
	if in.Workspace == nil {
		return in, nil
	}
	workspace, err := s.store.Workspace(ctx, in.Workspace.WorkspaceID)
	if err != nil {
		return in, err
	}
	if workspace.Revision != in.Workspace.ExpectedRevision || workspace.State != model.WorkspaceAvailable || strings.TrimSpace(workspace.Observation.ActualPath) == "" {
		return in, ErrConflict
	}
	if err := s.requireAuthority(ctx, model.AuthorityRequest{Principal: in.Context.Principal, Action: model.ActionInspectWorkspace, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace, WorkspaceID: workspace.ID}}, s.now().UTC()); err != nil {
		return in, err
	}
	options := model.ConfigurationOptions{}
	if in.ConfigurationOverrides != nil {
		options = *in.ConfigurationOverrides
	}
	if options.WorkingDirectory != nil && *options.WorkingDirectory != workspace.Observation.ActualPath {
		return in, fail(ErrInvalid, "working directory conflicts with selected checkout")
	}
	options.WorkingDirectory = &workspace.Observation.ActualPath
	in.ConfigurationOverrides = &options
	return in, nil
}
