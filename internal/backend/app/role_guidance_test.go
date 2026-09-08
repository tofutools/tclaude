package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"strings"
	"testing"
)

func TestRoleGuidancePersistsWithoutPermissionGrants(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "db")
	store, err := sqlite.Open(path)
	require.NoError(t, err)
	service := testService(store, newFakeProvider())
	req := app.PutRoleRequest{Principal: model.OperatorPrincipal(), Role: model.Role{ID: "writer", Name: "Writer", Description: "  Documentation  ", Brief: "First\r\nSecond\rThird"}}
	saved, err := service.PutRole(ctx, req)
	require.NoError(t, err)
	require.Empty(t, saved.Role.Actions)
	require.Equal(t, "Documentation", saved.Role.Description)
	require.Equal(t, "First\nSecond\nThird", saved.Role.Brief)
	require.NoError(t, store.Close())
	store, err = sqlite.Open(path)
	require.NoError(t, err)
	defer store.Close()
	state, err := store.AuthorityState(ctx)
	require.NoError(t, err)
	require.Contains(t, state.Roles, saved.Role)
	service = testService(store, newFakeProvider())
	req.ExpectedRevision = saved.Role.Revision
	req.Role.Brief = strings.Repeat("x", 16385)
	_, err = service.PutRole(ctx, req)
	require.ErrorIs(t, err, app.ErrInvalid)
	req.Role.Brief = ""
	req.Role.Description = ""
	cleared, err := service.PutRole(ctx, req)
	require.NoError(t, err)
	require.Empty(t, cleared.Role.Brief)
	require.Empty(t, cleared.Role.Description)
	require.Empty(t, cleared.Role.Actions)
	_, err = service.PutRole(ctx, req)
	require.ErrorIs(t, err, app.ErrConflict)
}
