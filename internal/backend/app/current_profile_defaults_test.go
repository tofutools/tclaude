package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

type editingGroupProfileStore struct {
	*sqlite.Store
	before func()
}

func (s *editingGroupProfileStore) AdmitGroupMember(ctx context.Context, admission app.GroupMemberAdmission) (app.GroupMemberResult, error) {
	s.before()
	return s.Store.AdmitGroupMember(ctx, admission)
}

func TestGroupDefaultConcurrentProfileEditRefusesStaleAdmission(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	_, err = service.CreateGroup(ctx, app.CreateGroupRequest{Context: op, ID: "group", Name: "Group"})
	require.NoError(t, err)
	save := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "profile", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "codex", Model: "old", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}
	profile, err := service.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	defaults, err := service.SetGroupConfiguration(ctx, app.SetGroupConfigurationRequest{Principal: op, GroupID: "group", Profile: &profile.Revision.Ref})
	require.NoError(t, err)
	wrapped := &editingGroupProfileStore{Store: store, before: func() {
		save.Context.RequestID = "edit"
		save.ExpectedRevision = 1
		save.RevisionID = "two"
		save.Desired.Model = "new"
		_, err := service.SaveConfigurationProfile(ctx, save)
		require.NoError(t, err)
	}}
	racing := app.New(wrapped, providers.NewRegistry())
	req := app.CreateGroupMemberRequest{Context: app.RequestContext{Principal: op, RequestID: "member"}, GroupID: "group", ID: "member", Name: "Worker", ExpectedGroupRevision: 1, ExpectedDefaultRevision: defaults.Revision}
	_, err = racing.CreateGroupMember(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	_, err = store.Agent(ctx, "member")
	require.ErrorIs(t, err, app.ErrNotFound)
	// Reusing the refused intent creates from the new profile, with no stale receipt.
	result, err := service.CreateGroupMember(ctx, req)
	require.NoError(t, err)
	require.Equal(t, "new", result.Agent.Desired.Model)
}

type editingDefaultsStore struct {
	*sqlite.Store
	before func()
}

func (s *editingDefaultsStore) SaveConfigurationDefaults(ctx context.Context, w app.ConfigurationDefaultsWrite) (model.ConfigurationDefaults, error) {
	s.before()
	return s.Store.SaveConfigurationDefaults(ctx, w)
}

func TestConfigurationDefaultSaveChecksCurrentHarnessAndRetainsReceipt(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	op := model.OperatorPrincipal()
	save := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "profile", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "claude", Model: "old", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}
	profile, err := service.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	req := app.SaveConfigurationDefaultsRequest{Context: app.RequestContext{Principal: op, RequestID: "default"}, Harnesses: map[string]model.ConfigurationProfileRef{"claude": profile.Revision.Ref}}
	first, err := service.SaveConfigurationDefaults(ctx, req)
	require.NoError(t, err)
	save.Context.RequestID = "edit"
	save.RevisionID = "two"
	save.ExpectedRevision = 1
	save.Desired.Harness = "codex"
	current, err := service.SaveConfigurationProfile(ctx, save)
	require.NoError(t, err)
	repeated, err := service.SaveConfigurationDefaults(ctx, req)
	require.NoError(t, err)
	require.Equal(t, first, repeated)
	req.Context.RequestID = "stale"
	req.ExpectedRevision = first.Revision
	_, err = service.SaveConfigurationDefaults(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
	req.Context.RequestID = "global"
	req.Harnesses = nil
	req.Global = &profile.Revision.Ref
	updated, err := service.SaveConfigurationDefaults(ctx, req)
	require.NoError(t, err)
	require.Equal(t, &current.Revision.Ref, updated.Global)
}

func TestConfigurationDefaultSaveRefusesConcurrentProfileChange(t *testing.T) {
	for _, scope := range []string{"global", "claude"} {
		t.Run(scope, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlite.Open(filepath.Join(t.TempDir(), "state.db"))
			require.NoError(t, err)
			defer store.Close()
			service := app.New(store, providers.NewRegistry())
			op := model.OperatorPrincipal()
			save := app.SaveConfigurationProfileRequest{Context: app.RequestContext{Principal: op, RequestID: "profile"}, ID: "profile", RevisionID: "one", Name: "Worker", Desired: model.DesiredConfiguration{Harness: "claude", Model: "old", WorkingDirectory: t.TempDir(), Approval: model.ApprovalSupervised, Sandbox: model.SandboxWorkspaceWrite}}
			profile, err := service.SaveConfigurationProfile(ctx, save)
			require.NoError(t, err)
			wrapped := &editingDefaultsStore{Store: store, before: func() {
				save.Context.RequestID = "edit"
				save.RevisionID = "two"
				save.ExpectedRevision = 1
				save.Desired.Harness = "codex"
				_, err := service.SaveConfigurationProfile(ctx, save)
				require.NoError(t, err)
			}}
			req := app.SaveConfigurationDefaultsRequest{Context: app.RequestContext{Principal: op, RequestID: "default"}}
			if scope == "global" {
				req.Global = &profile.Revision.Ref
			} else {
				req.Harnesses = map[string]model.ConfigurationProfileRef{"claude": profile.Revision.Ref}
			}
			_, err = app.New(wrapped, providers.NewRegistry()).SaveConfigurationDefaults(ctx, req)
			require.ErrorIs(t, err, app.ErrConflict)
			defaults, err := service.GetConfigurationDefaults(ctx, op)
			require.NoError(t, err)
			require.Zero(t, defaults.Revision)
		})
	}
}
