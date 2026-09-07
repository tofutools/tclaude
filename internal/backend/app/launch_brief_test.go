package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestLaunchBriefIsPreparedOnceAndExactRetrySurvivesReopen(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "started", true: "unknown"}[unknown], func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "backend.sqlite")
			store, err := sqlite.Open(path)
			require.NoError(t, err)
			p := &launchBriefProvider{supported: true, unknown: unknown}
			service := app.New(store, providers.NewRegistry(p))
			agent, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "writer", Name: "Writer", Desired: model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
			require.NoError(t, err)
			req := app.LaunchRequest{RequestContext: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "launch"}, Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.Agent.ID, ExpectedRevision: agent.Agent.Revision}}, InitialMessage: "Inspect the change.\nReport the evidence."}
			result, err := service.Launch(ctx, req)
			if unknown {
				require.ErrorIs(t, err, app.ErrUncertain)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, p.preparations, 1)
			require.Equal(t, req.InitialMessage, p.preparation.InitialInput.Body)
			require.Equal(t, string(result.Operation.ID), p.preparation.InitialInput.Correlation)
			require.True(t, p.preparation.InitialInput.RequiredBeforeFirstWork)
			if unknown {
				require.Equal(t, model.OperationUncertain, result.Operation.State)
			}
			require.NoError(t, store.Close())
			store, err = sqlite.Open(path)
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			service = app.New(store, providers.NewRegistry(p))
			retry, err := service.Launch(ctx, req)
			require.NoError(t, err)
			require.True(t, retry.Repeated)
			require.Equal(t, result.Operation.ID, retry.Operation.ID)
			require.Equal(t, result.Operation.State, retry.Operation.State)
			for _, body := range []string{"different", ""} {
				changed := req
				changed.InitialMessage = body
				_, err = service.Launch(ctx, changed)
				require.ErrorIs(t, err, app.ErrConflict)
			}
			require.Len(t, p.preparations, 1)
		})
	}
}

func TestLaunchBriefRejectsUnsupportedMalformedAndUnprovenInput(t *testing.T) {
	for _, scenario := range []string{"unsupported", "oversize", "nul", "unproven"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })
			p := &launchBriefProvider{supported: scenario != "unsupported", unproven: scenario == "unproven"}
			service := app.New(store, providers.NewRegistry(p))
			body := "first work"
			if scenario == "oversize" {
				body = strings.Repeat("x", 32769)
			}
			if scenario == "nul" {
				body = "a\x00b"
			}
			req := app.LaunchRequest{RequestContext: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "launch"}, Target: app.LaunchTarget{Standalone: &app.StandaloneLaunchTarget{Desired: model.DesiredConfiguration{Harness: p.Name(), Model: "fixture", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}}, InitialMessage: body}
			_, err = service.Launch(ctx, req)
			require.Error(t, err)
			require.Zero(t, p.releases)
			if scenario != "unproven" {
				require.Empty(t, p.preparations)
			}
		})
	}
}

type launchBriefProvider struct {
	preparedWorkProvider
	supported, unknown, unproven bool
	releases                     int
}

func (p *launchBriefProvider) Capabilities() ports.ProviderCapabilities {
	return ports.ProviderCapabilities{PreparedInitialInput: p.supported}
}
func (p *launchBriefProvider) Prepare(_ context.Context, r ports.PreparationRequest) (ports.PreparedAttempt, error) {
	p.preparation = r
	p.preparations = append(p.preparations, r)
	return &launchBriefAttempt{preparedWorkAttempt: preparedWorkAttempt{request: r}, owner: p}, nil
}

type launchBriefAttempt struct {
	preparedWorkAttempt
	owner *launchBriefProvider
}

func (p *launchBriefAttempt) Describe() ports.PreparedDescription {
	d := p.preparedWorkAttempt.Describe()
	if p.owner.unproven {
		d.InitialInput = nil
	}
	return d
}
func (p *launchBriefAttempt) Release(ctx context.Context, permit ports.ReleasePermit) (ports.ReleaseResult, error) {
	p.owner.releases++
	if p.owner.unknown {
		if err := permit.Consume(ctx); err != nil {
			return ports.ReleaseResult{}, err
		}
		return ports.ReleaseResult{State: ports.ReleaseUncertain}, nil
	}
	return p.preparedWorkAttempt.Release(ctx, permit)
}
