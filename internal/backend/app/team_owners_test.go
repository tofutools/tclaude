//go:build linux || darwin

package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestTeamMultipleOwnersDeployAndReinforceWithoutReplacingOwners(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	hookStore := &ownerChangeDuringAdmissionStore{Store: store}
	service := app.New(hookStore, providers.NewRegistry(&preparedWorkProvider{}))
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "a", Name: "Lead", Desired: desired, Owner: true, Required: true}, {Key: "b", Name: "Co-lead", Desired: desired, Owner: true, Required: true}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"a", "b"}}}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: op, RequestID: "save"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "Team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "test", Team: &team}})
	require.NoError(t, err)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}
	var owners []model.AgentID
	var preferred model.AgentID
	for _, deploymentID := range []model.DeploymentID{"initial", "reinforcement"} {
		workspaceID := model.WorkspaceID(deploymentID)
		now := time.Now().UTC()
		require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: workspaceID, State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
		target := model.TeamDeploymentTarget{Kind: model.TeamTargetNewGroup, GroupID: "group"}
		if deploymentID == "reinforcement" {
			target.Kind = model.TeamTargetExistingGroup
			// Simulate an operator selection after DeployTeam read the group but
			// before its admission transaction executes.
			preferred = owners[1]
			hookStore.before = func() {
				group, err := store.Group(ctx, "group")
				require.NoError(t, err)
				_, err = store.SetGroupOwners(ctx, "group", []model.AgentID{owners[1], owners[0]}, model.ConfigurationBounds{}, group.Revision, time.Now())
				require.NoError(t, err)
			}
		}
		req := app.DeployTeamRequest{Context: app.RequestContext{Principal: op, RequestID: model.RequestID(deploymentID)}, DeploymentID: deploymentID, Instantiation: model.TeamInstantiation{Definition: ref, Target: target, Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: workspaceID, ExpectedRevision: 1}}}}
		result, err := service.DeployTeam(ctx, req)
		require.NoError(t, err)
		retry, err := service.DeployTeam(ctx, req)
		require.NoError(t, err)
		require.Equal(t, result.Deployment.ID, retry.Deployment.ID)
		owners = append(owners, result.Deployment.Members["a"], result.Deployment.Members["b"])
		group, err := store.Group(ctx, "group")
		require.NoError(t, err)
		require.ElementsMatch(t, owners, group.OwnerAgentIDs)
		if preferred == "" {
			preferred = owners[0]
		}
		require.Equal(t, preferred, group.OwnerAgentID)
		for _, owner := range owners {
			allowed, err := store.Authorize(ctx, model.AuthorityRequest{Principal: model.AgentPrincipal(owner), Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "group"}}, now)
			require.NoError(t, err)
			require.True(t, allowed.Allowed)
			outside, err := store.Authorize(ctx, model.AuthorityRequest{Principal: model.AgentPrincipal(owner), Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "other"}}, now)
			require.NoError(t, err)
			require.False(t, outside.Allowed)
		}
	}
}

// Only the interleaving is injected; both competing mutations use real SQLite.
type ownerChangeDuringAdmissionStore struct {
	*sqlite.Store
	before func()
}

func (s *ownerChangeDuringAdmissionStore) CreateTeamDeployment(ctx context.Context, deployment model.TeamDeployment, group model.Group, agents []model.Agent, assignments []model.RoleAssignment, principal model.Principal, requestID model.RequestID, requestDigest string, at time.Time) (model.TeamDeployment, bool, error) {
	if s.before != nil {
		before := s.before
		s.before = nil
		before()
	}
	return s.Store.CreateTeamDeployment(ctx, deployment, group, agents, assignments, principal, requestID, requestDigest, at)
}
