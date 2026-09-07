package app_test

import (
	"context"
	"encoding/json"
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

func TestUnrelatedExpiredStandingRuleDoesNotBlockEligibleGuidance(t *testing.T) {
	ctx := context.Background()
	store, _, now := regressionService(t)
	provider := &preparedWorkProvider{}
	service := app.New(store, providers.NewRegistry(provider)).WithClock(func() time.Time { return now }).WithCallbackIngress(noopCallbackIngress{})
	_, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: model.OperatorPrincipal(), ID: "worker", Name: "worker", Desired: model.DesiredConfiguration{Harness: "prepared-work", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "other_rule"}, ID: "a_other", RevisionID: "a_other_v1", Name: "standing", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionInteract}, Resources: []model.ResourceSelector{{Kind: model.ResourceAgent, AgentID: "worker"}}, ExpiresAt: now.Add(time.Hour)}, Condition: model.AutomationCondition{Kind: model.AutomationStandingOrder, StandingOrder: &model.StandingOrderCondition{FactKind: "prompt", Pattern: "review", Timing: model.StandingOrderSameContinuation, DispatchDeadline: time.Second}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "guidance", AgentIDs: []model.AgentID{"other"}}}, Policy: regressionOccurrencePolicy()})
	require.NoError(t, err)
	_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "rule"}, ID: "standing", RevisionID: "standing_v1", Name: "standing", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: model.AutomationDelegation{Actions: []model.Action{model.ActionInteract}, Resources: []model.ResourceSelector{{Kind: model.ResourceAgent, AgentID: "worker"}}, ExpiresAt: now.Add(time.Hour)}, Condition: model.AutomationCondition{Kind: model.AutomationStandingOrder, StandingOrder: &model.StandingOrderCondition{FactKind: "prompt", Pattern: "review", Timing: model.StandingOrderSameContinuation, DispatchDeadline: time.Minute}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "guidance", AgentIDs: []model.AgentID{"worker"}}}, Policy: regressionOccurrencePolicy()})
	require.NoError(t, err)
	graph := model.WorkGraph{CompilerVersion: "1", EntryNodeID: "task", Nodes: []model.WorkNode{{ID: "task", Kind: model.WorkNodeTask, Performer: &model.Performer{Kind: model.PerformerAgent, Agent: &model.AgentPerformer{AgentID: "worker", Brief: "work"}}}, {ID: "done", Kind: model.WorkNodeEnd, End: &model.EndPolicy{Outcome: model.WorkOutcomeVerified}}}, Edges: []model.WorkEdge{{From: "task", To: "done"}}}
	_, err = service.StartProcess(ctx, app.StartProcessRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "start"}, ID: "run", Start: model.WorkStart{InlineGraph: &graph, Deadline: now.Add(time.Hour)}})
	require.NoError(t, err)
	_, err = service.ReconcilePendingWork(ctx)
	require.NoError(t, err)
	guidance, err := provider.preparation.NativeGuidance.EvaluateNativeGuidance(ctx, ports.NormalizedNativeEvent{EventID: "event", Kind: "prompt", ObservedAt: now.Add(-2 * time.Second), OccurredAt: now.Add(-2 * time.Second), NativeCorrelation: "turn", Payload: json.RawMessage(`{"text":"review"}`), Timing: model.StandingOrderSameContinuation})
	require.NoError(t, err, "unrelated expired rule must not block the worker's unexpired standing guidance")
	require.Equal(t, "guidance", guidance.Guidance)
}

func TestManualRoleOccurrenceRetryPreservesAuthoredRequestAndSnapshot(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "backend.sqlite")
	now := time.Date(2026, 9, 7, 8, 0, 0, 0, time.UTC)
	open := func() (*sqlite.Store, *app.Service) {
		store, err := sqlite.Open(path)
		require.NoError(t, err)
		return store, app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	}
	store, service := open()
	operator := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "fake", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalAutomatic, Sandbox: model.SandboxWorkspaceWrite}
	_, err := service.CreateAgent(ctx, app.CreateAgentRequest{Context: operator, ID: "reviewer", Name: "reviewer", Desired: desired})
	require.NoError(t, err)
	_, err = service.PutRole(ctx, app.PutRoleRequest{Principal: operator, Role: model.Role{ID: "review_role", Name: "Reviewer", Actions: []model.Action{model.ActionReadStatus}}})
	require.NoError(t, err)
	assignment, err := service.PutRoleAssignment(ctx, app.PutRoleAssignmentRequest{Principal: operator, Assignment: model.RoleAssignment{RoleID: "review_role", Subject: model.AuthoritySubject{Kind: model.AuthorityAgent, AgentID: "reviewer"}, Resource: model.ResourceSelector{Kind: model.ResourceSelf}}})
	require.NoError(t, err)
	delegation := model.AutomationDelegation{Actions: []model.Action{model.ActionSendMessage}, Resources: []model.ResourceSelector{{Kind: model.ResourceAgent, AgentID: "reviewer"}}, ExpiresAt: now.Add(time.Hour)}
	policy := model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineQueue, ExpiresAfter: time.Hour, Overlap: model.OverlapAllow, MaxActive: 2, Deadline: time.Minute, Retry: model.RetryPolicy{MaxAttempts: 1}}
	rule, err := service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: operator, RequestID: "save_role_rule"}, ID: "role_rule", RevisionID: "role_rule_v1", Name: "role rule", Enabled: true, Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Delegation: delegation, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Minute, Anchor: now.Add(time.Hour)}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "review now", RoleID: "review_role"}}, Policy: policy})
	require.NoError(t, err)

	first, err := service.RunRuleNow(ctx, app.RunRuleNowRequest{Context: app.RequestContext{Principal: operator, RequestID: "fire_one"}, RuleID: rule.Rule.ID, ExpectedRuleRevision: rule.Rule.Revision, OccurrenceID: "role_occurrence_one", SourceOccurrenceKey: "one"})
	require.NoError(t, err)
	require.Equal(t, []model.OccurrenceRecipient{{AgentID: "reviewer", Disposition: model.RecipientPending}}, first.Occurrence.Recipients)

	defer func() { _ = store.Close() }()
	require.NoError(t, service.DeleteRoleAssignment(ctx, app.DeleteRoleAssignmentRequest{Principal: operator, Assignment: assignment.Assignment, ExpectedRevision: assignment.Assignment.Revision}))
	repeated, err := service.RunRuleNow(ctx, app.RunRuleNowRequest{Context: app.RequestContext{Principal: operator, RequestID: "fire_one"}, RuleID: rule.Rule.ID, ExpectedRuleRevision: rule.Rule.Revision, OccurrenceID: "role_occurrence_one", SourceOccurrenceKey: "one"})
	require.NoError(t, err, "exact request retry should return the admitted recipient snapshot")
	require.Equal(t, first.Occurrence.Recipients, repeated.Occurrence.Recipients)
	require.NoError(t, store.Close())
	store, service = open()
	_, err = service.SaveAutomationRule(ctx, app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: operator, RequestID: "disable"}, ID: rule.Rule.ID, RevisionID: "role_rule_v2", Name: rule.Rule.Name, ExpectedRevision: rule.Rule.Revision, Enabled: false, Owner: rule.Revision.Owner, Delegation: rule.Revision.Delegation, Condition: rule.Revision.Condition, Action: rule.Revision.Action, Policy: rule.Revision.Policy})
	require.NoError(t, err)
	original := app.RunRuleNowRequest{Context: app.RequestContext{Principal: operator, RequestID: "fire_one"}, RuleID: rule.Rule.ID, ExpectedRuleRevision: rule.Rule.Revision, OccurrenceID: "role_occurrence_one", SourceOccurrenceKey: "one"}
	repeated, err = service.RunRuleNow(ctx, original)
	require.NoError(t, err, "restart and rule edit must not change an admitted retry")
	require.Equal(t, first.Occurrence.Recipients, repeated.Occurrence.Recipients)
	changed := original
	changed.Recipients = []model.AgentID{"reviewer"}
	_, err = service.RunRuleNow(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict, "explicit recipients differ from authored role resolution")
	changed = original
	changed.SourceOccurrenceKey = "different"
	_, err = service.RunRuleNow(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)

}
