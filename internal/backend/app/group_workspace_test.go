package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

func TestGroupWorkspaceSelectionPersistsAndClaimsBeforeLaunch(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := sqlite.Open(db)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	provider := &workspaceLaunchProvider{launchBriefProvider: launchBriefProvider{supported: true, unknown: true}}
	svc := app.New(store, providers.NewRegistry(provider))
	op := model.OperatorPrincipal()
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
	require.NoError(t, err)
	cwd := t.TempDir()
	now := time.Now().UTC()
	workspace := model.Workspace{ID: "checkout", State: model.WorkspaceAvailable, Intent: model.WorkspaceIntent{Ownership: model.WorkspaceOwned}, Observation: model.WorkspaceObservation{ActualPath: cwd}, Resource: model.WorkspaceResourceEvidence{Owner: "fixture", Version: 1, Payload: []byte(`{}`)}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.RegisterWorkspace(ctx, workspace))
	desired := model.DesiredConfiguration{Harness: provider.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	profile, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: effect(op, "profile"), ID: "profile", RevisionID: "one", Name: "Profile", Desired: desired})
	require.NoError(t, err)
	_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "team", Profile: &profile.Revision.Ref})
	require.NoError(t, err)
	req := app.CreateGroupMemberRequest{Context: effect(op, "create"), ID: "child", Name: "Child", GroupID: "team", ExpectedGroupRevision: 1, ExpectedDefaultRevision: 1, Workspace: &model.WorkspaceSelection{WorkspaceID: "checkout", ExpectedRevision: 1}}
	created, err := svc.CreateGroupMember(ctx, req)
	require.NoError(t, err)
	require.Equal(t, cwd, created.Agent.Desired.WorkingDirectory)
	saved, err := store.ConfigurationProfile(ctx, "profile", "")
	require.NoError(t, err)
	require.Equal(t, desired.WorkingDirectory, saved.Revision.Desired.WorkingDirectory)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(db)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry(provider))
	launch := app.LaunchRequest{RequestContext: effect(op, "launch"), InitialMessage: "Inspect checkout", Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: "child", ExpectedRevision: created.Agent.Revision}}}
	result, err := svc.Launch(ctx, launch)
	require.ErrorIs(t, err, app.ErrUncertain)
	require.Equal(t, cwd, provider.preparation.Spec.WorkingDirectory)
	uses, err := store.ActiveWorkspaceUses(ctx, "checkout")
	require.NoError(t, err)
	require.Len(t, uses, 1)
	require.Equal(t, result.Execution.ID, uses[0].ExecutionID)
	_, err = svc.RemoveCheckout(ctx, app.RemoveCheckoutRequest{Context: effect(op, "remove"), WorkspaceID: "checkout", ExpectedRevision: 1})
	require.ErrorIs(t, err, app.ErrConflict)
	replay, err := svc.CreateGroupMember(ctx, req)
	require.NoError(t, err)
	require.True(t, replay.Repeated)
	changed := req
	changed.Workspace = &model.WorkspaceSelection{WorkspaceID: "other", ExpectedRevision: 1}
	_, err = svc.CreateGroupMember(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = svc.Launch(ctx, launch)
	require.NoError(t, err)
	require.Equal(t, 1, provider.releases)
	provider.exited = true
	_, err = svc.Recover(ctx, app.RecoverRequest{Principal: op})
	require.NoError(t, err)
	uses, err = store.ActiveWorkspaceUses(ctx, "checkout")
	require.NoError(t, err)
	require.Empty(t, uses)
	agent, err := store.Agent(ctx, "child")
	require.NoError(t, err)
	launch.RequestID = "restart"
	launch.Target.Agent.ExpectedRevision = agent.Revision
	_, err = svc.Launch(ctx, launch)
	require.ErrorIs(t, err, app.ErrUncertain)
	uses, err = store.ActiveWorkspaceUses(ctx, "checkout")
	require.NoError(t, err)
	require.Len(t, uses, 1)

	agent, err = store.Agent(ctx, "child")
	require.NoError(t, err)
	changedDesired := agent.Desired
	changedDesired.WorkingDirectory = t.TempDir()
	updated, err := svc.UpdateAgent(ctx, app.UpdateAgentRequest{Context: op, ID: agent.ID, ExpectedRevision: agent.Revision, Name: agent.Name, Desired: changedDesired})
	require.NoError(t, err)
	uses, err = store.ActiveWorkspaceUses(ctx, "checkout")
	require.NoError(t, err)
	require.Len(t, uses, 1, "editing future cwd must not release an active execution")
	_, err = svc.Recover(ctx, app.RecoverRequest{Principal: op})
	require.NoError(t, err)
	launch.RequestID = "new-directory"
	launch.Target.Agent.ExpectedRevision = updated.Agent.Revision
	_, err = svc.Launch(ctx, launch)
	require.ErrorIs(t, err, app.ErrUncertain)
	require.Equal(t, changedDesired.WorkingDirectory, provider.preparation.Spec.WorkingDirectory)
	uses, err = store.ActiveWorkspaceUses(ctx, "checkout")
	require.NoError(t, err)
	require.Empty(t, uses)

}

func TestGroupWorkspaceSelectionFencesConcurrentChange(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	op := model.OperatorPrincipal()
	now := time.Now().UTC()
	cwd := t.TempDir()
	workspace := model.Workspace{ID: "checkout", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.RegisterWorkspace(ctx, workspace))
	changed := false
	wrapped := &editingGroupProfileStore{Store: store, before: func() {
		if changed {
			return
		}
		changed = true
		_, err := store.UpdateWorkspaceObservation(ctx, "checkout", 1, model.WorkspaceUncertain, workspace.Observation, workspace.Resource, now)
		require.NoError(t, err)
	}}
	svc := app.New(wrapped, providers.NewRegistry(&partialTeamProvider{&preparedWorkProvider{}}))
	_, err := svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
	require.NoError(t, err)
	harness := "prepared-work"
	req := app.CreateGroupMemberRequest{Context: effect(op, "member"), ID: "child", Name: "Child", GroupID: "team", ExpectedGroupRevision: 1, Workspace: &model.WorkspaceSelection{WorkspaceID: "checkout", ExpectedRevision: 1}, ConfigurationOverrides: &model.ConfigurationOptions{Harness: &harness}}
	_, err = svc.CreateGroupMember(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.Agent(ctx, "child")
	require.ErrorIs(t, err, app.ErrNotFound)
	workspace, err = store.Workspace(ctx, "checkout")
	require.NoError(t, err)
	workspace, err = store.UpdateWorkspaceObservation(ctx, "checkout", workspace.Revision, model.WorkspaceAvailable, workspace.Observation, workspace.Resource, now)
	require.NoError(t, err)
	req.Workspace = &model.WorkspaceSelection{WorkspaceID: "checkout", ExpectedRevision: workspace.Revision}
	created, err := svc.CreateGroupMember(ctx, req)
	require.NoError(t, err)
	require.Equal(t, cwd, created.Agent.Desired.WorkingDirectory)
}

// Only native recovery is doubled; settlement and restart use the public app.
type workspaceLaunchProvider struct {
	launchBriefProvider
	exited bool
}

func (p *workspaceLaunchProvider) Recover(context.Context, ports.RecoveryRequest) (ports.RecoveryResult, error) {
	if p.exited {
		return ports.RecoveryResult{State: ports.RecoveryExited}, nil
	}
	return ports.RecoveryResult{State: ports.RecoveryUnknown}, nil
}

func TestGroupWorkspaceRemovalReservesBeforeNativeEffect(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	op := model.OperatorPrincipal()
	provider := &partialTeamProvider{&preparedWorkProvider{}}
	host := &groupWorkspaceRemovalHost{}
	svc := app.New(store, providers.NewRegistry(provider)).WithWorkspaceHost(host)
	workspace, err := svc.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: effect(op, "checkout"), ID: "checkout", Intent: model.WorkspaceIntent{IntendedPath: t.TempDir()}})
	require.NoError(t, err)
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "team", Name: "Team"})
	require.NoError(t, err)
	harness := "prepared-work"
	created, err := svc.CreateGroupMember(ctx, app.CreateGroupMemberRequest{Context: effect(op, "child"), ID: "child", Name: "Child", GroupID: "team", ExpectedGroupRevision: 1, ConfigurationOverrides: &model.ConfigurationOptions{Harness: &harness}, Workspace: &model.WorkspaceSelection{WorkspaceID: "checkout", ExpectedRevision: workspace.Workspace.Revision}})
	require.NoError(t, err)
	host.before = func() {
		current, err := store.Workspace(ctx, "checkout")
		require.NoError(t, err)
		_, err = store.UpdateWorkspaceObservation(ctx, current.ID, current.Revision, model.WorkspaceAvailable, current.Observation, current.Resource, time.Now().UTC())
		require.NoError(t, err)
		_, err = svc.Launch(ctx, app.LaunchRequest{RequestContext: effect(op, "launch"), InitialMessage: "Inspect", Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: "child", ExpectedRevision: created.Agent.Revision}}})
		require.ErrorIs(t, err, app.ErrConflict)
		require.Empty(t, provider.preparations)
	}
	removed, err := svc.RemoveCheckout(ctx, app.RemoveCheckoutRequest{Context: effect(op, "remove"), WorkspaceID: "checkout", ExpectedRevision: workspace.Workspace.Revision})
	require.NoError(t, err)
	require.Equal(t, model.WorkspaceRemoved, removed.Workspace.State)
}

type groupWorkspaceRemovalHost struct {
	journeyWorkspaceHost
	before func()
}

func (h *groupWorkspaceRemovalHost) RemoveCheckout(ctx context.Context, req ports.CheckoutRemoveRequest, permit ports.EffectPermit) (ports.WorkspaceEffectResult, error) {
	h.before()
	return h.journeyWorkspaceHost.RemoveCheckout(ctx, req, permit)
}

// Losing the process after durable admission must not strand the original
// request behind the reservation's own revision bump or replay native removal.
func TestGroupWorkspaceRemovalRetryAfterAdmissionAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	op := model.OperatorPrincipal()
	host := &journeyWorkspaceHost{}
	svc := app.New(store, providers.NewRegistry()).WithWorkspaceHost(host)
	workspace, err := svc.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: effect(op, "checkout"), ID: "checkout", Intent: model.WorkspaceIntent{IntendedPath: t.TempDir()}})
	require.NoError(t, err)
	interrupted := &interruptedWorkspaceRemovalStore{Store: store}
	svc = app.New(interrupted, providers.NewRegistry()).WithWorkspaceHost(host)
	req := app.RemoveCheckoutRequest{Context: effect(op, "remove"), WorkspaceID: "checkout", ExpectedRevision: workspace.Workspace.Revision}
	_, err = svc.RemoveCheckout(ctx, req)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, host.removes)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc = app.New(store, providers.NewRegistry()).WithWorkspaceHost(host)
	replay, err := svc.RemoveCheckout(ctx, req)
	require.NoError(t, err)
	require.Equal(t, model.WorkspacePending, replay.Workspace.State)
	require.Greater(t, replay.Workspace.Revision, req.ExpectedRevision)
	require.Zero(t, host.removes)
	changed := req
	changed.Destructive = true
	_, err = svc.RemoveCheckout(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	changed = req
	changed.ExpectedRevision = replay.Workspace.Revision
	_, err = svc.RemoveCheckout(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	// A new request cannot overlap the reserved removal either.
	changed.Context.RequestID = "another"
	_, err = svc.RemoveCheckout(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
}

type interruptedWorkspaceRemovalStore struct{ *sqlite.Store }

func (s *interruptedWorkspaceRemovalStore) AdmitWorkspaceEffect(ctx context.Context, in app.WorkspaceEffectAdmission) (app.WorkspaceEffectAdmissionResult, error) {
	out, err := s.Store.AdmitWorkspaceEffect(ctx, in)
	if err == nil && in.Operation.Kind == model.OperationRemoveWorkspace {
		return out, context.Canceled
	}
	return out, err
}

func TestGroupWorkspaceRemovalExcludesBoundedWorkPublication(t *testing.T) {
	ctx := context.Background()
	store, _, _ := regressionService(t)
	op := model.OperatorPrincipal()
	host := &groupWorkspaceRemovalHost{}
	svc := app.New(store, providers.NewRegistry()).WithWorkspaceHost(host)
	workspace, err := svc.CreateCheckout(ctx, app.CreateCheckoutRequest{Context: effect(op, "checkout"), ID: "checkout", Intent: model.WorkspaceIntent{IntendedPath: t.TempDir()}})
	require.NoError(t, err)
	now := time.Now().UTC()
	run := model.WorkRun{ID: "work", RequestID: "work", Requester: op, Spec: model.WorkRunSpec{WorkspaceID: "checkout", WorkspaceRevision: workspace.Workspace.Revision}, WorkspaceUseID: "use", Revision: 1, CreatedAt: now, UpdatedAt: now}
	host.before = func() {
		_, _, err := store.CreateWorkRun(ctx, run, nil)
		require.ErrorIs(t, err, app.ErrConflict)
		_, err = store.WorkRun(ctx, "work")
		require.ErrorIs(t, err, app.ErrNotFound)
		uses, err := store.ActiveWorkspaceUses(ctx, "checkout")
		require.NoError(t, err)
		require.Empty(t, uses)
		// Even an inspection reporting available must not cancel an in-flight removal.
		current, err := store.Workspace(ctx, "checkout")
		require.NoError(t, err)
		current, err = store.UpdateWorkspaceObservation(ctx, current.ID, current.Revision, model.WorkspaceAvailable, current.Observation, current.Resource, now)
		require.NoError(t, err)
		run.Spec.WorkspaceRevision = current.Revision
		_, _, err = store.CreateWorkRun(ctx, run, nil)
		require.ErrorIs(t, err, app.ErrConflict)
	}
	_, err = svc.RemoveCheckout(ctx, app.RemoveCheckoutRequest{Context: effect(op, "remove"), WorkspaceID: "checkout", ExpectedRevision: workspace.Workspace.Revision})
	require.NoError(t, err)
}
