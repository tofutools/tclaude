package app_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestGroupDisbandPreservesMembersHistoryAndExactRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "shared", Name: "Shared", Desired: model.DesiredConfiguration{Harness: "codex", Model: "fixture", WorkingDirectory: "/tmp", Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}})
	require.NoError(t, err)
	for _, id := range []model.GroupID{"remove", "keep", "child"} {
		_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: id, Name: string(id), Members: []model.AgentID{"shared"}})
		require.NoError(t, err)
	}
	_, err = svc.SetGroupParent(ctx, app.SetGroupParentRequest{Context: app.RequestContext{Principal: op, RequestID: "nest"}, ID: "child", ParentGroupID: "remove", ExpectedRevision: 1})
	require.NoError(t, err)
	save := app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: op, RequestID: "save"}, ID: "rule", RevisionID: "rule-v1", Name: "Group schedule", Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Hour}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "Hello", GroupID: "remove"}}, Policy: model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineQueue, Overlap: model.OverlapForbid, MaxActive: 1, ExpiresAfter: time.Hour, Deadline: time.Hour}}
	rule, err := svc.SaveAutomationRule(ctx, save)
	require.NoError(t, err)
	req := app.DisbandGroupRequest{Context: app.RequestContext{Principal: op, RequestID: "disband"}, ID: "remove", ExpectedRevision: 1}
	denied := req
	denied.Context.Principal = model.AgentPrincipal("shared")
	_, err = svc.DisbandGroup(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	stale := req
	stale.ExpectedRevision = 2
	_, err = svc.DisbandGroup(ctx, stale)
	require.ErrorIs(t, err, app.ErrConflict)
	result, err := svc.DisbandGroup(ctx, req)
	require.NoError(t, err)
	require.Equal(t, []model.AutomationRuleID{"rule"}, result.ArchivedRules)
	require.Equal(t, []model.GroupID{"child"}, result.DetachedChildren)
	_, err = store.Group(ctx, "remove")
	require.ErrorIs(t, err, app.ErrNotFound)
	keep, err := store.Group(ctx, "keep")
	require.NoError(t, err)
	require.Equal(t, []model.AgentID{"shared"}, keep.Members)
	child, err := store.Group(ctx, "child")
	require.NoError(t, err)
	require.Empty(t, child.ParentGroupID)
	require.EqualValues(t, 3, child.Revision)
	agent, err := store.Agent(ctx, "shared")
	require.NoError(t, err)
	require.Equal(t, model.AgentActive, agent.Lifecycle)
	rules, err := svc.ListAutomationRules(ctx, app.ListAutomationRulesRequest{Principal: op, IncludeTombstoned: true})
	require.NoError(t, err)
	require.Len(t, rules, 1)
	require.True(t, rules[0].Tombstoned)
	require.False(t, rules[0].Enabled)
	require.Equal(t, rule.Rule.HeadRevisionID, rules[0].HeadRevisionID)
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "remove", Name: "Reused"})
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry())
	replay, err := svc.DisbandGroup(ctx, req)
	require.NoError(t, err)
	require.Equal(t, result, replay)
	changed := req
	changed.ID = "keep"
	_, err = svc.DisbandGroup(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
}
