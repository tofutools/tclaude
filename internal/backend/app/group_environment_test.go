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

func TestGroupEnvironmentPrecedencePinsAndRetriesAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := db.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "group", Name: "Group"})
	require.NoError(t, err)
	save := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile_one"}, ID: "profile", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Environment: model.Environment{"SHARED": "profile", "PROFILE_ONLY": "yes"}, Harness: "codex", Model: "first", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}, Startup: &model.ProfileStartup{AgentName: "Suggested", Context: "Context", InitialMessage: "Brief"}}
	first, err := svc.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	set := app.SetGroupConfigurationRequest{Environment: model.Environment{"SHARED": "group", "GROUP_ONLY": "yes"}, Principal: op, GroupID: "group", Profile: &first.Revision.Ref}
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
	in := app.CreateGroupMemberRequest{Environment: model.Environment{"SHARED": "explicit"}, Context: app.RequestContext{Principal: op, RequestID: "member"}, GroupID: "group", ID: "member", Name: "Named", ExpectedGroupRevision: 1, ExpectedDefaultRevision: defaults.Revision}
	result, err := svc.CreateGroupMember(ctx, in)
	require.NoError(t, err)
	require.Equal(t, "first", result.Agent.Desired.Model)
	require.Equal(t, model.Environment{"SHARED": "explicit", "GROUP_ONLY": "yes", "PROFILE_ONLY": "yes"}, result.Agent.Desired.Environment)
	require.Equal(t, &first.Revision.Ref, result.Agent.ConfigurationProfile)
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
	changed := in
	changed.Environment = model.Environment{"SHARED": "different"}
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
	environmentOnly, err := svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", ExpectedRevision: current.Revision, Environment: model.Environment{"ONLY": "group"}})
	require.NoError(t, err)
	cloneRequest := app.CloneGroupRequest{Context: app.RequestContext{Principal: op, RequestID: "clone"}, SourceID: "group", ID: "copy", Name: "Copy", ExpectedGroupRevision: snapshot.Groups[0].Revision, ExpectedDefaultRevision: environmentOnly.Revision, CopyDefault: true}
	_, err = svc.CloneGroup(ctx, cloneRequest)
	require.NoError(t, err)
	copied, err := svc.GetGroupConfiguration(ctx, op, "copy")
	require.NoError(t, err)
	require.Nil(t, copied.Profile)
	require.Equal(t, environmentOnly.Environment, copied.Environment)
	_, err = svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", ExpectedRevision: environmentOnly.Revision})
	require.NoError(t, err)
	cloneRetry, err := svc.CloneGroup(ctx, cloneRequest)
	require.NoError(t, err)
	require.True(t, cloneRetry.Repeated)

}
