package app_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

// Regression retained from the independent cold review of launch briefs.
func TestExactBriefRetryDoesNotRecheckProviderCapability(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	p := &launchBriefProvider{supported: true}
	service := app.New(store, providers.NewRegistry(p))
	agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{
		Context: model.OperatorPrincipal(), ID: "writer", Name: "Writer",
		Desired: model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite},
	})
	require.NoError(t, err)
	req := app.LaunchRequest{
		RequestContext: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "launch"},
		Target:         app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.Agent.ID, ExpectedRevision: agent.Agent.Revision}},
		InitialMessage: "Inspect the change.",
	}
	first, err := service.Launch(ctx, req)
	require.NoError(t, err)

	p.supported = false // Simulate a provider/version change after durable admission.
	retry, err := service.Launch(ctx, req)
	require.NoError(t, err)
	require.True(t, retry.Repeated)
	require.Equal(t, first.Operation.ID, retry.Operation.ID)
	require.Len(t, p.preparations, 1)
	service = app.New(store, providers.NewRegistry())
	retry, err = service.Launch(ctx, req)
	require.NoError(t, err)
	require.True(t, retry.Repeated)
	changed := req
	changed.InitialMessage = "different"
	_, err = service.Launch(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	changed = req
	changed.Target.Agent = &app.AgentLaunchTarget{AgentID: "another", ExpectedRevision: 1}
	_, err = service.Launch(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	require.Len(t, p.preparations, 1)
}
