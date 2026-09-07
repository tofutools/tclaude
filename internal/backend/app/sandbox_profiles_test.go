package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
)

func TestSandboxCatalogRetainsExactIncludesReceiptsAndCASAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "backend.sqlite")
	store, err := sqlite.Open(dbPath)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	parentReq := app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "parent-create"}, ID: "sandbox_parent", Name: "Parent", Policy: model.SandboxPolicy{Environment: model.Environment{"VALUE": "first"}}}
	parent, err := service.SaveSandboxProfile(ctx, parentReq)
	require.NoError(t, err)
	childReq := app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: operator, RequestID: "child-create"}, ID: "sandbox_child", Name: "Child", Policy: model.SandboxPolicy{Includes: []model.SandboxProfileRef{parent.Revision.Ref}, PreLaunch: []model.SandboxSetupBlock{{Name: "prepare", Script: "printf '%s' \"$VALUE\""}}}}
	child, err := service.SaveSandboxProfile(ctx, childReq)
	require.NoError(t, err)
	update := parentReq
	update.Context.RequestID = "parent-update"
	update.ExpectedRevision = parent.Profile.Revision
	update.Name = "Parent renamed"
	update.Policy.Environment = model.Environment{"VALUE": "second"}
	current, err := service.SaveSandboxProfile(ctx, update)
	require.NoError(t, err)
	require.NotEqual(t, parent.Revision.Ref, current.Revision.Ref)
	stale := update
	stale.Context.RequestID = "stale"
	_, err = service.SaveSandboxProfile(ctx, stale)
	require.ErrorIs(t, err, app.ErrConflict)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(dbPath)
	require.NoError(t, err)
	defer store.Close()
	service = app.New(store, providers.NewRegistry())
	replay, err := service.SaveSandboxProfile(ctx, parentReq)
	require.NoError(t, err)
	require.True(t, replay.Repeated)
	require.Equal(t, parent.Profile, replay.Profile)
	require.Equal(t, parent.Revision, replay.Revision)
	mutated := parentReq
	mutated.ExpectedRevision = current.Profile.Revision
	_, err = service.SaveSandboxProfile(ctx, mutated)
	require.ErrorIs(t, err, app.ErrConflict)
	closure, err := service.InspectSandboxClosure(ctx, operator, child.Revision.Ref)
	require.NoError(t, err)
	require.Len(t, closure.Entries, 2)
	require.Equal(t, "first", closure.Entries[0].Policy.Environment["VALUE"])
	require.Equal(t, childReq.Policy.PreLaunch, closure.Entries[1].Policy.PreLaunch)
	read, err := service.GetSandboxProfile(ctx, operator, parent.Profile.ID)
	require.NoError(t, err)
	require.Equal(t, current.Profile, read.Profile)
	all, err := service.ListSandboxProfiles(ctx, operator, false)
	require.NoError(t, err)
	require.Len(t, all, 2)
}

func TestSandboxCatalogRefusesInvalidAuthorityAndMissingIncludesAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	req := app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: model.AgentPrincipal("caller"), RequestID: "save"}, ID: "sandbox_profile", Name: "Profile"}
	_, err = service.SaveSandboxProfile(ctx, req)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = store.SaveSandboxProfile(ctx, req, "sandbox_revision", time.Now())
	require.ErrorIs(t, err, app.ErrUnauthorized)
	req.Context.Principal = model.OperatorPrincipal()
	req.Policy.Environment = model.Environment{"HOME": "/untrusted"}
	_, err = store.SaveSandboxProfile(ctx, req, "sandbox_revision", time.Now())
	require.ErrorIs(t, err, app.ErrInvalid)
	req.Policy = model.SandboxPolicy{Includes: []model.SandboxProfileRef{{ProfileID: "missing_profile", RevisionID: "missing_revision", ContentHash: strings.Repeat("a", 64)}}}
	_, err = service.SaveSandboxProfile(ctx, req)
	require.ErrorIs(t, err, app.ErrNotFound)
	all, err := service.ListSandboxProfiles(ctx, model.OperatorPrincipal(), true)
	require.NoError(t, err)
	require.Empty(t, all)
	// The refused attempt wrote no request receipt, so correcting its missing
	// include does not require a different identity or leave a partial profile.
	req.Policy = model.SandboxPolicy{}
	saved, err := service.SaveSandboxProfile(ctx, req)
	require.NoError(t, err)
	require.False(t, saved.Repeated)
	_, err = service.GetSandboxProfile(ctx, model.AgentPrincipal("caller"), saved.Profile.ID)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = service.InspectSandboxClosure(ctx, model.AgentPrincipal("caller"), saved.Revision.Ref)
	require.ErrorIs(t, err, app.ErrUnauthorized)
	_, err = service.ListSandboxProfiles(ctx, model.AgentPrincipal("caller"), true)
	require.ErrorIs(t, err, app.ErrUnauthorized)
}

func TestSandboxArchiveRetainsRevisionsAndExactRetry(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	principal := model.OperatorPrincipal()
	req := app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: principal, RequestID: "create"}, ID: "sandbox_profile", Name: "Profile"}
	saved, err := service.SaveSandboxProfile(ctx, req)
	require.NoError(t, err)
	archiveReq := app.SetSandboxProfileArchivedRequest{Context: app.RequestContext{Principal: principal, RequestID: "archive"}, ID: saved.Profile.ID, ExpectedRevision: 1, Archived: true}
	archived, err := service.SetSandboxProfileArchived(ctx, archiveReq)
	require.NoError(t, err)
	require.True(t, archived.Profile.Archived)
	require.Equal(t, saved.Revision, archived.Revision)
	all, err := service.ListSandboxProfiles(ctx, principal, false)
	require.NoError(t, err)
	require.Empty(t, all)
	closure, err := service.InspectSandboxClosure(ctx, principal, saved.Revision.Ref)
	require.NoError(t, err)
	require.Len(t, closure.Entries, 1)
	update := req
	update.Context.RequestID = "edit"
	update.ExpectedRevision = archived.Profile.Revision
	_, err = service.SaveSandboxProfile(ctx, update)
	require.ErrorIs(t, err, app.ErrConflict)
	restore := archiveReq
	restore.Context.RequestID = "restore"
	restore.ExpectedRevision = archived.Profile.Revision
	restore.Archived = false
	restored, err := service.SetSandboxProfileArchived(ctx, restore)
	require.NoError(t, err)
	require.False(t, restored.Profile.Archived)
	replay, err := service.SetSandboxProfileArchived(ctx, archiveReq)
	require.NoError(t, err)
	require.True(t, replay.Repeated)
	require.Equal(t, archived.Profile, replay.Profile)
	read, err := service.GetSandboxProfile(ctx, principal, saved.Profile.ID)
	require.NoError(t, err)
	require.Equal(t, restored.Profile, read.Profile)
	archiveReq.Archived = false
	_, err = service.SetSandboxProfileArchived(ctx, archiveReq)
	require.ErrorIs(t, err, app.ErrConflict)
}

func TestSandboxProfileRefusesUnresolvedOrDuplicateDestinationPacks(t *testing.T) {
	ctx := context.Background()
	store, err := sqlite.Open(filepath.Join(t.TempDir(), "backend.sqlite"))
	require.NoError(t, err)
	defer store.Close()
	service := app.New(store, providers.NewRegistry())
	for _, network := range []model.SandboxNetwork{
		{Baseline: model.SandboxNetworkDeny, Packs: []string{"net-typo"}},
		{Baseline: model.SandboxNetworkDeny, Packs: []string{"net-anthropic", "net-anthropic"}},
		{Baseline: model.SandboxNetworkAllow, DenyPacks: []string{"net-local", "net-local"}},
	} {
		req := app.SaveSandboxProfileRequest{Context: app.RequestContext{Principal: model.OperatorPrincipal(), RequestID: "invalid-pack"}, ID: "invalid_pack", Name: "Invalid", Policy: model.SandboxPolicy{Network: &network}}
		_, err = service.SaveSandboxProfile(ctx, req)
		require.ErrorIs(t, err, app.ErrInvalid)
		_, err = store.SaveSandboxProfile(ctx, req, "invalid_revision", time.Now())
		require.ErrorIs(t, err, app.ErrInvalid)
	}
	profiles, err := service.ListSandboxProfiles(ctx, model.OperatorPrincipal(), true)
	require.NoError(t, err)
	require.Empty(t, profiles)
}
