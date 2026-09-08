package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
	"time"
)

func TestPerformerTimeoutQueueRestartAndFreshRetryDeadline(t *testing.T) {
	for _, profileTimeout := range []time.Duration{500 * time.Millisecond, time.Minute} {
		for _, retry := range []bool{false, true} {
			t.Run(profileTimeout.String()+map[bool]string{false: "queued", true: "retry"}[retry], func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(t.TempDir(), "db")
				store, err := sqlite.Open(path)
				require.NoError(t, err)
				now := time.Now().UTC()
				initial := now
				budget := 2 * time.Second
				if profileTimeout > 0 && profileTimeout < budget {
					budget = profileTimeout
				}
				host := &programHostFake{exitCodes: []int{1, 0}}
				service := app.New(store, providers.NewRegistry()).WithProgramHost(host).WithClock(func() time.Time { return now })
				require.NoError(t, store.RegisterWorkspace(ctx, model.Workspace{ID: "workspace", State: model.WorkspaceAvailable, Observation: model.WorkspaceObservation{ActualPath: t.TempDir(), ObservedAt: now}, Revision: 1, CreatedAt: now, UpdatedAt: now}))
				profile, err := service.SaveProgramProfile(ctx, app.SaveProgramProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "profile"}, ID: "check", RevisionID: "v1", Name: "check", Executable: "check", Sandbox: model.SandboxWorkspaceWrite, Timeout: profileTimeout, OutputLimitBytes: 1024, EffectAuthority: []model.ProgramEffectRequirement{{Action: model.ActionExecuteProgram, Resource: model.ResourceSelector{Kind: model.ResourceWorkspace}}}})
				require.NoError(t, err)
				graph := programGraph(profile)
				graph.Nodes[0].Performer.Timeout = " 2s "
				if retry {
					graph.Nodes[0].Retry = model.RetryPolicy{MaxAttempts: 2, Backoff: 10 * time.Second, Retryable: []string{model.RetryableProgramFailure}}
				}
				start := app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Scope: model.WorkScope{WorkspaceID: "workspace"}, AuthorizedProgramProfiles: []model.ProgramProfileRef{graph.Nodes[0].Performer.Program.Profile}, Deadline: now.Add(time.Hour)}}
				graph.Nodes[0].Performer.Timeout = "2h"
				_, err = service.StartProcess(ctx, start)
				require.ErrorIs(t, err, app.ErrUnsupported)
				graph.Nodes[0].Performer.Timeout = " 2s "
				admitted, err := service.StartProcess(ctx, start)
				require.NoError(t, err)
				require.Equal(t, initial.Add(budget), admitted.Run.NodeAttempts[0].Deadline)
				if retry {
					_, err = service.ReconcilePendingWork(ctx)
					require.NoError(t, err)
					require.Equal(t, 1, host.prepares)
				}
				require.NoError(t, store.Close())
				store, err = sqlite.Open(path)
				require.NoError(t, err)
				defer store.Close()
				now = initial.Add(750 * time.Millisecond)
				if budget >= time.Second {
					now = initial.Add(11 * time.Second)
				}
				if retry {
					now = initial.Add(10*time.Second + budget/2)
				}
				service = app.New(store, providers.NewRegistry()).WithProgramHost(host).WithClock(func() time.Time { return now })
				for range 3 {
					_, err = service.ReconcilePendingWork(ctx)
					require.NoError(t, err)
				}
				read, err := service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
				require.NoError(t, err)
				if retry {
					require.Equal(t, 2, host.prepares)
					require.Equal(t, model.WorkRunSucceeded, read.Run.State)
					found := false
					for _, attempt := range read.Run.NodeAttempts {
						if attempt.Ref.NodeID == "program" && attempt.Ref.Attempt == 2 {
							found = true
							require.Equal(t, initial.Add(10*time.Second+budget), attempt.Deadline)
						}
					}
					require.True(t, found)
				} else {
					require.Zero(t, host.prepares)
					require.Equal(t, model.WorkRunFailed, read.Run.State)
					require.Empty(t, read.Run.NodeAttempts[0].Ref.IssuanceID)
					require.Contains(t, read.Run.NodeAttempts[0].Detail, "timeout")
					require.Equal(t, initial.Add(budget), read.Run.NodeAttempts[0].Deadline)
				}
			})
		}
	}

}

func TestPerformerTimeoutAuthoringAndUnsupportedHumanStage(t *testing.T) {
	ctx := context.Background()
	_, service, now := regressionService(t)
	graph := stagedHumanGraph()
	graph.ProgramActivationTimeouts = map[model.WorkNodeID]time.Duration{"task": time.Second}
	_, err := service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "forged"}, ID: "forged", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.ErrorIs(t, err, app.ErrInvalid)
	graph.ProgramActivationTimeouts = nil
	graph.Nodes[0].Stages.Plan.Performer.Timeout = " 30s "
	draft := app.DefinitionDraft{ID: "timeouts", RevisionID: "v1", Name: "timeout", Kind: model.DefinitionProcess, SchemaVersion: 1, Source: "test", Process: &model.ProcessDefinition{Graph: graph}}
	saved, err := service.SaveDefinition(ctx, app.SaveDefinitionRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, Draft: draft})
	require.NoError(t, err)
	require.Equal(t, " 30s ", saved.Revision.Process.Graph.Nodes[0].Stages.Plan.Performer.Timeout)
	ref := model.DefinitionRef{DefinitionID: saved.Definition.ID, RevisionID: saved.Revision.ID, ContentHash: saved.Revision.ContentHash, Kind: model.DefinitionProcess}
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{Definition: &ref, Deadline: now.Add(time.Hour)}})
	require.ErrorIs(t, err, app.ErrUnsupported)
	_, err = service.InspectWork(ctx, app.InspectWorkRequest{Principal: model.OperatorPrincipal(), WorkRunID: "run"})
	require.ErrorIs(t, err, app.ErrNotFound)
	graph.Nodes[0].Stages.Plan.Performer.Timeout = "2h"
	_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
	require.NoError(t, err, "long authored timeouts remain representable even when execution is unavailable")
	graph.Nodes[0].Stages.Plan.Performer.Timeout = "  "
	_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
	require.NoError(t, err)
	for _, invalid := range []string{"0s", "-1s", "999999999999999999999h", "{{ params.duration }}", "bad"} {
		graph.Nodes[0].Stages.Plan.Performer.Timeout = invalid
		_, err = service.ValidateDefinition(ctx, app.ValidateDefinitionRequest{Principal: model.OperatorPrincipal(), Draft: draft})
		require.ErrorIs(t, err, app.ErrInvalid)
	}
}
