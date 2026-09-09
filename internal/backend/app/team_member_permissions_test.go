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

type teamPermissionAdmissionStore struct {
	*sqlite.Store
	mutate func(*model.TeamDeployment, *model.Principal)
}

func (s *teamPermissionAdmissionStore) CreateTeamDeployment(ctx context.Context, d model.TeamDeployment, g model.Group, agents []model.Agent, roles []model.RoleAssignment, p model.Principal, r model.RequestID, digest string, at time.Time) (model.TeamDeployment, bool, error) {
	if s.mutate != nil {
		s.mutate(&d, &p)
	}
	return s.Store.CreateTeamDeployment(ctx, d, g, agents, roles, p, r, digest, at)
}

func TestTeamMemberPermissionsPublishAtomicallyAndStayRevokedOnRetry(t *testing.T) {
	for _, mode := range []string{"success", "foreign subject", "nonoperator", "member overrides role", "conflicting roles", "role edit before admission"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db")
			store, err := sqlite.Open(path)
			require.NoError(t, err)
			defer func() { require.NoError(t, store.Close()) }()
			wrapper := &teamPermissionAdmissionStore{Store: store}
			provider := &preparedWorkProvider{}
			service := app.New(wrapper, providers.NewRegistry(provider))
			now, cwd := time.Now().UTC(), t.TempDir()
			require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
			team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: cwd, Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}, Permissions: []model.TeamMemberPermission{{Action: model.ActionManageMembership, Scope: model.PermissionScope{"group": {"team deployment"}}}, {Action: model.ActionReadStatus, Denied: true}}, Required: true}}, Waves: []model.TeamWave{{ID: "initial", MemberKeys: []string{"worker"}, RequiredReady: true}}}

			if mode == "member overrides role" || mode == "conflicting roles" || mode == "role edit before admission" {
				for i, name := range []string{"first", "second"} {
					_, err = service.PutRole(ctx, app.PutRoleRequest{Principal: model.OperatorPrincipal(), Role: model.Role{ID: model.RoleID(name), Name: name, Actions: []model.Action{model.ActionManageMembership}, Scopes: model.ActionScopes{model.ActionManageMembership: {"group": {name}}}}})
					require.NoError(t, err)
					team.Members[0].Roles = append(team.Members[0].Roles, model.RoleID(name))
					if mode != "conflicting roles" && i == 0 {
						break
					}
				}
			}
			saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "Team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "test", Team: &team}})
			require.NoError(t, err)
			var attempted model.TeamDeployment
			wrapper.mutate = func(d *model.TeamDeployment, p *model.Principal) {
				attempted = *d
				if mode == "role edit before admission" {
					state, readErr := store.AuthorityState(ctx)
					require.NoError(t, readErr)
					for _, role := range state.Roles {
						if role.ID == "first" {
							role.Scopes[model.ActionManageMembership] = model.PermissionScope{"group": {"changed"}}
							_, writeErr := service.PutRole(ctx, app.PutRoleRequest{Principal: model.OperatorPrincipal(), Role: role, ExpectedRevision: role.Revision})
							require.NoError(t, writeErr)
						}
					}
				}

				if mode == "foreign subject" {
					d.MemberDenials[0].Subject.AgentID = "foreign"
				}
				if mode == "nonoperator" {
					*p = model.AgentPrincipal("foreign")
				}
			}
			request := app.DeployTeamRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, Mission: "Work", GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}}
			deployed, err := service.DeployTeam(ctx, request)
			if mode == "conflicting roles" {
				require.ErrorIs(t, err, app.ErrInvalid)
				require.Contains(t, err.Error(), "incompatible scopes")
				require.Empty(t, attempted.ID, "role conflicts must fail before deployment publication")
				state, readErr := store.AuthorityState(ctx)
				require.NoError(t, readErr)
				require.Empty(t, state.Grants)
				require.Empty(t, state.Assignments)
				return
			}
			if mode == "role edit before admission" {
				require.ErrorIs(t, err, app.ErrConflict)
			}
			if mode == "foreign subject" || mode == "nonoperator" || mode == "role edit before admission" {
				require.Error(t, err)
				require.NotEmpty(t, attempted.Members)
				_, err = store.Agent(ctx, attempted.Members["worker"])
				require.ErrorIs(t, err, app.ErrNotFound)
				state, err := store.AuthorityState(ctx)
				require.NoError(t, err)
				require.Empty(t, state.Grants)
				require.Empty(t, state.Denials)
				return
			}
			require.NoError(t, err)
			require.Empty(t, deployed.Deployment.MemberGrants)
			id := deployed.Deployment.Members["worker"]
			require.NoError(t, store.Close())
			store, err = sqlite.Open(path)
			require.NoError(t, err)
			service = app.New(store, providers.NewRegistry(provider))
			state, err := store.AuthorityState(ctx)
			require.NoError(t, err)
			require.Len(t, state.Grants, 1)
			require.Len(t, state.Denials, 1)
			require.Equal(t, id, state.Grants[0].Subject.AgentID)
			require.Equal(t, team.Members[0].Permissions[0].Scope, state.Grants[0].Scope)
			allowed, err := store.Authorize(ctx, model.AuthorityRequest{Principal: model.AgentPrincipal(id), Action: model.ActionManageMembership, Resource: model.ResourceSelector{Kind: model.ResourceGroup, GroupID: "group"}}, now)
			require.NoError(t, err)
			require.True(t, allowed.Allowed)
			decision, err := store.Authorize(ctx, model.AuthorityRequest{Principal: model.AgentPrincipal(id), Action: model.ActionReadStatus, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: id}}, now)
			require.NoError(t, err)
			require.False(t, decision.Allowed)
			require.Equal(t, model.AuthoritySourceKind("deny"), decision.SourceKind)
			require.NoError(t, service.DeleteGrant(ctx, app.DeleteGrantRequest{Principal: model.OperatorPrincipal(), GrantID: state.Grants[0].ID, ExpectedRevision: state.Grants[0].Revision}))
			_, err = service.DeployTeam(ctx, request)
			require.NoError(t, err)
			state, err = store.AuthorityState(ctx)
			require.NoError(t, err)
			require.Empty(t, state.Grants, "retry must not recreate a revoked birth grant")
			require.Len(t, state.Denials, 1)
		})
	}
}
