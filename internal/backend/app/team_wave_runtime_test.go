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
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestTeamWaveWaitSurvivesReopenAndStartsLaterMember(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "settled_after_restart", true: "maximum_wait"}[timeout], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db")
			store, err := sqlite.Open(path)
			require.NoError(t, err)
			defer func() { require.NoError(t, store.Close()) }()
			now := time.Now().UTC()
			provider := &waveActivityProvider{activity: ports.AgentActivityIdle, runtimes: map[model.ExecutionID]*waveActivityRuntime{}}
			service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
			cwd := t.TempDir()
			require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: cwd, ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
			desired := model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: cwd, Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
			team := model.TeamDefinition{WorkspacePolicy: model.WorkspacePolicyShared, Members: []model.TeamMemberSpec{{Key: "lead", Name: "Lead", Desired: desired, Required: true}, {Key: "worker", Name: "Worker", Desired: desired, Required: true}}, Waves: []model.TeamWave{{ID: "planning", MemberKeys: []string{"lead"}, RequiredReady: true, WaitForIdle: true}, {ID: "implementation", MemberKeys: []string{"worker"}, DependsOn: []string{"planning"}, RequiredReady: true}}}
			saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: app.DefinitionDraft{ID: "team", RevisionID: "team_v1", Name: "Team", Kind: model.DefinitionTeam, SchemaVersion: 1, Source: "test", Team: &team}})
			require.NoError(t, err)
			deployed, err := service.DeployTeam(ctx, app.DeployTeamRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "deploy"}, DeploymentID: "deployment", Instantiation: model.TeamInstantiation{Definition: model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionTeam}, GroupID: "group", Workspaces: model.TeamWorkspaceSelection{Shared: &model.TeamWorkspaceInput{WorkspaceID: "workspace", ExpectedRevision: 1}}}})
			require.NoError(t, err)
			reconcile := func() {
				for range 8 {
					_, err := service.ReconcilePendingWork(ctx)
					require.NoError(t, err)
				}
			}
			reconcile()
			require.Len(t, provider.preparations, 1, "initial idle must hold the worker")
			if !timeout {
				provider.activity = ports.AgentActivityActive
				reconcile()
				run, e := store.WorkRun(ctx, deployed.Deployment.WorkRunID)
				require.NoError(t, e)
				require.NotEmpty(t, run.NodeEvidence, "%+v", run.Run.NodeAttempts)
			}
			require.Len(t, provider.preparations, 1)
			require.NoError(t, store.Close())
			store, err = sqlite.Open(path)
			require.NoError(t, err)
			service = app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now })
			_, err = service.Recover(ctx, app.RecoverRequest{Principal: model.OperatorPrincipal()})
			require.NoError(t, err)
			if timeout {
				now = now.Add(8 * time.Minute)
			} else {
				provider.activity = ports.AgentActivityIdle
			}
			reconcile()
			require.Len(t, provider.preparations, 2, "the worker must start after settlement or the maximum wait, even after reopening")
			result, err := service.GetTeamDeployment(ctx, app.GetTeamDeploymentRequest{Principal: model.OperatorPrincipal(), DeploymentID: deployed.Deployment.ID})
			require.NoError(t, err)
			require.Equal(t, model.DeploymentReady, result.Deployment.State)
		})
	}
}

type waveActivityProvider struct {
	preparedWorkProvider
	activity ports.AgentActivityObservedState
	runtimes map[model.ExecutionID]*waveActivityRuntime
}

func (p *waveActivityProvider) Prepare(ctx context.Context, req ports.PreparationRequest) (ports.PreparedAttempt, error) {
	a, e := p.preparedWorkProvider.Prepare(ctx, req)
	return &waveActivityAttempt{PreparedAttempt: a, provider: p}, e
}
func (p *waveActivityProvider) Recover(ctx context.Context, req ports.RecoveryRequest) (ports.RecoveryResult, error) {
	r := p.runtimes[req.ExecutionID]
	if r == nil {
		return ports.RecoveryResult{State: ports.RecoveryUnknown}, nil
	}
	o, e := r.Observe(ctx)
	return ports.RecoveryResult{State: ports.RecoveryControlled, Runtime: r, Observation: o, Evidence: o.Evidence, Attempt: req.Attempt}, e
}

type waveActivityAttempt struct {
	ports.PreparedAttempt
	provider *waveActivityProvider
}

func (a *waveActivityAttempt) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	r, e := a.PreparedAttempt.Release(ctx, permit)
	if e == nil {
		runtime := &waveActivityRuntime{Runtime: r.Runtime, provider: a.provider}
		a.provider.runtimes[r.Runtime.ExecutionID()] = runtime
		r.Runtime = runtime
	}
	return r, e
}

type waveActivityRuntime struct {
	ports.Runtime
	provider *waveActivityProvider
}

func (r *waveActivityRuntime) Observe(ctx context.Context) (ports.Observation, error) {
	o, e := r.Runtime.Observe(ctx)
	o.AgentActivity = r.provider.activity
	o.AgentActivityObservedAt = o.ObservedAt
	return o, e
}
