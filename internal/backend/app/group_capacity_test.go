package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	sqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestGroupCapacitySerializesMembershipAndReactivation(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	for _, id := range []model.AgentID{"a", "b", "c"} {
		_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: id, Name: string(id), Desired: desired})
		require.NoError(t, err)
	}
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "g", Name: "G", Members: []model.AgentID{"a", "b"}})
	require.NoError(t, err)
	retired, err := svc.RetireAgent(ctx, app.RetireAgentRequest{Context: op, ID: "b", ExpectedRevision: 1, Reason: "inactive"})
	require.NoError(t, err)
	set := app.SetGroupCapacityRequest{Principal: op, ID: "g", ExpectedRevision: 1, MaxActiveMembers: 2}
	denied := set
	denied.Principal = model.AgentPrincipal("a")
	_, err = svc.SetGroupCapacity(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	group, err := svc.SetGroupCapacity(ctx, set)
	require.NoError(t, err)
	_, err = svc.SetGroupCapacity(ctx, set)
	require.ErrorIs(t, err, app.ErrConflict)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, e := svc.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "g", Name: "G", ExpectedRevision: group.Revision, Members: []model.AgentID{"a", "b", "c"}})
		results <- e
	}()
	go func() {
		defer wg.Done()
		<-start
		_, e := svc.ReactivateAgent(ctx, app.ReactivateAgentRequest{Context: op, ID: "b", ExpectedRevision: retired.Agent.Revision})
		results <- e
	}()
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for e := range results {
		if e == nil {
			successes++
		}
	}
	require.Equal(t, 1, successes)
	current, err := store.Group(ctx, "g")
	require.NoError(t, err)
	active := 0
	for _, id := range current.Members {
		a, e := store.Agent(ctx, id)
		require.NoError(t, e)
		if a.Lifecycle == model.AgentActive {
			active++
		}
	}
	require.Equal(t, 2, active)
	// Lowering a limit retains agents; name/order-only edits and removals stay usable.
	group, err = svc.SetGroupCapacity(ctx, app.SetGroupCapacityRequest{Principal: op, ID: "g", ExpectedRevision: current.Revision, MaxActiveMembers: 1})
	require.NoError(t, err)
	updated, err := svc.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "g", Name: "Renamed", ExpectedRevision: group.Revision, Members: group.Members})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry())
	got, err := store.Group(ctx, "g")
	require.NoError(t, err)
	require.Equal(t, int64(1), got.MaxActiveMembers)
	_, err = svc.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "g", Name: "Renamed", ExpectedRevision: updated.Group.Revision, Members: []model.AgentID{"a"}})
	require.NoError(t, err)
	snapshot, err := svc.Snapshot(ctx, app.SnapshotRequest{Principal: op})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 3)
	require.Empty(t, snapshot.Executions)
}

func TestGroupCapacityRejectsReinforcementWithoutPartialAdmission(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	shared := model.Agent{ID: "shared_member", Name: "shared", Lifecycle: model.AgentActive, Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateAgent(ctx, shared))
	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "existing_group", Name: "existing", Members: []model.AgentID{shared.ID}})
	require.NoError(t, err)
	workspacePath := t.TempDir()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "existing_workspace", Intent: model.WorkspaceIntent{IntendedPath: workspacePath, Provenance: model.WorkspaceRegistered, Ownership: model.WorkspaceExternal}, State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: workspacePath, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "reinforcement", Name: "reinforcement", Desired: shared.Desired, Required: true}}, Waves: []model.TeamWave{{ID: "join", MemberKeys: []string{"reinforcement"}, RequiredReady: true}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_reinforcement"}, Draft: app.DefinitionDraft{ID: "reinforcement_team", RevisionID: "reinforcement_v1", Name: "reinforcement", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "reinforce", Team: &team}})
	require.NoError(t, err)
	_, err = service.SetGroupCapacity(ctx, app.SetGroupCapacityRequest{Principal: operator, ID: "existing_group", ExpectedRevision: 1, MaxActiveMembers: 1})
	require.NoError(t, err)
	_, err = service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: operator, RequestID: "deploy_reinforcement"}, DeploymentID: "reinforcement_deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}, Target: model.TeamDeploymentTarget{Kind: model.TeamTargetExistingGroup, GroupID: "existing_group"}, Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "existing_workspace", ExpectedRevision: 1}}}})
	require.ErrorIs(t, err, app.ErrConflict)
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Empty(t, snapshot.Executions)
	group, err := store.Group(ctx, "existing_group")
	require.NoError(t, err)
	require.Equal(t, []model.AgentID{shared.ID}, group.Members)
	deployments, err := service.ListTeamDeployments(ctx, app.ListTeamDeploymentsRequest{Principal: operator})
	require.NoError(t, err)
	require.Empty(t, deployments)
}

func TestGroupCapacityRejectsDefaultMemberWithoutOrphan(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "group", Name: "Group"})
	require.NoError(t, err)
	save := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile_one"}, ID: "profile", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "codex", Model: "first", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}, Startup: &model.ProfileStartup{AgentName: "Suggested", Context: "Context", InitialMessage: "Brief"}}
	first, err := svc.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	set := app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", Profile: &first.Revision.Ref}
	defaults, err := svc.SetGroupConfiguration(ctx, set)
	require.NoError(t, err)
	_, err = svc.SetGroupConfiguration(ctx, set)
	require.ErrorIs(t, err, app.ErrConflict)
	denied := set
	denied.Principal = model.AgentPrincipal("caller")
	_, err = svc.SetGroupConfiguration(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	save.Context.RequestID = "profile_two"
	save.ExpectedRevision = first.Profile.Revision
	save.RevisionID = "two"
	save.Desired.Model = "second"
	_, err = svc.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	in := app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: op, RequestID: "member"}, GroupID: "group", ID: "member", Name: "Named", ExpectedGroupRevision: 1, ExpectedDefaultRevision: defaults.Revision}
	result, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.Equal(t, "first", result.Agent.Desired.Model)
	require.Equal(t, &first.Revision.Ref, result.Agent.ConfigurationProfile)
	require.Equal(t, []model.AgentID{"member"}, result.Group.Members)
	group, err := svc.SetGroupCapacity(ctx, app.SetGroupCapacityRequest{Principal: op, ID: "group", ExpectedRevision: result.Group.Revision, MaxActiveMembers: 1})
	require.NoError(t, err)
	in.ID = "overflow"
	in.Context.RequestID = "overflow"
	in.ExpectedGroupRevision = group.Revision
	_, err = svc.CreateGroupMember(ctx, in)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.Agent(ctx, in.ID)
	require.ErrorIs(t, err, app.ErrNotFound)
}

func TestGroupCapacityRefusesBeforeCheckoutCreation(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC)
	provider := &preparedWorkProvider{}
	checkoutHost, err := host.NewCheckoutHost("git")
	require.NoError(t, err)
	repository := lifecycleRepository(t)
	checkoutPath := filepath.Join(t.TempDir(), "refused-checkout")
	service := app.New(store, providers.NewRegistry(provider)).WithWorkspaceHost(checkoutHost).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	shared := model.Agent{ID: "shared_member", Name: "shared", Lifecycle: model.AgentActive, Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateAgent(ctx, shared))
	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: operator, ID: "existing_group", Name: "existing", Members: []model.AgentID{shared.ID}})
	require.NoError(t, err)
	workspacePath := t.TempDir()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "existing_workspace", Intent: model.WorkspaceIntent{IntendedPath: workspacePath, Provenance: model.WorkspaceRegistered, Ownership: model.WorkspaceExternal}, State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: workspacePath, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "reinforcement", Name: "reinforcement", Desired: shared.Desired, Required: true}}, Waves: []model.TeamWave{{ID: "join", MemberKeys: []string{"reinforcement"}, RequiredReady: true}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_reinforcement"}, Draft: app.DefinitionDraft{ID: "reinforcement_team", RevisionID: "reinforcement_v1", Name: "reinforcement", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "reinforce", Team: &team}})
	require.NoError(t, err)
	_, err = service.SetGroupCapacity(ctx, app.SetGroupCapacityRequest{Principal: operator, ID: "existing_group", ExpectedRevision: 1, MaxActiveMembers: 1})
	require.NoError(t, err)
	_, err = service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: operator, RequestID: "deploy_reinforcement"}, DeploymentID: "reinforcement_deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}, Target: model.TeamDeploymentTarget{Kind: model.TeamTargetExistingGroup, GroupID: "existing_group"}, Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "new_workspace", CreateIntent: &model.WorkspaceIntent{Repository: repository, IntendedPath: checkoutPath, BaseRevision: "HEAD", Branch: "capacity-refused", RetainOnFinish: true}}}}})
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoDirExists(t, checkoutPath)
	snapshot, err := service.Snapshot(ctx, app.SnapshotRequest{Principal: operator})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Empty(t, snapshot.Executions)
	group, err := store.Group(ctx, "existing_group")
	require.NoError(t, err)
	require.Equal(t, []model.AgentID{shared.ID}, group.Members)
	deployments, err := service.ListTeamDeployments(ctx, app.ListTeamDeploymentsRequest{Principal: operator})
	require.NoError(t, err)
	require.Empty(t, deployments)
}
