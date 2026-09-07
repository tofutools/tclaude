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

func TestAutomationArchivePreservesPinsAndRequiresExplicitEnable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	now := time.Now().UTC()
	service := app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	save := app.SaveAutomationRuleRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "save"}, ID: "rule", RevisionID: "revision", Name: "Schedule", Owner: model.AuthoritySubject{Kind: model.AuthorityOperator}, Condition: model.AutomationCondition{Kind: model.AutomationSchedule, Schedule: &model.ScheduleCondition{Timezone: "UTC", Interval: time.Hour}}, Action: model.AutomationAction{Kind: model.AutomationSendMessage, Message: &model.AutomationMessageAction{Body: "Hello", AgentIDs: []model.AgentID{"recipient"}}}, Policy: model.OccurrencePolicy{MissedTicks: model.MissedTickSkip, OfflineDelivery: model.OfflineQueue, Overlap: model.OverlapForbid, MaxActive: 1, ExpiresAfter: time.Hour, Deadline: time.Hour}}
	rule, err := service.SaveAutomationRule(ctx, save)
	require.NoError(t, err)

	request := app.SetAutomationArchivedRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "archive"}, ID: rule.Rule.ID, ExpectedRevision: rule.Rule.Revision, Archived: true}
	archived, err := service.SetAutomationArchived(ctx, request)
	require.NoError(t, err)
	require.True(t, archived.Tombstoned)
	require.False(t, archived.Enabled)
	require.Equal(t, rule.Rule.HeadRevisionID, archived.HeadRevisionID)
	edit := save
	edit.Context.RequestID = "edit-archived"
	edit.RevisionID = "edited"
	edit.ExpectedRevision = archived.Revision
	edit.Name = "Must not resurrect"
	_, err = service.SaveAutomationRule(ctx, edit)
	require.ErrorIs(t, err, app.ErrConflict)

	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry()).WithClock(func() time.Time { return now })
	repeated, err := service.SetAutomationArchived(ctx, request)
	require.NoError(t, err)
	require.Equal(t, archived, repeated)
	changed := request
	changed.Archived = false
	_, err = service.SetAutomationArchived(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = service.SetAutomationEnabled(ctx, app.SetAutomationEnabledRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "bad-enable"}, ID: archived.ID, ExpectedRevision: archived.Revision, Enabled: true})
	require.ErrorIs(t, err, app.ErrConflict)
	list, err := service.ListAutomationRules(ctx, app.ListAutomationRulesRequest{Principal: model.OperatorPrincipal()})
	require.NoError(t, err)
	require.Empty(t, list)
	list, err = service.ListAutomationRules(ctx, app.ListAutomationRulesRequest{Principal: model.OperatorPrincipal(), IncludeTombstoned: true})
	require.NoError(t, err)
	require.Len(t, list, 1)
	denied := request
	denied.Context = app.RequestContext{Principal: model.Principal{Kind: model.PrincipalAgent, AgentID: "unknown"}, RequestID: "denied"}
	denied.ExpectedRevision = archived.Revision
	denied.Archived = false
	_, err = service.SetAutomationArchived(ctx, denied)
	require.Error(t, err)
	restore := request
	restore.Context.RequestID = "restore"
	restore.Archived = false
	restore.ExpectedRevision = archived.Revision
	restored, err := service.SetAutomationArchived(ctx, restore)
	require.NoError(t, err)
	require.False(t, restored.Tombstoned)
	require.False(t, restored.Enabled)
	require.Equal(t, archived.HeadRevisionID, restored.HeadRevisionID)
	again, err := service.SetAutomationArchived(ctx, request)
	require.NoError(t, err)
	require.Equal(t, archived, again)
	current, err := service.GetAutomationRule(ctx, app.GetAutomationRuleRequest{Principal: model.OperatorPrincipal(), ID: restored.ID})
	require.NoError(t, err)
	require.False(t, current.Rule.Tombstoned)
}
