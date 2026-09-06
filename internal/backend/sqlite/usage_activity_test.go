package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestUsageCumulativeSourceDeduplicatesAcrossExecutionsAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage.db")
	store, err := Open(path)
	require.NoError(t, err)
	at := time.Date(2026, 9, 7, 1, 2, 3, 0, time.UTC)
	seedUsageExecution(t, store, "agent_usage", "conversation_usage", "execution_one", at)
	seedUsageExecution(t, store, "agent_resumed", "conversation_usage", "execution_two", at.Add(time.Minute))

	first := usageWrite("usage_first", "revision_one", at, 10)
	stored, repeated, err := store.RecordUsage(ctx, first)
	require.NoError(t, err)
	require.False(t, repeated)
	duplicate := first
	duplicate.Observation.ID = "usage_duplicate"
	duplicate.Observation.CollectedAt = at.Add(time.Second)
	storedAgain, repeated, err := store.RecordUsage(ctx, duplicate)
	require.NoError(t, err)
	require.True(t, repeated)
	require.Equal(t, stored.ID, storedAgain.ID)

	second := usageWrite("usage_second", "revision_two", at.Add(time.Minute), 25)
	second.Observation.CollectedAt = at.Add(2 * time.Minute)
	_, repeated, err = store.RecordUsage(ctx, second)
	require.NoError(t, err)
	require.False(t, repeated)
	require.NoError(t, store.Close())

	store, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	result, err := store.QueryUsage(ctx, app.UsageFilter{Target: app.UsageTarget{ConversationID: "conversation_usage"}}, model.AuthorityRequest{Principal: model.OperatorPrincipal(), Action: model.ActionReadUsage}, at.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, result.Observations, 1, "latest cumulative observation replaces, rather than adds to, its source")
	require.Equal(t, int64(25), result.Observations[0].Counters[0].Value)
	require.Empty(t, result.Observations[0].Attribution.ExecutionID)
	require.Empty(t, result.Observations[0].Attribution.AgentID, "conversation-cumulative usage does not inherit the latest execution's agent")
	require.Equal(t, model.UsageAttributionConversation, result.Observations[0].Attribution.Precision)
	require.Nil(t, result.Observations[0].Cost, "absent native cost survives reopen")

	historical := model.UsageObservation{ID: "usage_historical", Attribution: model.UsageAttribution{AgentID: "agent_usage", ConversationID: "conversation_usage", Precision: model.UsageAttributionConversation}, Harness: "claude", Source: "legacy.audit", SourceRevision: "snapshot:1", ObservedAt: at.Add(-time.Hour), CollectedAt: at, Counters: []model.UsageCounter{{Unit: model.UsageInputTokens, Value: 3}}, Cost: &model.UsageCost{Amount: "0.125", Currency: "USD", Kind: model.UsageCostHistoricalEstimate}, Coverage: model.UsageCoverage{Counters: model.UsageCoveragePartial, Cost: model.UsageCoveragePartial}, Historical: true, Provenance: "snapshot-v228"}
	imported, repeated, err := store.ImportHistoricalUsage(ctx, app.HistoricalUsageWrite{Observation: historical, SourceKey: "legacy:usage:1", Cumulative: true})
	require.NoError(t, err)
	require.False(t, repeated)
	require.Equal(t, model.UsageCostHistoricalEstimate, imported.Cost.Kind)
	require.Equal(t, "snapshot-v228", imported.Provenance)
}

func TestUsageTargetMismatchAndActorScopeAreRejectedInReadTransaction(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "scope.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	at := time.Now().UTC()
	seedUsageExecution(t, store, "agent_a", "conversation_a", "execution_a", at)
	seedUsageExecution(t, store, "agent_b", "conversation_b", "execution_b", at)

	_, err = store.ResolveUsageTarget(ctx, app.UsageTarget{ConversationID: "conversation_a", ExecutionID: "execution_b"}, model.AuthorityRequest{Principal: model.OperatorPrincipal(), Action: model.ActionRefreshUsage}, at)
	require.ErrorIs(t, err, app.ErrNotFound)
	_, err = store.QueryUsage(ctx, app.UsageFilter{Target: app.UsageTarget{ExecutionID: "execution_b"}}, model.AuthorityRequest{Principal: model.AgentPrincipal("agent_a"), Action: model.ActionReadUsage}, at)
	require.ErrorIs(t, err, app.ErrUnauthorized)
}

func TestActivityProjectsSafeOperationAndHistoricalRecords(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "activity.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	at := time.Date(2026, 9, 7, 2, 0, 0, 0, time.UTC)
	seedUsageExecution(t, store, "agent_activity", "conversation_activity", "execution_activity", at)
	_, err = store.db.Exec(`INSERT INTO operations(id,request_id,request_scope,kind,principal_kind,principal_agent_id,principal_execution_id,principal_generation,principal_automation_run,authority_subject_kind,authority_subject_id,execution_id,state,result_code,detail,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		"operation_activity", "request_activity", "agent:agent_activity", model.OperationInteract, model.PrincipalAgent, "agent_activity", "", 0, "", "agent", "agent_activity", "execution_activity", model.OperationFailed, "provider_refused", "private provider receipt must not escape", 1, nanos(at), nanos(at.Add(time.Second)))
	require.NoError(t, err)
	historical := model.ActivityRecord{ID: "historical_one", Kind: model.ActivityHistorical, Actor: model.ActivityActor{Kind: model.PrincipalAgent, AgentID: "agent_activity"}, AgentID: "agent_activity", ConversationID: "conversation_activity", Outcome: "accepted", Reason: "imported operator decision", StartedAt: at.Add(-time.Hour), Historical: true, Provenance: "snapshot-v228"}
	_, repeated, err := store.ImportHistoricalActivity(ctx, app.HistoricalActivityWrite{Record: historical, SourceKey: "audit:1", SourceRevision: "sha256:one"})
	require.NoError(t, err)
	require.False(t, repeated)
	_, repeated, err = store.ImportHistoricalActivity(ctx, app.HistoricalActivityWrite{Record: historical, SourceKey: "audit:1", SourceRevision: "sha256:one"})
	require.NoError(t, err)
	require.True(t, repeated)

	result, err := store.QueryActivity(ctx, app.ActivityFilter{Target: app.ActivityTarget{AgentID: "agent_activity"}}, model.AuthorityRequest{Principal: model.AgentPrincipal("agent_activity"), Action: model.ActionReadActivity, Resource: model.ResourceSelector{Kind: model.ResourceAgent, AgentID: "agent_activity"}}, at.Add(time.Hour))
	require.NoError(t, err)
	require.Len(t, result.Records, 2)
	require.Equal(t, "provider_refused", result.Records[0].Reason)
	require.NotContains(t, result.Records[0].Reason, "receipt")
	require.Equal(t, model.ActivityActor{Kind: model.PrincipalAgent, AgentID: "agent_activity"}, result.Records[0].Actor)
}

func usageWrite(id model.UsageObservationID, revision string, observed time.Time, input int64) app.UsageWrite {
	return app.UsageWrite{SourceKey: "codex:conversation_usage", Cumulative: true, Observation: model.UsageObservation{
		ID: id, Attribution: model.UsageAttribution{ConversationID: "conversation_usage", Precision: model.UsageAttributionConversation},
		Harness: "codex", Source: "codex.rollout.token_count", SourceRevision: revision, ObservedAt: observed, CollectedAt: observed,
		Counters: []model.UsageCounter{{Unit: model.UsageInputTokens, Value: input}}, Coverage: model.UsageCoverage{Counters: model.UsageCoverageComplete, Cost: model.UsageCoverageUnsupported},
	}}
}

func seedUsageExecution(t *testing.T, store *Store, agent model.AgentID, conversation model.ConversationID, execution model.ExecutionID, at time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Agent(ctx, agent); err != nil {
		require.NoError(t, store.CreateAgent(ctx, model.Agent{ID: agent, Name: string(agent), Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxUnconfined}, Revision: 1, CreatedAt: at, UpdatedAt: at}))
	}
	_, err := store.db.ExecContext(ctx, `INSERT OR IGNORE INTO conversations(id,revision,created_at,updated_at) VALUES(?,1,?,?)`, conversation, nanos(at), nanos(at))
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `INSERT OR IGNORE INTO agent_conversations(agent_id,conversation_id,current,revision,associated_at) VALUES(?,?,1,1,?)`, agent, conversation, nanos(at))
	require.NoError(t, err)
	_, err = store.db.ExecContext(ctx, `INSERT INTO executions(id,agent_id,conversation_id,harness,model,working_directory,approval,sandbox,state,native_namespace,native_reference,native_observed_at,revision,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,1,?,?)`,
		execution, agent, conversation, "codex", "fixture", "/tmp", model.ApprovalSupervised, model.SandboxUnconfined, model.ExecutionExited, "codex", string(conversation), nanos(at), nanos(at), nanos(at))
	require.NoError(t, err)
}
