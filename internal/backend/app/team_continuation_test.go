//go:build linux || darwin

package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/host"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

func continuationFixture(t *testing.T, provider ports.Provider, gated bool) (*app.Service, *sqlite.Store, app.TeamDeploymentResult, time.Time) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	now := time.Date(2026, 9, 7, 11, 0, 0, 0, time.UTC)
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	cwd := t.TempDir()
	require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", Intent: model.WorkspaceIntent{IntendedPath: cwd, Provenance: model.WorkspaceRegistered, Ownership: model.WorkspaceExternal}, State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "reinforcement", Name: "worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: cwd, Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}, Required: true}}, Waves: []model.TeamWave{{ID: "wave", MemberKeys: []string{"reinforcement"}, RequiredReady: gated}}, Briefings: []model.TeamBriefing{{ID: "later", Body: "follow-up instruction", Timing: model.BriefingAfterReady, MemberKeys: []string{"reinforcement"}}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "fixture", Team: &team}})
	require.NoError(t, err)
	deployment, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}, Target: model.TeamDeploymentTarget{Kind: model.TeamTargetNewGroup, GroupID: "group"}, Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}})
	require.NoError(t, err)
	return service, store, deployment, now
}
func TestTeamDeferredBriefSurvivesUngatedCompletion(t *testing.T) {
	ctx := context.Background()
	provider := &continuationDelayedProvider{}
	service, store, deployed, _ := continuationFixture(t, provider, false)
	operator := model.OperatorPrincipal()
	var err error
	for range 4 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	current, err := service.GetTeamDeployment(ctx, app.GetTeamDeploymentRequest{Principal: operator, DeploymentID: deployed.Deployment.ID})
	require.NoError(t, err)
	require.Equal(t, model.DeploymentReady, current.Deployment.State)
	provider.ready = true
	for range 4 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	after, err := store.TeamDeployment(ctx, deployed.Deployment.ID)
	require.NoError(t, err)
	require.NotEmpty(t, after.BriefingOperationIDs["reinforcement"], "ungated after-ready briefing must eventually be admitted when member becomes ready")
}

type continuationDelayedProvider struct {
	preparedWorkProvider
	ready bool
}

func (p *continuationDelayedProvider) Prepare(_ context.Context, r ports.PreparationRequest) (ports.PreparedAttempt, error) {
	return &continuationDelayedAttempt{preparedWorkAttempt: preparedWorkAttempt{request: r}, provider: p}, nil
}

type continuationDelayedAttempt struct {
	preparedWorkAttempt
	provider *continuationDelayedProvider
}

func (p *continuationDelayedAttempt) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	result, err := p.preparedWorkAttempt.Release(ctx, permit)
	if err == nil {
		result.Runtime = &continuationDelayedRuntime{preparedWorkRuntime: preparedWorkRuntime{id: p.request.Spec.ExecutionID}, provider: p.provider}
	}
	return result, err
}

type continuationDelayedRuntime struct {
	preparedWorkRuntime
	provider *continuationDelayedProvider
}

func (r *continuationDelayedRuntime) Observe(ctx context.Context) (ports.Observation, error) {
	o, err := r.preparedWorkRuntime.Observe(ctx)
	if !r.provider.ready {
		o.Context = ports.ContextPending
	}
	return o, err
}

func TestTeamAsyncStopSettlesWithoutReissuing(t *testing.T) {
	ctx := context.Background()
	provider := &continuationAsyncProvider{}
	service, store, deployed, _ := continuationFixture(t, provider, true)
	operator := model.OperatorPrincipal()
	var err error
	for range 4 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	current, err := service.GetTeamDeployment(ctx, app.GetTeamDeploymentRequest{Principal: operator, DeploymentID: deployed.Deployment.ID})
	require.NoError(t, err)
	stopping, err := service.StandDownDeployment(ctx, app.StandDownDeploymentRequest{Context: app.RequestContext{Principal: operator, RequestID: "stop_async"}, DeploymentID: current.Deployment.ID, ExpectedRevision: current.Deployment.Revision, Reason: "done"})
	require.NoError(t, err)
	require.Equal(t, model.DeploymentStandingDown, stopping.Deployment.State)
	provider.exited = true
	for range 4 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	after, err := store.TeamDeployment(ctx, current.Deployment.ID)
	require.NoError(t, err)
	require.Equal(t, model.DeploymentStopped, after.State, "asynchronous accepted stop must settle after runtime reports exit")
	require.Equal(t, 1, provider.stops)
}

type continuationAsyncProvider struct {
	preparedWorkProvider
	exited bool
	stops  int
}

func (p *continuationAsyncProvider) Prepare(_ context.Context, r ports.PreparationRequest) (ports.PreparedAttempt, error) {
	return &continuationAsyncAttempt{preparedWorkAttempt: preparedWorkAttempt{request: r}, provider: p}, nil
}

type continuationAsyncAttempt struct {
	preparedWorkAttempt
	provider *continuationAsyncProvider
}

func (p *continuationAsyncAttempt) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	result, err := p.preparedWorkAttempt.Release(ctx, permit)
	if err == nil {
		result.Runtime = &continuationAsyncRuntime{preparedWorkRuntime: preparedWorkRuntime{id: p.request.Spec.ExecutionID}, provider: p.provider}
	}
	return result, err
}

type continuationAsyncRuntime struct {
	preparedWorkRuntime
	provider *continuationAsyncProvider
}

func (r *continuationAsyncRuntime) Observe(ctx context.Context) (ports.Observation, error) {
	o, err := r.preparedWorkRuntime.Observe(ctx)
	if r.provider.exited {
		o.Workload = ports.WorkloadExited
	}
	return o, err
}
func (r *continuationAsyncRuntime) Stop(context.Context, ports.StopRequest) (ports.StopResult, error) {
	r.provider.stops++
	return ports.StopResult{Disposition: ports.EffectAccepted, Acknowledged: true, Exited: false}, nil
}

func TestTeamRebriefAdmissionSurvivesServiceRestart(t *testing.T) {
	ctx := context.Background()
	provider := &preparedWorkProvider{}
	service, store, deployed, now := continuationFixture(t, provider, true)
	operator := model.OperatorPrincipal()
	var err error
	for range 4 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	current, err := service.GetTeamDeployment(ctx, app.GetTeamDeploymentRequest{Principal: operator, DeploymentID: deployed.Deployment.ID})
	require.NoError(t, err)
	crashing := app.New(&rebriefAdmissionLossStore{Store: store}, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	_, err = crashing.RebriefDeployment(ctx, app.RebriefDeploymentRequest{Context: app.RequestContext{Principal: operator, RequestID: "rebrief_crash"}, DeploymentID: current.Deployment.ID, ExpectedRevision: current.Deployment.Revision, Definition: current.Deployment.Definition})
	require.ErrorIs(t, err, context.Canceled)
	service = app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	for range 4 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	after, err := store.TeamDeployment(ctx, current.Deployment.ID)
	require.NoError(t, err)
	require.Len(t, after.Rebriefs, 1)
	require.Equal(t, model.TeamRebriefCompleted, after.Rebriefs[0].State, "committed rebrief must resume after service restart without client retry")
}

type rebriefAdmissionLossStore struct{ app.Store }

func (s *rebriefAdmissionLossStore) BeginTeamRebrief(ctx context.Context, id model.DeploymentID, expected model.Revision, definition model.DefinitionRef, principal model.Principal, requestID model.RequestID, digest string, at time.Time) (model.TeamDeployment, model.TeamRebrief, bool, error) {
	d, r, repeated, err := s.Store.BeginTeamRebrief(ctx, id, expected, definition, principal, requestID, digest, at)
	if err != nil {
		return d, r, repeated, err
	}
	return d, r, repeated, context.Canceled
}

func TestTeamUnmaterializedRhythmAllowsRetainingCleanup(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	checkout, err := host.NewCheckoutHost("git")
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry(&preparedWorkProvider{})).WithWorkspaceHost(checkout)
	operator := model.OperatorPrincipal()
	rhythm, err := service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: operator, RequestID: "source"}, ID: "source", RevisionID: "source_v1", Name: "source", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: time.Now().Add(time.Hour)}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "hello", GroupID: "group"}}, Policy: model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineQueue, ExpiresAfter: time.Hour, Overlap: model.OverlapReplace, MaxActive: 1, Deadline: time.Minute, Retry: model.RetryPolicy{MaxAttempts: 1}}})
	require.NoError(t, err)
	team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "worker", Name: "worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: "/placeholder", Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}, Required: true}}, Waves: []model.TeamWave{{ID: "wave", MemberKeys: []string{"worker"}, RequiredReady: true}}, Automation: []model.AutomationRuleRef{{RuleID: rhythm.Rule.ID, RevisionID: rhythm.Revision.ID, ContentHash: rhythm.Revision.ContentHash}}}
	definition, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: operator, RequestID: "save"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "fixture", Team: &team}})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "checkout")
	deployed, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: operator, RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: definition.Definition.ID, RevisionID: definition.Revision.ID, ContentHash: definition.Revision.ContentHash, Kind: model.DefinitionTeam}, Target: model.TeamDeploymentTarget{Kind: model.TeamTargetNewGroup, GroupID: "group"}, Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", CreateIntent: &model.WorkspaceIntent{Repository: lifecycleRepository(t), IntendedPath: path, BaseRevision: "HEAD", Branch: "team-worker", RetainOnFinish: true}}}}})
	require.ErrorIs(t, err, app.ErrInvalid)
	require.Equal(t, model.DeploymentPartial, deployed.Deployment.State)
	stopped, err := service.StandDownDeployment(ctx, app.StandDownDeploymentRequest{Context: app.RequestContext{Principal: operator, RequestID: "stop"}, DeploymentID: deployed.Deployment.ID, ExpectedRevision: deployed.Deployment.Revision, Reason: "abandon partial creation"})
	require.NoError(t, err)
	require.Equal(t, model.DeploymentStopped, stopped.Deployment.State)
	require.DirExists(t, path)
	for _, id := range stopped.Deployment.Members {
		member, err := store.Agent(ctx, id)
		require.NoError(t, err)
		require.Equal(t, model.AgentRetired, member.Lifecycle)
	}
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
}

func TestTeamUnavailableDeferredBriefLeavesIndependentDeploymentRunnable(t *testing.T) {
	ctx := context.Background()
	provider := &continuationDelayedProvider{}
	service, store, first, now := continuationFixture(t, provider, false)
	for range 4 {
		_, err := service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	// Recovery cannot establish the old runtime; unrelated work must still advance.
	service = app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
	_, err := service.Recover(ctx, app.RecoverRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	second, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "second"}, DeploymentID: "second", Instantiation: model.TeamInstantiation{Definition: first.Deployment.Definition, Target: model.TeamDeploymentTarget{Kind: model.TeamTargetNewGroup, GroupID: "second"}, Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}})
	require.NoError(t, err)
	for range 4 {
		_, err = service.ReconcilePendingWork(ctx)
		require.NoError(t, err)
	}
	firstAfter, err := store.TeamDeployment(ctx, first.Deployment.ID)
	require.NoError(t, err)
	require.Empty(t, firstAfter.BriefingOperationIDs, "unknown runtime does not fabricate delivery")
	after, err := store.TeamDeployment(ctx, second.Deployment.ID)
	require.NoError(t, err)
	require.Equal(t, model.DeploymentReady, after.State, "unavailable deferred briefing must not block unrelated fresh deployment")
}
