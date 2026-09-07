package app_test

import (
	"context"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/providers"
	backendsqlite "github.com/tofutools/tclaude/internal/backend/sqlite"
	"path/filepath"
	"testing"
)

func TestPresentationPreferencesRequireOperatorCASAndSurviveReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := backendsqlite.Open(path)
	require.NoError(t, err)
	service := app.New(store, providers.NewRegistry())
	operator := model.OperatorPrincipal()
	result, err := service.ReadPresentation(ctx, operator)
	require.NoError(t, err)
	require.Equal(t, model.DefaultPresentation(), result.Preferences)
	p := result.Preferences
	p.Mode = "wizard"
	p.Channel = "thistle"
	p.MusicVolume = .42
	p.SoundEnabled = true
	_, err = service.PutPresentation(ctx, app.PutPresentationRequest{Principal: model.AgentPrincipal("agent"), Preferences: p})
	require.ErrorIs(t, err, app.ErrUnauthorized)
	saved, err := service.PutPresentation(ctx, app.PutPresentationRequest{Principal: operator, Preferences: p})
	require.NoError(t, err)
	_, err = service.PutPresentation(ctx, app.PutPresentationRequest{Principal: operator, Preferences: p})
	require.ErrorIs(t, err, app.ErrConflict)
	p.Channel = "https://untrusted.invalid"
	_, err = service.PutPresentation(ctx, app.PutPresentationRequest{Principal: operator, Preferences: p, ExpectedRevision: 1})
	require.ErrorIs(t, err, app.ErrInvalid)
	require.NoError(t, store.Close())
	store, err = backendsqlite.Open(path)
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	result, err = app.New(store, providers.NewRegistry()).ReadPresentation(ctx, operator)
	require.NoError(t, err)
	require.Equal(t, saved, result)
}
