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
	"time"
)

func TestGroupCloneIsAtomicOfflineAndExactlyRepeatable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := db.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "saved", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	profile, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "profile", RevisionID: "one", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	for _, id := range []model.AgentID{"active", "retired"} {
		_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: id, Name: string(id), Labels: &model.AgentLabels{Role: "engineer", Description: "literal <b>description</b>"}, TaskReference: "task", ConfigurationProfile: &profile.Revision.Ref})
		require.NoError(t, err)
	}
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "source", Name: "Source", Members: []model.AgentID{"active", "retired"}, OwnerAgentID: "active"})
	require.NoError(t, err)
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "shared", Name: "Shared", Members: []model.AgentID{"active"}})
	require.NoError(t, err)
	retired, err := svc.RetireAgent(ctx, app.RetireAgentRequest{Context: op, ID: "retired", ExpectedRevision: 1, Reason: "retained"})
	require.NoError(t, err)
	details := &model.GroupDetails{Description: "Description", Mission: "Mission", LinkURL: "https://example.com/task", LinkLabel: "Task"}
	_, err = svc.SetGroupDetails(ctx, app.SetGroupDetailsRequest{Principal: op, ID: "source", ExpectedRevision: 1, Details: *details})
	require.NoError(t, err)
	_, err = svc.SetGroupCapacity(ctx, app.SetGroupCapacityRequest{Principal: op, ID: "source", ExpectedRevision: 2, MaxActiveMembers: 2})
	require.NoError(t, err)
	defaults, err := svc.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "source", Profile: &profile.Revision.Ref})
	require.NoError(t, err)
	in := app.CloneGroupRequest{Context: app.RequestContext{Principal: op, RequestID: "clone"}, SourceID: "source", ID: "copy", Name: "Copy", ExpectedGroupRevision: 3, ExpectedDefaultRevision: defaults.Revision, ExpectedMembers: map[model.AgentID]model.Revision{"active": 1, "retired": retired.Agent.Revision}, CopyMembers: true, CopyDefault: true, MaxActiveMembers: 2}
	denied := in
	denied.Context.Principal = model.AgentPrincipal("active")
	_, err = svc.CloneGroup(ctx, denied)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	stale := in
	stale.ExpectedDefaultRevision++
	_, err = svc.CloneGroup(ctx, stale)
	require.ErrorIs(t, err, app.ErrConflict)
	before, err := svc.Snapshot(ctx, app.SnapshotRequest{Principal: op})
	require.NoError(t, err)
	require.Len(t, before.Agents, 2)
	require.Len(t, before.Groups, 2)
	result, err := svc.CloneGroup(ctx, in)
	require.NoError(t, err)
	require.False(t, result.Repeated)
	require.Len(t, result.Members, 1)
	require.NotEmpty(t, result.Members["active"])
	require.NotEqual(t, model.AgentID("active"), result.Members["active"])
	require.Empty(t, result.Group.OwnerAgentID)
	require.Empty(t, result.Group.ParentGroupID)
	require.Equal(t, details, result.Group.Details)
	require.Equal(t, int64(2), result.Group.MaxActiveMembers)
	copied, err := store.Agent(ctx, result.Members["active"])
	require.NoError(t, err)
	expectedDesired := desired
	expectedDesired.HostSandbox = model.SandboxInGroup(desired.HostSandbox, "copy")
	require.Equal(t, expectedDesired, copied.Desired)
	require.Equal(t, model.AgentDisplayLabels{Role: "engineer", Description: "literal <b>description</b>"}, copied.Labels.InGroup("copy"))
	require.Empty(t, copied.Labels.InGroup("other"))
	require.Equal(t, &profile.Revision.Ref, copied.ConfigurationProfile)
	require.Equal(t, model.AgentID("active"), copied.CloneSourceAgentID)
	require.Empty(t, copied.PrimaryExecutionID)
	require.Empty(t, copied.ParentAgentID)
	pinned, err := svc.GetGroupConfiguration(ctx, op, "copy")
	require.NoError(t, err)
	require.Equal(t, defaults.Profile, pinned.Profile)
	// Mutating the source after a committed response loss must not change the replay.
	_, err = svc.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "source", ExpectedRevision: 3, Name: "Changed", Members: []model.AgentID{"active", "retired"}})
	require.NoError(t, err)
	_, err = svc.RetireAgent(ctx, app.RetireAgentRequest{Context: op, ID: "active", ExpectedRevision: 1, Reason: "changed"})
	require.NoError(t, err)
	require.NoError(t, store.Close())
	store, err = db.Open(path)
	require.NoError(t, err)
	svc = app.New(store, providers.NewRegistry())
	retry, err := svc.CloneGroup(ctx, in)
	require.NoError(t, err)
	require.True(t, retry.Repeated)
	retry.Repeated = false
	require.Equal(t, result, retry)
	changed := in
	changed.Name = "Other"
	_, err = svc.CloneGroup(ctx, changed)
	require.ErrorIs(t, err, app.ErrConflict)
	snapshot, err := svc.Snapshot(ctx, app.SnapshotRequest{Principal: op})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 3)
	require.Len(t, snapshot.Groups, 3)
	require.Empty(t, snapshot.Executions)
	require.Empty(t, snapshot.Operations)
	require.Empty(t, snapshot.WorkRuns)
	require.Empty(t, snapshot.Messages)
	shared, err := store.Group(ctx, "shared")
	require.NoError(t, err)
	require.Equal(t, []model.AgentID{"active"}, shared.Members)
	authority, err := store.AuthorityState(ctx)
	require.NoError(t, err)
	for _, a := range authority.Assignments {
		require.NotEqual(t, result.Members["active"], a.Subject.AgentID)
		require.NotEqual(t, model.GroupID("copy"), a.Resource.GroupID)
	}
	_, err = svc.UpdateGroup(ctx, app.UpdateGroupRequest{Context: op, ID: "copy", Name: result.Group.Name, ExpectedRevision: result.Group.Revision})
	require.NoError(t, err)
	detached, err := store.Agent(ctx, copied.ID)
	require.NoError(t, err)
	require.Empty(t, detached.Labels.InGroup("copy"))
	require.NotContains(t, detached.Labels.Groups, model.GroupID("copy"))
}

func TestGroupCloneRejectsStaleMembersAndArchivedConfigurations(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "db"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "saved", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	profile, err := svc.SaveConfigurationProfile(ctx, app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "profile", RevisionID: "one", Name: "Worker", Desired: desired})
	require.NoError(t, err)
	for _, id := range []model.AgentID{"a", "b"} {
		_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: id, Name: string(id), ConfigurationProfile: &profile.Revision.Ref})
		require.NoError(t, err)
	}
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "source", Name: "Source", Members: []model.AgentID{"a", "b"}})
	require.NoError(t, err)
	in := app.CloneGroupRequest{Context: app.RequestContext{Principal: op, RequestID: "clone"}, SourceID: "source", ID: "copy", Name: "Copy", ExpectedGroupRevision: 1, ExpectedMembers: map[model.AgentID]model.Revision{"a": 1, "b": 1}, CopyMembers: true, MaxActiveMembers: 1}
	_, err = svc.CloneGroup(ctx, in)
	require.ErrorIs(t, err, app.ErrConflict)
	in.MaxActiveMembers = 0
	retired, err := svc.RetireAgent(ctx, app.RetireAgentRequest{Context: op, ID: "b", ExpectedRevision: 1, Reason: "retired"})
	require.NoError(t, err)
	_, err = svc.CloneGroup(ctx, in)
	require.ErrorIs(t, err, app.ErrConflict)
	in.ExpectedMembers["b"] = retired.Agent.Revision
	_, err = svc.SetConfigurationProfileArchived(ctx, app.SetConfigurationProfileArchivedRequest{Context: app.RequestContext{Principal: op, RequestID: "archive"}, ID: "profile", ExpectedRevision: 1, Archived: true})
	require.NoError(t, err)
	_, err = svc.CloneGroup(ctx, in)
	require.ErrorIs(t, err, app.ErrConflict)
	snapshot, err := svc.Snapshot(ctx, app.SnapshotRequest{Principal: op})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 2)
	require.Len(t, snapshot.Groups, 1)
	in.CopyMembers = false
	in.ExpectedMembers = nil
	empty, err := svc.CloneGroup(ctx, in)
	require.NoError(t, err)
	require.Empty(t, empty.Group.Members)
}

type groupCloneRaceStore struct {
	*db.Store
	before func()
}

func (s *groupCloneRaceStore) AdmitGroupClone(ctx context.Context, in app.CloneGroupRequest, agents []model.Agent, at time.Time) (app.CloneGroupResult, error) {
	s.before()
	return s.Store.AdmitGroupClone(ctx, in, agents, at)
}
func TestGroupCloneRechecksMemberRevisionInsideAdmission(t *testing.T) {
	ctx := context.Background()
	store, err := db.Open(filepath.Join(t.TempDir(), "db"))
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	svc := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	desired := model.DesiredConfiguration{Harness: "codex", Model: "test", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}
	_, err = svc.CreateAgent(ctx, app.CreateAgentRequest{Context: op, ID: "member", Name: "Member", Desired: desired})
	require.NoError(t, err)
	_, err = svc.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "source", Name: "Source", Members: []model.AgentID{"member"}})
	require.NoError(t, err)
	wrapped := &groupCloneRaceStore{Store: store, before: func() {
		_, e := svc.RetireAgent(ctx, app.RetireAgentRequest{Context: op, ID: "member", ExpectedRevision: 1, Reason: "concurrent retirement"})
		require.NoError(t, e)
	}}
	_, err = app.New(wrapped, providers.NewRegistry()).CloneGroup(ctx, app.CloneGroupRequest{Context: app.RequestContext{Principal: op, RequestID: "clone"}, SourceID: "source", ID: "copy", Name: "Copy", ExpectedGroupRevision: 1, CopyMembers: true, ExpectedMembers: map[model.AgentID]model.Revision{"member": 1}})
	require.ErrorIs(t, err, app.ErrConflict)
	snapshot, err := svc.Snapshot(ctx, app.SnapshotRequest{Principal: op})
	require.NoError(t, err)
	require.Len(t, snapshot.Agents, 1)
	require.Len(t, snapshot.Groups, 1)
}
