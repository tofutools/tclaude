package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestSandboxDefaultsComposeAndRefreshAssignmentsOnRestart(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	paths, err := host.NewSandboxPathInspector([]string{t.TempDir()})
	require.NoError(t, err)
	provider := &sandboxAdmissionProvider{supported: true, proof: "prepared"}
	service := app.New(store, providers.NewRegistry(provider)).WithSandboxPathInspector(paths)
	operator := model.OperatorPrincipal()
	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "group", Name: "Group"})
	require.NoError(t, err)
	for _, id := range []model.SandboxProfileID{"global_one", "group_one", "global_two", "group_two", "explicit"} {
		_, err = service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: request(operator, model.RequestID(id)), ID: id, Name: string(id), Policy: model.SandboxPolicy{Environment: model.Environment{"VALUE": string(id), "FROM_" + string(id): "yes"}}})
		require.NoError(t, err)
	}
	defaults, err := service.SaveSandboxDefaults(ctx, app.SaveSandboxDefaultsRequest{Context: request(operator, "defaults"), Global: "global_one", Groups: map[model.GroupID]model.SandboxProfileID{"group": "group_one"}})
	require.NoError(t, err)
	choice := &model.SandboxSelection{Scopes: []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: model.SandboxProfileRef{ProfileID: "explicit"}}}}
	desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite, HostSandbox: choice}
	agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "worker", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	_, err = service.UpdateGroup(ctx, app.UpdateGroupRequest{Context: operator, ID: "group", Name: "Group", Members: []model.AgentID{agent.Agent.ID}, ExpectedRevision: 1})
	require.NoError(t, err)
	launch := app.LaunchRequest{InitialMessage: "Work", RequestContext: request(operator, "launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.Agent.ID, ExpectedRevision: agent.Agent.Revision}}}
	first, err := service.Launch(ctx, launch)
	require.NoError(t, err)
	require.Equal(t, model.GroupID("group"), first.Execution.Spec.HostSandbox.GroupID)
	require.Equal(t, []model.SandboxProfileID{"global_one", "group_one", "explicit"}, []model.SandboxProfileID{first.Execution.Spec.HostSandbox.Scopes[0].Ref.ProfileID, first.Execution.Spec.HostSandbox.Scopes[1].Ref.ProfileID, first.Execution.Spec.HostSandbox.Scopes[2].Ref.ProfileID})
	require.Equal(t, model.Environment{"VALUE": "explicit", "FROM_global_one": "yes", "FROM_group_one": "yes", "FROM_explicit": "yes"}, provider.preparations[0].HostSandboxPolicy.Composition.Values.Environment)
	_, err = service.Stop(ctx, app.StopRequest{RequestContext: request(operator, "stop"), ExecutionID: first.Execution.ID})
	require.NoError(t, err)
	_, err = service.SaveSandboxDefaults(ctx, app.SaveSandboxDefaultsRequest{Context: request(operator, "change"), ExpectedRevision: defaults.Revision, Global: "global_two", Groups: map[model.GroupID]model.SandboxProfileID{"group": "group_two"}})
	require.NoError(t, err)
	repeated, err := service.Launch(ctx, launch)
	require.NoError(t, err)
	require.True(t, repeated.Repeated)
	require.Len(t, provider.preparations, 1)
	current, err := store.Agent(ctx, agent.Agent.ID)
	require.NoError(t, err)
	require.Equal(t, choice, current.Desired.HostSandbox)
	launch.RequestID = "restart"
	launch.Target.Agent.ExpectedRevision = current.Revision
	_, err = service.Launch(ctx, launch)
	require.NoError(t, err)
	require.Equal(t, model.Environment{"VALUE": "explicit", "FROM_global_two": "yes", "FROM_group_two": "yes", "FROM_explicit": "yes"}, provider.preparations[1].HostSandboxPolicy.Composition.Values.Environment)
	// Omission is explicit and persists independently of subsequent defaults.
	desired.HostSandbox = &model.SandboxSelection{OmitProfiles: true}
	omitted, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "omitted", Name: "Omitted", Desired: desired})
	require.NoError(t, err)
	provider.proof = ""
	_, err = service.Launch(ctx, app.LaunchRequest{InitialMessage: "Work", RequestContext: request(operator, "omit-launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: omitted.Agent.ID, ExpectedRevision: omitted.Agent.Revision}}})
	require.NoError(t, err)
	require.Nil(t, provider.preparations[2].HostSandboxPolicy)
}

func TestSandboxDefaultsPersistAndRequireExplicitValidAssignments(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	_, err = service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: request(operator, "profile"), ID: "profile", Name: "Profile"})
	require.NoError(t, err)
	req := app.SaveSandboxDefaultsRequest{Context: request(operator, "save"), Global: "profile"}
	saved, err := service.SaveSandboxDefaults(ctx, req)
	require.NoError(t, err)
	repeated, err := service.SaveSandboxDefaults(ctx, req)
	require.NoError(t, err)
	require.Equal(t, saved, repeated)
	req.Context.RequestID = "stale"
	_, err = service.SaveSandboxDefaults(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	req.ExpectedRevision = saved.Revision
	req.Global = "missing"
	_, err = service.SaveSandboxDefaults(ctx, req)
	require.ErrorIs(t, err, app.ErrNotFound)
	req.Context.Principal = model.AgentPrincipal("caller")
	_, err = service.SaveSandboxDefaults(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	restored, err := store.SandboxDefaults(ctx)
	require.NoError(t, err)
	require.Equal(t, saved, restored)
}

func TestSandboxDefaultsClearArchivedProfilesAndDisbandedGroups(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	profile, err := service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: request(operator, "profile"), ID: "profile", Name: "Profile"})
	require.NoError(t, err)
	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "group", Name: "Group"})
	require.NoError(t, err)
	defaults, err := service.SaveSandboxDefaults(ctx, app.SaveSandboxDefaultsRequest{Context: request(operator, "defaults"), Global: "profile", Groups: map[model.GroupID]model.SandboxProfileID{"group": "profile"}})
	require.NoError(t, err)
	_, err = service.DisbandGroup(ctx, app.DisbandGroupRequest{Context: request(operator, "disband"), ID: "group", ExpectedRevision: 1})
	require.NoError(t, err)
	current, err := service.GetSandboxDefaults(ctx, operator)
	require.NoError(t, err)
	require.Empty(t, current.Groups)
	require.Equal(t, defaults.Revision+1, current.Revision)
	require.Equal(t, defaults.Global, current.Global)
	_, err = service.SetSandboxProfileArchived(ctx, app.SetSandboxProfileArchivedRequest{Context: request(operator, "archive"), ID: profile.Profile.ID, ExpectedRevision: profile.Profile.Revision, Archived: true})
	require.NoError(t, err)
	current, err = service.GetSandboxDefaults(ctx, operator)
	require.NoError(t, err)
	require.Empty(t, current.Global)
}

func TestTeamDeploymentRetainsSandboxGroupAndLiteralEnvironment(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	cwd := t.TempDir()
	now := time.Now().UTC()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	desired := model.DesiredConfiguration{Harness: "codex", Model: "test", WorkingDirectory: cwd, Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite, Environment: model.Environment{"VALUE": "literal $HOME"}, HostSandbox: &model.SandboxSelection{Scopes: []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: model.SandboxProfileRef{ProfileID: "profile"}}}}}
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "Worker", Desired: desired}}, Waves: []model.TeamWave{{ID: "first", MemberKeys: []string{"worker"}}}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: request(operator, "save"), Draft: app.DefinitionDraft{ID: "team", RevisionID: "one", Name: "Team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "test", Team: &team}})
	require.NoError(t, err)
	deployed, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: request(operator, "deploy"), DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}})
	require.NoError(t, err)
	agent, err := store.Agent(ctx, deployed.Deployment.Members["worker"])
	require.NoError(t, err)
	require.Equal(t, desired.Environment, agent.Desired.Environment)
	require.Equal(t, model.GroupID("group"), agent.Desired.HostSandbox.GroupID)
	require.Equal(t, desired.HostSandbox.Scopes, agent.Desired.HostSandbox.Scopes)
	require.Equal(t, model.AgentActive, agent.Lifecycle)
}

func TestSandboxDefaultsSurvivingAgentRestartsAfterGroupDisband(t *testing.T) {
	for _, authoredGroup := range []bool{false, true} {
		t.Run(map[bool]string{false: "execution-source", true: "authored-source"}[authoredGroup], func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
			require.NoError(t, err)
			defer store.Close()
			paths, err := host.NewSandboxPathInspector([]string{t.TempDir()})
			require.NoError(t, err)
			provider := &sandboxAdmissionProvider{supported: true, proof: "prepared"}
			service := app.New(store, providers.NewRegistry(provider)).WithSandboxPathInspector(paths)
			operator := model.OperatorPrincipal()
			_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "team", Name: "Team"})
			require.NoError(t, err)
			for _, id := range []model.SandboxProfileID{"global", "group", "explicit"} {
				_, err = service.SaveSandboxProfile(ctx, app.SaveSandboxProfileRequest{Context: request(operator, model.RequestID(id)), ID: id, Name: string(id), Policy: model.SandboxPolicy{Environment: model.Environment{"FROM_" + string(id): "yes"}}})
				require.NoError(t, err)
			}
			_, err = service.SaveSandboxDefaults(ctx, app.SaveSandboxDefaultsRequest{Context: request(operator, "defaults"), Global: "global", Groups: map[model.GroupID]model.SandboxProfileID{"team": "group"}})
			require.NoError(t, err)
			choice := &model.SandboxSelection{Scopes: []model.SandboxScopeSelection{{Scope: model.SandboxScopeExplicit, Ref: model.SandboxProfileRef{ProfileID: "explicit"}}}}
			if authoredGroup {
				choice.GroupID = "team"
			}
			created, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "worker", Name: "Worker", Desired: model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite, HostSandbox: choice}})
			require.NoError(t, err)
			group, err := service.UpdateGroup(ctx, app.UpdateGroupRequest{Context: operator, ID: "team", Name: "Team", Members: []model.AgentID{"worker"}, ExpectedRevision: 1})
			require.NoError(t, err)
			first, err := service.Launch(ctx, app.LaunchRequest{InitialMessage: "Work", RequestContext: request(operator, "first"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: "worker", ExpectedRevision: created.Agent.Revision}}})
			require.NoError(t, err)
			require.Equal(t, model.GroupID("team"), first.Execution.Spec.HostSandbox.GroupID)
			_, err = service.Stop(ctx, app.StopRequest{RequestContext: request(operator, "stop"), ExecutionID: first.Execution.ID})
			require.NoError(t, err)
			_, err = service.DisbandGroup(ctx, app.DisbandGroupRequest{Context: request(operator, "disband"), ID: "team", ExpectedRevision: group.Group.Revision})
			require.NoError(t, err)
			current, err := store.Agent(ctx, "worker")
			require.NoError(t, err)
			restarted, err := service.Launch(ctx, app.LaunchRequest{InitialMessage: "Again", RequestContext: request(operator, "restart"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: "worker", ExpectedRevision: current.Revision}}})
			require.NoError(t, err)
			require.Empty(t, restarted.Execution.Spec.HostSandbox.GroupID)
			require.Equal(t, model.Environment{"FROM_global": "yes", "FROM_explicit": "yes"}, provider.preparations[1].HostSandboxPolicy.Composition.Values.Environment)
		})
	}
}
