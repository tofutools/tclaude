package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestAccessRequestConfigurationDenialGenerationIdempotencyAndRetirementFence(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "access-request.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newAccessProvider()
	now := time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC)
	service := testAccessService(store, provider).WithClock(func() time.Time { return now })
	operator := model.OperatorPrincipal()
	requester := createAgent(t, ctx, service, operator, "agent_requester")
	target := createAgent(t, ctx, service, operator, "agent_target")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_requester"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: requester.ID, ExpectedRevision: requester.Revision}}})
	require.NoError(t, err)
	caller, err := service.AuthenticateAction(ctx, provider.delivery.secrets[launched.Execution.ID])
	require.NoError(t, err)

	request := app.RequestAccessRequest{
		Context: app.RequestContext{Principal: caller, RequestID: "request_launch_target"},
		Action:  model.ActionLaunch, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: target.ID},
		RequestedConfiguration: &target.Desired, Reason: "launch the exact reviewed target", Lifetime: 2 * time.Minute,
	}
	withoutConfiguration := request
	withoutConfiguration.RequestedConfiguration = nil
	_, err = service.RequestAccess(ctx, withoutConfiguration)
	require.ErrorIs(t, err, app.ErrInvalid, "launch authority cannot be requested without naming its configuration")
	_, err = service.RequestAccess(ctx, request)
	require.ErrorIs(t, err, app.ErrInvalid, "configuration-bearing access cannot acquire an empty bound")
	request.Bounds = configurationBounds(target.Desired)
	pending, err := service.RequestAccess(ctx, request)
	require.NoError(t, err)

	status, err := service.ExecutionAccessStatus(ctx, app.ExecutionAccessStatusRequest{Principal: operator, ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	_, err = service.RenewExecutionAccess(ctx, app.RenewExecutionAccessRequest{ExecutionID: launched.Execution.ID, ExpectedRevision: status.Access.Revision})
	require.NoError(t, err)
	caller, err = service.AuthenticateAction(ctx, provider.delivery.secrets[launched.Execution.ID])
	require.NoError(t, err)
	request.Context.Principal = caller
	repeated, err := service.RequestAccess(ctx, request)
	require.NoError(t, err)
	require.True(t, repeated.Repeated, "credential rotation does not fork the request idempotency scope")
	require.Equal(t, pending.Request.ID, repeated.Request.ID)

	denied, err := service.DecideAccessRequest(ctx, app.DecideAccessRequestRequest{
		Context:    app.RequestContext{Principal: operator, RequestID: "deny_launch_target"},
		DecisionID: pending.Decision.ID, ExpectedWindowRevision: pending.Decision.Revision,
		Answer: model.AccessAnswerDeny, Reason: "operator declines this launch",
	})
	require.NoError(t, err)
	require.Equal(t, model.AccessRequestDenied, denied.Request.State)
	_, err = service.Launch(ctx, app.LaunchRequest{RequestContext: effect(caller, "launch_still_denied"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: target.ID, ExpectedRevision: target.Revision}}})
	require.ErrorIs(t, err, app.ErrUnauthorized, "denial creates no grant")

	_, err = service.RequestAccess(ctx, app.RequestAccessRequest{
		Context: app.RequestContext{Principal: caller, RequestID: "already_allowed"},
		Action:  model.ActionReadStatus, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: launched.Execution.ID},
		Reason: "default authority should not open a request", Lifetime: time.Minute,
	})
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = service.RequestAccess(ctx, app.RequestAccessRequest{
		Context: app.RequestContext{Principal: caller, RequestID: "too_long"},
		Action:  model.ActionStop, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: launched.Execution.ID},
		Reason: "unbounded request", Lifetime: app.MaxAccessRequestLifetime + time.Second,
	})
	require.ErrorIs(t, err, app.ErrInvalid)

	stale, err := service.RequestAccess(ctx, app.RequestAccessRequest{
		Context: app.RequestContext{Principal: caller, RequestID: "retirement_fence"},
		Action:  model.ActionStop, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: launched.Execution.ID},
		Reason: "must not outlive requester retirement", Lifetime: time.Minute,
	})
	require.NoError(t, err)
	_, err = service.Stop(ctx, app.StopRequest{RequestContext: effect(operator, "stop_for_retirement"), ExecutionID: launched.Execution.ID})
	require.NoError(t, err)
	current, err := store.Agent(ctx, requester.ID)
	require.NoError(t, err)
	_, err = service.RetireAgent(ctx, app.RetireAgentRequest{Context: operator, ID: requester.ID, ExpectedRevision: current.Revision, Reason: "retirement fences pending access"})
	require.NoError(t, err)
	_, err = service.DecideAccessRequest(ctx, app.DecideAccessRequestRequest{
		Context:    app.RequestContext{Principal: operator, RequestID: "approve_stale"},
		DecisionID: stale.Decision.ID, ExpectedWindowRevision: stale.Decision.Revision,
		Answer: model.AccessAnswerApprove, Reason: "must be refused",
	})
	require.ErrorIs(t, err, app.ErrConflict)
}

func TestAccessRequestCompetingDecisionsUseOneRevisionCAS(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "access-request-cas.db"))
	require.NoError(t, err)
	defer store.Close()
	provider := newAccessProvider()
	service := testAccessService(store, provider)
	operator := model.OperatorPrincipal()
	agent := createAgent(t, ctx, service, operator, "agent_request_cas")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(operator, "launch_request_cas"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)
	caller, err := service.AuthenticateAction(ctx, provider.delivery.secrets[launched.Execution.ID])
	require.NoError(t, err)
	pending, err := service.RequestAccess(ctx, app.RequestAccessRequest{
		Context: app.RequestContext{Principal: caller, RequestID: "request_cas"},
		Action:  model.ActionStop, Resource: model.ResourceSelector{Kind: model.ResourceExecution, ExecutionID: launched.Execution.ID},
		Reason: "competing decision fixture", Lifetime: time.Minute,
	})
	require.NoError(t, err)

	results := make(chan error, 2)
	for _, answer := range []string{model.AccessAnswerApprove, model.AccessAnswerDeny} {
		answer := answer
		go func() {
			_, decideErr := service.DecideAccessRequest(ctx, app.DecideAccessRequestRequest{
				Context:    app.RequestContext{Principal: operator, RequestID: model.RequestID("decision_" + answer)},
				DecisionID: pending.Decision.ID, ExpectedWindowRevision: pending.Decision.Revision,
				Answer: answer, Reason: "competing " + answer,
			})
			results <- decideErr
		}()
	}
	var succeeded, conflicted int
	for range 2 {
		err := <-results
		if err == nil {
			succeeded++
		} else if errors.Is(err, app.ErrConflict) {
			conflicted++
		} else {
			t.Fatalf("unexpected competing decision error: %v", err)
		}
	}
	require.Equal(t, 1, succeeded)
	require.Equal(t, 1, conflicted)
	settled, err := service.GetAccessRequest(ctx, app.GetAccessRequestRequest{Principal: operator, ID: pending.Request.ID})
	require.NoError(t, err)
	require.NotEqual(t, model.AccessRequestPending, settled.Request.State)
}
