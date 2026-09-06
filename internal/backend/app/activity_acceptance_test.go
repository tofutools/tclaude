package app_test

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestActivityReadsOperationProducedByLaunch(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	service := testService(store, newFakeProvider())
	desired := model.DesiredConfiguration{Harness: "fake", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}

	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "activity_launch"), Target: app.LaunchTarget{Standalone: &app.StandaloneLaunchTarget{Desired: desired}}})
	require.NoError(t, err)

	activity, err := service.QueryActivity(ctx, app.QueryActivityRequest{Principal: model.OperatorPrincipal(), Filter: app.ActivityFilter{Target: app.ActivityTarget{ExecutionID: launched.Execution.ID}}})
	require.NoError(t, err)
	require.NotEmpty(t, activity.Records)
	require.Equal(t, launched.Operation.ID, model.OperationID(activity.Records[0].ID))
	require.Equal(t, model.ActivityOperation, activity.Records[0].Kind)
	require.Equal(t, model.ActivityActor{Kind: model.PrincipalOperator}, activity.Records[0].Actor)
	require.False(t, activity.Records[0].StartedAt.IsZero())
}

type acceptanceUsageProvider struct{ *fakeProvider }

func (acceptanceUsageProvider) Usage() ports.UsageReader { return acceptanceUsageReader{} }

type acceptanceUsageReader struct{}

func (acceptanceUsageReader) Capabilities() ports.UsageCapabilities {
	return ports.UsageCapabilities{CollectCounters: true}
}

func (acceptanceUsageReader) Collect(_ context.Context, request ports.UsageCollectionRequest) (ports.CollectedUsage, error) {
	return ports.CollectedUsage{
		SourceKey: "conversation:" + string(request.Execution.ConversationID), Source: "acceptance.native", SourceRevision: "revision-one",
		ObservedAt: time.Unix(100, 0), Counters: []model.UsageCounter{{Unit: model.UsageInputTokens, Value: 12}},
		Coverage: model.UsageCoverage{Counters: model.UsageCoverageComplete, Cost: model.UsageCoverageUnsupported}, Cumulative: true, Attribution: model.UsageAttributionConversation,
	}, nil
}

func TestConversationUsageRefreshDoesNotInheritLaunchingAgent(t *testing.T) {
	ctx := context.Background()
	store, err := backendsqlite.Open(filepath.Join(t.TempDir(), "replacement.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	provider := acceptanceUsageProvider{fakeProvider: newFakeProvider()}
	sequence := 0
	service := app.New(store, fakeRegistry{provider: provider}).WithIDGenerator(func(prefix string) string {
		sequence++
		return prefix + strconv.Itoa(sequence)
	})
	agent := createAgent(t, ctx, service, model.OperatorPrincipal(), "usage_agent")
	launched, err := service.Launch(ctx, app.LaunchRequest{RequestContext: effect(model.OperatorPrincipal(), "usage_launch"), Target: app.LaunchTarget{Agent: &app.AgentLaunchTarget{AgentID: agent.ID, ExpectedRevision: agent.Revision}}})
	require.NoError(t, err)

	usage, err := service.RefreshUsage(ctx, app.RefreshUsageRequest{Principal: model.OperatorPrincipal(), Target: app.UsageTarget{ExecutionID: launched.Execution.ID}})
	require.NoError(t, err)
	require.Empty(t, usage.Observation.Attribution.AgentID)
	require.Empty(t, usage.Observation.Attribution.ExecutionID)
	require.Equal(t, launched.Execution.ConversationID, usage.Observation.Attribution.ConversationID)
}
