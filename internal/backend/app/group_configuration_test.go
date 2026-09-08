package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	db "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestGroupConfigurationUsesCurrentProfileAndRetriesAfterDefaultChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := db.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "group", Name: "Group"})
	require.NoError(t, err)
	save := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile_one"}, ID: "profile", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "codex", Model: "first", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}, Startup: &model.ProfileStartup{Role: "reviewer", Description: "Reviews changes", AgentName: "Suggested", Context: "Context", InitialMessage: "Brief"}}
	first, err := svc.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	set := app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", Profile: &first.Revision.Ref}
	defaults, err := svc.SetGroupConfiguration(ctx, set)
	require.NoError(t, err)
	_, err = svc.SetGroupConfiguration(ctx, set)
	require.ErrorIs(t, err, app.ErrConflict)
	denied := set
	denied.Principal = model.AgentPrincipal("caller")
	_, err = svc.SetGroupConfiguration(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	save.Context.RequestID = "profile_two"
	save.ExpectedRevision = first.Profile.Revision
	save.RevisionID = "two"
	save.Desired.Model = "second"
	second, err := svc.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	in := app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: op, RequestID: "member"}, GroupID: "group", ID: "member", Name: "Named", ExpectedGroupRevision: 1, ExpectedDefaultRevision: defaults.Revision}
	result, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.Equal(t, model.AgentDisplayLabels{Role: "reviewer", Description: "Reviews changes"}, result.Agent.Labels.InGroup("group"))
	require.Empty(t, result.Agent.Labels.InGroup("other"))
	require.Equal(t, "second", result.Agent.Desired.Model)
	require.Equal(t, &second.Revision.Ref, result.Agent.ConfigurationProfile)
	require.Equal(t, []model.AgentID{"member"}, result.Group.Members)
	stale := in
	stale.Context.RequestID = "stale"
	stale.ID = "stale"
	_, err = svc.CreateGroupMember(ctx, stale)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.Agent(ctx, "stale")
	require.ErrorIs(t, err, app.ErrNotFound)
	deniedMember := in
	deniedMember.Context.Principal = model.AgentPrincipal("caller")
	_, err = svc.CreateGroupMember(ctx, deniedMember)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	archive := app.SetConfigurationProfileArchivedRequest{Context: app.RequestContext{Principal: op, RequestID: "archive"}, ID: "profile", ExpectedRevision: second.Profile.Revision, Archived: true}
	_, err = svc.SetConfigurationProfileArchived(ctx, archive)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", ExpectedRevision: defaults.Revision})
	require.NoError(t, err)
	_, err = svc.SetConfigurationProfileArchived(ctx, archive)
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = db.Open(path)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry())
	retry, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.True(t, retry.Repeated)
	retry.Repeated = false
	require.Equal(t, result, retry)
	changedLabels := in
	changedLabels.Labels = &model.AgentDisplayLabels{}
	_, err = svc.CreateGroupMember(ctx, changedLabels)
	require.ErrorIs(t, err, app.ErrConflict)
	changed := in
	changed.Name = "Different"
	_, err = svc.CreateGroupMember(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	current, err := svc.GetGroupConfiguration(ctx, op, "group")
	require.NoError(t, err)
	require.Nil(t, current.Profile)
	snapshot, err := svc.Snapshot(ctx, app.SnapshotRequest{Principal: op})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Empty(t, snapshot.Executions)
	require.Equal(t, []model.AgentID{"member"}, snapshot.Groups[0].Members)
}
